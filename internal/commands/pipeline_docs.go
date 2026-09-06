package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The pipeline loop's DOCUMENTATION half: what a folder says a table and its
// columns are, and how that reaches the organization.
//
// TWO CARRIERS, ONE CONTRACT (internal/tabledocs.Docs):
//
//  1. The opt-in `-- @table` / `-- @column` header a .sql file may open with,
//     for the tables this folder BUILDS. The comment stays in `code` — it is
//     bytes like any other bytes, hashes as ordinary SQL, and so an edit to a
//     description shows up in `status` and is picked up by a bare push with no
//     new machinery at all.
//  2. A committed docs sidecar (`tables/<alias>.json`), for the tables it does
//     NOT build — integration, foundation, dynamic, workflow-written. Those
//     tables have no folder anywhere, and column prose matters most on exactly
//     them. See wfdir.TableDocsDirName for why it lives here rather than in a
//     `ronja table` loop of its own.
//
// THE JOIN RULE is the server's and is never anticipated here. The measured
// column set is the truth; prose is joined onto it. A documented name the table
// does not have is stored, reported, and never turned into a column — so a
// committed file may legitimately drift from the data, which is the whole reason
// it is safe to keep one in git. Every push reports what attached and what did
// not, READ BACK from the write, and an unmatched name is INFORMATION rather
// than a failure.
//
// THE ORDER is push → build → attach → report. Documentation is written after
// the build, never with it: before the first sync no column has been measured,
// so a `fields` write folded into the SQL PUT would report every column
// unmatched on a new table and report against the PREVIOUS build's columns on an
// old one.

// tableDocsResult is what a documentation write answered, per file. It is the
// --json shape and the human renderer's input, so the two cannot describe
// different things.
type tableDocsResult struct {
	// AttachedColumns are the documented names the table HAS, sorted.
	AttachedColumns []string `json:"attachedColumns,omitempty"`
	// UnmatchedColumns are the documented names it does not have. NOT A FAILURE:
	// the prose is stored and re-attaches by itself if a build later produces the
	// column. Reported so the author can see a typo or a rename, and so a file
	// committed ahead of its data reads as pending rather than as broken.
	UnmatchedColumns []string `json:"unmatchedColumns,omitempty"`
	// Description reports that the table's own prose was written.
	Description bool `json:"description,omitempty"`
	// ColumnsWritten is how many column names this write CARRIED, and is set
	// only when the instance answered no join report at all.
	//
	// It exists because the two empty answers are different facts and were being
	// reported as the same one. api.UpdateTableDocs folds an absent report into
	// nil — an instance older than the report, or a body that carried no
	// `fields` — so a header documenting only columns (no `-- @table`) produced
	// an all-zero result: no human line at all, and `"docs":{}` in --json, on a
	// push that really did write prose. The count comes from the FILE, which is
	// the only thing that still knows, and it is deliberately not a list of
	// names: what ATTACHED is read back from the write or not claimed at all.
	ColumnsWritten int `json:"columnsWritten,omitempty"`
	// UpToDate reports a carrier the row already agreed with, so nothing was
	// sent.
	UpToDate bool `json:"upToDate,omitempty"`
	// Error is a documentation write that did not land. The SQL half of the same
	// push may well have succeeded — see pushOutcomeDocsFailed.
	Error string `json:"error,omitempty"`
}

// tableDocsSidecar is one committed docs file, resolved as far as the folder can
// resolve it without the network.
type tableDocsSidecar struct {
	// Path is the folder-relative, slash-separated path, which is what the lock
	// and the local baseline are keyed by.
	Path string `json:"path"`
	// Alias is the file's stem — the name ronja.json declares and each stack
	// binds, or a literal table id.
	Alias string `json:"alias"`
	// TableID is what the alias resolved to on the selected stack, empty when it
	// resolved to nothing. Empty is a refusal, not a fall-through: a sidecar that
	// documented "whichever table this name happens to mean" would write prose
	// into a row nobody chose.
	TableID string `json:"tableID,omitempty"`
	// Problem is why this file cannot be used as it stands — it does not parse,
	// or its alias is not declared and bound here. Reported per file rather than
	// aborting: the other sidecars, and the whole SQL half of the push, are still
	// worth doing.
	Problem string `json:"problem,omitempty"`
	// Warning is something worth saying about a file that is nonetheless FINE —
	// today, only a sidecar that declares nothing at all.
	//
	// Apart from Problem because the two have different consequences and that is
	// the whole point of the split: a Problem refuses the file and makes the run
	// exit non-zero, and an empty `{}` sidecar used to be one. That turned a file
	// somebody committed and had not filled in yet into a folder where EVERY
	// push exits non-zero for ever, including pushes that name unrelated files.
	// The warning says the same sentence and breaks nothing.
	Warning string `json:"warning,omitempty"`

	Docs tabledocs.Docs `json:"-"`
}

