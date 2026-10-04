package commands

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/checkfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Applying one checks file to its table: the push/publish pass, the plan, and
// the per-check writes. See pipeline_checks.go for the load-bearing order.

// pipelineChecksResult is what happened to one checks file.
type pipelineChecksResult struct {
	Path    string `json:"path"`
	TableID string `json:"tableID,omitempty"`
	// FileDeleted: the checks file is gone from the folder, and this is what
	// was done about the checks it had created.
	FileDeleted bool `json:"fileDeleted,omitempty"`
	// Outcome is set when something decided the WHOLE file — pending publish,
	// drift, orphans, an unsupported server, a refusal before any check was
	// looked at. Otherwise empty, and Checks says what happened per check.
	Outcome string                `json:"outcome,omitempty"`
	Error   string                `json:"error,omitempty"`
	Checks  []pipelineCheckResult `json:"checks,omitempty"`
	// Unmanaged names the checks on the table that are neither in the file nor
	// owned by the folder. Never touched.
	Unmanaged []string `json:"unmanaged,omitempty"`

	// unknown is set when this file's checks did not fully apply for want of an
	// ANSWER — a timeout, a 5xx, a dropped connection, or a read-back that could
	// not be read — rather than a refusal. `sync apply` reports that `unknown`,
	// never `refused`: a write that timed out may well have landed.
	unknown error
}

// pipelineCheckResult is what happened to one check.
type pipelineCheckResult struct {
	Name    string `json:"name"`
	CheckID string `json:"checkID,omitempty"`
	Outcome string `json:"outcome"`
	// Changed names the fields this push wrote (the declared ones that differed).
	Changed []string `json:"changed,omitempty"`
	// Verdict is the check's result when this push ran it: pass or fail. A fail
	// is not a push failure — the check was stored and said something true.
	Verdict string `json:"verdict,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`

	// unknown: see pipelineChecksResult.unknown.
	unknown error
}

// unanswered is the first failure of this file's checks that had no answer.
func (r pipelineChecksResult) unanswered() error {
	if r.unknown != nil {
		return r.unknown
	}
	for _, c := range r.Checks {
		if c.unknown != nil && checksOutcomeFails(c.Outcome) {
			return c.unknown
		}
	}
	return nil
}

// noAnswer returns err when it is a failure to get an answer rather than an
// answer, and nil otherwise.
func noAnswer(err error) error {
	if api.Unanswered(err) || api.IsTimeout(err) {
		return err
	}
	return nil
}

// checksNotAppliedError is a push or publish whose ONLY problems were in the
// health-check leg. Typed, so `sync apply` can publish a folder's SQL past it:
// checks are ungoverned and advisory — "a failing check never blocks anything"
// — so an unfinished checks leg must not hold a table deploy hostage. It is
// still an error: the command exits non-zero and says why.
type checksNotAppliedError struct {
	msg string
	// problems names each checks file that did not fully apply, and why.
	problems []string
	// cause is non-nil when a problem was a failure to get an answer.
	cause error
}

func (e *checksNotAppliedError) Error() string { return e.msg }
func (e *checksNotAppliedError) Unwrap() error { return e.cause }

// newChecksNotApplied reads the failed files out of a push's or publish's
// checks results.
func newChecksNotApplied(msg string, results []pipelineChecksResult) *checksNotAppliedError {
	out := &checksNotAppliedError{msg: msg}
	for _, r := range results {
		if !r.failed() {
			continue
		}
		if out.cause == nil {
			out.cause = r.unanswered()
		}
		why := r.Error
		outcome := r.Outcome
		if outcome == "" {
			for _, c := range r.Checks {
				if checksOutcomeFails(c.Outcome) {
					outcome, why = c.Outcome, fmt.Sprintf("%q: %s", c.Name, c.Error)
					break
				}
			}
		}
		out.problems = append(out.problems, fmt.Sprintf("%s %s (%s)", r.Path, strings.ReplaceAll(outcome, "_", " "), why))
	}
	return out
}

