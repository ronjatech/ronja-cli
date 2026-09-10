package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The pipeline loop's METRIC half: the numbers a folder DEFINES, as opposed to
// the tables it builds and the tables it documents.
//
// A metric is a recipe, so no .sql file can hold one — it lives in
// `metrics/<alias>.json` beside the folder's SQL, on the docs sidecar's model
// and for the docs sidecar's reasons (see wfdir.MetricsDirName). What it is NOT
// is a third lifecycle: a metric is DRAFTABLE with the identical
// checkout/sync/commit flow a derived table runs, so `push` stages a draft and
// builds it, `publish` commits it or files it for review, and `discard` drops
// it. Nothing here invents a governance step, and that is exactly what made a
// file type inside this folder available where an automation needed a loop of
// its own.
//
// THREE THINGS ARE DIFFERENT, and they are the whole of this file:
//
//  1. THE ORDER IS BOUGHT, NOT DERIVED. A metric contributes no `{{ ref }}`
//     edge — no marker resolves a metric — so it cannot join topoOrder. The
//     metric pass therefore runs AFTER the .sql pass, which buys the same
//     guarantee: a metric reading a table this folder builds is always pushed
//     after that table's create. That is the main case, and unlike a docs
//     sidecar there is no refusal against it — `recipe.source` is a REFERENCE to
//     another row, not a second carrier of it.
//  2. THE RECIPE IS THE SERVER'S GRAMMAR. It is carried from the file to the
//     wire as raw bytes and is never decoded into a Go shape here; the only
//     field this loop reaches into is `source`, which has to be rewritten from
//     the folder's alias to this organization's id. See internal/metricfile.
//  3. --force IS A DESTRUCTIVE GATE HERE IN A WAY IT IS NOWHERE ELSE. Leg (a)
//     of the drift guard firing on a metric means a colleague committed a
//     definition change to the company's official KPI. So --force prints the
//     difference before overwriting, and REFUSES OUTRIGHT on a metric an admin
//     has verified unless a second, metric-specific flag is typed.

// Per-metric push outcomes. These are --json `outcome` values, so they are a
// contract for anything scripting the CLI, and they are deliberately the same
// words the .sql half uses for the same states.
const (
	metricOutcomePushed      = "pushed"
	metricOutcomeBuildFailed = "build_failed"
	metricOutcomeUpToDate    = "up_to_date"
	metricOutcomeRefused     = "refused"
	// metricOutcomeDescriptionFailed is the DEFINITION landing and the PROSE
	// not: the recipe was staged, the draft built, and the write that would have
	// put this file's `description` on the metric did not go through.
	//
	// Split from `pushed` for pushOutcomeDocsFailed's reason — it is the one
	// state where a green push would leave a metric whose committed file says one
	// thing and whose row says another — and split from `refused` because the
	// work is on the server and publishing it is still the right next step.
	metricOutcomeDescriptionFailed = "description_failed"
)

// pipelineMetricFile is one committed metric file, resolved as far as the folder
// can resolve it without the network.
type pipelineMetricFile struct {
	// Path is the folder-relative, slash-separated path, which is what the lock
	// and the local baseline are keyed by.
	Path string `json:"path"`
	// Alias is the file's stem. It does double duty, and both are deliberate:
	// it is the NAME a metric this folder creates is given, and it is an alias a
	// stack may BIND to an existing metric so one committed folder can point at
	// a row it did not create.
	Alias string `json:"alias"`
	// MetricID is the row this file is; empty means this folder has never
	// created it here and a push will. Resolved from the alias first and from
	// the recorded id second — see resolveMetricFiles.
	MetricID string `json:"metricID,omitempty"`
	// SourceID is what `recipe.source` resolved to on the selected stack: a
	// sibling .sql file's table, a declared alias, or a literal id.
	SourceID string `json:"sourceID,omitempty"`
	// Problem is why this file cannot be used as it stands — it does not parse,
	// its source is not a table this folder can name here. Reported per file
	// rather than aborting: the other metrics, and the whole SQL half of the
	// push, are still worth doing.
	Problem string `json:"problem,omitempty"`

	// File is the parsed content, and Resolved is its recipe with `source`
	// rewritten to SourceID — the exact bytes a push sends.
	File     metricfile.File `json:"-"`
	Resolved json.RawMessage `json:"-"`
	// Declared is the fingerprint of the whole of what a push would send, which
	// is what the "has this file changed since the last sync" comparison reads.
	Declared string `json:"-"`
}

// readMetricFiles reads a pipeline folder's `metrics/` directory.
//
// A MISSING DIRECTORY IS NOT AN ERROR and is the ordinary case: every pipeline
// folder that existed before this feature has none, and one that defines no
// metric never grows one.
//
// FLAT, one level, for wfdir.MetricAlias's reason. A nested file is reported
// rather than silently skipped, which is the difference between "you put it in
// the wrong place" and silence. Non-.json files are ignored outright — a README
// in that directory is somebody explaining the folder to a colleague.
func readMetricFiles(root string) ([]pipelineMetricFile, error) {
	dir := filepath.Join(root, wfdir.MetricsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []pipelineMetricFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if hasJSONBeneath(filepath.Join(dir, name)) {
				out = append(out, pipelineMetricFile{
					Path: wfdir.MetricsDirName + "/" + name,
					Problem: fmt.Sprintf("%s/%s/ is a directory, and a metric is one flat file per metric (%s) — its stem is the metric's name, and a name is a single word",
						wfdir.MetricsDirName, name, wfdir.MetricPath("<name>")),
				})
			}
			continue
		}
		path := wfdir.MetricsDirName + "/" + name
		alias, ok := wfdir.MetricAlias(path)
		if !ok {
			continue
		}
		file := pipelineMetricFile{Path: path, Alias: alias}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			file.Problem = fmt.Sprintf("could not be read: %v", err)
			out = append(out, file)
			continue
		}
		parsed, err := metricfile.Parse(body)
		if err != nil {
			file.Problem = err.Error()
			out = append(out, file)
			continue
		}
		file.File = parsed
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// resolveMetricFiles turns each file's two names into the ids THIS organization
// uses for them: the STEM into the row this file is (when there is one), and
// `recipe.source` into the table the metric reads.
//
// THE TWO NAMES ARE RESOLVED DIFFERENTLY, and the asymmetry is the design:
//
//   - The SOURCE must resolve to something. A metric whose source resolved to
//     "whichever table this name happens to mean" would compute a number from a
//     row nobody chose, so an unresolvable one is a Problem.
//   - The STEM may resolve to NOTHING, and usually does. That is a metric this
//     folder has not created here yet, and the push creates it under that name.
//     Refusing it would mean a metric could never be authored from a folder at
//     all, which is the whole point of this file.
//
// The recorded id is the SECOND source of identity and never the first: a stack
// that BINDS the stem is the author saying "this file is that row here", which
// is explicit config and outranks a machine recording. When they disagree the
// lock's own invariant drops the stale fingerprint rather than comparing one
// metric's definition against a hash taken from another's.
// ⚠️ IT IS TWO PASSES, and `push` runs them at two different MOMENTS. The
// identities can be resolved before a single request — they come from the
// manifest and the lock — and `push` needs them early, to decide whether this
// run creates anything and therefore whether the folder has to bind itself to an
// organization first. The SOURCES cannot: the main case is a metric reading a
// table this same push CREATES, whose id does not exist until that create has
// returned. Every other command's binding is already complete, so they call
// resolveMetricFiles and get both at once.
func resolveMetricFiles(f *folder, codec pipelineCodec, live liveHashes, files []pipelineMetricFile) []pipelineMetricFile {
	return resolveMetricSources(f, codec, resolveMetricIdentities(f, live, files))
}

