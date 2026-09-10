package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/tablerefs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// Per-file push outcomes. These are the --json `outcome` values, so they are a
// contract for anything scripting the CLI.
//
// `build_failed` is the one worth spelling out: the file WAS written to the
// draft and the draft WAS synced — what failed is the build, so the draft is
// still there to iterate on and the live table is untouched. A caller that
// treated it as "nothing happened" would discard a draft that holds their work.
const (
	pushOutcomePushed      = "pushed"
	pushOutcomeBuildFailed = "build_failed"
	pushOutcomeRefused     = "refused"
	// `docs_failed` is the SQL half landing and the DOCUMENTATION half not: the
	// file was written, the draft built, and the write that would have attached
	// the header's prose to the built columns did not go through. Split from
	// `pushed` because it is the one state where a green push would leave a
	// table whose committed file says one thing and whose row says another —
	// exactly the silent divergence this loop exists to remove — and split from
	// `refused` because the work is on the server and publishing it is still the
	// right next step.
	//
	// ⚠️ An UNMATCHED column name is not this. A documented name the table does
	// not have is the join rule working: the prose is stored, the write
	// succeeded, and the outcome is `pushed`.
	pushOutcomeDocsFailed = "docs_failed"
)

// `ronja pipeline push` syncs each changed .sql file into YOUR draft of its
// table, builds it, and reports what the build produced.
//
// The order of operations is the design. Local guards first (they need no
// network and their diagnosis is entirely local), then the files in topological
// order, and per file the drift guard before the first byte is written.
func newPipelinePushCmd() *cobra.Command {
	var force, forceVerifiedMetric bool
	cmd := &cobra.Command{
		Use:   "push [paths...]",
		Short: "Sync changed .sql files and metrics into your drafts and build them",
		Long: `Sync changed .sql files and metrics into your drafts and build them.

Pushes to a DRAFT, always — the live table is never written, so a failed build
changes nothing anyone else can see. For each file that differs from your last
sync (or that has no table yet):

  1. the table is created, if this file has never been pushed
  2. your draft is resumed, or checked out
  3. the SQL and its derived inputs are written to the draft
  4. the draft is built, and the result is reported: schema changes against the
     live table, row counts, and a few sample rows

Files are pushed in dependency order, so a table is built after everything in
this folder that it reads from. Name paths to push only those.

DOCUMENTATION. A .sql file may open with a header saying what its table and
columns hold, and a push writes it after the build:

  -- @table One row per invoice line, from Fortnox, refreshed nightly.
  -- Amounts are in SEK.
  -- @column invoice_no: the supplier's own number, not Ronja's id
  -- @column amount: line total, excluding VAT

It is OPT-IN: a leading comment that does not open with "-- @table" is ordinary
commentary and is left alone. For a table this folder does NOT build — an
integration or foundation table, or one a workflow writes — put the same thing
in tables/<alias>.json instead, where <alias> is a "table" dependency this
folder declares and "ronja bind" points at a row:

  {"description": "...", "columns": {"amount": "line total, excluding VAT"}}

Both are three-state: a column you do not name is left exactly as it is, so a
file may document three columns of twelve. A documented name the table does not
have is REPORTED, not refused — the prose is kept and attaches by itself if a
build later produces the column, which is what makes a committed file safe to
keep in git alongside data that changes.

METRICS. A metric is a recipe rather than SQL, so it lives in
metrics/<name>.json, where <name> is the metric's own name (or a "table"
dependency this folder declares and "ronja bind" points at an existing metric):

  {"recipe": {"source": "orders", "time": {"column": "created_at",
   "native_grain": "day"}, "base_measures": [{"name": "revenue", "agg": "sum",
   "column": "amount"}], "value": "revenue"}, "description": "..."}

It runs the identical cycle: your draft is staged, built and reported, and
"ronja pipeline publish" commits it. "source" may name a table this folder
builds — metrics are pushed after the SQL, so the table is always there first. A
metric and a table cannot share a name inside one feature, and this command
refuses a folder that claims one twice before it sends anything.

It refuses a table whose SQL changed on the server since your last sync — chat
and the web builder edit the same draft — naming what moved, and it refuses on
the same terms when a documented table's DESCRIPTION or column prose moved, or
when a metric's DEFINITION did. --force overwrites, and prints the difference
before it does — except on a metric an admin has VERIFIED, which additionally
needs --force-verified-metric.

A failed build is not the end of the push: the remaining files are still
attempted, each result is reported, and the command exits non-zero.

Publishing is a separate step:

  ronja pipeline publish            commit your drafts, or submit them for review`,
		// Positional arguments are FILE PATHS, resolved against the working
		// directory and validated by resolveArgPaths — which refuses anything
		// outside the folder or outside the syncable set, by name. Declared
		// explicitly, like every sibling command, so the shape of the command is
		// visible where cobra reads it rather than only in the resolver.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --force-verified-metric ONLY WIDENS what --force overwrites: the
			// refusal it unlocks is nested inside the --force arm of the metric
			// drift guard, so on its own it changes nothing at all. Silently inert
			// was the bug — an author reaching for what reads as the stronger of
			// the two flags typed it alone, met the ordinary drift refusal, and was
			// told nothing about the flag they had just passed.
			//
			// Refused HERE, before the folder is opened and before the first
			// request, because a flag combination that cannot mean anything is
			// answerable with no state at all — and because a folder push that has
			// already created tables is the wrong moment to learn the command line
			// was wrong.
			if forceVerifiedMetric && !force {
				return fmt.Errorf("--force-verified-metric does nothing on its own: it only widens what --force is allowed to overwrite.\n  Pass both (`--force --force-verified-metric`) to overwrite a VERIFIED metric whose definition changed on the server, or drop it and push with --force alone")
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.PipelineKind)
			if err != nil {
				return err
			}
			result, err := runPipelinePush(cmd.Context(), f, args,
				pipelinePushOptions{Force: force, ForceVerifiedMetric: forceVerifiedMetric})
			// The report is emitted even on failure: a push that stopped part-way
			// has already created tables and staged drafts, and "which ones" is
			// the first thing anyone needs to know.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printPipelinePushReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"push even though a table's SQL, or a metric's definition, changed on the server since your last sync")
	cmd.Flags().BoolVar(&forceVerifiedMetric, "force-verified-metric", false,
		"with --force, also overwrite a VERIFIED metric whose definition a colleague changed — discarding the admin's verification of what is there now")
	return cmd
}

type pipelinePushOptions struct {
	Force bool
	// ForceVerifiedMetric is the SECOND flag a metric an admin has verified
	// costs, and it does nothing on its own — it only widens what --force is
	// allowed to overwrite.
	//
	// Two flags rather than one because the two acts are not the same size.
	// Forcing past drift on a .sql file overwrites SQL whose previous version is
	// in the table's own history; forcing past drift on a VERIFIED metric
	// overwrites the company's official definition of a number, discards an
	// admin's assertion that the definition there now was checked, and does it
	// under a flag whose message is about your own file. Overwriting that should
	// cost a sentence you had to type.
	ForceVerifiedMetric bool
}

// pipelinePushResult is the --json shape and the human renderer's input, so the
// two cannot describe different things.
type pipelinePushResult struct {
	FeatureID string `json:"featureID,omitempty"`
	// Target names the instance and organization this push landed in, so a
	// mistake is visible where it happens rather than only from a later status.
	Target string `json:"target,omitempty"`
	// Files is one entry per file this push ATTEMPTED, in the order it attempted
	// them — which is dependency order, and is worth preserving for that reason.
	Files []pipelineFileResult `json:"files"`
	// Docs is one entry per DOCS SIDECAR this push attempted — the tables this
	// folder documents but does not build. Absent for the folders that keep
	// none, which is every folder that has not adopted them.
	Docs []pipelineDocsFileResult `json:"docs,omitempty"`
	// Metrics is one entry per METRIC FILE this push attempted — the numbers this
	// folder defines. Absent for the folders that keep none, which is every
	// folder that has not adopted them.
	Metrics []pipelineMetricFileResult `json:"metrics,omitempty"`
	// UpToDate reports a push that found nothing to do.
	UpToDate bool `json:"upToDate"`
	// Error summarises a push that did not fully succeed. The per-file entries
	// say what actually happened; this is the one-line version.
	Error string `json:"error,omitempty"`
}

// pipelineFileResult is what happened to one file.
type pipelineFileResult struct {
	Path    string `json:"path"`
	TableID string `json:"tableID,omitempty"`
	// DraftID is the row the SQL was written to. It survives a failed build on
	// purpose — that draft is where the work is.
	DraftID string `json:"draftID,omitempty"`
	Outcome string `json:"outcome"`
	// Created reports that this push brought the table into existence.
	Created bool `json:"created"`
	// Verdict is the build verdict, one of api.BuildVerdict*.
	Verdict string `json:"verdict,omitempty"`
	// Error is the reason this file did not succeed — the build's own message
	// for a failed build, which for a derived table is usually the SQL writer's
	// full explanation and is the single most useful thing a failed push prints.
	Error string `json:"error,omitempty"`
	// Hint is a DIAGNOSIS added to a failure whose own message does not carry
	// one. Today that is the server's bare "no data found", which has two
	// entirely different causes and says neither — see pipelineNoDataHint.
	//
	// Never a substitute for Error: it is advice about what to do next, and a
	// caller that only reads one field must read the one the server wrote.
	Hint string `json:"hint,omitempty"`
	// Conflict reports that what refused this file was a compare-and-swap
	// losing a race -- somebody edited the draft between this push's read and
	// its write -- rather than a rule the caller cannot satisfy at all.
	//
	// It exists because Outcome and Error are both PROSE to a script: a
	// `refused` covers "re-read and try again" and "you may not do this"
	// identically, and telling them apart otherwise means substring-matching a
	// message the server rewords whenever that reads better. Same field, same
	// reason, as pushResult's and appPushResult's -- and the 409 here is the
	// RETRYABLE one, which is the half worth splitting out.
	Conflict bool            `json:"conflict"`
	Review   *pipelineReview `json:"review,omitempty"`
	// Docs is what this file's `-- @table` header wrote, and what the server
	// joined it onto. Absent for a file that carries no header, which is every
	// file in every folder that has not opted in.
	Docs *tableDocsResult `json:"docs,omitempty"`
	// Sample and URL are rendered by the human report only — see pushResult.URL
	// for why a link stays out of a shape scripts parse, and samples are garnish
	// nothing should be scripted against.
	Sample string   `json:"-"`
	URL    string   `json:"-"`
	Notes  []string `json:"notes,omitempty"`
}

// printPipelineDocsResult renders one docs sidecar's outcome.
func printPipelineDocsResult(out *os.File, doc pipelineDocsFileResult) {
	if doc.Outcome == docsOutcomeRefused {
		fmt.Fprintf(out, "\n  %s — not documented\n", doc.Path)
		for _, line := range strings.Split(strings.TrimRight(doc.Error, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		return
	}
	fmt.Fprintf(out, "\n  %s — %s\n", doc.Path, doc.Outcome)
	if doc.Outcome == docsOutcomeNothing {
		// No table line: nothing was read, because a file that declares nothing
		// is answered before the network.
		fmt.Fprintf(out, "    %s\n", doc.Warning)
		return
	}
	fmt.Fprintf(out, "    Table:  %s\n", doc.TableID)
	if line := describeDocsResult(doc.Result); line != "" {
		fmt.Fprintf(out, "    Docs:   %s\n", line)
	}
}

// printPipelineMetricResult renders one metric file's outcome.
//
// It leads with the metric's DERIVED shape rather than with row counts, which is
// the one place this renderer diverges from the .sql one and the divergence is
// the point: a metric produces no table to sample, and what a reader has to
// check before publishing is whether the server derived the definition they
// meant — additivity above all, since a ratio that came out additive would
// re-aggregate by summing and be wrong at every grain but one.
func printPipelineMetricResult(out *os.File, metric pipelineMetricFileResult) {
	switch metric.Outcome {
	case metricOutcomeRefused:
		fmt.Fprintf(out, "\n  %s — not pushed\n", metric.Path)
		for _, line := range strings.Split(strings.TrimRight(metric.Error, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		return
	case metricOutcomeBuildFailed:
		fmt.Fprintf(out, "\n  %s — build FAILED\n", metric.Path)
		fmt.Fprintf(out, "    Draft:  %s (kept, so you can fix the recipe and push again)\n", metric.DraftID)
		fmt.Fprintf(out, "    Metric: %s is untouched — a draft build never changes what anybody reads\n", metric.MetricID)
		for _, line := range strings.Split(strings.TrimRight(metric.Error, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		return
	case metricOutcomeUpToDate:
		fmt.Fprintf(out, "\n  %s — up to date\n", metric.Path)
		fmt.Fprintf(out, "    Metric: %s\n", metric.MetricID)
		return
	case metricOutcomeDescriptionFailed:
		fmt.Fprintf(out, "\n  %s — staged, description NOT written\n", metric.Path)
		fmt.Fprintf(out, "    Draft:  %s (holds the definition, and is still publishable)\n", metric.DraftID)
		for _, line := range strings.Split(strings.TrimRight(metric.Error, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		return
	}

	verb := "staged"
	if metric.Created {
		verb = "created and staged"
	}
	fmt.Fprintf(out, "\n  %s — %s\n", metric.Path, verb)
	fmt.Fprintf(out, "    Metric: %s\n", metric.MetricID)
	fmt.Fprintf(out, "    Draft:  %s\n", metric.DraftID)
	fmt.Fprintf(out, "    Reads:  %s\n", metric.SourceID)
	for _, note := range metric.Notes {
		fmt.Fprintf(out, "    Shape:  %s\n", note)
	}
	for _, warning := range metric.Warnings {
		fmt.Fprintf(out, "    Note:   %s\n", warning)
	}
	printResourceURL(out, statusKeyWidth, metric.URL)
}

// pipelineReview is the confidence report: what committing this draft would
// change, computed entirely from state that already exists.
type pipelineReview struct {
	// FieldsAdded / FieldsRemoved are column names. The server compares names
	// only, so a retype is invisible here — reported as neither.
	FieldsAdded   []string `json:"fieldsAdded,omitempty"`
	FieldsRemoved []string `json:"fieldsRemoved,omitempty"`
	// InputsAdded / InputsRemoved are the table ids this draft's declared
	// lineage gains and loses against the live row. Reported because it is the
	// one change committing can make with no SQL diff to look at, and because
	// the push that caused it derived the list from the file's own refs — so a
	// line here is the author's own edit read back from the server.
	InputsAdded   []string `json:"inputsAdded,omitempty"`
	InputsRemoved []string `json:"inputsRemoved,omitempty"`
	// DraftRowCount / LiveRowCount are -1 for "not available", which is NOT
	// zero: printing it as zero would report a table that was never built as one
	// that came back empty.
	DraftRowCount int64 `json:"draftRowCount"`
	LiveRowCount  int64 `json:"liveRowCount"`
	// BaseStale reports that the live table advanced after this draft forked, so
	// committing would silently revert whatever landed in between.
	BaseStale           bool     `json:"baseStale,omitempty"`
	InterveningVersions []string `json:"interveningVersions,omitempty"`
}

// pipelineChangedTargets is the set of files a no-argument push sends: the ones
// with no table behind them, and the ones whose bytes differ from the baseline
// this folder last synced.
//
// Extracted because `sync apply`'s local pre-flight has to know the same set —
// the positional-ref refusal is checked on what is being SENT, and a second
// spelling of "what a push would send" is exactly how a dry-run starts
// disagreeing with the run it previews.
func pipelineChangedTargets(f *folder, local, known map[string]string) []string {
	var targets []string
	for _, path := range sortedPaths(local) {
		if f.Binding.Tables[path] == "" || known[path] != wfdir.HashString(local[path]) {
			targets = append(targets, path)
		}
	}
	return targets
}

func runPipelinePush(ctx context.Context, f *folder, args []string, opts pipelinePushOptions) (*pipelinePushResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pipelinePushResult{
		FeatureID: f.Binding.FeatureID,
		Target:    describeTarget(f.Resolved),
		Files:     []pipelineFileResult{},
	}

	// 1. Local guards. Nothing here needs the network, and each refusal names a
	// file the author can act on immediately.
	local, enumeration, err := readPipelineFiles(f.Root)
	if err != nil {
		return nil, err
	}
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
	if err := checkPushable(local, wfdir.PipelineKind, maxFileBytes); err != nil {
		return nil, err
	}
	// The names this folder's code may use: its own files' stems, and whatever
	// ronja.json declares. Built once, read live — a sibling's table id only
	// exists after that sibling's own create, which happens inside the loop below.
	codec := newPipelineCodec(f, local)
	// The alias pre-flight, still with no network in sight. The stems go in
	// because a pipeline folder is the one kind whose files claim local names, and
	// a dependency declared under one of them is dead text that reads as if it
	// were in force.
	aliases := checkAliases(f.Manifest, f.selection(), f.Codec, local, folderStems(f.Kind, local), folderFieldRefs(f.Kind, f.Root, local))
	if err := aliases.err(); err != nil {
		return nil, err
	}
	noteAliasWarnings(aliases)

	baseline := f.State.For(f.Key)
	// A baseline entry for a file that has left BOTH the folder and the binding
	// describes nothing at all, and left standing it makes `status` report a
	// deletion for ever. Dropped here rather than only reported, so removing the
	// ronja.json entry is the whole of the cleanup.
	pruned := prunePhantoms(baseline, f.Binding, local)
	if len(pruned) > 0 {
		fmt.Fprintf(os.Stderr, "  Note: dropped the local baseline for %s — gone from this folder and from the bindings in %s.\n",
			strings.Join(pruned, ", "), wfdir.ManifestName)
	}
	known := baseline.Hashes()

	// 2. What to push. Named paths win outright — asking for a file explicitly
	// means pushing it whether or not it looks changed, which is how somebody
	// recovers from a draft they edited elsewhere.
	// The docs sidecars, read and resolved with no network in sight: which table
	// each one names here, and which cannot be used as it stands. Read even when
	// the folder keeps none, which costs one failed stat.
	sidecars, err := readTableDocsSidecars(f.Root)
	if err != nil {
		return nil, err
	}
	sidecars = resolveTableDocsSidecars(f, sidecars)
	// The METRIC files, read with no network in sight either. Resolution needs
	// the baseline, because a metric's identity comes from the recorded id when
	// no alias binds its stem — see resolveMetricFiles.
	metrics, err := readMetricFiles(f.Root)
	if err != nil {
		return nil, err
	}
	// The one local guard the metric half adds, and it belongs beside
	// checkPushable rather than inside the pass: a folder that names one thing
	// twice fails server-side half way through, after some of it has already
	// been created, and no per-file refusal can undo that.
	//
	// ⚠️ BEFORE splitFileKindArgs NARROWS `metrics` BELOW, and that ordering is
	// the guard rather than an accident of layout. A collision is a property of
	// the FOLDER, and only one of its two halves need be named on the command
	// line — `ronja pipeline push metrics/Revenue.json` in a folder that also
	// holds `metrics/revenue.json` is the whole failure. Run on the narrowed set
	// this would see one file, find nothing, and let the push walk into exactly
	// the half-finished server-side refusal it exists to pre-empt.
	if err := checkMetricStemCollisions(local, metrics); err != nil {
		return nil, err
	}
	// IDENTITIES ONLY, here. Which row each file is comes from the manifest and
	// the lock and is needed NOW — the create decision below turns on it. The
	// SOURCES are resolved after the .sql pass, because the main case is a metric
	// reading a table this very push creates.
	metrics = resolveMetricIdentities(f, f.live(baseline), metrics)

	var targets []string
	if len(args) > 0 {
		var sqlArgs []string
		sqlArgs, sidecars, metrics, err = splitFileKindArgs(f.Root, args, sidecars, metrics)
		if err != nil {
			return nil, err
		}
		if targets, err = resolveArgPaths(f.Root, sqlArgs, local); err != nil {
			return nil, err
		}
	} else {
		targets = pipelineChangedTargets(f, local, known)
	}
	// A file removed from the folder is NOT a table this push deletes. There is
	// no delete verb here on purpose (doctrine: sync verbs yes, resource verbs
	// no), and quietly dropping a colleague's table because somebody deleted a
	// file in a merge is exactly the kind of thing a sync loop must not do.
	for _, path := range sortedPaths(known) {
		if _, onDisk := local[path]; !onDisk {
			fmt.Fprintf(os.Stderr, "  Note: %s is gone from this folder — the table it was bound to (%s) is left alone; delete it in the web app if that is what you meant.\n",
				path, f.Binding.Tables[path])
			fmt.Fprintf(os.Stderr, "        Until then this folder keeps reporting it. Remove its entry from \"tables\" in %s to stop; the local baseline is dropped with it.\n",
				wfdir.ManifestName)
		}
	}
	if len(targets) == 0 && len(sidecars) == 0 && len(metrics) == 0 {
		// The prune above is a real edit to the baseline, and this is the path it
		// is most likely to be taken on: the ordinary way to reach it is a folder
		// with nothing left to push. Saved here or the note is printed on every
		// run for ever, having dropped nothing that survives the process.
		if len(pruned) > 0 {
			if err := f.saveBaseline(); err != nil {
				fmt.Fprintf(os.Stderr, "  Note: could not record the pruned baseline (%v) — it will be reported again next time.\n", err)
			}
		}
		result.UpToDate = true
		return result, nil
	}

	// 3. Dependency order, over the WHOLE folder, so a cycle is caught wherever
	// it is rather than only inside the pushed subset.
	ordered, err := topoOrder(codec, local, targets)
	if err != nil {
		return nil, err
	}
	pushing := make(map[string]bool, len(ordered))
	sending := make(map[string]string, len(ordered))
	for _, path := range ordered {
		pushing[path] = true
		sending[path] = local[path]
	}
	// Checked on what is actually being SENT: a positional ref in a file this run
	// is not pushing cannot mis-resolve anything, and refusing on it would let one
	// stale file block every other table in the folder.
	if err := refusePositionalRefs(sending); err != nil {
		return nil, err
	}

	// 4. The local decision, per file — create or update, and the refusal a
	// create earns when this folder names no feature. Taken through
	// applyDecisionsFor so that a tree-wide apply and its dry-run read the same
	// answer this loop acts on; nothing here needs a request. See
	// sync_decision.go for where that boundary is and why it is there.
	decisions := applyDecisionsFor(f, ordered)
	if err := errFromDecisions(decisions); err != nil {
		return nil, err
	}
	creating := false
	for _, d := range decisions {
		if d.Creates() {
			creating = true
		}
	}
	// A metric file with no row behind it creates one too, and it counts here for
	// the same reason a .sql file does: a create WRITES a binding, and a binding
	// must name its organization authoritatively. Folded into the same flag
	// rather than checked separately, so a folder whose only creates are metrics
	// binds exactly as one whose only creates are tables does — that folder
	// otherwise reached the create with no organization resolved and wrote an
	// entry no later command matches.
	for _, metric := range metrics {
		if metric.Problem == "" && metric.MetricID == "" {
			creating = true
		}
	}
	// A create WRITES a binding, and a binding must name its organization
	// authoritatively — an environment token has none resolved until it asks, and
	// an entry written without one is a manifest no later command matches. Only
	// when this folder is not bound here: a MATCHED entry already names the
	// organization it belongs to, and re-resolving could adopt a different one.
	if creating && !f.Bound {
		if _, err := f.bindNew(ctx); err != nil {
			return nil, err
		}
	}

	inst := pipelineBaseline(f)
	// The tables this run brought into existence. A created table is LIVE and
	// EMPTY — the create posts the SQL but nothing builds it, and the data only
	// arrives when the draft behind it is published — so a downstream file that
	// reads one gets the server's bare "no data found" with nothing to say why.
	// Carried forward per file because the answer depends on what earlier files
	// in the same push did.
	created := map[string]bool{}
	// failed and docsFailed are counted APART. A docs_failed file's SQL was
	// written, built and is publishable — only its prose did not land — so
	// counting it among the files that "did not land" tells an author their table
	// is not there when it is, and tells a script the push produced nothing to
	// publish when it produced everything.
	failed, docsFailed := 0, 0
	for _, path := range ordered {
		// One Ctrl-C, one message. Without this every remaining file makes its
		// own doomed request and prints its own refusal, turning a single
		// interrupt into a page of them.
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		file := pushOneTable(ctx, client, f, codec, inst, path, local[path],
			upstreamsInPush(codec, local[path], pushing), created, opts)
		result.Files = append(result.Files, file)
		if file.Created {
			created[path] = true
		}
		// Saved after EVERY file, success or failure: a push that dies half way
		// has really created tables and really staged drafts, and a baseline that
		// does not know about them makes the retry read its own work as somebody
		// else's drift.
		if err := f.saveBaseline(); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not record what was pushed in the local baseline (%v).\n", err)
		}
		switch file.Outcome {
		case pushOutcomePushed:
		case pushOutcomeDocsFailed:
			docsFailed++
		default:
			failed++
		}
	}

	// 6. The DOCS SIDECARS — the tables this folder documents but does not build.
	// After the SQL, so a report reads as "here is what I built, here is what I
	// documented", and so a sidecar for a table an earlier file in this same push
	// created is written against a row that now exists.
	result.Docs = pushTableDocs(ctx, client, f, inst, sidecars, opts)
	refusedDocs, settledDocs := 0, 0
	for _, doc := range result.Docs {
		switch doc.Outcome {
		case docsOutcomeRefused:
			refusedDocs++
		case docsOutcomeUpToDate, docsOutcomeNothing:
			// Both mean "this file changed nothing and needs nothing" — the row
			// already agrees, or the file declares nothing to disagree with — and
			// that is the question UpToDate below is asking. Counting only the
			// first would make a folder holding one placeholder sidecar report
			// upToDate: false on every otherwise-clean run.
			settledDocs++
		}
	}
	// 7. The METRIC FILES — the numbers this folder defines. After the SQL for
	// the reason wfdir.MetricsDirName states: a metric contributes no `{{ ref }}`
	// edge and so cannot join topoOrder, and running last is what makes a metric
	// reading a table this same push created land after that table's create.
	// After the docs sidecars too, so the report reads as "built, documented,
	// defined".
	//
	// THE SOURCES ARE RESOLVED HERE, not with the identities above: a metric
	// reading a table this same push created resolves through
	// f.Binding.Tables, which the loop above has just filled in.
	result.Metrics = pushMetricFiles(ctx, client, f, inst, resolveMetricSources(f, codec, metrics), opts)
	refusedMetrics, settledMetrics, metricProseFailed := 0, 0, 0
	for _, metric := range result.Metrics {
		switch metric.Outcome {
		case metricOutcomeRefused, metricOutcomeBuildFailed:
			refusedMetrics++
		case metricOutcomeDescriptionFailed:
			metricProseFailed++
		case metricOutcomeUpToDate:
			settledMetrics++
		}
	}
	// UP TO DATE IS AN ANSWER ABOUT THE OUTCOMES, not about the early return that
	// used to be its only home. That branch is now taken only by a folder with no
	// sidecars and no metrics at all, so any folder keeping one reported
	// `upToDate: false` on every clean run — the one key a script watches to
	// decide whether a push changed the organization, saying "yes" every time.
	result.UpToDate = len(result.Files) == 0 &&
		settledDocs == len(result.Docs) &&
		settledMetrics == len(result.Metrics)

	// The problems, counted apart and said apart. Three different things go wrong
	// here and they have different fixes: a file that never landed, a file whose
	// SQL landed and whose prose did not, and a docs sidecar refused before any
	// request.
	var problems []string
	if failed > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d file(s) did not land", failed, len(result.Files)))
	}
	if docsFailed > 0 {
		problems = append(problems, fmt.Sprintf("%d file(s) built, and are publishable, but their documentation did not land", docsFailed))
	}
	if refusedDocs > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d docs file(s) were refused", refusedDocs, len(result.Docs)))
	}
	if refusedMetrics > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d metric(s) did not land", refusedMetrics, len(result.Metrics)))
	}
	if metricProseFailed > 0 {
		problems = append(problems, fmt.Sprintf("%d metric(s) were staged and built, and are publishable, but their description did not land", metricProseFailed))
	}
	if len(problems) == 0 {
		return result, nil
	}
	result.Error = strings.Join(problems, ", and ")
	return result, fmt.Errorf("%s", result.Error)
}

// splitFileKindArgs takes the docs sidecars and the metric files out of a push's
// positional arguments.
//
// `ronja pipeline push tables/orders.json` — or `metrics/aov.json` — is the
// obvious thing to type after editing one, and resolveArgPaths would refuse it,
// correctly for a .sql resolver, with a message about a file that is not SQL. So
// both non-SQL carriers are matched first, by path, and only what is left goes
// to the SQL resolver.
//
// Naming ANY path narrows EVERY pass to what was named, exactly as it narrows
// the SQL half to the files named: `push orders.sql` means that file and nothing
// else, and quietly re-pushing eleven docs files and four metrics beside it
// would be the command doing more than it was asked.
//
// THREE WAYS rather than two, and the split is on the DIRECTORY the path is in
// rather than on its extension — both carriers are `.json`, so the extension
// says nothing. A path that looks like one of them and is not in the folder is
// refused by name, which is the difference between "you spelled it wrong" and
// this file quietly going to the SQL resolver to be refused for the wrong reason.
func splitFileKindArgs(root string, args []string, sidecars []tableDocsSidecar, metrics []pipelineMetricFile) (
	sqlArgs []string, selectedDocs []tableDocsSidecar, selectedMetrics []pipelineMetricFile, err error) {

	docsByPath := make(map[string]tableDocsSidecar, len(sidecars))
	for _, file := range sidecars {
		docsByPath[file.Path] = file
	}
	metricsByPath := make(map[string]pipelineMetricFile, len(metrics))
	for _, file := range metrics {
		metricsByPath[file.Path] = file
	}
	for _, arg := range args {
		abs, absErr := filepath.Abs(arg)
		if absErr != nil {
			return nil, nil, nil, fmt.Errorf("resolve %s: %w", arg, absErr)
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil, nil, nil, fmt.Errorf("resolve %s against %s: %w", arg, root, relErr)
		}
		rel = filepath.ToSlash(rel)
		if file, ok := docsByPath[rel]; ok {
			selectedDocs = append(selectedDocs, file)
			continue
		}
		if file, ok := metricsByPath[rel]; ok {
			selectedMetrics = append(selectedMetrics, file)
			continue
		}
		if _, isSidecarPath := wfdir.TableDocsAlias(rel); isSidecarPath {
			return nil, nil, nil, fmt.Errorf("%s is not a docs file in this folder", arg)
		}
		if _, isMetricPath := wfdir.MetricAlias(rel); isMetricPath {
			return nil, nil, nil, fmt.Errorf("%s is not a metric file in this folder", arg)
		}
		sqlArgs = append(sqlArgs, arg)
	}
	return sqlArgs, selectedDocs, selectedMetrics, nil
}

// explainUnreadableRefs names the `{{ ref }}` ids in this file that the caller
// cannot read, rendered as the marker lines a refusal quotes — or "" when every
// ref reads fine.
//
// It exists because a 403 from a table write has two causes and the status
// cannot tell them apart. rmodelv2.AssertTablesReadable refuses code whose refs
// the caller cannot reach, and rjerr.Forbiddenf deliberately withholds WHICH id
// — a message naming it would answer "does table-X exist?" for anyone who can
// guess an id. So the CLI, which is entitled to ask on its own behalf, asks:
// GET /feature/model/:id refuses an id the caller cannot reach, and that is the
// same reach question the refusal asked. An id that reads fine is not the cause,
// and the caller's admin explanation stands.
//
// ⚠️ THE REFUSAL'S STATUS DEPENDS ON THE INSTANCE, and that is the whole
// cross-organization case. A row the caller cannot see is not "forbidden" and is
// not "missing" — RLS makes it invisible, so the read finds no row and the
// handler answers the lookup-miss sentinel: `400 {"error":"no rows"}` on older
// backends still in the field, `404 {"error":"not found"}` on current ones. This
// binary ships against both and accepts both, which is why the 400 arm stays.
// Reachability and existence are indistinguishable here by design (see
// rjerr.Forbiddenf above: naming the difference would answer "does table-X
// exist?"), which is exactly why all three statuses count as "not reachable by
// you" and none of them claims to know which.
//
// This was wrong once and shipped green in the other direction: the fake
// instance answered 404 for an unknown table when every real one answered 400,
// so the test passed against a server that did not exist. Both answers are
// staged now, because both are somebody's production.
//
// A round trip PER REF, and NONE of it on the happy path — this runs only after
// a write has already been refused, where the requests are cheap and a second
// wrong diagnosis is not. Bounded by the refs in one file.
//
// An id that fails for any OTHER reason is left out rather than accused: a
// transport error says nothing about reachability, and a refusal that named a
// table for being unreadable during a network blip would be the same wrong
// guess in nicer words.
//
// The wording matches `ronja wf validate`'s unresolved_ref finding on purpose.
// One root cause reported in two vocabularies is how the reader learns that
// neither is to be trusted.
func explainUnreadableRefs(ctx context.Context, client *api.Client, inputs []string) string {
	var out strings.Builder
	for _, id := range inputs {
		if _, err := client.GetTable(ctx, id); err == nil {
			continue
		} else if status := api.StatusOf(err); status != 400 && status != 403 && status != 404 {
			continue
		}
		fmt.Fprintf(&out, "      {{ ref('%s') }} — %q isn't a table reachable through your feature membership\n", id, id)
	}
	return out.String()
}

// upstreamsInPush names the folder files this one reads from that are ALSO being
// pushed in this run.
//
// It exists because of a limit worth stating out loud rather than hiding: a
// draft builds against its inputs' LIVE data, never against their drafts. So
// when an upstream is dirty in the same push, the downstream's confidence report
// describes a build that read the upstream as it is TODAY — not as it will be
// once the upstream is published. Draft-overlay chaining is a follow-up; until
// then the honest thing is to say which upstream it was.
func upstreamsInPush(c pipelineCodec, code string, pushing map[string]bool) []string {
	var out []string
	// folderUpstreams, so a ref spelled as a sibling's stem counts — see
	// topoOrder, which builds its edges from the same answer. A note that fired
	// only for the id spelling would go quiet for exactly the folders this slice
	// exists to make possible.
	for _, path := range c.folderUpstreams(code) {
		if pushing[path] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// pushOneTable runs the whole edit cycle for one file.
//
// Failure is DATA, not an error return: the caller records what happened, moves
// on to the next file, and exits non-zero at the end. A push of twelve tables
// that stopped dead on the third would leave nine perfectly pushable files
// unattempted for no reason.
func pushOneTable(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec, inst *wfdir.InstanceState,
	path, content string, upstreams []string, created map[string]bool, opts pipelinePushOptions) pipelineFileResult {

	out := pipelineFileResult{Path: path, TableID: f.Binding.Tables[path]}
	// The file's OPT-IN documentation header, read before anything else because
	// three later decisions turn on whether there is one: the create body, the
	// third drift leg (which arms only for a push that carries documentation),
	// and the write that follows the build.
	//
	// Warnings, never refusals — a malformed comment must not be able to stop a
	// push of SQL that is perfectly good. They are printed AND carried in the
	// result, so a --json caller sees the same thing a person does.
	docs, docWarnings := tabledocs.ParseHeader(content)
	for _, warning := range docWarnings {
		fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", path, warning)
		out.Notes = append(out.Notes, warning)
	}
	// Where this folder's LIVE fingerprints live — the lock file on a named
	// stack, the local baseline on a legacy one. See liveHashes.
	liveHash := f.live(inst)
	refuse := func(format string, args ...any) pipelineFileResult {
		out.Outcome = pushOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
		return out
	}
	// refuseConflict is refuse for the one refusal a caller can DO something
	// about: it stamps Conflict so a script never has to read the prose to tell
	// "somebody got there first" from "you may not do this at all".
	refuseConflict := func(format string, args ...any) pipelineFileResult {
		out.Conflict = true
		return refuse(format, args...)
	}

	// `content` is DISK form — the names the author committed. Everything SENT
	// from here on is `wire`, and everything COMPARED or RECORDED stays disk form.
	// Resolved here, inside the loop, because a sibling's id only exists once that
	// sibling's own create has returned earlier in this same run.
	wire, wireErr := codec.toWire(path, content)
	if wireErr != nil {
		return refuse("%s", wireErr)
	}
	// Derived from the RESOLVED code, never from the disk form: input_models is a
	// list of real table ids, and deriving it from names would declare an empty
	// lineage for a folder written entirely against aliases — which the server
	// then fills in by its own derivation, or refuses.
	inputs := tablerefs.DeriveInputModels(wire)

	// --- 0. Is the baseline even about this row? ----------------------------
	// The manifest says one table and the baseline was taken from another, so
	// every fingerprint below is a fingerprint of something else. Silently
	// inheriting them would compare a colleague's table against a hash from a
	// different table and conclude nothing had changed.
	if recorded := inst.Tables[path].TableID; recorded != "" && out.TableID != "" && recorded != out.TableID {
		if !opts.Force {
			return refuse("%s is bound to %s in %s, but the local baseline was taken from %s — the binding was repointed by hand, by a merge, or by a second clone, so nothing here can say what changed.\n    Re-clone the feature into a fresh folder, delete %s to resync, or push --force to accept the new binding",
				path, out.TableID, wfdir.ManifestName, recorded, wfdir.StatePath(f.Root))
		}
		fmt.Fprintf(os.Stderr, "  Note: --force — %s was rebound from %s to %s; the old baseline is discarded.\n",
			path, recorded, out.TableID)
		delete(inst.Files, path)
		delete(inst.Tables, path)
	}

	// --- 1. Create, for a file with no table behind it yet ------------------
	// The row a timed-out create was reconciled onto, and the ONLY thing that
	// makes a created table's live fingerprint readable below: an ordinary create
	// answers with the row it made, a reconciled one has to be read back.
	var adoptedRow *api.Table
	if out.TableID == "" {
		name := tableNameFor(path)
		// The header's prose rides the create so the table does not exist even
		// briefly with none. Its `fields` all come back UNMATCHED — a create has
		// measured nothing — which is why the report a reader is shown is taken
		// from the documentation write after the build, not from here.
		create := api.CreateTableInput{
			Name:        name,
			FeatureID:   f.Binding.FeatureID,
			Kind:        api.TableKindDerived,
			Engine:      api.TableEngineDuckDB,
			Code:        wire,
			InputModels: inputs,
			Fields:      docsCreateFields(docs),
		}
		if docs.Description != nil {
			create.Description = *docs.Description
			if create.Description != "" {
				create.DescriptionSource = api.DescriptionSourceUser
			}
		}
		row, err := client.CreateTable(ctx, create)
		switch {
		case err == nil:
			out.TableID = row.IdentityID()
		case api.IsTimeout(err):
			// A create whose request TIMED OUT may well have committed: the
			// deadline was ours, the transaction was the server's. Treated as a
			// plain failure the table stays unbound, and the NEXT push creates a
			// second live table with the same name beside the first — the one
			// mistake this loop cannot undo, because it has no delete verb.
			adopted, why := reconcileTimedOutCreate(ctx, client, f.Binding, name, wire)
			if adopted == nil {
				return refuse("creating the table for %s in feature %s timed out, and %s: %v\n    Look at the feature in the web app before pushing again — if the table IS there and holds this file's SQL, add its id to \"tables\" in %s, or the next push creates a second one beside it",
					path, f.Binding.FeatureID, why, err, wfdir.ManifestName)
			}
			fmt.Fprintf(os.Stderr, "  Note: creating the table for %s timed out, but it had landed — adopting %s rather than creating a second one.\n", path, adopted.ID)
			out.TableID = adopted.ID
			adoptedRow = adopted
		case api.StatusOf(err) == 403:
			// TWO refusals wear this status, and the CLI used to report both as
			// the second one. See explainUnreadableRefs: an unreachable
			// {{ ref }} is checked first, because it is the answer the message
			// cannot guess and the one an admin token still gets.
			if why := explainUnreadableRefs(ctx, client, inputs); why != "" {
				return refuse("creating the table for %s in feature %s was refused, and it reads a table you cannot:\n%s    Point the ref at a table in this organization, or get access to the feature it lives in. A folder cloned from another organization carries that organization's ids in its SQL",
					path, f.Binding.FeatureID, why)
			}
			// The other one, and it is about WHO you are rather than what you
			// sent. Creating a table is admin-only by design (POST
			// /feature/model); EDITING the tables that exist is not — the whole
			// draft-and-review path this folder drives is open to a non-admin. So
			// a push that refuses here is not a push that cannot run: it is one
			// file that needs an admin once.
			return refuse("creating the table for %s in feature %s was refused (%v).\n    Creating a NEW table needs an admin. Editing tables that already exist does not — that is what your drafts are, and `ronja pipeline push` runs the whole cycle for them.\n    Ask an admin to create %q in that feature (or to run this one push), then add its id to \"tables\" in %s and push again",
				path, f.Binding.FeatureID, err, name, wfdir.ManifestName)
		default:
			// A name already taken in the target feature, which the generic
			// arm reported as "HTTP 409" and nothing else. It is the one
			// create refusal the author fixes in the FOLDER — the file stem is
			// the name — so say which of the two things to rename, and never
			// suggest a retry: this create will refuse identically forever.
			if why, taken := api.AsNameTaken(err); taken {
				return refuse("creating the table for %s in feature %s was refused — that name is taken:\n      %s\n    Rename the file (its stem is the table's name), or rename the table already holding the name in the web app, then push again",
					path, f.Binding.FeatureID, why)
			}
			return refuse("create the table for %s in feature %s: %v", path, f.Binding.FeatureID, err)
		}
		out.Created = true
		// Recorded IMMEDIATELY, before anything else is attempted. CreateTable
		// returns a LIVE row — colleagues can see it the moment it exists — so a
		// push that died before saving the manifest would leave a table nobody
		// can find and the next push would create a second one beside it. That is
		// worse here than for a workflow, whose create makes a hidden draft.
		f.recordBinding(f.Binding.WithTable(path, out.TableID))
		if err := f.saveFolder(); err != nil {
			out.Outcome = pushOutcomeRefused
			out.Error = fmt.Sprintf("table %s was CREATED, but %s does not name it: %v", out.TableID, wfdir.ManifestPath(f.Root), err)
			fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n  Add it by hand, or the next push will create a second table for this file.\n", path, out.Error)
			return out
		}
	}

	// --- 2. Resolve the draft ----------------------------------------------
	// GetTableDraft FIRST, always. CheckoutTable is not idempotent — a second
	// checkout while a draft is open is an error — so "check out and see" is not
	// available here the way it is for a workflow.
	draft, err := client.GetTableDraft(ctx, out.TableID)
	if err != nil {
		return refuse("check for your draft of %s: %v", out.TableID, err)
	}

	// --- 3. The drift guard, in two legs -----------------------------------
	// The server now carries both compare-and-swap layers — baseCodeSha256 on the
	// PUT below and a base-version CAS on the commit — so this is no longer the
	// whole protection, and the belt-and-braces is deliberate rather than
	// leftover. This guard runs BEFORE a write, a sync and a build that the
	// server's precondition would only refuse at the end of; it covers the LIVE
	// row, which no precondition on a draft write can speak about at all; and it
	// can explain the difference in terms of the FOLDER — this file, that table —
	// where the server can only name two digests. It remains a check at one
	// instant, which is what layer 1 exists to close.
	//
	// Each leg compares a row against the fingerprint TAKEN FROM THAT ROW, which
	// is the whole of wfdir.TableState's invariant: leg (a) the live table against
	// LiveSHA256, leg (b) our own draft against DraftSHA256. One shared hash
	// refused an ordinary push → edit → push, because the bytes we had written
	// into the draft are not the bytes live holds and never were.
	// `content` is the DISK form, and the baseline has to be disk form — but a
	// row we just created holds the RESOLVED wire form, not this. The two are
	// the same fingerprint by construction: what we sent is this file with its
	// aliases substituted, so de-aliasing the row would return exactly these
	// bytes, and the round trip is skipped rather than performed.
	liveCode := content
	liveKnown := out.Created
	if adoptedRow != nil {
		// A reconciled create is the one place "what we sent" is a claim about
		// somebody else's row, so the fingerprint is taken from the row that was
		// read back. The reconcile adopts only on an exact match, so this is the
		// same bytes — read from the copy that is actually going to be compared
		// against, rather than from the file that hopes to be it.
		if code, unresolved := codec.canonicalDisk(content, adoptedRow); !unresolved {
			liveCode = code
		}
	}
	if !out.Created {
		recorded := inst.Tables[path]
		live, err := client.GetTable(ctx, out.TableID)
		if err != nil {
			return refuse("read %s: %v", out.TableID, err)
		}
		if code, unresolved := codec.canonicalDisk(content, live); !unresolved {
			liveCode, liveKnown = code, true
		}
		var drifted []string
		// sqlDrifted counts the legs that are about CODE, so the refusal below
		// can head itself with what actually fired. Legs (a) and (b) are the SQL;
		// leg (c) is the prose, and a run where only it fired must not be
		// announced as "the SQL changed" — the SQL is byte-identical, and a
		// reader sent to look for a code change that is not there stops believing
		// the message.
		sqlDrifted := 0
		// (a) The LIVE table moved: a colleague committed while we were away.
		if reason := driftReason(codec, content, "the live table", live, liveHash.get(path)); reason != "" {
			drifted = append(drifted, reason)
			sqlDrifted++
		}
		// (c) The live table's DOCUMENTATION moved — an admin rewrote a
		// description in the web app, or the agent documented a column — and this
		// push is about to write prose of its own over it at the next publish.
		//
		// A third leg rather than a widening of (a), by the invariant on
		// wfdir.TableState: it is taken from a different part of the row and
		// answers a different question, so folding it into the SQL hash would
		// report "the SQL changed" for a description nobody's SQL touched.
		//
		// ARMED ONLY BY A FILE THAT CARRIES A HEADER. A folder that documents
		// nothing makes no claim about anybody's prose, so there is nothing here
		// to protect and this leg does not exist for it.
		if !docs.Empty() {
			if reason := docsDriftReason("the live table", live, liveHash.getMeta(path)); reason != "" {
				drifted = append(drifted, reason)
			}
		}
		// (b) Our OWN draft moved. Not paranoia: the chat agent's editDerivedTable
		// works in exactly this row, so without this leg a web or chat edit to
		// your own draft vanishes silently under the PUT below.
		if draft != nil {
			against := recorded.DraftSHA256
			// ONLY when the fingerprint was taken from THIS draft. A recorded id
			// that is not the open one describes a draft that has since been
			// committed or discarded, and the row now open is a different one —
			// comparing it against the old draft's bytes reports drift on a draft
			// nobody touched, or, if they happen to match, agreement on one
			// somebody did.
			if recorded.DraftID != draft.ID {
				against = ""
			}
			if against == "" && liveKnown {
				// A draft this checkout never wrote to — opened in the web builder
				// or by chat, or replacing one this folder used to know about. There
				// is no fingerprint of it, but a draft nobody has edited is
				// byte-identical to the parent it forked from, so the LIVE row is a
				// sound thing to compare it against: a difference is somebody's
				// unsynced work, which is exactly what this leg is for.
				against = wfdir.HashString(liveCode)
			}
			if reason := driftReason(codec, content, "your draft "+draft.ID, draft, against); reason != "" {
				drifted = append(drifted, reason)
				sqlDrifted++
			}
		}
		forced := false
		if len(drifted) > 0 {
			if !opts.Force {
				return refuse("%s of %s changed on the server since your last sync — pushing would overwrite it:\n      %s\n    Run `ronja pipeline status` to see the detail, or push --force to overwrite",
					driftHeadline(sqlDrifted, len(drifted)), out.TableID, strings.Join(drifted, "\n      "))
			}
			fmt.Fprintf(os.Stderr, "  Note: --force — overwriting %s: %s\n", path, strings.Join(drifted, "; "))
			forced = true
		}
		// An absent live baseline is adopted from the row just read, rather than
		// left absent for ever: "no baseline" is a state a folder should be able to
		// leave, and the read that would prove it has already happened. Only when
		// there was none — an agreement recorded at a fork is not re-taken here,
		// or leg (a) would forget a colleague's commit the moment it saw it.
		//
		// A FORCED push is the other moment the live fingerprint moves: --force is
		// the author saying they have seen what is on the server and mean to write
		// over it, so the acknowledgement is recorded. Without it the same
		// difference is re-reported on every subsequent push and --force becomes
		// a permanent part of the command line — which trains it past the one time
		// it means something.
		if liveKnown && (forced || liveHash.get(path) == "") {
			recordLiveAgreement(liveHash, path, out.TableID, liveCode)
		}
		// The documentation half of the same acknowledgement, on the same rule
		// and for the same two reasons: an absent recording is a state a folder
		// should be able to leave, and a --force push is the author saying they
		// have seen what is on the server. Only for a folder that documents
		// something — recording a fingerprint for a folder that makes no claim
		// would arm a guard nothing ever needed and refuse a later push for a
		// change that was never this folder's business.
		//
		// `live` is a GetTable answer, which is the only response that carries
		// the column catalog — see metaOf.
		if !docs.Empty() && (forced || liveHash.getMeta(path) == "") {
			recordMetaAgreement(liveHash, path, out.TableID, metaOf(live))
		}
	}

	if draft == nil {
		draft, err = client.CheckoutTable(ctx, out.TableID)
		if err != nil {
			return refuse("check out a draft of %s: %v", out.TableID, err)
		}
		// A fresh draft forks from live, so this is the moment the two rows agree
		// and the fingerprint leg (a) will compare against from now on.
		if liveKnown {
			recordLiveAgreement(liveHash, path, out.TableID, liveCode)
		}
	}
	out.DraftID = draft.ID
	out.URL = draft.URL
	// Recorded before the write, so a command killed mid-build still knows which
	// draft holds the work.
	recordDraftPointer(inst, path, out.TableID, draft.ID)

	// --- 4. Write, sync, wait ----------------------------------------------
	// The server's layer-1 precondition, and the ONE fingerprint that may be sent
	// as it: the WIRE form of what this checkout last wrote into THIS draft. What
	// goes over the PUT is `wire` — the file with its aliases and sibling stems
	// substituted for ids — so that is what the row stores, and the disk-form
	// DraftSHA256 beside it would 409 every push from any folder that spells a
	// ref by name. See wfdir.TableState: a hash is only ever compared against the
	// row it was taken from.
	//
	// EMPTY sends no precondition, which is the server's "write
	// unconditionally", and it is the honest answer in the two cases that reach
	// it: a draft this checkout has never written to (recordDraftPointer above
	// has just cleared a fingerprint that described a different row), and a
	// --force push. --force is the author saying they have seen what is on the
	// server and mean to write over it; sending the precondition anyway would
	// make the flag refuse the very thing it exists to permit.
	basis := inst.TableStateFor(path).DraftWireSHA256
	if opts.Force {
		basis = ""
	}
	if err := client.UpdateTableCode(ctx, draft.ID, wire, inputs, basis); err != nil {
		// The precondition was refused: somebody edited this draft — the chat
		// agent, the web builder — between our read a moment ago and this write.
		// Nothing was written, so their edit is intact and so is the file.
		//
		// Reachable even though leg (b) above just compared the same row, because
		// the two look at different instants: that comparison is a read, this is
		// the write, and the whole reason the precondition exists is the gap
		// between them.
		if api.StatusOf(err) == api.StatusConflict {
			return refuseConflict("draft %s changed between reading it and writing to it — somebody edited it in the web app or by chat, and nothing was written:\n      %v\n    Run `ronja pipeline status` to see what it holds now, or push --force to overwrite it",
				draft.ID, err)
		}
		// Same refusal as the create's, reached by the other door: a folder whose
		// binding already names a table here still writes SQL whose refs may not
		// be readable. Diagnosed here too, or the second push of a promotion
		// reports the same cross-organization ref as a bare 403 with nothing in
		// it — which is how one root cause grew two qualities of diagnosis.
		if api.StatusOf(err) == 403 {
			if why := explainUnreadableRefs(ctx, client, inputs); why != "" {
				return refuse("writing the SQL of %s into draft %s was refused, and it reads a table you cannot:\n%s    Point the ref at a table in this organization, or get access to the feature it lives in",
					path, draft.ID, why)
			}
		}
		return refuse("write the SQL of %s into draft %s: %v", path, draft.ID, err)
	}
	// The draft now holds these bytes whatever the build makes of them, so leg (b)
	// is advanced HERE and not after the build: a failed build that left this
	// alone would read its own last attempt as somebody else's edit and refuse the
	// fix.
	recordDraftWrite(inst, path, out.TableID, draft.ID, content, wire)
	// A 404 while watching is terminal and specific: a draft stops existing the
	// moment it is committed or discarded, so somebody landed or dropped this one
	// from the web UI mid-build. The recorded pointer goes with it, or every later
	// command asks the server about a row that is gone.
	awaitBuild := func() (*api.TableBuildResult, error) {
		build, err := client.WaitForTableBuild(ctx, draft.ID)
		if err != nil && api.StatusOf(err) == 404 {
			recordDraftPointer(inst, path, out.TableID, "")
		}
		return build, err
	}
	if err := client.SyncTable(ctx, draft.ID); err != nil {
		// A sync is refused outright while a build of the same row is still
		// running, and that is exactly the state an interrupted push leaves
		// behind: Ctrl-C stops the CLI listening, never the build. Treating it as
		// a plain refusal makes the resume this loop is built for die on its
		// second step every time.
		//
		// Told apart from the other refusals by the ROW rather than by the
		// sentence: the error envelope carries a message and no code, and a CLI
		// that matched on English would start accepting the wrong 400 the day the
		// wording changed.
		if api.StatusOf(err) != 400 || !buildRunning(ctx, client, draft.ID) {
			return refuse("build draft %s: %v", draft.ID, err)
		}
		fmt.Fprintf(os.Stderr, "  Note: a build of %s was already running — waiting for it before building the SQL this push just wrote.\n", path)
		if _, err := awaitBuild(); err != nil {
			return refuse("%s", pipelineErrorText(err))
		}
		// Re-issued rather than joined. The build that was running started before
		// this push wrote its bytes, so its data describes the PREVIOUS SQL —
		// reporting it as this push's result would record a baseline whose code
		// and data came from two different versions of the file.
		if err := client.SyncTable(ctx, draft.ID); err != nil {
			return refuse("build draft %s: %v", draft.ID, err)
		}
	}
	for _, upstream := range upstreams {
		// Said out loud because the confidence report below would otherwise be
		// quietly describing a build against yesterday's upstream.
		fmt.Fprintf(os.Stderr, "  Note: %s was built against the LIVE %s — the draft of that upstream is not part of this build.\n",
			path, upstream)
		out.Notes = append(out.Notes, fmt.Sprintf("validated against live %s", upstream))
	}
	build, err := awaitBuild()
	if err != nil {
		return refuse("%s", pipelineErrorText(err))
	}
	out.Verdict = build.Verdict
	if !build.OK() {
		out.Outcome = pushOutcomeBuildFailed
		out.Error = build.ErrorMessage()
		if out.Error == "" {
			out.Error = "the build failed and recorded no reason"
		}
		// The one failure message that needs a diagnosis rather than a repeat.
		if hint := pipelineNoDataHint(out.Error, inputs, f.Binding, inst, created); hint != "" {
			out.Hint = hint
			fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", path, hint)
		}
		return out
	}

	// --- 5. The documentation, AFTER the build ------------------------------
	// THE ORDER IS THE RULE: push → build → attach → report. The join rule
	// attaches prose to the columns the table has MEASURED, so a `fields` write
	// sent with the SQL would be judged against the previous build's columns —
	// and against no columns at all on a table that has just been created. Sent
	// here, the report is about the schema this push actually produced.
	//
	// It goes to the DRAFT, like the SQL: committing copies the draft's column
	// rows onto the live table, so prose written on the live row while a draft is
	// open is lost the moment anybody publishes.
	//
	// BEFORE recordSynced, which is the whole reason this step moved above it.
	// The content baseline is what the bare-push selector compares against, so a
	// file recorded as synced while its prose did NOT land is a file the next
	// bare push skips — for ever, silently, with the committed file and the row
	// saying different things. The one divergence this loop exists to remove.
	if in, send := docsWriteInput(docs); send {
		report, err := client.UpdateTableDocs(ctx, draft.ID, in)
		out.Docs = docsResultFrom(docs, report, err)
		if err != nil {
			// A 404 is the draft being GONE — committed or discarded from the web
			// app between the build and this write — and the recorded pointer has
			// to go with it, exactly as awaitBuild's 404 arm does above. Without
			// this the next `publish` commits a row that no longer exists, and
			// the failure it reports is about a draft id rather than about what
			// happened.
			if api.StatusOf(err) == 404 {
				recordDraftPointer(inst, path, out.TableID, "")
			}
			// The SQL landed and built; only the prose did not. Reported as its
			// own outcome rather than folded into a green push, and returned
			// WITHOUT advancing the baseline — the same shape as a failed build
			// above, and for the stronger reason: a build failure is loud on the
			// next push, and a skipped file is silent for ever.
			out.Docs.Error = pipelineErrorText(err)
			out.Outcome = pushOutcomeDocsFailed
			out.Error = fmt.Sprintf("the SQL of %s was written and built, but its documentation was not: %s", path, out.Docs.Error)
			if hint := docsRefusalHint(err); hint != "" {
				out.Error += "\n    " + hint
			}
			fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", path, out.Error)
			return out
		}
		if line := describeDocsResult(out.Docs); line != "" {
			fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", path, line)
		}
	}

	// --- 6. Success: baseline, then the confidence report -------------------
	// Written, built AND documented, which is what makes this a complete sync —
	// the one thing "changed since your last push" is measured against. Recorded
	// per file as it lands, which is what lets a push that stops later leave an
	// accurate baseline behind.
	recordSynced(inst, path, content)
	out.Outcome = pushOutcomePushed

	out.Review = fetchReview(ctx, client, draft.ID, path)
	// Samples are read only AFTER a successful sync: a draft that has been
	// checked out and never synced has no partitions of its own. And only when
	// the row count the report just fetched says the table is small enough to be
	// worth it — see maxSampledRowCount for why five rows of garnish are not
	// worth an LLM routing call and a Batch job.
	switch {
	case flagJSON:
		// The sample is rendered by the human report alone — the field is
		// json:"-" — and reading it costs a duckdb query, which means LLM routing
		// and, on a big table, a Batch job. Nothing should pay that for output
		// this run will not print.
	case out.Review != nil && out.Review.DraftRowCount > maxSampledRowCount:
		fmt.Fprintf(os.Stderr, "  Note: no sample rows for %s — %s rows is large enough that the read costs real compute; query it directly if you want a look.\n",
			path, describeRowCount(out.Review.DraftRowCount))
	default:
		if csv, err := sampleRows(ctx, client, draft.ID); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: no sample rows for %s (%v) — the build itself succeeded.\n", path, err)
		} else {
			out.Sample = csv
		}
	}
	return out
}

// driftHeadline names WHAT moved, from the legs that actually fired.
//
// Three legs, two subjects: (a) the live table's SQL and (b) our own draft are
// code, (c) is the row's prose. A refusal that always said "the SQL changed"
// sent a reader looking for a code difference that does not exist whenever a
// colleague had merely reworded a description — and a message that is wrong in a
// case anybody meets is one nobody reads in the cases it is right.
func driftHeadline(sqlLegs, total int) string {
	switch {
	case sqlLegs == 0:
		return "the documentation"
	case sqlLegs == total:
		return "the SQL"
	default:
		return "the SQL and the documentation"
	}
}

// buildRunning reads a row back to ask whether a build of it is in flight.
//
// The question the refused sync raises, asked of the state rather than of the
// message: the API's error envelope carries a sentence and no machine-readable
// code, so the only stable signal available is the row itself.
//
// Answers false on a read that fails, which keeps the caller on the refusal it
// already has rather than sitting out a build nothing says is running.
func buildRunning(ctx context.Context, client *api.Client, id string) bool {
	row, err := client.GetTable(ctx, id)
	if err != nil {
		return false
	}
	// The status, not the verdict: `building` is the raw state the server's own
	// in-flight guard is computed from, and it is the one an older instance that
	// serves no verdict at all still reports.
	return row.Status == api.TableStatusBuilding
}

// reconcileTimedOutCreate asks what the feature actually holds after a table
// create whose outcome is unknown, returning the row the create landed as, or
// nil and the clause the caller folds into its refusal.
//
// The trigger is a TIMEOUT and nothing else, on reconcileTimedOutPut's rule: a
// create the server ANSWERED with an error is one it considered and refused, and
// asking after those is precisely how a folder adopts a table it never made.
//
// A NAME IS NOT AN IDENTITY. Nothing in the schema makes a table name unique
// within a feature, so "one row carries this name" is evidence and never proof:
// the same name can belong to a table that has been there for a year, to a
// colleague's create racing this one, or to another file in this very folder.
// Adopting the wrong one binds this file to somebody else's row, and the very
// next push hands their SQL to be overwritten — worse than the duplicate table
// this reconcile exists to prevent. So the name only NARROWS the candidates, and
// three further conditions have to hold before one is adopted:
//
//   - it is not already bound to another file in this folder, which would point
//     two files at one table and make every later push a fight between them;
//   - it is a LIVE row — the listing excludes drafts and committed version
//     snapshots, and this asserts that rather than assuming it;
//   - its stored SQL is byte-for-byte what this push sent. That is the actual
//     proof of authorship, and it is also what makes the caller's live
//     fingerprint truthful: a row adopted on the name alone would have this
//     file's hash recorded as the server's state, and both drift legs would then
//     read clean over somebody else's work until a publish overwrote it.
//
// Comparing canonical SQL is sound because push refuses to send a positional ref
// at all, so what was sent is already in the form the row stores.
//
// ⚠️ `content` here is the RESOLVED code — what the create actually sent — and the
// comparison is against the row's canonical code in ID form, deliberately NOT
// de-aliased. Both sides are then in the one vocabulary the server stores, which
// is the vocabulary the question is about: did this push write this row.
//
// One read plus one row read, no retry loop: the question has a single answer
// and it is worth exactly that. A read that ALSO fails leaves the create
// unreconciled, which is the conservative half of the same choice.
func reconcileTimedOutCreate(ctx context.Context, client *api.Client, binding wfdir.Binding,
	name, content string) (*api.Table, string) {

	noSingleMatch := fmt.Sprintf("reading the feature back did not identify a single unbound table named %q that it left behind", name)
	if binding.FeatureID == "" || name == "" {
		return nil, noSingleMatch
	}
	items, err := client.ListFeatureTables(ctx, binding.FeatureID)
	if err != nil {
		return nil, noSingleMatch
	}
	bound := map[string]bool{}
	for _, id := range binding.Tables {
		bound[id] = true
	}
	found := ""
	for _, item := range items {
		if item == nil || item.Name != name || item.Kind != api.TableKindDerived {
			continue
		}
		if item.ParentModelID != "" || item.ShadowStatus != "" || bound[item.ID] {
			continue
		}
		if found != "" {
			return nil, fmt.Sprintf("reading the feature back found more than one unbound table named %q, so which of them this push created cannot be told apart", name)
		}
		found = item.ID
	}
	if found == "" {
		return nil, noSingleMatch
	}
	row, err := client.GetTable(ctx, found)
	if err != nil {
		return nil, noSingleMatch
	}
	code, unresolved := row.CanonicalCode()
	if unresolved || code != content {
		return nil, fmt.Sprintf("the one unbound table named %q in that feature (%s) holds SQL this push did not send, so it is somebody else's row rather than the create that timed out", name, found)
	}
	return row, ""
}

// pipelineNoDataHint diagnoses a build failure whose message is the server's
// bare "no data found", returning "" for every other failure.
//
// It exists because that message is the one failure in this loop whose text
// says nothing about its cause, and the cause is almost always the same thing:
// a draft builds against its inputs' LIVE data, never against their drafts, so
// a NEW downstream that reads a NEW upstream in the same folder builds against
// an upstream row that exists, is empty, and will stay empty until its draft is
// published. Nothing in "no data found" points at that, and the reader's own
// file is the last place the answer is.
//
// The second cause is a race: the same message appears transiently in the
// moment after an upstream IS published (issue #4607). The two are told apart by
// the baseline — an upstream whose rows are still in a draft has one recorded,
// or was created by this very push — and NOT by retrying, deliberately: a
// retry that succeeded would hide the first cause, and the wrong one to guess is
// the one that reports a publish nobody did.
//
// Conservative on the match. "no data" is checked as a substring rather than
// parsed, because the point is to add advice to a failure the reader already
// has in full — a false positive costs a sentence, a false negative costs the
// afternoon this hint exists to save.
func pipelineNoDataHint(message string, inputs []string, binding wfdir.Binding,
	inst *wfdir.InstanceState, created map[string]bool) string {

	if !strings.Contains(strings.ToLower(message), "no data") {
		return ""
	}
	byID := tablePaths(binding)
	seen := map[string]bool{}
	var unpublished []string
	for _, id := range inputs {
		path, inFolder := byID[id]
		if !inFolder || seen[path] {
			continue
		}
		// Either half is enough. A draft recorded in the baseline is the general
		// case (the upstream was pushed at some point and never published); the
		// created set covers this run's own tables, including one whose push
		// failed before a draft pointer was ever recorded.
		if created[path] || inst.Tables[path].DraftID != "" {
			seen[path] = true
			unpublished = append(unpublished, path)
		}
	}
	sort.Strings(unpublished)

	switch len(unpublished) {
	case 0:
		return "this can be transient right after the input was published — push again; if it persists, the input table may genuinely hold no rows."
	case 1:
		return fmt.Sprintf("the input %s has no published data yet — its rows live in your unpublished draft. Run `ronja pipeline publish %s` first, then push this file again.",
			unpublished[0], unpublished[0])
	default:
		return fmt.Sprintf("the inputs %s have no published data yet — their rows live in your unpublished drafts. Run `ronja pipeline publish %s` first, then push this file again.",
			strings.Join(unpublished, ", "), strings.Join(unpublished, " "))
	}
}

// driftReason describes a row whose canonical SQL is not what the baseline
// recorded FOR THAT ROW, or "" when it matches.
//
// The caller owns which fingerprint goes with which row (see wfdir.TableState).
// Handing this the wrong one does not fail anywhere visible — it reports drift
// on a row nobody touched, or worse, agreement on one somebody did.
//
// A row we cannot canonicalize counts as drift rather than as agreement: the
// stored SQL and the stored input list disagree with each other, and pushing
// over that without saying so is exactly the silent case this guard exists for.
//
// An EMPTY baseline hash disarms the guard for that file, deliberately. It means
// this folder has never recorded what the server held — a copy cloned from git,
// whose .ronja/ was correctly never committed — and refusing every such push
// would be refusing the normal way a colleague joins a pipeline.
func driftReason(c pipelineCodec, local, what string, row *api.Table, baselineHash string) string {
	if baselineHash == "" {
		return ""
	}
	// Canonicalized and THEN de-aliased, in that order — see
	// pipelineCodec.canonicalDisk — because baselineHash is a hash of the bytes on
	// disk, which are in name form.
	code, unresolved := c.canonicalDisk(local, row)
	if unresolved {
		return fmt.Sprintf("%s carries a positional ref that cannot be resolved, so its SQL cannot be compared", what)
	}
	if wfdir.HashString(code) == baselineHash {
		return ""
	}
	return fmt.Sprintf("%s holds SQL that is not what your last sync recorded", what)
}

// fetchReview reads the confidence report, degrading to a note.
//
// NEVER fails the push. The draft is built and the work is safe; the report is
// how you decide whether to publish it, not part of getting there. The one
// failure worth naming is the scope quirk: the review route lives in the `admin`
// scope group while the rest of the authoring loop is `data`, so a token scoped
// to `data` alone runs everything except this.
func fetchReview(ctx context.Context, client *api.Client, draftID, path string) *pipelineReview {
	review, err := client.GetTableDraftReview(ctx, draftID)
	if err != nil {
		switch api.StatusOf(err) {
		case 403, 404:
			fmt.Fprintf(os.Stderr, "  Note: no confidence report available for %s (%v) — this read needs `admin:read`, unlike the rest of the loop. The build itself succeeded.\n",
				path, err)
		default:
			fmt.Fprintf(os.Stderr, "  Note: no confidence report available for %s (%v) — the build itself succeeded.\n", path, err)
		}
		return nil
	}
	out := &pipelineReview{
		DraftRowCount: review.DraftRowCount,
		LiveRowCount:  review.LiveRowCount,
		BaseStale:     review.BaseStale,
	}
	for _, delta := range review.FieldsDelta {
		switch delta.Change {
		case "added":
			out.FieldsAdded = append(out.FieldsAdded, delta.Name)
		case "removed":
			out.FieldsRemoved = append(out.FieldsRemoved, delta.Name)
		}
	}
	if lineage := review.InputModelsDelta; lineage != nil {
		out.InputsAdded = lineage.Added
		out.InputsRemoved = lineage.Removed
	}
	for _, v := range review.InterveningVersions {
		out.InterveningVersions = append(out.InterveningVersions, v.VersionID)
	}
	return out
}

// tableNameFor is the display name a new table is created with: the file's stem.
//
// Renames are unmanaged in v1 — the name is stamped once, at create, and a later
// rename in the web app is never reverted by a push. That is deliberate: a
// folder that pushed the name too would revert a colleague's rename silently,
// and the metadata three-state convention that would fix it properly is a
// follow-up.
func tableNameFor(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// describeLineageDelta renders a lineage change as signed ids: "+table-a,
// -table-b".
//
// Signs rather than two labelled lines, because the two halves are one fact —
// the commonest lineage change of all is a swap, and reading "Adds" three lines
// from "Drops" is reading it as two.
func describeLineageDelta(added, removed []string) string {
	parts := make([]string, 0, len(added)+len(removed))
	for _, id := range added {
		parts = append(parts, "+"+id)
	}
	for _, id := range removed {
		parts = append(parts, "-"+id)
	}
	return strings.Join(parts, ", ")
}

func printPipelinePushReport(r *pipelinePushResult) {
	out := os.Stdout
	if r.UpToDate {
		fmt.Fprintf(out, "  Up to date — every .sql file, docs sidecar and metric in this folder matches your last sync.\n")
		return
	}
	for _, file := range r.Files {
		printPipelineFileResult(out, file)
	}
	for _, doc := range r.Docs {
		printPipelineDocsResult(out, doc)
	}
	for _, metric := range r.Metrics {
		printPipelineMetricResult(out, metric)
	}
	if r.Target != "" {
		fmt.Fprintf(out, "\n  Target:   %s\n", r.Target)
	}
	if r.Error != "" {
		fmt.Fprintf(out, "  %s. The drafts above hold what did land; fix and push again.\n", r.Error)
		return
	}
	fmt.Fprintf(out, "\n  Next: ronja pipeline publish\n")
}

func printPipelineFileResult(out *os.File, file pipelineFileResult) {
	switch file.Outcome {
	case pushOutcomeRefused:
		fmt.Fprintf(out, "\n  %s — not pushed\n", file.Path)
		fmt.Fprintf(out, "    %s\n", file.Error)
		return
	case pushOutcomeBuildFailed:
		fmt.Fprintf(out, "\n  %s — build FAILED\n", file.Path)
		fmt.Fprintf(out, "    Draft:  %s (kept, so you can fix the SQL and push again)\n", file.DraftID)
		fmt.Fprintf(out, "    Table:  %s is untouched — a draft build never changes live data\n", file.TableID)
		for _, line := range strings.Split(strings.TrimRight(file.Error, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		return
	}

	if file.Outcome == pushOutcomeDocsFailed {
		fmt.Fprintf(out, "\n  %s — built, documentation NOT written\n", file.Path)
		fmt.Fprintf(out, "    Draft:  %s (holds the SQL, and is still publishable)\n", file.DraftID)
		fmt.Fprintf(out, "    Table:  %s\n", file.TableID)
		if file.Docs != nil {
			fmt.Fprintf(out, "    %s\n", file.Docs.Error)
		}
		return
	}

	verb := "pushed"
	if file.Created {
		verb = "created"
	}
	fmt.Fprintf(out, "\n  %s — %s (%s)\n", file.Path, verb, describeVerdict(file.Verdict))
	fmt.Fprintf(out, "    Table:  %s\n", file.TableID)
	fmt.Fprintf(out, "    Draft:  %s\n", file.DraftID)
	for _, note := range file.Notes {
		fmt.Fprintf(out, "    Note:   %s\n", note)
	}
	if r := file.Review; r != nil {
		if len(r.FieldsAdded) > 0 {
			fmt.Fprintf(out, "    Adds:   %s\n", strings.Join(r.FieldsAdded, ", "))
		}
		if len(r.FieldsRemoved) > 0 {
			fmt.Fprintf(out, "    Drops:  %s\n", strings.Join(r.FieldsRemoved, ", "))
		}
		// The lineage line, printed whenever it moved. Committing repoints what
		// this table READS, and a positional ref makes that possible with no SQL
		// diff at all — so it is the one change nothing else in this report shows.
		if len(r.InputsAdded) > 0 || len(r.InputsRemoved) > 0 {
			fmt.Fprintf(out, "    Inputs: %s\n", describeLineageDelta(r.InputsAdded, r.InputsRemoved))
		}
		fmt.Fprintf(out, "    Rows:   %s (live: %s)\n",
			describeRowCount(r.DraftRowCount), describeRowCount(r.LiveRowCount))
		if r.BaseStale {
			fmt.Fprintf(out, "    Warning: the live table has been published to since this draft forked (%s) — committing would revert that.\n",
				strings.Join(r.InterveningVersions, ", "))
		}
	}
	// The documentation line, and there is exactly one of it whatever the header
	// declared. UNMATCHED NAMES ARE INFORMATION: the push succeeded, the prose is
	// stored, and it attaches by itself if a build later produces the column — so
	// it reads as a sentence about the data rather than as a warning.
	if line := describeDocsResult(file.Docs); line != "" {
		fmt.Fprintf(out, "    Docs:   %s\n", line)
	}
	if file.Sample != "" {
		fmt.Fprintf(out, "    Sample:\n")
		printSample(out, file.Sample)
	}
	printResourceURL(out, reportKeyWidth, file.URL)
}
