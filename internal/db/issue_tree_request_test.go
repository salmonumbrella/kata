package db_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"pgregory.net/rapid"
	"testing"
)

func TestMoveTreeRequestDefaults(t *testing.T) {
	r := validMoveTreeRequest()
	normalized, err := r.Normalize()
	require.NoError(t, err)
	require.Equal(t, "subtree", normalized.Selection)
	require.Equal(t, "preserve", normalized.ProviderDisposition)
	require.Equal(t, r.RequestUID, normalized.RequestUID)
}

func validMoveTreeRequest() db.MoveTreeRequest {
	return db.MoveTreeRequest{SourceAuthorityUID: newMoveUID(), SourceProjectUID: newMoveUID(), IssueUID: newMoveUID(), DestinationAuthorityUID: newMoveUID(), DestinationProjectUID: newMoveUID(), RequestUID: newMoveUID(), ActorAuthorityUID: newMoveUID(), Actor: "operator"}
}

// The public contract requires stable ULIDs, never names or malformed IDs.
func TestMoveTreeRequestRejectsInvalidUID(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		invalid := rapid.String().Draw(t, "identity")
		if uid.Valid(invalid) {
			return
		}
		r := validMoveTreeRequest()
		fields := []*string{&r.SourceAuthorityUID, &r.SourceProjectUID, &r.IssueUID, &r.DestinationAuthorityUID, &r.DestinationProjectUID, &r.RequestUID, &r.ActorAuthorityUID}
		*fields[rapid.IntRange(0, len(fields)-1).Draw(t, "field")] = invalid
		_, err := r.Normalize()
		require.Error(t, err)
	})
}

func TestMoveTreeRequestRejectsUnsupportedOptions(t *testing.T) {
	for _, mutate := range []func(*db.MoveTreeRequest){func(r *db.MoveTreeRequest) { r.Selection = "all" }, func(r *db.MoveTreeRequest) { r.ProviderDisposition = "drop" }, func(r *db.MoveTreeRequest) { r.Actor = "" }} {
		r := validMoveTreeRequest()
		mutate(&r)
		_, err := r.Normalize()
		require.Error(t, err)
	}
}

func newMoveUID() string {
	value, err := uid.New()
	if err != nil {
		panic(err)
	}
	return value
}
