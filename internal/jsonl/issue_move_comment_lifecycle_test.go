package jsonl_test

import "testing"

func TestMovedCommentProofAfterSourceAdoption(t *testing.T) { movedCommentProofLifecycle(t, "adopt") }

func TestMovedCommentProofAfterOwnerProjectPurge(t *testing.T) {
	movedCommentProofLifecycle(t, "owner-project-purge")
}

func TestMovedCommentProofAfterSignedSourceReset(t *testing.T) {
	movedCommentProofLifecycle(t, "signed-reset")
}

func TestMovedCommentProofAfterOldSignedSourceReset(t *testing.T) {
	movedCommentProofLifecycle(t, "signed-reset-old")
}

func TestMovedCommentProofAfterCompaction(t *testing.T) { movedCommentProofLifecycle(t, "compact") }