// readTableDocsSidecars reads a pipeline folder's `tables/` directory.
//
// A MISSING DIRECTORY IS NOT AN ERROR and is the ordinary case: every pipeline
// folder that existed before this feature has none, and one that never
// documents a table it does not build never grows one.
//
// FLAT, one level. wfdir.TableDocsAlias states why: the stem is an alias, and an
// alias is a single name, so a nested file would be committed, never read, and
// never reported. A nested one is reported here rather than silently skipped,
// which is the difference between "you put it in the wrong place" and silence.
//
// Non-.json files are ignored outright — a README in that directory is somebody
// explaining the folder to a colleague, not a mistake.
func readTableDocsSidecars(root string) ([]tableDocsSidecar, error) {
	dir := filepath.Join(root, wfdir.TableDocsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []tableDocsSidecar
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if hasJSONBeneath(filepath.Join(dir, name)) {
				out = append(out, tableDocsSidecar{
					Path: wfdir.TableDocsDirName + "/" + name,
					Problem: fmt.Sprintf("%s/%s/ is a directory, and a docs sidecar is one flat file per table (%s) — its stem is the alias %s declares, and an alias is a single name",
						wfdir.TableDocsDirName, name, wfdir.TableDocsPath("<alias>"), wfdir.ManifestName),
				})
			}
			continue
		}
		path := wfdir.TableDocsDirName + "/" + name
		alias, ok := wfdir.TableDocsAlias(path)
		if !ok {
			continue
		}
		file := tableDocsSidecar{Path: path, Alias: alias}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			file.Problem = fmt.Sprintf("could not be read: %v", err)
			out = append(out, file)
			continue
		}
		docs, err := tabledocs.ParseSidecar(body)
		if err != nil {
			file.Problem = err.Error()
			out = append(out, file)
			continue
		}
		file.Docs = docs
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// hasJSONBeneath reports whether a directory under `tables/` holds anything that
// looks like a misplaced sidecar. A `tables/fixtures/` full of CSVs is somebody's
// own business; one holding JSON is very likely a sidecar in the wrong place, and
// that is the only case worth a message.
func hasJSONBeneath(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), wfdir.TableDocsExt) {
			return true
		}
	}
	return false
}

