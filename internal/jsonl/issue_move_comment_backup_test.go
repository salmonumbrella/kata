package jsonl_test

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func rejectedCommentProofRestores(t *testing.T, target db.Storage, backup []byte, eventUID, commentUID, issueUID, sourceUID string, proof db.AttributionReceipt) {
	t.Helper()
	for _, mutation := range []string{"source-actor", "receipt-signature", "comment-identity", "issue-owner", "missing-source"} {
		t.Run("rejected-restore-"+mutation, func(t *testing.T) {
			records, err := jsonl.NewDecoder(bytes.NewReader(backup)).ReadAll(t.Context())
			require.NoError(t, err)
			changed := false
			for i := range records {
				var value any
				switch records[i].Kind {
				case jsonl.KindEvent:
					var event db.EventExport
					require.NoError(t, json.Unmarshal(records[i].Data, &event))
					if event.UID != eventUID {
						continue
					}
					if mutation == "source-actor" {
						var wire struct {
							Event   db.RemoteEvent `json:"event"`
							Payload []byte         `json:"payload"`
						}
						require.NoError(t, json.Unmarshal(event.SourceEventBytes, &wire))
						wire.Event.Actor = "other"
						event.SourceEventBytes, err = json.Marshal(wire)
						require.NoError(t, err)
						value = &event
					} else if mutation == "missing-source" {
						event.SourceEventBytes = nil
						value = &event
					}
				case jsonl.KindEventProvenance:
					if mutation != "receipt-signature" {
						continue
					}
					var receipt db.AttributionReceipt
					require.NoError(t, json.Unmarshal(records[i].Data, &receipt))
					if receipt.EventUID != eventUID {
						continue
					}
					receipt.Signature[0] ^= 1
					value = &receipt
				case jsonl.KindEntityProvenance:
					if mutation != "comment-identity" {
						continue
					}
					var ref db.EntityProvenance
					require.NoError(t, json.Unmarshal(records[i].Data, &ref))
					if ref.EntityUID != commentUID {
						continue
					}
					ref.EntityUID = "01NNNNNNNNNNNNNNNNNNNNNNNN"
					value = &ref
				case jsonl.KindPurgeLog:
					if mutation != "issue-owner" {
						continue
					}
					var owner db.PurgeLogExport
					require.NoError(t, json.Unmarshal(records[i].Data, &owner))
					if owner.IssueUID == nil || *owner.IssueUID != issueUID {
						continue
					}
					owner.IssueUID = new("01NNNNNNNNNNNNNNNNNNNNNNNN")
					value = &owner
				}
				if value != nil {
					records[i].Data, err = json.Marshal(value)
					require.NoError(t, err)
					changed = true
				}
			}
			require.True(t, changed, "mutation must reach the real backup record")
			var invalid bytes.Buffer
			encoder := jsonl.NewEncoder(&invalid)
			for _, record := range records {
				require.NoError(t, encoder.Write(record))
			}
			require.Error(t, jsonl.Import(t.Context(), &invalid, target))
			actual, err := target.EntityAttribution(t.Context(), sourceUID, "comment", commentUID)
			require.NoError(t, err)
			require.Equal(t, proof, actual, "failed validation must not replace the existing target")
		})
	}
}