func (r pipelineChecksResult) failed() bool {
	if checksOutcomeFails(r.Outcome) {
		return true
	}
	for _, c := range r.Checks {
		if checksOutcomeFails(c.Outcome) {
			return true
		}
	}
	return false
}

func (r pipelineChecksResult) settled() bool {
	if r.Outcome != "" {
		return false
	}
	for _, c := range r.Checks {
		if !checksOutcomeSettled(c.Outcome) {
			return false
		}
	}
	return true
}

type checksApplyOptions struct {
	Prune bool
	Force bool
}

// pushChecksFiles is the push's checks pass: each file whose table satisfies the
// live-matches rule is applied, every other one is pending publish.
func pushChecksFiles(ctx context.Context, client *api.Client, f *folder, inst *wfdir.InstanceState,
	codec pipelineCodec, local map[string]string, files []pipelineChecksFile, opts checksApplyOptions) []pipelineChecksResult {

	if len(files) == 0 {
		return nil
	}
	out := make([]pipelineChecksResult, 0, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			out = append(out, pipelineChecksResult{Path: file.Path, Outcome: checksOutcomeRefused, Error: interruptedMessage})
			continue
		}
		var claim checksClaim
		if file.Deleted {
			claim = checksClaimFor(f, files, f.live(inst), file.TableID, file.Path)
		}
		result := applyChecksFile(ctx, client, f, inst, codec, local, file, opts, claim)
		// A deleted file whose checks are all gone or silenced leaves nothing to
		// report: its record is dropped and that is the whole of it.
		if !file.Deleted || result.Outcome != "" || len(result.Checks) > 0 {
			out = append(out, result)
		}
		if err := f.saveBaseline(); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not record the checks %s applied (%v).\n", file.Path, err)
		}
	}
	return out
}

// applyChecksFile is one file through the live-matches rule and, when it holds,
// through applyTableChecks.
func applyChecksFile(ctx context.Context, client *api.Client, f *folder, inst *wfdir.InstanceState,
	codec pipelineCodec, local map[string]string, file pipelineChecksFile, opts checksApplyOptions, claim checksClaim) pipelineChecksResult {

	out := pipelineChecksResult{Path: file.Path}
	if file.Deleted {
		// No live-matches rule: nothing is created or run, only silenced.
		return applyTableChecks(ctx, client, f.live(inst), file, file.TableID, opts, claim)
	}
	if file.Problem != "" {
		out.Outcome, out.Error = checksOutcomeRefused, file.Problem
		return out
	}
	tableID, pending, err := checksQualify(ctx, client, f, codec, local[file.SQLPath], file.SQLPath)
	out.TableID = tableID
	switch {
	case err != nil:
		out.Outcome, out.Error, out.unknown = checksOutcomeRefused, err.Error(), noAnswer(err)
		return out
	case pending != "":
		out.Outcome, out.Error = checksOutcomePendingPublish, pending
		return out
	}
	return applyTableChecks(ctx, client, f.live(inst), file, tableID, opts, claim)
}

