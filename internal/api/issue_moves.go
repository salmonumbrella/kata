package api

import "go.kenn.io/kata/internal/db"

// IssueTreeMoveRequest is the workspace-independent UID move facade.
type IssueTreeMoveRequest struct {
	Body struct {
		Request db.MoveTreeRequest `json:"request"`
		DryRun  bool               `json:"dry_run,omitempty"`
	}
}

type IssueTreeMoveResponse struct {
	Body struct {
		Preview *db.MoveTreePreview `json:"preview,omitempty"`
		Result  *db.MoveTreeResult  `json:"result,omitempty"`
	}
}

// IssueTreeTransferRequest carries only the authorized ephemeral selection.
// There is no durable operation or transport state in this protocol.
type IssueTreeTransferRequest struct {
	Body struct {
		Request      db.MoveTreeRequest      `json:"request"`
		SourceSeries []db.MoveTreeSeriesRoot `json:"source_series,omitempty"`
		SourceRoots  []db.MoveTreeMember     `json:"source_roots,omitempty"`
		Snapshot     *db.IssueTreeSnapshot   `json:"snapshot,omitempty"`
		Preview      *db.MoveTreePreview     `json:"preview,omitempty"`
		Accepted     *db.MoveTreeResult      `json:"accepted,omitempty"`
	}
}

type IssueTreeTransferResponse struct {
	Body struct {
		Snapshot *db.IssueTreeSnapshot `json:"snapshot,omitempty"`
		Result   *db.MoveTreeResult    `json:"result,omitempty"`
	}
}
