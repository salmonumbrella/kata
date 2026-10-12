package db_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json/v2"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func retainedCommentBackup(t testing.TB) []db.ImportRecord {
	t.Helper()
	seed := sha256.Sum256([]byte("original-comment-proof"))
	private := ed25519.NewKeyFromSeed(seed[:])
	public := private.Public().(ed25519.PublicKey)
	project := &db.ProjectExport{ID: 42, UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Name: "current-project"}
	originalUID := "01BBBBBBBBBBBBBBBBBBBBBBBB"
	issueUID := "01CCCCCCCCCCCCCCCCCCCCCCCC"
	commentUID := "01DDDDDDDDDDDDDDDDDDDDDDDD"
	event := &db.EventExport{ID: 1, UID: "01EEEEEEEEEEEEEEEEEEEEEEEE", OriginInstanceUID: "01FFFFFFFFFFFFFFFFFFFFFFFF", ProjectID: 0, SourceProjectUID: &originalUID, ProjectName: "original-project", IssueUID: &issueUID, Type: "issue.commented", Actor: "assistant", Payload: []byte(`{"comment_uid":"` + commentUID + `","body":"Signed original message","teammate":"writer"}`), HLCPhysicalMS: 1, CreatedAt: "2026-10-12T00:00:00.000Z"}
	require.NoError(t, db.PrepareReplayEvent(event, originalUID, event.ProjectName, true))
	remote := db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: originalUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload, HLCPhysicalMS: event.HLCPhysicalMS, ContentHash: event.ContentHash, CreatedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	raw, err := db.EncodeRelaySourceEvent(remote)
	require.NoError(t, err)
	event.SourceEventBytes = raw
	pin := &db.RootKeyPin{ProjectUID: originalUID, AuthorityUID: "01GGGGGGGGGGGGGGGGGGGGGGGG", KeyID: db.RootPublicKeyID(public), PublicKey: public, Retired: true}
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: originalUID, EventUID: event.UID, ContentHash: event.ContentHash, AuthorityUID: pin.AuthorityUID, AccountableActor: "member", SourceActor: event.Actor, Teammate: "writer", IngressInstanceUID: event.OriginInstanceUID, AcceptedAt: remote.CreatedAt, ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, private)
	require.NoError(t, err)
	ref := &db.EntityProvenance{ProjectUID: originalUID, Kind: "comment", EntityUID: commentUID, EventUID: event.UID}
	tombstone := &db.PurgeLogExport{ID: 1, UID: "01HHHHHHHHHHHHHHHHHHHHHHHH", ProjectID: project.ID, IssueUID: &issueUID, ProjectUID: &project.UID}
	return []db.ImportRecord{project, tombstone, event, pin, &receipt, ref}
}

func TestRetainedCommentProofRequiresExactOwnerAndSource(t *testing.T) {
	require.NoError(t, db.ValidateImportReplay(retainedCommentBackup(t), db.ImportOptions{}))
	for _, mutation := range []string{"source-bytes", "source-hash", "event-actor", "source-project", "comment-identity", "issue-owner", "receipt-actor", "missing-source", "missing-owner"} {
		t.Run(mutation, func(t *testing.T) {
			records := retainedCommentBackup(t)
			event := records[2].(*db.EventExport)
			switch mutation {
			case "source-bytes":
				event.SourceEventBytes = []byte(`{"marker":"not a creation envelope"}`)
			case "source-hash":
				var remote struct {
					Event   db.RemoteEvent `json:"event"`
					Payload []byte         `json:"payload"`
				}
				require.NoError(t, json.Unmarshal(event.SourceEventBytes, &remote))
				remote.Event.Actor = "other"
				raw, err := json.Marshal(remote)
				require.NoError(t, err)
				event.SourceEventBytes = raw
			case "event-actor":
				event.Actor = "other"
			case "source-project":
				event.SourceProjectUID = new("01JJJJJJJJJJJJJJJJJJJJJJJJ")
			case "comment-identity":
				records[5].(*db.EntityProvenance).EntityUID = "01JJJJJJJJJJJJJJJJJJJJJJJJ"
			case "issue-owner":
				records[1].(*db.PurgeLogExport).IssueUID = new("01JJJJJJJJJJJJJJJJJJJJJJJJ")
			case "receipt-actor":
				records[4].(*db.AttributionReceipt).AccountableActor = "other"
			case "missing-source":
				event.SourceEventBytes = nil
			case "missing-owner":
				records = append(records[:1], records[2:]...)
			}
			require.Error(t, db.ValidateImportReplay(records, db.ImportOptions{}))
		})
	}
}

// Any changed source actor must invalidate an immutable signed envelope. The
// generator keeps the complete string domain, including empty and Unicode text.
func FuzzRetainedCommentSourceActor(f *testing.F) {
	f.Add("other")
	f.Add("")
	f.Add("assistant")
	f.Add("雪")
	f.Fuzz(func(t *testing.T, actor string) {
		records := retainedCommentBackup(t)
		event := records[2].(*db.EventExport)
		var original struct {
			Event   db.RemoteEvent `json:"event"`
			Payload []byte         `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(event.SourceEventBytes, &original))
		original.Event.Actor = actor
		raw, err := json.Marshal(original)
		if err != nil {
			require.False(t, utf8.ValidString(actor), "only invalid JSON strings may fail materialization")
			return
		}
		event.SourceEventBytes = raw
		err = db.ValidateImportReplay(records, db.ImportOptions{})
		if actor == "assistant" {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	})
}

// A duplicate event UID cannot conceal earlier corrupt proof bytes from the
// pre-replacement validator, even when the later copy is otherwise valid.
func TestRetainedCommentReplayRejectsDuplicateSourceIdentity(t *testing.T) {
	records := retainedCommentBackup(t)
	bad := *records[2].(*db.EventExport)
	bad.SourceEventBytes = []byte(`{"marker":"not an original source"}`)
	records = append(records[:2], append([]db.ImportRecord{&bad}, records[2:]...)...)
	require.Error(t, db.ValidateImportReplay(records, db.ImportOptions{}))
}