// checksQualify is the live-matches rule: the live table exists, the caller has
// no open draft of it, and its SQL equals the file — read from the LIVE ROW, the
// same comparison status makes for its live leg, never from the lock (the lock
// records agreement at checkout of a table that was never built).
//
// A non-empty `pending` says why the checks wait. Whether the live row has ever
// BUILT is the server's rule, not this one: a check write on a never-built table
// answers 409 table_not_built, which is pending too.
func checksQualify(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec, content, sqlPath string) (tableID, pending string, err error) {
	tableID = f.Binding.Tables[sqlPath]
	if tableID == "" {
		return "", fmt.Sprintf("%s has no table yet — its checks are applied once a push creates it and a publish builds it", sqlPath), nil
	}
	draft, err := client.GetTableDraft(ctx, tableID)
	if err != nil {
		return tableID, "", fmt.Errorf("check for your draft of %s: %w", tableID, err)
	}
	if draft != nil {
		return tableID, fmt.Sprintf("you have an open draft of %s — checks attach to the live table, so they are applied when `ronja pipeline publish` commits it", sqlPath), nil
	}
	live, err := client.GetTable(ctx, tableID)
	if err != nil {
		return tableID, "", fmt.Errorf("read %s: %w", tableID, err)
	}
	code, unresolved := codec.canonicalDisk(content, live)
	if unresolved || code != content {
		return tableID, fmt.Sprintf("the live table %s does not hold %s as it stands — push and publish it; its checks are applied once it does", tableID, sqlPath), nil
	}
	return tableID, "", nil
}

// liveCheckOf is the part of a server row the checkfile projection compares.
func liveCheckOf(row *api.TableCheck) checkfile.Live {
	return checkfile.Live{
		Name: row.Name, Kind: row.Kind, Expression: row.Expression,
		ExpectedIntervalSeconds: row.ExpectedIntervalSeconds,
		Severity:                row.Severity, Description: row.Description, Enabled: row.Enabled,
	}
}

// checkWriteFailure is one refused check write, classified by response SHAPE.
type checkWriteFailure struct {
	outcome string
	message string
	// wholeTable reports a failure that is about the table or the server rather
	// than this check, so the remaining writes are not attempted.
	wholeTable bool
	// unknown: the write got no answer, and may have landed.
	unknown error
}

// classifyCheckWrite reads a failed check write. `fail` in either the live
// row's severity or the entry's makes a 403 name the admin requirement too —
// the server does not say which of the two refused it.
func classifyCheckWrite(err error, path string, severities ...string) checkWriteFailure {
	switch {
	case api.IsUnmatchedRoute(err):
		return checkWriteFailure{checksOutcomeUnsupported, unmatchedRouteMessage(err, path), true, nil}
	case api.StatusOf(err) == 400 && strings.HasPrefix(api.ErrorMessageOf(err), oldPutRefusal):
		// The older silence-only PUT: it requires `enabled` and can change
		// nothing else. About the server, so the rest of the table is not tried.
		return checkWriteFailure{checksOutcomeUnsupported,
			fmt.Sprintf("this Ronja server can only silence a health check over HTTP, not edit one — upgrade the server; %s was not applied", path), true, nil}
	case api.ErrorCodeOf(err) == api.TableNotBuiltCode:
		return checkWriteFailure{checksOutcomePendingPublish,
			"the table has not been built yet, so the check could not be run — push the table's SQL and publish it; the checks are applied after its first build", false, nil}
	case api.StatusOf(err) == 403:
		for _, sev := range severities {
			if sev == checkfile.SeverityFail {
				return checkWriteFailure{checksOutcomeRefused, "refused — needs admin for a fail check, or no write access to this table", false, nil}
			}
		}
		return checkWriteFailure{checksOutcomeRefused, "refused — no write access to this table", false, nil}
	case api.StatusOf(err) == 404:
		return checkWriteFailure{checksOutcomeRefused,
			"refused — the table is not reachable (in the trash, or you cannot read it): " + api.ErrorMessageOf(err), true, nil}
	}
	if cause := noAnswer(err); cause != nil {
		return checkWriteFailure{outcome: checksOutcomeRefused, unknown: cause,
			message: fmt.Sprintf("no answer from the server (%v) — the write may have landed; push again", err)}
	}
	return checkWriteFailure{checksOutcomeRefused, "refused — " + api.ErrorMessageOf(err), false, nil}
}

// oldPutRefusal is how an older server's silence-only PUT refuses a body without
// `enabled`. Matched as a PREFIX of the message, so a newer wording that keeps
// the sentence still reads as the old route.
const oldPutRefusal = "enabled is required"