// resolveMetricIdentities answers "which row is this file", from the manifest
// and the lock alone — no request, and nothing that depends on what an earlier
// pass of this same push did.
func resolveMetricIdentities(f *folder, live liveHashes, files []pipelineMetricFile) []pipelineMetricFile {
	for i := range files {
		file := &files[i]
		if file.Problem != "" {
			continue
		}
		switch {
		case markers.IsResourceID(markers.KindTable, file.Alias):
			file.MetricID = file.Alias
		default:
			if id, ok := f.Codec.resolve(markers.KindTable, file.Alias); ok && id != "" {
				file.MetricID = id
			} else if recorded, _, _ := live.metricSeen(file.Path); recorded != "" {
				file.MetricID = recorded
			}
		}
	}
	return files
}

// resolveMetricSources answers "which table does this metric read", and computes
// the bytes a push would send plus the fingerprint of them.
//
// Called AFTER the .sql pass by `push`, because the sibling leg of
// resolveMetricSource reads f.Binding.Tables — which that pass fills in as it
// creates.
func resolveMetricSources(f *folder, codec pipelineCodec, files []pipelineMetricFile) []pipelineMetricFile {
	for i := range files {
		file := &files[i]
		if file.Problem != "" {
			continue
		}
		source, err := file.File.Source()
		if err != nil {
			file.Problem = err.Error()
			continue
		}
		id, err := resolveMetricSource(f, codec, source)
		if err != nil {
			file.Problem = err.Error()
			continue
		}
		file.SourceID = id
		resolved, err := file.File.Resolve(id)
		if err != nil {
			file.Problem = fmt.Sprintf("this file's recipe could not be rewritten with the resolved source: %v", err)
			continue
		}
		file.Resolved = resolved
		file.Declared = metricfile.Fingerprint(resolved, file.File.Description, file.File.ReportingTimezone)
	}
	return files
}

// resolveMetricSource answers the one question `recipe.source` asks: which table
// in THIS organization does that name mean?
//
// Three legs, in the order pipelineCodec.toWire resolves a `{{ ref }}` — a
// sibling .sql file's stem, then a declared alias, then a literal id — so a
// metric's source and a derived table's ref cannot disagree about what one name
// means in one folder. The sibling leg is the MAIN case and is the reason the
// metric pass runs after the .sql pass: the sibling's table id only exists once
// that sibling's own create has returned earlier in this same run.
func resolveMetricSource(f *folder, codec pipelineCodec, name string) (string, error) {
	if markers.IsResourceID(markers.KindTable, name) {
		return name, nil
	}
	if sibling, claimed := codec.pathByStem[name]; claimed {
		if len(codec.ambiguous[name]) > 0 {
			return "", fmt.Errorf("this metric reads %q, and two files in this folder are called %q (%s) — rename one of them, or write the table's id",
				name, name, strings.Join(codec.ambiguous[name], " and "))
		}
		if id := f.Binding.Tables[sibling]; id != "" {
			return id, nil
		}
		return "", fmt.Errorf("this metric reads %q and no table id is recorded for %s yet — push the SQL half of this folder first, so the table this metric reads exists before the metric that reads it",
			name, sibling)
	}
	if id, ok := f.Codec.resolve(markers.KindTable, name); ok && id != "" {
		return id, nil
	}
	where, fix := describeBindSite(f.selection())
	return "", fmt.Errorf(
		"this metric reads %q, which is not a table this folder can name here: no .sql file in this folder is called %q, %s declares no dependency called %q of kind \"table\", and %s binds nothing to it.\n    Declare it in \"dependencies\" and %s — or write the table's id if this folder is only ever pushed to one organization",
		name, name, wfdir.ManifestName, name, where, strings.TrimSuffix(fix, "."))
}