// resolveTableDocsSidecars turns each sidecar's alias into the id THIS
// organization uses for it, through the folder's own alias codec — the same map
// `{{ ref('orders') }}` resolves through, so the two can never disagree about
// what a name means.
//
// A literal `table-…` id resolves to itself, accepted exactly as it is
// everywhere else in the alias layer. It means the folder can only ever be
// pushed to the organization that minted the id, which is the author's choice to
// make and is a warning nobody needs twice: `ronja bind` and the alias pre-flight
// already say it.
func resolveTableDocsSidecars(f *folder, files []tableDocsSidecar) []tableDocsSidecar {
	// Which .sql file, if any, already OWNS each table this folder builds here.
	// The two carriers have to be disjoint — see wfdir.TableDocsDirName — and
	// this is the check that makes them so.
	//
	// It is an ID CHECK, and CheckAliasCollisions is not a substitute for it:
	// that one compares a declared alias NAME against a sibling .sql stem and
	// says nothing whatever about which rows they resolve to. Two spellings walk
	// straight past it — a sidecar named after a literal `table-…` id a .sql file
	// also builds, and an alias whose bind value IS that id — and either one
	// documents one table twice: the header onto the draft, the sidecar onto the
	// live row. The damage outlives the push, because `publish` then copies the
	// draft's rows onto live and the sidecar's own baseline is stale, so every
	// later push refuses with drift this folder caused itself.
	builtBy := make(map[string]string, len(f.Binding.Tables))
	for path, id := range f.Binding.Tables {
		if id == "" {
			continue
		}
		// Sorted-lowest wins, so a folder that has somehow bound two files to one
		// table names the same one on every run and the refusal is diffable.
		if other, taken := builtBy[id]; !taken || path < other {
			builtBy[id] = path
		}
	}
	for i := range files {
		file := &files[i]
		if file.Problem != "" {
			continue
		}
		switch {
		case markers.IsResourceID(markers.KindTable, file.Alias):
			file.TableID = file.Alias
		default:
			id, ok := f.Codec.resolve(markers.KindTable, file.Alias)
			if !ok || id == "" {
				where, fix := describeBindSite(f.selection())
				file.Problem = fmt.Sprintf(
					"%q is not a table this folder can name here: %s declares no dependency called %q of kind \"table\", or %s binds nothing to it.\n    Declare it in \"dependencies\" and %s — or rename the file to the table's id if this folder is only ever pushed to one organization",
					file.Alias, wfdir.ManifestName, file.Alias, where, strings.TrimSuffix(fix, "."))
				continue
			}
			file.TableID = id
		}
		if owner, built := builtBy[file.TableID]; built {
			file.Problem = fmt.Sprintf(
				"this documents %s, which %s in this folder already builds — a table has ONE carrier, and documenting it twice writes the header onto the draft and this file onto the live row.\n    Put the prose in %s's `%s` header and delete this file, or point the alias at a table this folder does not build",
				file.TableID, owner, owner, tabledocs.HeaderPrefix)
			continue
		}
		if file.Docs.Empty() {
			// A file that declares nothing is not an error and is not a write.
			// Said out loud rather than skipped in silence, because a committed
			// file that does nothing at all is nearly always a file somebody
			// meant to fill in.
			//
			// A WARNING RATHER THAN A PROBLEM, which is the difference between
			// saying that and breaking the folder. As a Problem it refused the
			// file, and a refused sidecar makes the whole push exit non-zero —
			// so an empty `{}` somebody committed as a placeholder failed every
			// push in that folder from then on, including `push one-file.sql`,
			// which has nothing to do with it. Nothing is at stake: the file
			// declares nothing, so nothing is sent and nothing is overwritten.
			file.Warning = "this file declares no description and no columns, so it documents nothing — fill in \"description\" or \"columns\", or delete it"
		}
	}
	return files
}

// tableDocsFieldRefs reports each sidecar's alias as a USE of that alias, in the
// shape the alias pre-flight reads.
//
// Without it `unusedAliases` would report every dependency a folder declares
// purely to document a table as dead config — on every push, every status and
// every `sync check` — which is the warning crying wolf at the one folder shape
// that has no marker to point at. It is the same leg an automation folder needs,
// for the same reason: the reference is a FILE NAME here, and markers.Scan finds
// nothing to scan.
//
// A file that does not parse still counts as a USE, and that is the whole rule:
// naming an alias is what a sidecar's FILENAME does, and the body is not
// consulted. A file whose JSON is broken, or which declares nothing, has one
// mistake in it — reported by the docs pass, in the file, with the fix — and
// skipping it here earns it a SECOND warning saying the dependency it plainly
// names is dead config. Two warnings for one mistake sends the author to fix
// the manifest entry that is perfectly correct.
//
// An unreadable directory contributes nothing rather than failing, exactly as
// folderFieldRefs' automation leg does: this runs inside `status` and
// `sync check`, which have to keep answering.
func tableDocsFieldRefs(root string) []fieldRef {
	files, err := readTableDocsSidecars(root)
	if err != nil {
		return nil
	}
	var out []fieldRef
	for _, file := range files {
		if file.Alias == "" {
			continue
		}
		out = append(out, fieldRef{
			Where: file.Path, Kind: markers.KindTable, Value: file.Alias,
		})
	}
	return out
}

// docsWriteInput projects a Docs onto the wire body, or reports that there is
// nothing to send.
//
// DescriptionSourceUser on everything, and on nothing that is empty. Every
// carrier here is a file a person committed to a repository, so the prose is
// theirs and the agent may not rewrite it — but an EMPTY description is the
// caller handing the field back to Ronja, and stamping authorship on it would be
// a provenance claim with nothing to protect. The server applies the same rule
// from the other side; sending it explicitly means the CLI does not depend on
// which of the two runs first.
func docsWriteInput(docs tabledocs.Docs) (api.UpdateTableDocsInput, bool) {
	if docs.Empty() {
		return api.UpdateTableDocsInput{}, false
	}
	in := api.UpdateTableDocsInput{Description: docs.Description}
	if docs.Description != nil && *docs.Description != "" {
		in.DescriptionSource = api.DescriptionSourceUser
	}
	for _, name := range docs.Names() {
		text := docs.Columns[name]
		field := api.TableFieldInput{Name: name, Description: text}
		if text != "" {
			field.DescriptionSource = api.DescriptionSourceUser
		}
		in.Fields = append(in.Fields, field)
	}
	return in, true
}