// unmatchedRouteMessage words a check route the server did not match: a 405,
// or a 404 without a Ronja error body. A 404 of that shape is what an older
// Ronja server answers — and also what a proxy in front of one does, which is
// why it does not claim to know which.
func unmatchedRouteMessage(err error, path string) string {
	if api.StatusOf(err) == 404 {
		return fmt.Sprintf("the server answered 404 without a Ronja error body — it may be an older Ronja server that cannot write health checks over HTTP (upgrade the server), or a proxy in front of it; %s was not applied", path)
	}
	return fmt.Sprintf("the server does not accept this health-check request (%d) — an older Ronja server; upgrade the server; %s was not applied", api.StatusOf(err), path)
}

// applyTableChecks applies one checks file to one live table, in the order the
// header states.
func applyTableChecks(ctx context.Context, client *api.Client, store liveHashes, file pipelineChecksFile,
	tableID string, opts checksApplyOptions, claim checksClaim) pipelineChecksResult {

	out := pipelineChecksResult{Path: file.Path, TableID: tableID, FileDeleted: file.Deleted}
	whole := func(outcome, format string, args ...any) pipelineChecksResult {
		out.Outcome = outcome
		out.Error = fmt.Sprintf(format, args...)
		return out
	}

	// --- 1. One read -------------------------------------------------------
	rows, err := client.ListTableChecks(ctx, tableID)
	if err != nil {
		if file.Deleted && api.StatusOf(err) == 404 && !api.IsUnmatchedRoute(err) {
			// The table a deleted file's checks were on is gone (or out of reach):
			// there is nothing left to silence, and a record that cannot be acted
			// on would refuse every push from here on.
			fmt.Fprintf(os.Stderr, "  Note: %s is gone from this folder and its table %s cannot be read (%s) — its record is dropped.\n",
				file.Path, tableID, api.ErrorMessageOf(err))
			store.dropChecksSeen(file.Path)
			return out
		}
		if api.IsUnmatchedRoute(err) {
			return whole(checksOutcomeUnsupported, "%s", unmatchedRouteMessage(err, file.Path))
		}
		out.unknown = noAnswer(err)
		return whole(checksOutcomeRefused, "could not read the checks on %s: %v", tableID, err)
	}
	// What the folder owns, only if it was recorded against THIS table.
	managed := map[string]wfdir.LockCheckEntry{}
	if recordedTable, recorded := store.checksSeen(file.Path); recordedTable == tableID {
		for name, entry := range recorded {
			managed[name] = entry
		}
	}
	entries := file.File.Checks
	declared := map[string]bool{}
	for _, e := range entries {
		declared[checkfile.Fold(e.Name)] = true
	}
	plan := planTableChecks(entries, rows, managed, claim)
	if plan.dup != "" {
		return whole(checksOutcomeRefused, "%s", plan.dup)
	}
	targets := plan.targets
	for _, key := range plan.gone {
		delete(managed, key)
	}

	// --- 2. Drift, before anything is written ------------------------------
	var drifted []string
	for i, e := range entries {
		row := targets[i]
		if row == nil {
			continue
		}
		m, owned := plan.owned(row)
		if !owned || m.LiveSHA256 == "" {
			continue
		}
		live := liveCheckOf(row)
		if !e.Matches(live) && !checkfile.Unmoved(live, m.LiveSHA256) {
			drifted = append(drifted, e.Name)
		}
	}
	// An orphan changed in the app since the folder last verified it — an admin
	// re-enabled it, or re-severitied it — is drift too when this push would
	// silence it: a --prune must not undo that without the --force that says so.
	var movedOrphans []string
	if opts.Prune {
		for _, o := range plan.orphans {
			if o.moved {
				movedOrphans = append(movedOrphans, o.row.Name)
			}
		}
	}
	if len(drifted) > 0 || len(movedOrphans) > 0 {
		if !opts.Force {
			var why []string
			if len(drifted) > 0 {
				why = append(why, fmt.Sprintf("changed in the app since this folder last applied it: %s — push --force overwrites, or copy the change into %s",
					quoteNames(drifted), file.Path))
			}
			if len(movedOrphans) > 0 {
				why = append(why, fmt.Sprintf("no longer in %s and changed in the app since this folder last applied it: %s — push --prune --force silences them anyway, or put them back in the file",
					file.Path, quoteNames(movedOrphans)))
			}
			return whole(checksOutcomeDrift, "%s", strings.Join(why, "; and "))
		}
		fmt.Fprintf(os.Stderr, "  Note: --force — overwriting checks changed in the app: %s (%s).\n", quoteNames(append(drifted, movedOrphans...)), file.Path)
	}

	// --- 3. Orphans, before any create -------------------------------------
	orphans := plan.orphans
	if len(orphans) > 0 && !opts.Prune {
		names := make([]string, 0, len(orphans))
		for _, o := range orphans {
			names = append(names, o.row.Name)
		}
		if file.Deleted {
			return whole(checksOutcomeOrphaned, "%s is gone from this folder, and the checks it created on %s are still enabled: %s — push --prune silences them (the CLI never deletes a check), or restore the file",
				file.Path, tableID, quoteNames(names))
		}
		return whole(checksOutcomeOrphaned, "%s no longer declares %s, which this folder created — push --prune silences them (the CLI never deletes a check), or put them back in the file",
			file.Path, quoteNames(names))
	}
	pruned := map[string]bool{}
	for _, o := range orphans {
		enabled := false
		res, err := client.UpdateTableCheck(ctx, tableID, o.row.ID, api.UpdateTableCheckInput{Enabled: &enabled})
		if err != nil {
			fail := classifyCheckWrite(err, file.Path, o.row.Severity)
			out.Checks = append(out.Checks, pipelineCheckResult{Name: o.row.Name, CheckID: o.row.ID, Outcome: fail.outcome, Error: fail.message, unknown: fail.unknown})
			if fail.wholeTable {
				out.Outcome = fail.outcome
				return out
			}
			continue
		}
		pruned[o.row.ID] = true
		delete(managed, o.key)
		out.Checks = append(out.Checks, pipelineCheckResult{Name: res.Name, CheckID: res.ID, Outcome: checksOutcomeApplied,
			Changed: []string{checkfile.FieldEnabled}, Detail: "silenced — no longer in the file (--prune)"})
	}

	// --- 4. Updates, then creates ------------------------------------------
	results := make([]pipelineCheckResult, len(entries))
	stopped := ""
	update := func(i int, row *api.TableCheck) {
		var wholeTable bool
		_, owned := plan.owned(row)
		results[i], wholeTable = updateOneCheck(ctx, client, file.Path, tableID, entries[i], row, owned)
		if wholeTable {
			stopped = results[i].Outcome
		}
	}
	var creates []int
	for i := range entries {
		if targets[i] == nil {
			creates = append(creates, i)
			continue
		}
		if stopped != "" {
			break
		}
		update(i, targets[i])
	}
	for _, i := range creates {
		if stopped != "" {
			break
		}
		e := entries[i]
		res, err := client.CreateTableCheck(ctx, tableID, createInputOf(e))
		if err == nil {
			results[i] = pipelineCheckResult{Name: e.Name, CheckID: res.ID, Outcome: checksOutcomeApplied, Verdict: verdictOf(res)}
			continue
		}
		// Two failures that may leave the name on the table, told apart by the
		// error, because they say opposite things about who made the row:
		//
		//   - a TAKEN NAME: somebody else's create won (the server's unique index is
		//     the multi-instance guard). The row is theirs — re-enter the diff for
		//     it as an adoption, never as ours;
		//   - a TIMEOUT: ours may have landed and we stopped listening. A row that
		//     says exactly what the entry says is ours ("created; verdict not
		//     returned"); anything else re-enters the diff.
		//
		// A name the server's collation folds and Go's does not (İ, ß) is not
		// found by the re-read and ends as the refusal below, with the server's
		// name_taken message — never as a match.
		nameTaken, timedOut := api.ErrorCodeOf(err) == api.NameTakenCode, api.IsTimeout(err)
		if nameTaken || timedOut {
			if again, readErr := client.ListTableChecks(ctx, tableID); readErr == nil {
				var found *api.TableCheck
				for j := range again {
					if checkfile.Fold(again[j].Name) == checkfile.Fold(e.Name) {
						found = &again[j]
					}
				}
				switch {
				case found != nil && timedOut && e.Matches(liveCheckOf(found)):
					results[i] = pipelineCheckResult{Name: e.Name, CheckID: found.ID, Outcome: checksOutcomeApplied,
						Detail: "created; verdict not returned (the create timed out after it landed)"}
					continue
				case found != nil:
					update(i, found)
					continue
				}
			}
		}
		fail := classifyCheckWrite(err, file.Path, severityOf(e))
		results[i] = pipelineCheckResult{Name: e.Name, Outcome: fail.outcome, Error: fail.message, unknown: fail.unknown}
		if fail.wholeTable {
			stopped = fail.outcome
		}
	}
	if stopped != "" {
		out.Outcome = stopped
		out.Error = firstError(results)
		for i := range results {
			if results[i].Outcome == "" {
				results[i] = pipelineCheckResult{Name: entries[i].Name, Outcome: stopped, Error: "not attempted"}
			}
		}
	}

	// --- 5. Verify, then record, per check ---------------------------------
	final, err := client.ListTableChecks(ctx, tableID)
	finalByID := map[string]*api.TableCheck{}
	if err == nil {
		for i := range final {
			finalByID[final[i].ID] = &final[i]
		}
	}
	for i, e := range entries {
		r := &results[i]
		if r.CheckID == "" || checksOutcomeFails(r.Outcome) || r.Outcome == checksOutcomePendingPublish {
			continue
		}
		row := finalByID[r.CheckID]
		if err != nil || row == nil || !e.Matches(liveCheckOf(row)) {
			r.Outcome = checksOutcomeNotApplied
			if err != nil {
				// Unreadable read-back: whether the write landed is not known.
				r.unknown = err
				r.Error = fmt.Sprintf("the write answered, but the checks could not be read back to confirm it (%v) — push again", err)
			} else {
				r.Error = "the write answered, but the check does not now say what the file says — this server may not support editing checks over HTTP; upgrade the server and push again"
			}
			continue
		}
		// Recorded under the entry's name. Any other record of the same row — the
		// name the file used to give it — goes, and so does a record under this
		// name of a row that is gone.
		for key, m := range managed {
			if m.CheckID == row.ID {
				delete(managed, key)
			}
		}
		managed[checkfile.Fold(e.Name)] = wfdir.LockCheckEntry{CheckID: row.ID, LiveSHA256: e.LiveSHA256(liveCheckOf(row))}
	}

	// Unmanaged: on the table, not in the file, not owned — never touched.
	ownedIDs := map[string]bool{}
	for _, m := range managed {
		ownedIDs[m.CheckID] = true
	}
	for _, r := range results {
		ownedIDs[r.CheckID] = true
	}
	listed := rows
	if err == nil {
		listed = final
	}
	for _, row := range listed {
		if file.Deleted {
			break
		}
		if !ownedIDs[row.ID] && !pruned[row.ID] && !declared[checkfile.Fold(row.Name)] {
			out.Unmanaged = append(out.Unmanaged, row.Name)
		}
	}
	sort.Strings(out.Unmanaged)

	if file.Deleted && len(managed) == 0 {
		store.dropChecksSeen(file.Path)
	} else {
		store.setChecksSeen(file.Path, tableID, managed)
	}
	out.Checks = append(out.Checks, results...)
	return out
}