// checkMetricStemCollisions refuses a folder that names one thing twice.
//
// A table's name and a metric's name land in ONE namespace, case- and
// whitespace-insensitively: `requireNameFreeTx` has no `kind` predicate, so a
// metric and a table in one feature really do collide server-side. That refusal
// is the truth and nothing here tries to be authoritative — comparison there is
// SQL-only `lower(btrim())`, because Go and Postgres disagree about `İ` and `ß`.
//
// This is for the MESSAGE. Without it, a folder holding `revenue.sql` and
// `metrics/Revenue.json` pushes the table, creates it, and then fails half way
// through on a name refusal that names a row rather than the two files that
// caused it. It is the same check checkPushable already makes among .sql stems,
// extended across the one boundary those two file sets share, and it is a
// SIBLING of that check rather than a widening of it because checkPushable is
// kind-generic — a workflow and a data app have no metrics and no such
// namespace.
//
// TWO KINDS OF PAIR, and the second is the one nothing else covers.
// `metrics/Revenue.json` and `metrics/revenue.json` are two names for one thing
// exactly as a metric and a table are, and unlike two .sql files they can really
// coexist in a committed folder: a pipeline folder's SyncExt is `.sql`, so a
// metric file is invisible to Enumerate and never reaches checkPushable's
// case-only PATH guard at all, and a folder committed from Linux carries both.
// Each colliding GROUP is reported once, listing every file in it, rather than
// once per member.
func checkMetricStemCollisions(local map[string]string, metrics []pipelineMetricFile) error {
	byFold := map[string][]string{}
	for path := range local {
		fold := foldResourceName(tableNameFor(path))
		byFold[fold] = append(byFold[fold], path)
	}
	// One entry per fold a METRIC claims — the metric files claiming it plus the
	// .sql files that already do. Keyed on the metric side because that is this
	// guard's scope: two .sql stems that fold together are checkPushable's, whose
	// path guard sees the same pair, and widening this one to them would put two
	// refusals on one mistake.
	claimed := map[string][]string{}
	named := map[string]string{}
	for _, file := range metrics {
		if file.Alias == "" {
			continue
		}
		fold := foldResourceName(file.Alias)
		if _, seen := claimed[fold]; !seen {
			claimed[fold] = append(claimed[fold], byFold[fold]...)
		}
		claimed[fold] = append(claimed[fold], file.Path)
		// The smallest alias rather than the first one seen, so the name a group
		// is quoted under is a property of the FOLDER rather than of the order
		// its files happened to be read in. `metrics` arrives sorted by path
		// today, which makes the two the same thing — but that is readMetricFiles'
		// choice, not this function's contract, and a caller that ever handed the
		// set over in another order would otherwise change the refusal's wording
		// without changing the mistake it describes.
		if name, seen := named[fold]; !seen || file.Alias < name {
			named[fold] = file.Alias
		}
	}
	var collisions []string
	for fold, paths := range claimed {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		quantifier := "both"
		if len(paths) > 2 {
			quantifier = "all of them"
		}
		collisions = append(collisions, fmt.Sprintf("%s (%s would be called %q)",
			strings.Join(paths, " and "), quantifier, named[fold]))
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	// "metrics and tables" rather than "a metric and a table": the same list now
	// carries metric-vs-metric pairs, and a header naming one shape of pair reads
	// as if the two metric files beneath it were something else again.
	return fmt.Errorf("this folder names one thing twice — metrics and tables share ONE namespace inside a feature, case- and whitespace-insensitively:\n    %s\n  Rename one of each group before pushing; the file's stem is the name it claims",
		strings.Join(collisions, "\n    "))
}

// foldResourceName is the client-side approximation of how the store compares
// two names: `lower(btrim())`.
//
// AN APPROXIMATION, and deliberately so — the authoritative comparison is SQL,
// because Go and Postgres disagree about `İ` and `ß`, and a client that claimed
// to be the rule would refuse folders the server accepts. What the trim buys is
// the other direction: `metrics/revenue .json` claims the name `revenue.sql`'s
// table holds — the server trims what it stores, and compares trimmed — so
// folding case alone let a pair through that the push then failed on half way,
// server-side, naming a row rather than the two files.
func foldResourceName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// metricFieldRefs reports each metric file's TWO names as uses of an alias, in
// the shape the alias pre-flight reads.
//
// Both, and that is not belt-and-braces. Without the stem, a dependency a folder
// declares purely to point a metric file at an existing row is reported as dead
// config on every push, status and `sync check`; without the source, so is every
// dependency naming the table a metric reads. Neither is reachable by
// markers.Scan — one is a FILE NAME and the other is a JSON value — which is the
// same blind spot tableDocsFieldRefs exists to cover.
//
// A file that does not parse still counts as a USE of its stem, on
// tableDocsFieldRefs' rule: naming an alias is what the FILENAME does, and
// skipping it here would earn one mistake a SECOND warning saying the dependency
// it plainly names is dead config. Its SOURCE is not counted, because a file
// that does not parse has no readable source to count.
//
// An unreadable directory contributes nothing rather than failing: this runs
// inside `status` and `sync check`, which have to keep answering.
func metricFieldRefs(root string) []fieldRef {
	files, err := readMetricFiles(root)
	if err != nil {
		return nil
	}
	var out []fieldRef
	for _, file := range files {
		if file.Alias == "" {
			continue
		}
		out = append(out, fieldRef{Where: file.Path, Kind: markers.KindTable, Value: file.Alias})
		if file.Problem != "" {
			continue
		}
		if source, err := file.File.Source(); err == nil && source != "" {
			out = append(out, fieldRef{
				Where: file.Path + " → recipe.source", Kind: markers.KindTable, Value: source,
			})
		}
	}
	return out
}

// metricEdges is the metric files' contribution to `ronja sync check`: does
// every table a metric reads, and every metric a file is bound to, still
// resolve?
//
// Two products, for tableDocsEdges' two reasons:
//
//   - EDGES for a name written as a LITERAL id. A name written as an ALIAS is
//     already covered — dependencyEdges emits one per declared dependency — and
//     a second edge for the same id would double-count it in the summary. A
//     SIBLING stem is covered by neither and needs neither: it names a row this
//     folder itself builds, and on a first push that row does not exist yet, so
//     asking an instance about it would report a folder that deploys perfectly
//     as broken.
//   - FINDINGS for a file this folder cannot use as it stands. A finding rather
//     than an edge because nothing was looked up; the file is wrong before any
//     instance is asked about it.
func metricEdges(f *folder, codec pipelineCodec, live liveHashes) (edges []syncEdgeReport, findings []string) {
	files, err := readMetricFiles(f.Root)
	if err != nil || len(files) == 0 {
		return nil, nil
	}
	for _, file := range resolveMetricFiles(f, codec, live, files) {
		if file.Problem != "" {
			findings = append(findings, fmt.Sprintf("%s: %s", file.Path, file.Problem))
			continue
		}
		if markers.IsResourceID(markers.KindTable, file.Alias) {
			edges = append(edges, syncEdgeReport{
				Kind: markers.KindTable, Ref: file.Alias, ID: file.MetricID, Where: file.Path,
			})
		}
		if source, err := file.File.Source(); err == nil && markers.IsResourceID(markers.KindTable, source) {
			edges = append(edges, syncEdgeReport{
				Kind: markers.KindTable, Ref: source, ID: file.SourceID,
				Where: file.Path + " → recipe.source",
			})
		}
	}
	return edges, findings
}

// pipelineMetricFileResult is what happened to one metric file.
type pipelineMetricFileResult struct {
	Path     string `json:"path"`
	Alias    string `json:"alias,omitempty"`
	MetricID string `json:"metricID,omitempty"`
	SourceID string `json:"sourceID,omitempty"`
	// DraftID is the row the recipe was staged on. It survives a failed build on
	// purpose — that draft is where the work is.
	DraftID string `json:"draftID,omitempty"`
	Outcome string `json:"outcome"`
	// Created reports that this push brought the metric into existence.
	Created bool `json:"created"`
	// Verdict is the build verdict, one of api.BuildVerdict*.
	Verdict string `json:"verdict,omitempty"`
	Error   string `json:"error,omitempty"`
	// Conflict reports that what refused this file was a compare-and-swap losing
	// a race — somebody edited the draft between this push's read and its write —
	// rather than a rule the caller cannot satisfy at all. Same field, same
	// reason, as pipelineFileResult's.
	Conflict bool `json:"conflict"`
	// Warnings are what the write did beyond what the file asked for: an
	// unresolved source closure, a session-table promotion. Never an error — the
	// recipe is staged either way — but never swallowed, because the caller did
	// not ask for either.
	Warnings []string `json:"warnings,omitempty"`
	// Notes are what this push wants to say about the metric it staged: its
	// derived shape, and the governance consequence of committing it.
	Notes []string `json:"notes,omitempty"`
	URL   string   `json:"-"`
}

// pushMetricFiles is the METRIC half of a pipeline push.
//
// A separate pass rather than a step inside pushOneTable because it is a
// different loop over a different set — there is no .sql file here, and no
// dependency edge to order by. Ordered AFTER the SQL, which is where the
// ordering guarantee comes from: a metric reading a table an earlier file in
// this same push created is written against a row that now exists.
//
// Failure is DATA, not an error return, exactly as it is for a .sql file: one
// unresolvable source must not stop the other metrics, and the run's exit code
// is decided by the caller from the outcomes.
func pushMetricFiles(ctx context.Context, client *api.Client, f *folder, inst *wfdir.InstanceState,
	files []pipelineMetricFile, opts pipelinePushOptions) []pipelineMetricFileResult {

	if len(files) == 0 {
		return nil
	}
	live := f.live(inst)
	out := make([]pipelineMetricFileResult, 0, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			out = append(out, pipelineMetricFileResult{
				Path: file.Path, Alias: file.Alias, Outcome: metricOutcomeRefused, Error: interruptedMessage,
			})
			continue
		}
		out = append(out, pushOneMetric(ctx, client, f, live, file, opts))
		// Saved after EVERY metric, exactly as the SQL loop saves after every
		// file: the write has really landed on the server, so a run that dies on
		// the next file must not lose the recording of it. Losing one is not a
		// wasted round trip — the next push finds no id, creates a SECOND metric
		// of the same name beside the first, and this loop has no delete verb to
		// undo it with.
		if err := f.saveBaseline(); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not record what %s defined in the local baseline (%v).\n", file.Path, err)
		}
	}
	return out
}

