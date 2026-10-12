package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newMoveCmd() *cobra.Command {
	var opts moveTreeOptions
	cmd := &cobra.Command{
		Use:   "move <issue-ref>... <project>",
		Short: "move complete issue trees to another project",
		Long:  "Move complete issue trees, including completed and deleted children, preserving UIDs and history. Overlapping selections move once. Use --issue-only to select only each named issue.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMoveSelection(cmd, args[:len(args)-1], args[len(args)-1], opts)
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry", false, "preview the complete selection without changing work")
	cmd.Flags().BoolVar(&opts.IssueOnly, "issue-only", false, "select only the named issues")
	cmd.Flags().BoolVar(&opts.MoveSeries, "move-series", false, "include recurrence owners and their occurrences")
	cmd.Flags().StringVar(&opts.ProviderDisposition, "provider-disposition", "preserve", "provider ownership: preserve or detach")
	cmd.Flags().StringVar(&opts.ToDaemon, "to-daemon", "", "resolve the destination through this configured daemon")
	cmd.Flags().StringVar(&opts.RequestUID, "request-uid", "", "stable request UID for one tree and its exact retries")
	cmd.Flags().StringVar(&opts.PreviewDigest, "preview-digest", "", "reuse the original preview digest on an exact retry")
	cmd.Flags().StringVar(&opts.SourceProjectUID, "source-project-uid", "", "original source project UID when retrying a moved full issue UID")
	addCommentFlag(cmd)
	return cmd
}

type moveIssueWire struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	ShortID   string `json:"short_id"`
	Revision  int64  `json:"revision"`
}

type moveResponseWire struct {
	Issue      moveIssueWire `json:"issue"`
	EventID    int64         `json:"event_id"`
	NewShortID string        `json:"new_short_id"`
	Changed    bool          `json:"changed"`
}

func runMove(cmd *cobra.Command, rawRef, targetProject string, dryRun bool) error {
	return runMoveSelection(cmd, []string{rawRef}, targetProject, moveTreeOptions{DryRun: dryRun, ProviderDisposition: "preserve"})
}

func fetchMoveIssue(ctx context.Context, client *http.Client, baseURL string, projectID int64, ref string) (moveIssueWire, error) {
	_, bs, err := fetchMetaIssue(ctx, client, baseURL, projectID, ref)
	if err != nil {
		return moveIssueWire{}, err
	}
	var out struct {
		Issue moveIssueWire `json:"issue"`
	}
	if err := json.Unmarshal(bs, &out); err != nil {
		return moveIssueWire{}, err
	}
	return out.Issue, nil
}

func printMove(cmd *cobra.Command, bs []byte, sourceProject, oldShortID, targetProject string) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var b moveResponseWire
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	oldRef := moveQualifiedID(sourceProject, oldShortID)
	newRef := moveQualifiedID(targetProject, b.NewShortID)
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK move %s changed=%t dry_run=%t\n",
			agentValue(oldRef), b.Changed, false); err != nil {
			return err
		}
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("from", oldRef),
			agentRowField("to", newRef),
			agentRowField("event_id", strconv.FormatInt(b.EventID, 10)),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s to %s\n",
		"moved", textsafe.Line(oldRef), textsafe.Line(newRef))
	return err
}

func printMovePreview(cmd *cobra.Command, sourceProject, oldShortID, targetProject string) error {
	if flags.Quiet {
		return nil
	}
	oldRef := moveQualifiedID(sourceProject, oldShortID)
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		payload := map[string]any{
			"dry_run":         true,
			"changed":         false,
			"from":            oldRef,
			"to_project":      targetProject,
			"target_ref_note": "target short_id is assigned by the daemon during move",
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK move %s changed=false dry_run=true\n", agentValue(oldRef)); err != nil {
			return err
		}
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("from", oldRef),
			agentRowField("to_project", targetProject),
		)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"dry-run: would move %s to project %s; target short_id will be assigned by the daemon\n",
		textsafe.Line(oldRef), textsafe.Line(targetProject))
	return err
}

func moveQualifiedID(projectName, shortID string) string {
	return projectName + "#" + shortID
}