// docsCreateFields is the same projection for a CREATE body, which carries the
// header's columns so a table does not exist even briefly with prose its own
// file already holds. Under the join rule they all come back unmatched — a
// create has measured nothing — which is why the report a reader is shown comes
// from the write that follows the build.
func docsCreateFields(docs tabledocs.Docs) []api.TableFieldInput {
	in, ok := docsWriteInput(docs)
	if !ok {
		return nil
	}
	return in.Fields
}

// docsResultFrom folds one write's answer into the reported shape.
//
// The write's ERROR is an argument rather than something the caller stamps on
// afterwards, because the fields below are claims about what LANDED. A failed
// write landed nothing, so `description: true` beside `error` in --json would be
// the report contradicting itself — and a script reading the first key would
// believe a description was written that was not.
func docsResultFrom(docs tabledocs.Docs, report *api.ColumnDocReport, err error) *tableDocsResult {
	if err != nil {
		return &tableDocsResult{}
	}
	out := &tableDocsResult{Description: docs.Description != nil}
	if report != nil {
		out.AttachedColumns = report.AttachedColumns
		out.UnmatchedColumns = report.UnmatchedColumns
		return out
	}
	// No report, but columns were sent — see ColumnsWritten. A current instance
	// always answers one of the two lists for a `fields` write, so this is an
	// instance that predates the join report; the write landed, and the only
	// thing missing is which names attached.
	out.ColumnsWritten = len(docs.Columns)
	return out
}

// docsRefusalHint is the one sentence a REFUSED documentation write earns, for
// the two statuses that mean something a reader can act on. Shared by both
// carriers, so the .sql header and the sidecar cannot explain the same refusal
// two ways — or, as they did, one of them not at all.
//
// Everything else returns "": the server's own message is already printed beside
// this, and a hint that guessed at a status it does not know would be worse than
// the message it is padding.
func docsRefusalHint(err error) string {
	switch api.StatusOf(err) {
	case 403:
		return "Writing a table's description and its column prose needs write access to the feature that holds it — ask an admin, or take the documentation out of this file."
	case 400:
		// A documentation write's body is `description` and `fields` and nothing
		// else — see docsWriteInput — so a 400 is this instance refusing one of
		// those two keys, and the bare "field not allowed: fields" a non-admin
		// gets says nothing about who may do it.
		//
		// The status is what is matched, never the English: a CLI that read the
		// server's prose would start explaining the wrong 400 the day somebody
		// reworded it. Which is also why this hedges rather than asserting a
		// cause it cannot see.
		return "A documentation write sends `description` and `fields` and nothing else, so a 400 is this instance refusing one of them — writing column prose (`fields`) is admin-only on some instances. Ask an admin to run this push, or take the documentation out of this file."
	}
	return ""
}