// pushOneMetric runs the whole cycle for one metric file: create or resume,
// guard, write the recipe, build, report.
func pushOneMetric(ctx context.Context, client *api.Client, f *folder, live liveHashes,
	file pipelineMetricFile, opts pipelinePushOptions) pipelineMetricFileResult {

	out := pipelineMetricFileResult{
		Path: file.Path, Alias: file.Alias, MetricID: file.MetricID, SourceID: file.SourceID,
	}
	refuse := func(format string, args ...any) pipelineMetricFileResult {
		out.Outcome = metricOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", file.Path, out.Error)
		return out
	}
	refuseConflict := func(format string, args ...any) pipelineMetricFileResult {
		out.Conflict = true
		return refuse(format, args...)
	}
	if file.Problem != "" {
		return refuse("%s", file.Problem)
	}

	recordedID, liveBase, declared := live.metricSeen(file.Path)
	if recordedID != out.MetricID {
		// The recording describes a different row — the alias was repointed, a
		// stack switched, a merge landed — so neither fingerprint can be compared
		// against this one. By the invariant on wfdir.LockMetric: a hash is only
		// ever compared against the row it was taken from.
		liveBase, declared = "", ""
	}

	// --- 1. Create, for a file with no metric behind it yet -----------------
	if out.MetricID == "" {
		created, err := client.CreateMetric(ctx, api.CreateMetricInput{
			Name:              file.Alias,
			FeatureID:         f.Binding.FeatureID,
			Recipe:            file.Resolved,
			Description:       valueOrEmpty(file.File.Description),
			ReportingTimezone: valueOrEmpty(file.File.ReportingTimezone),
		})
		switch {
		case err == nil:
			out.MetricID = created.Metric.IdentityID()
			out.Warnings = append(out.Warnings, created.Warnings...)
			liveBase = metricfile.RecipeSHA256(created.Metric.MetricRecipe)
		case isNameTakenOnMetricCreate(err):
			// The likelier cause is not a real collision: it is a 200 this CLI
			// failed to persist. The metric exists, the recording does not, and
			// the next push tries to create the same name — which the server
			// refuses with something the author cannot act on from the folder.
			adopted, why := reconcileMetricNameTaken(ctx, client, f.Binding, file.Alias)
			if adopted == nil {
				taken, _ := api.AsNameTaken(err)
				return refuse("creating the metric for %s in feature %s was refused — that name is taken:\n      %s\n    %s\n    Rename the file (its stem is the metric's name), rename whatever holds the name in the web app, or point this file at the existing row with `ronja bind`",
					file.Path, f.Binding.FeatureID, taken, why)
			}
			fmt.Fprintf(os.Stderr, "  Note: %s names a metric that already exists in this feature (%s) and this folder had no record of it — adopting it rather than refusing.\n",
				file.Path, adopted.ID)
			out.MetricID = adopted.IdentityID()
			liveBase = metricfile.RecipeSHA256(adopted.MetricRecipe)
		case api.StatusOf(err) == 403:
			// Creating is admin-only; EDITING a metric that already exists is
			// not — the whole draft-and-review path this file drives is open to a
			// non-admin. So a push that refuses here is not a push that cannot
			// run: it is one file that needs an admin once.
			return refuse("creating the metric for %s in feature %s was refused (%v).\n    Creating a NEW metric needs an admin. Editing metrics that already exist does not — that is what your drafts are, and `ronja pipeline push` runs the whole cycle for them.\n    Ask an admin to create %q in that feature (or to run this one push), then push again",
				file.Path, f.Binding.FeatureID, err, file.Alias)
		default:
			return refuse("create the metric for %s in feature %s: %v", file.Path, f.Binding.FeatureID, err)
		}
		out.Created = true
		// RECORDED IMMEDIATELY, before anything else is attempted, and this is the
		// crash rule pushOneTable's create arm states in the same words. The
		// create returns a LIVE row — colleagues can see it the moment it exists —
		// so a push that died before saving would leave a metric nobody can find
		// and the next push would create a second one beside it.
		//
		// The DECLARED fingerprint is deliberately NOT recorded here: the file has
		// not finished being pushed (nothing has built), and claiming it had would
		// let the next bare push skip a metric whose build never ran.
		live.setMetricSeen(file.Path, out.MetricID, liveBase, "")
		if err := f.saveBaseline(); err != nil {
			out.Outcome = metricOutcomeRefused
			// NOT f.bindingFiles(), which names ronja.json for a legacy folder: a
			// metric's id never goes in the manifest. It goes in the lock on a named
			// stack and in the git-ignored local baseline otherwise, and sending
			// somebody to the wrong file is worse than sending them to none.
			out.Error = fmt.Sprintf("metric %s was CREATED, but %s does not name it: %v",
				out.MetricID, metricRecordingFile(f), err)
			fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n  Add it by hand, or the next push will create a second metric for this file.\n", file.Path, out.Error)
			return out
		}
	}

	// --- 2. Resolve the draft ----------------------------------------------
	// GetTableDraft FIRST, always. CheckoutTable is not idempotent — a second
	// checkout while a draft is open is an error — so "check out and see" is not
	// available here, exactly as it is not for a derived table.
	draft, err := client.GetTableDraft(ctx, out.MetricID)
	if err != nil {
		return refuse("check for your draft of %s: %v", out.MetricID, err)
	}

	// --- 3. The LIVE drift guard, and the brake on overwriting a KPI --------
	// ONE client-side leg, not two, and the asymmetry with the .sql half is
	// deliberate. The DRAFT is guarded SERVER-side by baseRecipeSha256 below,
	// which is strictly better than a local baseline because it also catches the
	// author's own chat agent editing the same draft. The LIVE row has no such
	// precondition — no draft precondition speaks about the live row at all —
	// so it is guarded here, before the first byte is written.
	var liveRow *api.Table
	if !out.Created {
		liveRow, err = client.GetTable(ctx, out.MetricID)
		if err != nil {
			if status := api.StatusOf(err); status == 404 || status == 400 {
				return refuse("%s is bound to %s, which is not a metric you can reach on this instance (deleted, moved, or in a feature you are not a member of): %v\n    Point the alias at a metric in this organization with `ronja bind`, or drop the file",
					file.Path, out.MetricID, err)
			}
			return refuse("read %s: %v", out.MetricID, err)
		}
		if liveRow.Kind != api.TableKindMetric {
			// A stem bound to a plain TABLE. `GET /api/v2/search` reports a metric
			// as kind `table`, so `ronja bind` cannot tell the two apart and binds
			// an exact-name match of either — which means this is a reachable
			// mistake rather than a corrupt folder, and the place it surfaces is
			// here, by name.
			return refuse("%s is bound to %s, which is a %s table rather than a metric — a metric file cannot define one.\n    `ronja bind` cannot tell a metric from a table of the same name (the search API reports both as \"table\"), so re-point the alias at the metric's own id, or rename the file",
				file.Path, out.MetricID, liveRow.Kind)
		}
		liveNow := metricfile.RecipeSHA256(liveRow.MetricRecipe)
		if liveBase != "" && liveNow != liveBase {
			if !opts.Force {
				return refuse("the definition of %s changed on the server since your last sync — pushing %s would overwrite it.\n    Run `ronja pipeline status` to see what the metric says now, or push --force to overwrite",
					out.MetricID, file.Path)
			}
			// THE ONE DESTRUCTIVE GATE IN THIS LOOP. Leg (a) firing on a metric is
			// not "somebody edited a file": it is a colleague having committed a
			// change to the company's official definition of a number. --force's
			// ordinary "overwrite and re-record" would make that last-write-wins,
			// driven by a flag whose message is about your own file — so the
			// difference is PRINTED, and a metric an admin has VERIFIED is refused
			// outright until a second flag is typed.
			if liveRow.MetricStatus == api.MetricStatusVerified && !opts.ForceVerifiedMetric {
				return refuse("%s is a VERIFIED metric whose definition changed on the server since your last sync, and --force alone will not overwrite it.\n%s    An admin verified the definition that is there now, and publishing over it retires that verification.\n    Pull their change into %s, or re-run with --force --force-verified-metric to overwrite it deliberately",
					out.MetricID, describeRecipeOverwrite(liveRow.MetricRecipe, file.Resolved), file.Path)
			}
			fmt.Fprintf(os.Stderr, "  Note: --force — overwriting the definition of %s (%s):\n%s",
				out.MetricID, file.Path, describeRecipeOverwrite(liveRow.MetricRecipe, file.Resolved))
			// The acknowledgement is recorded, on pushOneTable's rule: --force is
			// the author saying they have seen what is on the server and mean to
			// write over it, and without this the same difference is re-reported
			// on every later push until --force is a permanent part of the command
			// line — which trains it past the one time it means something.
			liveBase = liveNow
		}
		// An absent baseline is adopted from the row just read rather than left
		// absent for ever: "no baseline" is a state a folder should be able to
		// leave, and the read that would prove it has already happened.
		if liveBase == "" {
			liveBase = liveNow
		}
	}

	// --- 4. Nothing to do? --------------------------------------------------
	// ONE condition: the FILE has not changed since the last push from this
	// folder. Exactly what a bare push asks of a .sql file, and deliberately NOT
	// widened with "and there is no draft open" — a draft staged and not yet
	// published is the ordinary state between a push and a publish, and treating
	// it as work to redo would re-stage and re-BUILD every metric in the folder
	// on every run. `status` is where a staged draft is reported.
	//
	// It cannot be answered by comparing the file against the row. The row holds
	// the recipe the server CANONICALIZED — its version defaulted, its source a
	// real id — and the file holds aliases and may omit a defaultable key, so
	// such a comparison would report every metric as pending for ever.
	//
	// Every path that leaves work undone CLEARS this fingerprint rather than
	// relying on a second condition here: a failed build, a refused description
	// write, a discard, and a publish whose commit did not land.
	if !out.Created && declared != "" && declared == file.Declared {
		live.setMetricSeen(file.Path, out.MetricID, liveBase, declared)
		out.Outcome = metricOutcomeUpToDate
		return out
	}

	if draft == nil {
		draft, err = client.CheckoutTable(ctx, out.MetricID)
		if err != nil {
			return refuse("check out a draft of %s: %v", out.MetricID, err)
		}
	}
	out.DraftID = draft.ID
	out.URL = draft.URL

	// --- 5. Stage the recipe ------------------------------------------------
	// The server's compare-and-swap, and the ONE digest that may be sent as it:
	// the recipe THIS DRAFT currently holds, hashed over the bytes the server
	// itself sent. The stored recipe is canonical and the file is not, so a hash
	// of the file would be refused with a 409 describing a conflict nobody caused.
	//
	// EMPTY sends no precondition, which is the server's "write unconditionally",
	// and it is the honest answer in the two cases that reach it: a draft with no
	// recipe to speak of (a metric cannot have one, but an older row might), and
	// a --force push. --force is the author saying they mean to write over what
	// is there; sending the precondition anyway would make the flag refuse the
	// very thing it exists to permit.
	basis := metricfile.RecipeSHA256(draft.MetricRecipe)
	if opts.Force {
		basis = ""
	}
	staged, err := client.WriteMetricRecipe(ctx, draft.ID, api.WriteMetricRecipeInput{
		Recipe:            file.Resolved,
		ReportingTimezone: file.File.ReportingTimezone,
		BaseRecipeSha256:  basis,
	})
	if err != nil {
		if api.StatusOf(err) == api.StatusConflict {
			// Nothing was written, so their edit is intact and so is the file.
			// Reachable even though the guard above just read this same draft,
			// because the two look at different instants — which is the whole
			// reason the precondition exists.
			return refuseConflict("draft %s of %s changed between reading it and writing to it — somebody edited it in the web app or by chat, and nothing was written:\n      %v\n    Run `ronja pipeline status` to see what it holds now, or push --force to overwrite it",
				draft.ID, out.MetricID, err)
		}
		if api.StatusOf(err) == 403 {
			return refuse("staging the definition of %s on draft %s was refused (%v).\n    Writing a metric's recipe needs write access to the feature that holds it, and the draft has to be YOURS — a colleague's open draft is not writable, even by an admin's token, without taking it over in the web app",
				file.Path, draft.ID, err)
		}
		return refuse("stage the definition of %s on draft %s: %v", file.Path, draft.ID, err)
	}
	out.Warnings = append(out.Warnings, staged.Warnings...)
	out.Notes = append(out.Notes, describeMetricShape(staged))

	// --- 6. Build ------------------------------------------------------------
	if err := client.SyncTable(ctx, draft.ID); err != nil {
		if api.StatusOf(err) != 400 || !buildRunning(ctx, client, draft.ID) {
			return refuse("build draft %s: %v", draft.ID, err)
		}
		fmt.Fprintf(os.Stderr, "  Note: a build of %s was already running — waiting for it before building the definition this push just staged.\n", file.Path)
		if _, err := client.WaitForTableBuild(ctx, draft.ID); err != nil {
			return refuse("%s", pipelineErrorText(err))
		}
		// Re-issued rather than joined: the build that was running started before
		// this push staged its recipe, so its result describes the PREVIOUS
		// definition.
		if err := client.SyncTable(ctx, draft.ID); err != nil {
			return refuse("build draft %s: %v", draft.ID, err)
		}
	}
	build, err := client.WaitForTableBuild(ctx, draft.ID)
	if err != nil {
		return refuse("%s", pipelineErrorText(err))
	}
	out.Verdict = build.Verdict
	if !build.OK() {
		out.Outcome = metricOutcomeBuildFailed
		out.Error = build.ErrorMessage()
		if out.Error == "" {
			out.Error = "the build failed and recorded no reason"
		}
		// The recipe IS staged and the draft is still there to iterate on, but
		// the file is deliberately NOT recorded as pushed: a failed build must
		// leave the file re-pushable, or the next bare push skips the fix.
		live.setMetricSeen(file.Path, out.MetricID, liveBase, "")
		return out
	}

	// --- 7. The prose, after the build ---------------------------------------
	// Written to the DRAFT, like the recipe: committing copies the draft's
	// fields onto the live row, so prose written on the live metric while a
	// draft is open is lost the moment anybody publishes.
	//
	// Only when it would change something, which is what keeps a push that
	// changed only the recipe to one write. The comparison is against the DRAFT,
	// because the draft is the row about to be committed.
	if file.File.Description != nil && *file.File.Description != draft.Description {
		in := api.UpdateTableDocsInput{Description: file.File.Description}
		if *file.File.Description != "" {
			in.DescriptionSource = api.DescriptionSourceUser
		}
		if _, err := client.UpdateTableDocs(ctx, draft.ID, in); err != nil {
			// The definition landed and built; only the prose did not. Reported as
			// its own outcome rather than folded into a green push, and returned
			// WITHOUT recording the file as synced — a file recorded as pushed
			// while its prose did not land is a file the next bare push skips, for
			// ever, with the committed file and the row saying different things.
			live.setMetricSeen(file.Path, out.MetricID, liveBase, "")
			out.Outcome = metricOutcomeDescriptionFailed
			out.Error = fmt.Sprintf("the definition of %s was staged and built, but its description was not written: %v", file.Path, err)
			if hint := docsRefusalHint(err); hint != "" {
				out.Error += "\n    " + hint
			}
			fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", file.Path, out.Error)
			return out
		}
	}

	// --- 8. Success -----------------------------------------------------------
	live.setMetricSeen(file.Path, out.MetricID, liveBase, file.Declared)
	out.Outcome = metricOutcomePushed
	if note := metricPublishConsequence(liveRow); note != "" {
		out.Notes = append(out.Notes, note)
		fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", file.Path, note)
	}
	return out
}