// checksPlan is one checks file read against one table's live checks: which
// row each entry IS, which rows the folder owns, and which of those are orphans.
// Push and status both read it, so they cannot disagree about what an orphan is.
type checksPlan struct {
	// targets[i] is the live row entry i resolves to, or nil for a create.
	targets []*api.TableCheck
	// ownedBy maps an owned row's id to the key it is recorded under.
	ownedBy map[string]string
	managed map[string]wfdir.LockCheckEntry
	// orphans are owned, still-enabled rows that NO entry resolves to.
	orphans []checkOrphan
	// gone are recorded keys with nothing left to own: the row is gone, or
	// silenced, or another checks file claims it — and no entry resolves to it.
	gone []string
	// dup is non-empty when two entries resolve to one row — a refusal.
	dup string
}

type checkOrphan struct {
	key string
	row *api.TableCheck
	// moved: the row changed in the app since the folder last verified it.
	moved bool
}

func (p checksPlan) owned(row *api.TableCheck) (wfdir.LockCheckEntry, bool) {
	if row == nil {
		return wfdir.LockCheckEntry{}, false
	}
	key, ok := p.ownedBy[row.ID]
	if !ok {
		return wfdir.LockCheckEntry{}, false
	}
	return p.managed[key], true
}

// planTableChecks resolves each entry to a live row — the check the folder owns
// under the entry's name first, by id, so a rename in the app is still the same
// check; then by folded name — and only THEN decides orphans, BY ROW: an owned
// row is an orphan when no entry resolves to it. Deciding by name instead made a
// row an orphan and a target at once whenever the app and the file renamed it
// differently, and a --prune silenced the row the next push re-enabled.
func planTableChecks(entries []checkfile.Entry, rows []api.TableCheck, managed map[string]wfdir.LockCheckEntry, claim checksClaim) checksPlan {
	byID := map[string]*api.TableCheck{}
	byFold := map[string]*api.TableCheck{}
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
		byFold[checkfile.Fold(rows[i].Name)] = &rows[i]
	}
	p := checksPlan{targets: make([]*api.TableCheck, len(entries)), ownedBy: map[string]string{}, managed: managed}
	for key, m := range managed {
		p.ownedBy[m.CheckID] = key
	}
	targetedBy := map[string]int{}
	for i, e := range entries {
		fold := checkfile.Fold(e.Name)
		row := byFold[fold]
		if m, owned := managed[fold]; owned && byID[m.CheckID] != nil {
			row = byID[m.CheckID]
		}
		p.targets[i] = row
		if row == nil {
			continue
		}
		if prior, taken := targetedBy[row.ID]; taken && p.dup == "" {
			p.dup = fmt.Sprintf("%q and %q both resolve to the check %q on the table (one by the name this folder recorded for it, one by its name now) — rename one of the two entries",
				entries[prior].Name, e.Name, row.Name)
		}
		targetedBy[row.ID] = i
	}
	for _, key := range sortedKeys(managed) {
		m := managed[key]
		if _, targeted := targetedBy[m.CheckID]; targeted {
			continue
		}
		row := byID[m.CheckID]
		if row == nil || !row.Enabled || claim.has(row) {
			// Gone, silenced, or handed to another checks file on this table.
			p.gone = append(p.gone, key)
			continue
		}
		moved := m.LiveSHA256 != "" && !checkfile.Unmoved(liveCheckOf(row), m.LiveSHA256)
		p.orphans = append(p.orphans, checkOrphan{key: key, row: row, moved: moved})
	}
	return p
}

