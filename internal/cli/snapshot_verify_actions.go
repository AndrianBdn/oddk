package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/urfave/cli/v3"

	"github.com/andrianbdn/oddk/internal/operations"
)

// snapshotVerifyAction scrubs stored snapshot copies: one by id, or every
// catalogued snapshot with --all. Any copy that is missing, damaged, replaced
// or could not be checked makes the command exit non-zero, in --json mode too.
func (c *Client) snapshotVerifyAction(ctx context.Context, cmd *cli.Command) error {
	all := cmd.Bool("all")
	switch {
	case all && cmd.Args().Len() != 0:
		return fmt.Errorf("give either a snapshot id or --all, not both")
	case !all && cmd.Args().Len() != 1:
		return fmt.Errorf("usage: oddk snapshot verify <snapshot-id> | --all")
	}

	var ids []int
	if all {
		resp, err := c.request("GET", "/api/snapshots", nil)
		if err != nil {
			return err
		}
		var records []snapshotRecord
		if err := json.Unmarshal(resp, &records); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		for _, r := range records {
			ids = append(ids, r.ID)
		}
	} else {
		id, err := strconv.Atoi(cmd.Args().First())
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid snapshot id %q", cmd.Args().First())
		}
		ids = []int{id}
	}

	path := "/api/snapshot/%d/verify"
	if cmd.Bool("deep") {
		path += "?deep=true"
	}
	jsonOut := cmd.Bool("json")

	results := []verifyOutcome{}
	failed, checked := 0, 0
	for _, id := range ids {
		resp, err := c.request("POST", fmt.Sprintf(path, id), nil)
		if err != nil {
			if !all {
				return err
			}
			// Retention may remove a snapshot between the listing and its
			// check. It is gone from the catalogue, so there is nothing to fail.
			if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.Status == http.StatusNotFound {
				if !jsonOut {
					_, _ = fmt.Fprintf(c.out, "%s Snapshot %d: removed since the listing\n\n", glyphTodo, id)
				}
				continue
			}
			// Any other error must not hide the remaining snapshots, but it
			// counts, and --json carries it so a script can tell which failed.
			failed++
			results = append(results, verifyOutcome{ID: id, Error: err.Error()})
			if !jsonOut {
				_, _ = fmt.Fprintf(c.out, "%s Snapshot %d: %v\n\n", glyphBad, id, err)
			}
			continue
		}
		var result operations.VerifySnapshotResult
		if err := json.Unmarshal(resp, &result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		results = append(results, verifyOutcome{VerifySnapshotResult: result})
		checked++
		if !result.OK {
			failed++
		}
		if !jsonOut {
			c.printSnapshotVerify(&result)
			_, _ = fmt.Fprintln(c.out)
		}
	}

	if jsonOut {
		var out any = results
		if !all {
			out = results[0]
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.out, "%s\n", data)
	} else if all {
		if len(ids) == 0 {
			_, _ = fmt.Fprintln(c.out, "No snapshots recorded")
		} else {
			_, _ = fmt.Fprintf(c.out, "%d snapshot(s) checked, %d with problems\n", checked, failed)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d snapshot(s) failed verification", failed, len(results))
	}
	return nil
}

// verifyOutcome is one entry of --all --json: the daemon's result, or only the
// id and Error when the request itself failed.
type verifyOutcome struct {
	operations.VerifySnapshotResult
	Error string `json:"error,omitempty"`
}

func (c *Client) printSnapshotVerify(r *operations.VerifySnapshotResult) {
	_, _ = fmt.Fprintf(c.out, "Snapshot %d: %s\n", r.ID, r.Filename)
	printCopy := func(label string, check *operations.SnapshotCopyCheck) {
		if check == nil {
			return
		}
		glyph := glyphOK
		switch {
		case check.Problem():
			glyph = glyphBad
		case check.Status == operations.CopyPresent || check.Status == operations.CopyRemoved:
			glyph = glyphTodo
		}
		line := fmt.Sprintf("  %s %-6s %s", glyph, label, check.Status)
		if check.Status == operations.CopyVerified {
			line += " (SHA-256 matches the catalogue)"
		}
		_, _ = fmt.Fprintln(c.out, line)
		_, _ = fmt.Fprintf(c.out, "           %s\n", check.Location)
		if check.Detail != "" {
			_, _ = fmt.Fprintf(c.out, "           %s\n", check.Detail)
		}
	}
	printCopy("local", r.Local)
	printCopy("s3", r.Remote)
}