// describeMetricShape is the one line a staged recipe earns: what the server
// DERIVED from it.
//
// Worth printing rather than assuming, because every one of these is computed
// from the recipe and none of them is in the file: additivity above all, since a
// ratio that came out additive would re-aggregate by summing and quietly produce
// the wrong number at every grain but the one it was checked at.
func describeMetricShape(r *api.MetricRecipeResult) string {
	shape := "not additive (recomputed from source at every grain)"
	if r.Additive {
		shape = "additive (rolls up by summing)"
	}
	parts := []string{shape, "keyed on " + r.TimeColumn}
	if len(r.Dimensions) > 0 {
		parts = append(parts, fmt.Sprintf("%d dimension(s): %s", len(r.Dimensions), strings.Join(r.Dimensions, ", ")))
	}
	if len(r.InputModels) > 0 {
		parts = append(parts, "reads "+strings.Join(r.InputModels, ", "))
	}
	if len(r.SourceClosure) == 0 {
		// An EMPTY closure is a degrade, never a statement that the metric reads
		// nothing: the resolve did not succeed and the engine will walk the
		// sources live at query time.
		parts = append(parts, "source closure unresolved — the engine will walk the sources live")
	}
	return strings.Join(parts, "; ")
}

// metricPublishConsequence is the one governance sentence a push of a VERIFIED
// metric owes its author, said at push time rather than at publish time because
// this is where they can still change their mind.
//
// Committing a recipe change re-fingerprints the definition, and a metric whose
// definition_hash no longer equals its verified_against_definition_hash reads as
// DRIFTED — "verified · review pending" — until an admin verifies it again. That
// is a consequence the author caused and would otherwise meet in the UI, days
// later, as a badge that changed by itself.
func metricPublishConsequence(live *api.Table) string {
	if live == nil || live.MetricStatus != api.MetricStatusVerified {
		return ""
	}
	return "this metric is VERIFIED — publishing this definition change re-fingerprints it, so it will read \"verified · review pending\" until an admin verifies it again"
}