// describeDocsResult is the ONE human line a documentation write earns, so the
// push report and the docs pass cannot describe the same thing two ways.
//
// The unmatched half leads with what it MEANS rather than with the word
// "unmatched": a name that did not attach is prose that is stored and describes
// nothing yet, which is a sentence an author can act on.
func describeDocsResult(r *tableDocsResult) string {
	if r == nil {
		return ""
	}
	if r.Error != "" {
		return "documentation NOT written — " + r.Error
	}
	var parts []string
	if r.UpToDate {
		return "documentation already matches this file"
	}
	if r.Description {
		parts = append(parts, "description written")
	}
	if n := len(r.AttachedColumns); n > 0 {
		parts = append(parts, fmt.Sprintf("%d column(s) documented: %s", n, strings.Join(r.AttachedColumns, ", ")))
	}
	if n := len(r.UnmatchedColumns); n > 0 {
		parts = append(parts, fmt.Sprintf("%d name(s) the table does not have, kept for when it does: %s",
			n, strings.Join(r.UnmatchedColumns, ", ")))
	}
	// The write landed and this instance did not say which names attached. Said
	// as what it is, rather than as silence — a push that wrote prose and printed
	// nothing reads as a push that ignored the header.
	if n := r.ColumnsWritten; n > 0 {
		parts = append(parts, fmt.Sprintf("%d column(s) written; this instance does not report which of them the table has", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// metaOf is the documentation fingerprint of a row the server just answered
// with. Taken from a GetTable answer and nothing else — `fields[]` is populated
// by the single-row GET alone, so a hash taken from a create, a checkout or a
// list response would be a hash of a table with no columns and would report
// drift the moment anything read the real row.
func metaOf(row *api.Table) string {
	if row == nil {
		return ""
	}
	return tabledocs.MetaSHA256(row.Description, row.ColumnDescriptions())
}

// docsLanded asks the one question a publish cannot ask any other way: does this
// LIVE ROW already say what this carrier declares?
//
// It exists because `publish` runs in a separate process from the `push` that
// wrote the documentation, and so has no memory of whether that write landed. A
// push whose prose was refused (pushOutcomeDocsFailed) deliberately withholds
// recordSynced so the file stays re-pushable — and publish then banked the
// content baseline and the documentation agreement anyway, from a header it
// merely PARSED, undoing exactly the protection push had just bought. After that
// the file's hash matches the baseline, so a bare push skips it; the meta
// fingerprint says the folder agrees with the row, so status reads clean; and
// the committed header and the row disagree permanently. This is the predicate
// that stops publish claiming it.
//
// IT IS JUDGED UNDER THE JOIN RULE, which is the only way this can be asked at
// all. A documented name the table does not have is stored server-side and never
// reported back, so a name-by-name equality against the file would be false for
// ever on any folder documenting a column ahead of its data. Only the names the
// row HAS are compared; the rest are exempt by construction, which is the same
// exemption tabledocs.Docs.Fingerprint's comment describes from the other side.
//
// A false NEGATIVE costs one re-push, which converges. A false POSITIVE is the
// permanent silent divergence above, so the comparison is deliberately the
// strict one: every managed name the row carries must match exactly.
func docsLanded(docs tabledocs.Docs, row *api.Table) bool {
	if row == nil {
		return false
	}
	if docs.Description != nil && *docs.Description != row.Description {
		return false
	}
	// Presence and prose come from the SAME map, so "the row has no column
	// called x" and "the row's column x has no prose" cannot be confused: an
	// absent key is the first, an empty value the second.
	onRow := row.ColumnDescriptions()
	for _, name := range docs.Names() {
		text, measured := onRow[name]
		if !measured {
			continue
		}
		if text != docs.Columns[name] {
			return false
		}
	}
	return true
}

// docsDriftReason is the third leg of the pipeline drift guard: has this row's
// DOCUMENTATION moved since the folder last agreed with it?
//
// It arms ONLY for a push that carries documentation. A folder whose files open
// with no header and which keeps no sidecars declares nothing about any table's
// prose, so there is nothing to protect and this returns "" without so much as
// looking — which is what keeps every folder written before this feature
// behaving exactly as it did.
//
// An empty baseline is "no answer", never "agreed", and disarms the leg exactly
// as an absent LiveSHA256 disarms leg (a). The alternative — treating no
// recording as a mismatch — would refuse the first documenting push every folder
// ever makes.
func docsDriftReason(what string, row *api.Table, baseline string) string {
	if baseline == "" || row == nil {
		return ""
	}
	if metaOf(row) == baseline {
		return ""
	}
	return fmt.Sprintf("the documentation of %s changed since your last sync", what)
}

// Per-sidecar push outcomes. These are --json `outcome` values, so they are a
// contract for anything scripting the CLI.
const (
	docsOutcomeWritten  = "documented"
	docsOutcomeUpToDate = "up_to_date"
	docsOutcomeRefused  = "refused"
	// docsOutcomeNothing is a file that declares nothing, so nothing was sent.
	// Told apart from up_to_date because they are different facts — that one
	// says the row already holds what this file declares, this one says the file
	// declares nothing at all — and apart from refused because it is not a
	// failure and must not colour the run's exit code.
	docsOutcomeNothing = "nothing_to_document"
)

// pipelineDocsFileResult is what happened to one docs sidecar.
type pipelineDocsFileResult struct {
	Path    string `json:"path"`
	Alias   string `json:"alias,omitempty"`
	TableID string `json:"tableID,omitempty"`
	Outcome string `json:"outcome"`
	// Result is the write's own answer — what attached, what did not. Absent on
	// a refusal, which never reached a write.
	Result *tableDocsResult `json:"result,omitempty"`
	Error  string           `json:"error,omitempty"`
	// Warning is a note about a file that is nonetheless fine — see
	// tableDocsSidecar.Warning. Never a reason for a non-zero exit.
	Warning string `json:"warning,omitempty"`
}

// pushTableDocs is the SIDECAR half of a pipeline push: the tables this folder
// documents but does not build.
//
// It is a separate pass rather than a step inside pushOneTable because it is a
// different loop over a different set. There is no .sql file here, no draft, no
// checkout, no build and no publish: a sidecar's write goes STRAIGHT TO THE LIVE
// ROW, because a table this folder does not build has no draft in this loop for
// prose to wait in — and because the write is documentation only, which is the
// one kind of change to a table that cannot break a build.
//
// Ordered after the SQL, so a report reads as "here is what I built, here is
// what I documented".
//
// Failure is DATA, not an error return, exactly as it is for a .sql file: one
// unresolvable alias must not stop the other eleven sidecars, and the run's exit
// code is decided by the caller from the outcomes.
func pushTableDocs(ctx context.Context, client *api.Client, f *folder, inst *wfdir.InstanceState,
	files []tableDocsSidecar, opts pipelinePushOptions) []pipelineDocsFileResult {

	if len(files) == 0 {
		return nil
	}
	live := f.live(inst)
	out := make([]pipelineDocsFileResult, 0, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			out = append(out, pipelineDocsFileResult{
				Path: file.Path, Alias: file.Alias, Outcome: docsOutcomeRefused, Error: interruptedMessage,
			})
			continue
		}
		out = append(out, pushOneTableDocs(ctx, client, live, file, opts))
		// Saved after EVERY sidecar, exactly as the SQL loop saves after every
		// file and for the same reason: the write has really landed on the server,
		// so a run that dies on the next file must not lose the recording of it.
		// Losing one is not a wasted round trip — the next push finds no
		// fingerprint, adopts whatever the row says now, and the guard that would
		// have caught somebody else's edit is silently disarmed.
		if err := f.saveBaseline(); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not record what %s documented in the local baseline (%v).\n", file.Path, err)
		}
	}
	return out
}

// pushOneTableDocs runs the whole cycle for one sidecar: resolve, guard, compare,
// write, re-read, record.
func pushOneTableDocs(ctx context.Context, client *api.Client, live liveHashes,
	file tableDocsSidecar, opts pipelinePushOptions) pipelineDocsFileResult {

	out := pipelineDocsFileResult{Path: file.Path, Alias: file.Alias, TableID: file.TableID}
	refuse := func(format string, args ...any) pipelineDocsFileResult {
		out.Outcome = docsOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", file.Path, out.Error)
		return out
	}
	if file.Problem != "" {
		return refuse("%s", file.Problem)
	}
	// A file that declares nothing is answered before the network. There is no
	// row to read for it: nothing would be compared and nothing sent, and a
	// GetTable here would only turn an empty placeholder into a request and a
	// second way to fail.
	if file.Docs.Empty() {
		out.Outcome = docsOutcomeNothing
		out.Warning = file.Warning
		fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", file.Path, out.Warning)
		return out
	}

	// The LIVE row, read for three things at once: the drift guard's
	// fingerprint, the "would this change anything" comparison, and the
	// existence check that turns a broken bind into a message about a binding
	// rather than about a 400.
	row, err := client.GetTable(ctx, file.TableID)
	if err != nil {
		if status := api.StatusOf(err); status == 404 || status == 400 {
			return refuse("%s documents %s, which is not a table you can reach on this instance (deleted, moved, or in a feature you are not a member of): %v\n    Point the alias at a table in this organization with `ronja bind`, or drop the file",
				file.Path, file.TableID, err)
		}
		return refuse("read %s: %v", file.TableID, err)
	}

	// The drift guard. One leg, because a sidecar has exactly one row: the live
	// table's DOCUMENTATION against the fingerprint taken from that same row.
	// Compared only when the recording was taken from THIS table — a sidecar
	// rebound by `ronja bind` or by a stack switch describes a different row, and
	// a fingerprint from the old one can only ever refuse or, worse, agree by
	// accident.
	//
	// ⚠️ IT IS A READ-THEN-WRITE GUARD, NOT A COMPARE-AND-SWAP, and unlike the
	// SQL half it has no server-side precondition to become one. UpdateTableCode
	// sends `baseCodeSha256` and the server refuses the write if the row moved;
	// UpdateTableDocsInput has no analogue, because PUT /feature/model has one
	// precondition and it is on `code`. So a web edit landing between the
	// GetTable above and the PUT below is overwritten, silently — a window of
	// one round trip, on a surface where two writers to the same table's prose
	// is already the unusual case.
	//
	// Stated rather than papered over: a client-side re-read before the write
	// would only narrow the window, not close it, while reading like a guarantee
	// to the next person. Closing it properly is a SERVER change — a
	// `baseMetaSha256` on the documentation body, refused with a 409 the way the
	// code precondition is — and it belongs with that change, not here. The
	// damage is bounded meanwhile: prose only, never SQL or data, the losing
	// edit is in the row's audit history, and the NEXT push re-reads and reports
	// the divergence rather than compounding it.
	recordedID, baseline, declared := live.docsSeen(file.Path)
	if recordedID != file.TableID {
		baseline, declared = "", ""
	}
	if reason := docsDriftReason(file.TableID, row, baseline); reason != "" {
		if !opts.Force {
			return refuse("%s — pushing %s would overwrite it.\n    Run `ronja pipeline status` to see what the table says now, or push --force to overwrite",
				reason, file.Path)
		}
		fmt.Fprintf(os.Stderr, "  Note: --force — overwriting the documentation of %s (%s).\n", file.TableID, file.Path)
	}

	// Nothing to do, and said so rather than sent. TWO conditions, and both are
	// needed: the FILE has not changed since the last push from this folder
	// (declared), and the ROW has not moved since then either (baseline, which
	// the guard above has just proved equal or --forced past).
	//
	// It cannot be answered by comparing the file against the row, and that is
	// the join rule's doing: a documented name the table does not have is stored
	// server-side and never reported back, so such a file would read as pending
	// for ever and be re-sent on every push.
	if declared != "" && declared == file.Docs.Fingerprint() && baseline != "" && metaOf(row) == baseline {
		out.Outcome = docsOutcomeUpToDate
		out.Result = &tableDocsResult{UpToDate: true}
		return out
	}

	in, send := docsWriteInput(file.Docs)
	if !send {
		// Unreachable — an empty carrier is refused as a Problem at resolve time
		// — and answered rather than asserted, because a silent empty PUT would
		// be a write that clears a description nobody asked to clear.
		return refuse("this file declares no description and no columns, so it documents nothing")
	}
	report, err := client.UpdateTableDocs(ctx, file.TableID, in)
	if err != nil {
		if hint := docsRefusalHint(err); hint != "" {
			return refuse("documenting %s was refused (%v).\n    %s", file.TableID, err, hint)
		}
		return refuse("document %s: %v", file.TableID, err)
	}
	out.Outcome = docsOutcomeWritten
	out.Result = docsResultFrom(file.Docs, report, nil)

	// Re-read, so the recording is a fingerprint of what the row ACTUALLY holds
	// rather than of what this push believes it wrote. The two differ by exactly
	// the join rule: an unmatched name's prose is stored but is not a column, so
	// a locally-predicted fingerprint would drift from the row's own the moment
	// a documented column did not exist.
	//
	// A re-read that fails CLEARS the recording rather than leaving the old one.
	// The old fingerprint describes a state that has certainly moved — this push
	// just moved it — so keeping it would refuse the next push and name a change
	// this folder made itself.
	fresh, err := client.GetTable(ctx, file.TableID)
	if err != nil {
		live.setDocsSeen(file.Path, file.TableID, "", "")
		fmt.Fprintf(os.Stderr, "  Note: %s was documented, but reading it back failed (%v) — the next push will compare against nothing rather than against a stale fingerprint.\n", file.Path, err)
	} else {
		live.setDocsSeen(file.Path, file.TableID, metaOf(fresh), file.Docs.Fingerprint())
	}
	if line := describeDocsResult(out.Result); line != "" {
		fmt.Fprintf(os.Stderr, "  Note: %s — %s\n", file.Path, line)
	}
	return out
}

// pipelineDocsStatuses is the docs sidecars' half of `pipeline status`: for each
// committed docs file, which table it names here, whether that row's prose moved
// since this folder agreed with it, and whether a push would write anything.
//
// It costs one read per sidecar, and only for the folders that keep any — a
// folder with none pays a single failed stat, which is what keeps `status`
// unchanged for every folder that has not adopted them.
//
// It never REFUSES. `status` has to keep answering, so a file that does not
// parse and a table that cannot be read are both reported as this file's own
// problem while every other line of the report stands.
func pipelineDocsStatuses(ctx context.Context, client *api.Client, f *folder, live liveHashes) []pipelineDocsStatus {
	files, err := readTableDocsSidecars(f.Root)
	if err != nil || len(files) == 0 {
		return nil
	}
	files = resolveTableDocsSidecars(f, files)
	out := make([]pipelineDocsStatus, len(files))
	indices := make([]int, len(files))
	for i := range files {
		indices[i] = i
	}
	fillConcurrently(indices, func(i int) {
		out[i] = oneDocsStatus(ctx, client, live, files[i])
	})
	return out
}

func oneDocsStatus(ctx context.Context, client *api.Client, live liveHashes, file tableDocsSidecar) pipelineDocsStatus {
	out := pipelineDocsStatus{Path: file.Path, Alias: file.Alias, TableID: file.TableID}
	if file.Problem != "" {
		out.Problem = file.Problem
		out.Drift = driftUnreadable
		return out
	}
	row, err := client.GetTable(ctx, file.TableID)
	if err != nil {
		out.Problem = fmt.Sprintf("could not read %s: %v", file.TableID, err)
		out.Drift = driftUnreadable
		return out
	}
	out.Name = row.Name
	// A file that declares nothing is reported, and reported as FINE. The row is
	// still named and still compared — the folder is bound to it and a reader
	// asking about drift deserves the answer — but Pending stays false below,
	// because a file that says nothing can never say something the table does
	// not.
	out.Warning = file.Warning
	recordedID, baseline, declared := live.docsSeen(file.Path)
	if recordedID != file.TableID {
		// A sidecar rebound to a different table: the recording describes another
		// row, so it cannot be compared against this one. Reported as "not
		// compared yet" rather than as drift — nothing moved, this folder simply
		// has no answer about the row it now names.
		baseline, declared = "", ""
	}
	switch {
	case baseline == "":
		out.Drift = driftNoBaseline
	case metaOf(row) == baseline:
		out.Drift = driftNone
	default:
		out.Drift = driftChanged
	}
	// Pending is the LOCAL half — "this folder has not pushed this file's
	// current content here" — and is exactly what a modified .sql file means. A
	// file never pushed from anywhere reads as pending, which is right: nothing
	// has ever sent it.
	out.Pending = !file.Docs.Empty() && declared != file.Docs.Fingerprint()
	return out
}

// tableDocsEdges is the docs sidecars' contribution to `ronja sync check`: does
// every table this folder documents still resolve?
//
// Two products, because they answer two different questions and the command
// scores them differently:
//
//   - EDGES for a sidecar written against a LITERAL table id. A sidecar written
//     against an ALIAS is already covered — dependencyEdges emits one per
//     declared dependency — and emitting a second edge for the same id would
//     double-count it in the summary.
//   - FINDINGS for a file this folder cannot use as it stands: it does not
//     parse, or its alias is not declared and bound here. A finding rather than
//     an edge because nothing was looked up; the file is wrong before any
//     instance is asked about it.
func tableDocsEdges(f *folder) (edges []syncEdgeReport, findings []string) {
	files, err := readTableDocsSidecars(f.Root)
	if err != nil || len(files) == 0 {
		return nil, nil
	}
	for _, file := range resolveTableDocsSidecars(f, files) {
		if file.Problem != "" {
			findings = append(findings, fmt.Sprintf("%s: %s", file.Path, file.Problem))
			continue
		}
		if !markers.IsResourceID(markers.KindTable, file.Alias) {
			continue
		}
		edges = append(edges, syncEdgeReport{
			Kind: markers.KindTable, Ref: file.Alias, ID: file.TableID, Where: file.Path,
		})
	}
	return edges, findings
}