// updateOneCheck brings one existing live check in line with its entry: nothing
// when it already matches, else a PUT of the declared fields that differ.
//
// wholeTable reports a failure about the table or the server, after which the
// caller attempts nothing more on this table.
func updateOneCheck(ctx context.Context, client *api.Client, path, tableID string, e checkfile.Entry, row *api.TableCheck, owned bool) (_ pipelineCheckResult, wholeTable bool) {
	r := pipelineCheckResult{Name: e.Name, CheckID: row.ID}
	if row.Kind != e.Kind() {
		r.Outcome = checksOutcomeRefused
		r.Error = fmt.Sprintf("refused — %q on the table is a %s check and the file declares a %s one; a check's kind is fixed — give it a new name, and --prune the old one", row.Name, row.Kind, e.Kind())
		return r, false
	}
	live := liveCheckOf(row)
	silencedNote := ""
	if !row.Enabled && e.Enabled == nil {
		silencedNote = "exists but is silenced — add \"enabled\": true to the entry to bring it back"
	}
	diff := e.Differs(live)
	if len(diff) == 0 {
		switch {
		case silencedNote != "":
			r.Outcome, r.Detail = checksOutcomeSilenced, silencedNote
		case owned:
			r.Outcome = checksOutcomeUnchanged
		default:
			r.Outcome = checksOutcomeAdopted
		}
		return r, false
	}
	res, err := client.UpdateTableCheck(ctx, tableID, row.ID, updateInputOf(e, diff))
	if err != nil {
		fail := classifyCheckWrite(err, path, row.Severity, severityOf(e))
		r.Outcome, r.Error, r.unknown = fail.outcome, fail.message, fail.unknown
		return r, fail.wholeTable
	}
	r.Changed = diff
	r.Verdict = verdictOf(res)
	r.Detail = silencedNote
	if owned {
		r.Outcome = checksOutcomeApplied
	} else {
		// A same-named check this folder did not create, rewritten: said apart,
		// so overwriting a colleague's chat-authored check is visible.
		r.Outcome = checksOutcomeAdoptedChanged
	}
	return r, false
}

