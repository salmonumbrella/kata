package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

type portableReplicaTestEmbedder struct {
	*fakeEmbedder
	client *embedding.Client
}

func (e *portableReplicaTestEmbedder) Generation() kitvec.Generation {
	return e.client.Generation()
}

func (e *portableReplicaTestEmbedder) ArtifactIdentity(projectUID, issueUID, producerUID string) (embedding.ArtifactIdentity, error) {
	return e.client.ArtifactIdentity(projectUID, issueUID, producerUID)
}

// The portable-artifact worker must honor the same hub-owned document
// exclusion as ordinary fill, even while the hub has not computed a vector.
func TestReplicaImportWithPortableArtifactsKeepsHubPendingRowsOffProvider(t *testing.T) {
	f := newReplicaFixture(t)
	client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m1", Dims: 4})
	require.NoError(t, err)
	emb := &portableReplicaTestEmbedder{fakeEmbedder: f.emb, client: client}
	f.up.generation = generationDescriptor(emb.Generation())
	pending := f.issues[0]
	delete(f.up.docs, pending.UID)
	r := NewReconciler(f.store, f.idx, emb, ReconcilerConfig{
		BatchSize: 64, Upstream: f.up, Now: func() time.Time { return f.now },
	})
	require.NoError(t, r.reconcileOnce(f.ctx))
	assert.Equal(t, 1, f.emb.n, "only the local issue may reach the portable-artifact encoder")
	backlog, err := f.idx.Backlog(f.ctx, emb.Generation().Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, int64(1), backlog, "the missing hub vector must remain pending")
	health := r.Health()
	assert.Equal(t, int64(1), health.Replicated)
	assert.Equal(t, int64(1), health.AwaitingUpstream)
}

func TestReplicaImportWithPortableArtifactsRetainsActiveGenerationUntilHubReady(t *testing.T) {
	f := newReplicaFixture(t)
	client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m1", Dims: 4})
	require.NoError(t, err)
	emb := &portableReplicaTestEmbedder{fakeEmbedder: f.emb, client: client}
	f.up.generation = generationDescriptor(emb.Generation())
	r := NewReconciler(f.store, f.idx, emb, ReconcilerConfig{
		BatchSize: 64, Upstream: f.up, Now: func() time.Time { return f.now },
	})
	require.NoError(t, r.reconcileOnce(f.ctx))
	oldKey := emb.Generation().Fingerprint()
	emb.client, err = embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m2", Dims: 4})
	require.NoError(t, err)
	f.up.generation = generationDescriptor(emb.Generation())
	pending := f.issues[0]
	delete(f.up.docs, pending.UID)
	require.NoError(t, r.reconcileOnce(f.ctx))
	active, exists, err := f.idx.ActiveGeneration(f.ctx)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, oldKey, active, "waiting for a hub vector must hold the complete active generation")
	oldVectors, err := f.idx.LookupVectors(f.ctx, oldKey, f.replica.UID, []string{pending.UID})
	require.NoError(t, err)
	require.NotEmpty(t, oldVectors[pending.UID].Chunks)
	assert.Equal(t, 2, f.emb.n, "only the local issue is encoded once for each model")
	f.up.setDoc(pending.UID, fakeHubDoc{content: embedding.EmbedText(pending.Title, pending.Body)})
	f.now = f.now.Add(31 * time.Second)
	require.NoError(t, r.reconcileOnce(f.ctx))
	active, exists, err = f.idx.ActiveGeneration(f.ctx)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, emb.Generation().Fingerprint(), active)
	assert.Equal(t, 2, f.emb.n, "receiving the pending hub vector must not encode it locally")
}
