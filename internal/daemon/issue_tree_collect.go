package daemon

import (
	"context"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type issueTreeCollection struct {
	snapshot db.IssueTreeSnapshot
	sources  map[string]db.IssueTreeSnapshot
	roots    map[string][]db.MoveTreeMember
	series   map[string][]db.MoveTreeSeriesRoot
	targets  map[string]*issueTreeHTTP
}

// Every portion is collected before one destination accept. Growing selectors
// are re-exported together in a consistent native read, normalizing overlap.
func collectIssueTreeHTTP(ctx context.Context, cfg ServerConfig, r db.MoveTreeRequest, initial db.IssueTreeSnapshot) (issueTreeCollection, error) {
	collection := issueTreeCollection{sources: map[string]db.IssueTreeSnapshot{cfg.DB.InstanceUID(): initial}, roots: map[string][]db.MoveTreeMember{cfg.DB.InstanceUID(): {{IssueUID: r.IssueUID, AuthorityUID: r.SourceAuthorityUID, ProjectUID: r.SourceProjectUID}}}, series: map[string][]db.MoveTreeSeriesRoot{}, targets: map[string]*issueTreeHTTP{}}
	for round := 0; round < 1024; round++ {
		changed := map[string]bool{}
		for _, snapshot := range collection.sources {
			for _, required := range snapshot.RequiredSeries {
				if !slices.Contains(collection.series[required.AuthorityUID], required) {
					collection.series[required.AuthorityUID] = append(collection.series[required.AuthorityUID], required)
					changed[required.AuthorityUID] = true
				}
			}

			for _, required := range snapshot.Required {
				known := false
				for _, root := range collection.roots[required.AuthorityUID] {
					if root.IssueUID == required.IssueUID && root.ProjectUID == required.ProjectUID {
						known = true
						break
					}
				}
				if !known {
					collection.roots[required.AuthorityUID] = append(collection.roots[required.AuthorityUID], required)
					changed[required.AuthorityUID] = true
				}
			}
		}
		if len(changed) == 0 {
			// Every required UID must be present at its actual authority. A routing
			// loop or vanished body never becomes an incomplete successful union.
			for _, snapshot := range collection.sources {
				for _, required := range snapshot.RequiredSeries {
					found := false
					for _, portion := range collection.sources[required.AuthorityUID].Portions {
						for _, series := range portion.Recurrences {
							if series.UID == required.RecurrenceUID {
								found = true
							}
						}
					}
					if !found {
						return collection, api.NewError(409, "remote-selection-required", "selected series owner could not be resolved", "resolve the actual scheduler authority", nil)
					}
				}

				for _, required := range snapshot.Required {
					found := false
					for _, portion := range collection.sources[required.AuthorityUID].Portions {
						for _, member := range portion.Members {
							if member.IssueUID == required.IssueUID {
								found = true
							}
						}
					}
					if !found {
						return collection, api.NewError(409, "remote-selection-required", "selected current authority could not be resolved", "reconcile the retained routing references", nil)
					}
				}
			}
			authorities := []string{}
			for authority := range collection.sources {
				authorities = append(authorities, authority)
			}
			slices.Sort(authorities)
			collection.snapshot = db.IssueTreeSnapshot{Version: 1, RequestUID: r.RequestUID}
			for _, authority := range authorities {
				for _, portion := range collection.sources[authority].Portions {
					portion.SourceSnapshotDigest = collection.sources[authority].Digest
					collection.snapshot.Portions = append(collection.snapshot.Portions, portion)
				}
			}
			var err error
			collection.snapshot.Digest, err = db.IssueTreeSnapshotDigest(collection.snapshot)
			return collection, err
		}
		authorities := []string{}
		for authority := range changed {
			authorities = append(authorities, authority)
		}
		slices.Sort(authorities)
		for _, authority := range authorities {
			roots := collection.roots[authority]
			slices.SortFunc(roots, func(a, b db.MoveTreeMember) int {
				if a.IssueUID < b.IssueUID {
					return -1
				}
				if a.IssueUID > b.IssueUID {
					return 1
				}
				return 0
			})
			if authority == cfg.DB.InstanceUID() {
				snapshot, err := cfg.DB.ExportIssueTree(db.WithIssueTreeSourceSeries(db.WithIssueTreeSourceRoots(ctx, roots), collection.series[authority]), r)
				if err != nil {
					return collection, issueTreeAPIError(err)
				}
				collection.sources[authority] = snapshot
				continue
			}
			target := collection.targets[authority]
			if target == nil {
				var err error
				target, err = issueTreeTarget(ctx, cfg, authority, r.Actor)
				if err != nil {
					return collection, api.NewError(404, "issue_move_unavailable", "selected work is unavailable", "resolve access to the actual source authorities", nil)
				}
				collection.targets[authority] = target
			}
			in := &api.IssueTreeTransferRequest{}
			in.Body.Request = r
			in.Body.SourceRoots = roots
			in.Body.SourceSeries = collection.series[authority]
			out := &api.IssueTreeTransferResponse{}
			if err := target.call(ctx, "POST", "/api/v1/issue-moves/export", in.Body, &out.Body); err != nil {
				return collection, api.NewError(404, "issue_move_unavailable", "selected work is unavailable", "resolve access to the actual source authorities", nil)
			}
			if out.Body.Snapshot == nil {
				return collection, fmt.Errorf("source authority returned no selection")
			}
			if err := db.ValidateIssueTreeSnapshotDigest(*out.Body.Snapshot); err != nil {
				return collection, err
			}
			if out.Body.Snapshot.RequestUID != r.RequestUID {
				return collection, db.ErrMoveConflict
			}
			for _, portion := range out.Body.Snapshot.Portions {
				if portion.AuthorityUID != authority {
					return collection, db.ErrMoveAuthorityMismatch
				}
			}
			collection.sources[authority] = *out.Body.Snapshot
		}
	}
	return collection, fmt.Errorf("move source routing exceeded collection bound")
}
