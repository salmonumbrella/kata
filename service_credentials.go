package kata

import (
	"context"
	"errors"
	"slices"
	"sync"

	"go.kenn.io/kata/internal/config"
)

// FederationCredential is the secret material and local recovery state used
// by one spoke project to communicate with its hub.
type FederationCredential struct {
	HubURL        string
	HubProjectID  int64
	Token         string
	Capabilities  string
	Actor         string
	AllowInsecure bool
	// NativeLeave stores the original binding flags and outstanding
	// preparations needed to restore a native spoke leave after restart.
	NativeLeave *FederationNativeLeaveState
}

// FederationNativeLeaveState is temporary recovery state for an abortable
// native spoke leave. Credential stores must retain it across service restarts.
type FederationNativeLeaveState struct {
	Enabled           bool
	PushEnabled       bool
	CreatedCredential bool
	Preparations      []FederationNativeLeavePreparation
}

// FederationNativeLeavePreparation identifies one pending leave preparation.
type FederationNativeLeavePreparation struct {
	ID   string
	Keys []string
}

// FederationCredentialStore supplies service-scoped federation credentials.
// Implementations must be safe for concurrent use and retain every credential
// field, including NativeLeave, when storing a value. Callers that need
// credentials and recovery state to survive process restarts should provide a
// durable store; otherwise each Service receives its own in-memory store.
type FederationCredentialStore interface {
	FederationCredential(context.Context, string) (FederationCredential, bool, error)
	StoreFederationCredential(context.Context, string, FederationCredential) error
	DeleteFederationCredential(context.Context, string) error
}

type serviceCredentialStoreAdapter struct {
	store FederationCredentialStore
}

var errManagedFederationCredentialsUnsupported = errors.New(
	"mounted federation credential store does not support managed operations",
)

func (a serviceCredentialStoreAdapter) FederationCredential(
	ctx context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	credential, ok, err := a.store.FederationCredential(ctx, projectUID)
	if err != nil || !ok {
		return config.FederationCredential{}, ok, err
	}
	return internalFederationCredential(credential), true, nil
}

func (a serviceCredentialStoreAdapter) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	return a.store.StoreFederationCredential(ctx, projectUID, publicFederationCredential(credential))
}

func (a serviceCredentialStoreAdapter) DeleteFederationCredential(ctx context.Context, projectUID string) error {
	return a.store.DeleteFederationCredential(ctx, projectUID)
}

// Public mounted credentials cannot carry the management marker or spoke
// project name, so they are structurally unmanaged. The internal adapter
// exposes that fact to shared daemon services without expanding the public
// three-method credential-store contract.
func (serviceCredentialStoreAdapter) FindManagedFederationCredential(
	context.Context, string,
) (config.FederationManagedCredentialReservation, bool, error) {
	return config.FederationManagedCredentialReservation{}, false, nil
}

func (serviceCredentialStoreAdapter) ListManagedFederationCredentials(context.Context) ([]config.FederationManagedCredentialReservation, error) {
	return nil, nil
}

func (serviceCredentialStoreAdapter) ReserveManagedFederationCredential(
	context.Context, config.FederationManagedCredentialReservation,
) error {
	return errManagedFederationCredentialsUnsupported
}

func (serviceCredentialStoreAdapter) RekeyFederationCredential(
	context.Context, config.FederationCredentialRekey,
) error {
	return errManagedFederationCredentialsUnsupported
}

func (serviceCredentialStoreAdapter) DeleteManagedFederationCredential(
	context.Context, config.FederationManagedCredentialReservation,
) error {
	return errManagedFederationCredentialsUnsupported
}

func (serviceCredentialStoreAdapter) ReplaceManagedFederationCredential(
	context.Context,
	config.FederationManagedCredentialReservation,
	config.FederationManagedCredentialReservation,
) error {
	return errManagedFederationCredentialsUnsupported
}

func internalFederationCredential(credential FederationCredential) config.FederationCredential {
	return config.FederationCredential{
		HubURL:        credential.HubURL,
		HubProjectID:  credential.HubProjectID,
		Token:         credential.Token,
		Capabilities:  credential.Capabilities,
		Actor:         credential.Actor,
		AllowInsecure: credential.AllowInsecure,
		NativeLeave:   internalFederationNativeLeaveState(credential.NativeLeave),
	}
}

func publicFederationCredential(credential config.FederationCredential) FederationCredential {
	return FederationCredential{
		HubURL:        credential.HubURL,
		HubProjectID:  credential.HubProjectID,
		Token:         credential.Token,
		Capabilities:  credential.Capabilities,
		Actor:         credential.Actor,
		AllowInsecure: credential.AllowInsecure,
		NativeLeave:   publicFederationNativeLeaveState(credential.NativeLeave),
	}
}

func internalFederationNativeLeaveState(state *FederationNativeLeaveState) *config.FederationNativeLeaveState {
	if state == nil {
		return nil
	}
	replacement := &config.FederationNativeLeaveState{
		Enabled: state.Enabled, PushEnabled: state.PushEnabled,
		CreatedCredential: state.CreatedCredential,
		Preparations:      make([]config.FederationNativeLeavePreparation, len(state.Preparations)),
	}
	for i, preparation := range state.Preparations {
		replacement.Preparations[i] = config.FederationNativeLeavePreparation{
			ID: preparation.ID, Keys: slices.Clone(preparation.Keys),
		}
	}
	return replacement
}

func publicFederationNativeLeaveState(state *config.FederationNativeLeaveState) *FederationNativeLeaveState {
	if state == nil {
		return nil
	}
	replacement := &FederationNativeLeaveState{
		Enabled: state.Enabled, PushEnabled: state.PushEnabled,
		CreatedCredential: state.CreatedCredential,
		Preparations:      make([]FederationNativeLeavePreparation, len(state.Preparations)),
	}
	for i, preparation := range state.Preparations {
		replacement.Preparations[i] = FederationNativeLeavePreparation{
			ID: preparation.ID, Keys: slices.Clone(preparation.Keys),
		}
	}
	return replacement
}

type memoryFederationCredentialStore struct {
	mu      sync.RWMutex
	entries map[string]FederationCredential
}

func newMemoryFederationCredentialStore() *memoryFederationCredentialStore {
	return &memoryFederationCredentialStore{entries: make(map[string]FederationCredential)}
}

func (s *memoryFederationCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (FederationCredential, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	credential, ok := s.entries[projectUID]
	return credential, ok, nil
}

func (s *memoryFederationCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential FederationCredential,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[projectUID] = credential
	return nil
}

func (s *memoryFederationCredentialStore) DeleteFederationCredential(
	_ context.Context, projectUID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, projectUID)
	return nil
}
