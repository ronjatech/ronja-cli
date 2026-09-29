package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/checkfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The read-only half: what `pipeline status` reports for each checks file.

// pipelineChecksStatus is one checks file's remote state, the read-only twin
// of what applyTableChecks would do.
type pipelineChecksStatus struct {
	Path    string `json:"path"`
	TableID string `json:"tableID,omitempty"`
	// FileDeleted: the file is gone from the folder and its record still owns
	// enabled checks, which are Orphaned (or Drift).
	FileDeleted bool `json:"fileDeleted,omitempty"`
	// Problem is why this file could not be judged: it does not parse, its stem
	// names no .sql file, or the table or its checks could not be read.
	Problem string `json:"problem,omitempty"`
	// PendingPublish says why the checks wait for the table: the live-matches
	// rule does not hold yet. When set, nothing below was compared.
	PendingPublish string `json:"pendingPublish,omitempty"`
	// Pending names the declared checks a push would write (create or update).
	Pending []string `json:"pending,omitempty"`
	// Drift names the owned checks changed in the app since the folder last
	// verified them — a push refuses them without --force.
	Drift []string `json:"drift,omitempty"`
	// Orphaned names the owned checks the file no longer declares — a push
	// refuses them without --prune. An orphan changed in the app since is in
	// Drift instead: a --prune refuses it without --force.
	Orphaned []string `json:"orphaned,omitempty"`
	// Silenced names declared checks that are silenced on the table while the
	// file does not manage `enabled` — they guard nothing.
	Silenced []string `json:"silenced,omitempty"`
	// KindChange names declared checks whose body is the other kind from the
	// live check's (an expression against a watchdog, or the reverse). A push
	// REFUSES these — a check's kind is fixed — so they are not Pending, which
	// would promise a write the push can only fail on.
	KindChange []string `json:"kindChange,omitempty"`
	// Unmanaged counts the checks on the table the folder neither declares nor
	// owns.
	Unmanaged int `json:"unmanaged,omitempty"`
}

// pipelineChecksStatuses judges every checks file, concurrently.
func pipelineChecksStatuses(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec,
	local map[string]string, store liveHashes) []pipelineChecksStatus {

	files, err := readChecksFiles(f.Root, local)
	if err != nil {
		return []pipelineChecksStatus{{Path: wfdir.ChecksDirName, Problem: err.Error()}}
	}
	files = withDeletedChecksFiles(files, store)
	out := make([]pipelineChecksStatus, len(files))
	indices := make([]int, len(files))
	for i := range files {
		indices[i] = i
	}
	fillConcurrently(indices, func(i int) {
		var claim checksClaim
		if files[i].Deleted {
			claim = checksClaimFor(f, files, store, files[i].TableID, files[i].Path)
		}
		out[i] = oneChecksStatus(ctx, client, f, codec, local, store, files[i], claim)
	})
	// A deleted file whose record owns nothing still enabled is bookkeeping for
	// the next push, not state worth a line.
	kept := out[:0]
	for i, st := range out {
		if files[i].Deleted && st.Problem == "" && len(st.Orphaned) == 0 && len(st.Drift) == 0 {
			continue
		}
		kept = append(kept, st)
	}
	return kept
}

func oneChecksStatus(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec,
	local map[string]string, store liveHashes, file pipelineChecksFile, claim checksClaim) pipelineChecksStatus {

	out := pipelineChecksStatus{Path: file.Path, FileDeleted: file.Deleted}
	if file.Problem != "" {
		out.Problem = file.Problem
		return out
	}
	tableID := file.TableID
	if !file.Deleted {
		var pending string
		var err error
		tableID, pending, err = checksQualify(ctx, client, f, codec, local[file.SQLPath], file.SQLPath)
		out.TableID = tableID
		switch {
		case err != nil:
			out.Problem = err.Error()
			return out
		case pending != "":
			out.PendingPublish = pending
			return out
		}
	}
	out.TableID = tableID
	rows, err := client.ListTableChecks(ctx, tableID)
	if err != nil {
		if file.Deleted && api.StatusOf(err) == 404 && !api.IsUnmatchedRoute(err) {
			return out // the table is gone: a push drops the record
		}
		out.Problem = fmt.Sprintf("could not read the checks on %s: %v", tableID, err)
		return out
	}
	managed := map[string]wfdir.LockCheckEntry{}
	if recordedTable, recorded := store.checksSeen(file.Path); recordedTable == tableID {
		managed = recorded
	}
	plan := planTableChecks(file.File.Checks, rows, managed, claim)
	if plan.dup != "" {
		out.Problem = plan.dup
		return out
	}
	seen := map[string]bool{}
	for i, e := range file.File.Checks {
		row := plan.targets[i]
		if row == nil {
			out.Pending = append(out.Pending, e.Name)
			continue
		}
		seen[row.ID] = true
		live := liveCheckOf(row)
		m, owned := plan.owned(row)
		switch {
		case row.Kind != e.Kind():
			out.KindChange = append(out.KindChange, e.Name)
		case !e.Matches(live):
			if owned && m.LiveSHA256 != "" && !checkfile.Unmoved(live, m.LiveSHA256) {
				out.Drift = append(out.Drift, e.Name)
			} else {
				out.Pending = append(out.Pending, e.Name)
			}
		case !row.Enabled && e.Enabled == nil:
			out.Silenced = append(out.Silenced, e.Name)
		}
	}
	for _, o := range plan.orphans {
		seen[o.row.ID] = true
		if o.moved {
			out.Drift = append(out.Drift, o.row.Name)
		} else {
			out.Orphaned = append(out.Orphaned, o.row.Name)
		}
	}
	for _, key := range plan.gone {
		seen[managed[key].CheckID] = true
	}
	for _, row := range rows {
		if !seen[row.ID] && !file.Deleted {
			out.Unmanaged++
		}
	}
	return out
}

// printPipelineChecksStatus renders one checks file's remote state.
func printPipelineChecksStatus(out *os.File, s pipelineChecksStatus) {
	if s.FileDeleted {
		fmt.Fprintf(out, "    %s (deleted — the checks it created are still on the table)\n", s.Path)
	} else {
		fmt.Fprintf(out, "    %s\n", s.Path)
	}
	switch {
	case s.Problem != "":
		fmt.Fprintf(out, "      %s\n", s.Problem)
		return
	case s.PendingPublish != "":
		fmt.Fprintf(out, "      pending publish: %s\n", s.PendingPublish)
		return
	}
	clean := true
	for _, line := range []struct {
		label string
		names []string
		what  string
	}{
		{"pending", s.Pending, "a push would write"},
		{"drift", s.Drift, "changed in the app since your last sync — a push refuses without --force"},
		{"orphaned", s.Orphaned, "no longer in the file — a push refuses without --prune, which silences them"},
		{"silenced", s.Silenced, "silenced on the table — add \"enabled\": true to bring them back"},
		{"refused", s.KindChange, "kind change: the file declares the other kind of check than the table holds, and a check's kind is fixed — give it a new name, and --prune the old one"},
	} {
		if len(line.names) > 0 {
			clean = false
			fmt.Fprintf(out, "      %-9s %s (%s)\n", line.label+":", quoteNames(line.names), line.what)
		}
	}
	if clean {
		fmt.Fprintf(out, "      the table's checks match this file\n")
	}
	if s.Unmanaged > 0 {
		fmt.Fprintf(out, "      %d other %s on the table %s not managed by this file\n", s.Unmanaged, plural(s.Unmanaged, "check"), pluralVerb(s.Unmanaged, "is", "are"))
	}
}