// reconcileMetricNameTaken asks what the feature actually holds after a create
// the server refused on the name, returning the metric this file should adopt or
// nil and the clause the caller folds into its refusal.
//
// THE CASE IT IS FOR is not a race: it is a 200 this CLI failed to persist. The
// metric exists, the recording does not, and the next push tries to create the
// same name — which `nameHolderQuery` refuses with a `name_taken` the author
// cannot act on from the folder at all. That is a likelier failure here than for
// a table, because a metric's id lives only in the lock (and, for a legacy
// folder, only in a per-user baseline a colleague does not have).
//
// A NAME IS NOT AN IDENTITY, which is reconcileTimedOutCreate's rule and applies
// here in the same words. Adopting the wrong row binds this file to somebody
// else's metric, and the very next push hands their definition to be overwritten
// — worse than the refusal this reconcile exists to remove. So the name only
// NARROWS the candidates, and three further conditions have to hold:
//
//   - it is a `metric`, not a plain table. The namespace is shared, so a table
//     of this name is a REAL collision the author has to resolve, and adopting
//     it would point a metric file at a derived table.
//   - it is a LIVE row — the listing excludes drafts and committed version
//     snapshots, and this asserts that rather than assuming it.
//   - there is exactly ONE of them, so which row this file means is not a guess.
//
// Deliberately NOT gated on the recipe matching, which is where this differs
// from reconcileTimedOutCreate and the difference is honest. That one proves
// AUTHORSHIP, because a table's SQL is what the create sent. Here the create may
// never have run at all — the likely history is a create that landed and was not
// recorded, after which the FILE has been edited — so requiring a byte match
// would refuse exactly the case this exists for. The kind and the uniqueness are
// what is checked instead, and the adoption is said out loud rather than done
// quietly.
func reconcileMetricNameTaken(ctx context.Context, client *api.Client, binding wfdir.Binding, name string) (*api.Table, string) {
	noSingleMatch := fmt.Sprintf("reading the feature back did not identify a single metric named %q in it", name)
	if binding.FeatureID == "" || name == "" {
		return nil, noSingleMatch
	}
	items, err := client.ListFeatureTables(ctx, binding.FeatureID)
	if err != nil {
		return nil, noSingleMatch
	}
	found := ""
	for _, item := range items {
		if item == nil || item.Name != name {
			continue
		}
		if item.Kind != api.TableKindMetric {
			return nil, fmt.Sprintf("the row holding that name in this feature (%s) is a %s table rather than a metric, so it is a real collision and not this file's row",
				item.ID, item.Kind)
		}
		if item.ParentModelID != "" || item.ShadowStatus != "" {
			continue
		}
		if found != "" {
			return nil, fmt.Sprintf("reading the feature back found more than one metric named %q, so which of them this file means cannot be told apart", name)
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
	return row, ""
}

// isNameTakenOnMetricCreate reports the coded refusal the create answers when
// the name is held, told apart from every other 409 by the CODE rather than by
// the prose — a CLI that matched on English would start doing the wrong thing
// the day somebody reworded it.
func isNameTakenOnMetricCreate(err error) bool {
	_, taken := api.AsNameTaken(err)
	return taken
}

// metricRecordingFile names the file a metric's id is written into, which is not
// the same file for the two folder shapes and is not ronja.json for either.
//
// A NAMED STACK keeps it in the committed lock, where a colleague's fresh clone
// can read it — the whole reason a metric file is safe to keep in git. A LEGACY
// instances[] folder has nowhere committed to put one (wfdir.Lock.SetMetricSeen
// refuses an empty stack name rather than writing `"stacks":{"":{…}}` into a
// committed file), so it keeps it in the git-ignored local baseline, exactly as
// it keeps everything else it has.
//
// ⚠️ That legacy case has a real consequence worth knowing about: a colleague
// cloning such a folder starts with no metric id, and their first push tries to
// CREATE a metric that already exists. It converges rather than duplicating —
// reconcileMetricNameTaken adopts it by name — but the smooth path is a stack.
func metricRecordingFile(f *folder) string {
	if f.Stack != "" {
		return wfdir.LockPath(f.Root)
	}
	return wfdir.StatePath(f.Root)
}

// valueOrEmpty flattens a three-state claim for a body whose field is
// omitempty-suppressed, which is exactly what a CREATE wants: absent and an
// empty claim are the same instruction there, because there is nothing yet to
// clear.
//
// It is deliberately NOT used on the recipe route, where the two are different
// instructions and the pointer is carried through — see
// api.WriteMetricRecipeInput.ReportingTimezone.
func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// describeRecipeOverwrite renders the difference between the definition ON THE
// SERVER and the one this push is about to write over it, as an indented block
// the caller folds into a note or a refusal.
//
// IT EXISTS BECAUSE --force IS OTHERWISE A FLAG WHOSE MESSAGE IS ABOUT YOUR OWN
// FILE. Everywhere else in this loop, forcing past drift overwrites SQL that can
// be read back out of the row's version history. Here it overwrites the
// company's definition of a number, and the person typing it has, until this
// prints, seen only a sentence saying something changed. A diff is the cheapest
// thing that turns that into a decision.
//
// A LINE DIFF OF PRETTY-PRINTED JSON, deliberately, rather than a structural
// comparison: the recipe grammar belongs to the server, and a differ that
// understood `base_measures` would be a second copy of it — the thing this whole
// half of the loop is written to avoid. Line noise from a key that merely moved
// is the price, and it is a price paid on a path somebody has already opted into
// twice.
//
// Both sides are indented before comparing, so the two forms — the server's
// canonical bytes and this folder's resolved ones — are compared in one layout
// rather than as one long line each. A side that cannot be indented (it is not
// JSON at all, which nothing here should produce) is printed raw rather than
// dropped: a diff that silently showed one side would be worse than an ugly one.
func describeRecipeOverwrite(remote, local json.RawMessage) string {
	remoteLines := indentedJSONLines(remote)
	localLines := indentedJSONLines(local)
	var out strings.Builder
	for _, line := range diffLines(remoteLines, localLines) {
		fmt.Fprintf(&out, "      %s\n", line)
	}
	return out.String()
}

// indentedJSONLines pretty-prints one recipe into comparable lines.
func indentedJSONLines(body json.RawMessage) []string {
	if len(body) == 0 {
		return []string{"(none)"}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		return strings.Split(string(body), "\n")
	}
	return strings.Split(buf.String(), "\n")
}

// maxDiffLines is where the line diff stops being worth its quadratic table and
// starts being worth two blocks printed in full.
//
// A recipe is small — a source, a time column, a handful of measures — so the
// bound is never reached by anything this loop authored. It is here for the
// pathological file somebody pastes in, where an O(n²) table over ten thousand
// lines would be a visible hang in the middle of a refusal.
const maxDiffLines = 400

// diffLines is a longest-common-subsequence line diff, rendered with a leading
// "-" for what the server holds and "+" for what would replace it.
//
// Unchanged lines are kept, and kept in full rather than elided into a
// "@@ 3 unchanged @@" marker: a recipe is a dozen lines, and the context is what
// makes a one-line change legible as a change to THAT measure rather than to
// some measure.
func diffLines(before, after []string) []string {
	if len(before) > maxDiffLines || len(after) > maxDiffLines {
		out := []string{"on the server:"}
		out = append(out, before...)
		out = append(out, "in this file:")
		return append(out, after...)
	}
	// lcs[i][j] is the length of the longest common subsequence of before[i:]
	// and after[j:], which is what the walk below reads to decide each step.
	lcs := make([][]int, len(before)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(after)+1)
	}
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if before[i] == after[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		switch {
		case before[i] == after[j]:
			out = append(out, "  "+before[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+before[i])
			i++
		default:
			out = append(out, "+ "+after[j])
			j++
		}
	}
	for ; i < len(before); i++ {
		out = append(out, "- "+before[i])
	}
	for ; j < len(after); j++ {
		out = append(out, "+ "+after[j])
	}
	return out
}

// pipelineMetricStatuses is the metric files' half of `pipeline status`: for
// each committed metric file, which row it is here, whether that row's
// definition moved since this folder agreed with it, and whether a push would
// write anything.
//
// It costs one read per metric, and only for the folders that keep any — a
// folder with none pays a single failed stat, which is what keeps `status`
// unchanged for every folder that has not adopted them.
//
// It never REFUSES. `status` has to keep answering, so a file that does not
// parse and a metric that cannot be read are both reported as this file's own
// problem while every other line of the report stands.
func pipelineMetricStatuses(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec, live liveHashes) []pipelineMetricStatus {
	files, err := readMetricFiles(f.Root)
	if err != nil || len(files) == 0 {
		return nil
	}
	files = resolveMetricFiles(f, codec, live, files)
	out := make([]pipelineMetricStatus, len(files))
	indices := make([]int, len(files))
	for i := range files {
		indices[i] = i
	}
	fillConcurrently(indices, func(i int) {
		out[i] = oneMetricStatus(ctx, client, live, files[i])
	})
	return out
}

func oneMetricStatus(ctx context.Context, client *api.Client, live liveHashes, file pipelineMetricFile) pipelineMetricStatus {
	out := pipelineMetricStatus{
		Path: file.Path, Alias: file.Alias, MetricID: file.MetricID, SourceID: file.SourceID,
	}
	if file.Problem != "" {
		out.Problem = file.Problem
		out.Drift = driftUnreadable
		return out
	}
	if file.MetricID == "" {
		// Nothing on the server to compare against and nothing broken: a push
		// creates this metric. Reported as its own state rather than as drift,
		// exactly as a .sql file with no table behind it is.
		out.WillCreate = true
		out.Pending = true
		return out
	}
	row, err := client.GetTable(ctx, file.MetricID)
	if err != nil {
		out.Problem = fmt.Sprintf("could not read %s: %v", file.MetricID, err)
		out.Drift = driftUnreadable
		return out
	}
	out.Name = row.Name
	out.MetricStatus = row.MetricStatus
	if row.Kind != api.TableKindMetric {
		out.Problem = fmt.Sprintf("%s is a %s table rather than a metric — a metric file cannot define one; re-point the alias at the metric's own id",
			file.MetricID, row.Kind)
		out.Drift = driftUnreadable
		return out
	}
	recordedID, liveBase, declared := live.metricSeen(file.Path)
	if recordedID != file.MetricID {
		// Rebound to a different row: the recording describes another metric, so
		// it cannot be compared against this one. Reported as "not compared yet"
		// rather than as drift — nothing moved, this folder simply has no answer
		// about the row it now names.
		liveBase, declared = "", ""
	}
	switch {
	case liveBase == "":
		out.Drift = driftNoBaseline
	case metricfile.RecipeSHA256(row.MetricRecipe) == liveBase:
		out.Drift = driftNone
	default:
		out.Drift = driftChanged
	}
	// Pending is the LOCAL half — "this folder has not pushed this file's current
	// content here" — and is exactly what a modified .sql file means. A file
	// never pushed from anywhere reads as pending, which is right: nothing has
	// ever sent it.
	out.Pending = declared != file.Declared
	if draft, err := client.GetTableDraft(ctx, file.MetricID); err == nil && draft != nil {
		// A staged draft is work this folder has done and not published, which is
		// a different answer from both drift and pending — and the one `publish`
		// acts on.
		out.DraftID = draft.ID
	}
	return out
}