func firstError(results []pipelineCheckResult) string {
	for _, r := range results {
		if r.Error != "" {
			return r.Error
		}
	}
	return ""
}

// createInputOf is the POST for a new entry. A silenced entry is created
// silenced in the same write (`enabled: false`), so it never goes live — and
// never alerts — between a create and a silence.
func createInputOf(e checkfile.Entry) api.CreateTableCheckInput {
	in := api.CreateTableCheckInput{Name: e.Name, Description: e.Description, Severity: e.Severity}
	if e.Enabled != nil && !*e.Enabled {
		off := false
		in.Enabled = &off
	}
	if e.Kind() == checkfile.KindFreshness {
		hours := e.ExpectedIntervalHours
		in.ExpectedIntervalHours = &hours
	} else {
		expr := e.Expression
		in.Expression = &expr
	}
	return in
}

// updateInputOf sends only the fields named — the declared ones that differ.
// Sending an unchanged `enabled` or `severity` beside an expression edit would
// trip the server's admin gate on a fail check for no reason.
func updateInputOf(e checkfile.Entry, fields []string) api.UpdateTableCheckInput {
	var in api.UpdateTableCheckInput
	for _, field := range fields {
		switch field {
		case checkfile.FieldName:
			name := e.Name
			in.Name = &name
		case checkfile.FieldExpression:
			expr := e.Expression
			in.Expression = &expr
		case checkfile.FieldInterval:
			hours := e.ExpectedIntervalHours
			in.ExpectedIntervalHours = &hours
		case checkfile.FieldSeverity:
			in.Severity = e.Severity
		case checkfile.FieldDescription:
			in.Description = e.Description
		case checkfile.FieldEnabled:
			in.Enabled = e.Enabled
		}
	}
	return in
}

func severityOf(e checkfile.Entry) string {
	if e.Severity == nil {
		return ""
	}
	return *e.Severity
}

func verdictOf(res *api.TableCheckWriteResult) string {
	if res == nil || !res.ReRan || res.Verdict == nil {
		return ""
	}
	return res.Verdict.Status
}
