package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/tablerefs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline` is the CLI's fourth folder-sync loop, after `wf`, `app` and
// `db`.
//
// It earns its place the same way they do: iterating on a derived table over raw
// HTTP is five requests and two documented footguns per edit cycle (check out a
// draft, PUT the DRAFT's id, sync the DRAFT, poll status AND the error endpoint
// with a truth table, commit), and the draft-id bookkeeping is per table. The
// doctrine is unchanged — sync verbs yes, resource verbs no. There is no
// `pipeline list` and no `pipeline delete`; discovery stays on plain HTTP.
//
// Three things make it a different shape from the other two folder loops, and
// all three are visible in the first command:
//
//  1. A folder binds MANY rows. One .sql file is one derived table, so the
//     manifest carries a `tables` map rather than a single id — which is why
//     wfdir's single-id binding helpers REFUSE this kind instead of answering
//     with a workflow's field.
//  2. The pushable set is `.sql` and nothing else. Everything else in the folder
//     — a README, a Makefile, a fixtures directory — is SILENTLY ignored, so a
//     pipeline can live in an ordinary repo. That rule is one predicate
//     (pipelineSyncable) applied everywhere a file set is enumerated, because an
//     asymmetry between the walk and the baseline is a phantom deletion.
//  3. A push BUILDS. Every synced file ends in a real materialization of the
//     draft, so the loop's natural output is a verdict and a confidence report —
//     schema delta, row counts, sample rows — rather than a "saved" line.
//
// There is deliberately no `run` verb: committing a draft cascades server-side,
// so publishing already rebuilds everything downstream.
func newPipelineCmd() *cobra.Command {
	pl := &cobra.Command{
		Use:     "pipeline",
		Aliases: []string{"pl"},
		Short:   "Develop a feature's derived tables from a local folder",
		Long: `Develop a feature's derived tables from a local folder.

A pipeline folder holds one .sql file per derived table plus a small manifest
(ronja.json) recording which table each file builds on each instance. Anything
that is not a .sql file is ignored, so the folder can also hold a README, tests
or whatever else your repo needs. The sync baseline lives in .ronja/, which is
local-only and git-ignored for you.

Two ways in:

  ronja pipeline clone <feature-id>   pull a feature's derived tables down
  ronja pipeline init --feature <id>  start an empty folder

Then edit locally and check where you stand:

  ronja pipeline status               local changes, remote state and drift
  ronja pipeline push                 sync each changed file into your draft,
                                      build it, and report what it produced
  ronja pipeline publish              commit your drafts (or ask an admin to)
  ronja pipeline discard              throw your drafts away

Pushing builds a DRAFT, never the live table, so a failed build changes nothing
anyone else can see. Publishing commits — and a commit cascades, so every table
downstream of what you published rebuilds on its own.

Refs between tables are id-form: {{ ref('table-abc') }}. The CLI derives each
table's declared inputs from the refs in its file, so adding an upstream is
editing the SQL and nothing else.

Note the name: api.Workflow also has a "pipeline" KIND, and this command has
nothing to do with those — it operates on derived tables.

Bindings are per-instance, so the same folder can target a local backend and
production without either overwriting the other's binding.`,
	}
	pl.AddCommand(
		newPipelineInitCmd(), newPipelineCloneCmd(), newPipelineStatusCmd(),
		newPipelinePushCmd(), newPipelinePublishCmd(), newPipelineDiscardCmd(),
	)
	addStackFlag(pl)
	return pl
}

// pipelineSyncable reports whether a folder-relative path is one this loop
// pushes: a `.sql` file, and nothing else.
//
// A THIN read of wfdir.PipelineKind rather than a second copy of the rule. The
// predicate lives on the Kind because Enumerate and CheckLocalPaths have to
// answer with it identically — a walk that drops a path the baseline keeps
// leaves a baseline claiming a file no later walk sees, which `status` reports
// as a local deletion and a push acts on. A private copy here is exactly how the
// two sides drift.
func pipelineSyncable(path string) bool {
	return wfdir.PipelineKind.Syncable(path)
}

// enumerateFolder is wfdir.Enumerate plus the per-kind rule about what a
// pipeline folder REPORTS having ignored.
//
// The walk itself already applies the .sql rule (wfdir.NotSyncable), so nothing
// here filters Files. What is left is the Skipped list, which for this kind is
// mostly noise — see below.
func enumerateFolder(root string, kind wfdir.Kind) (*wfdir.Enumeration, error) {
	enumeration, err := wfdir.Enumerate(root, kind)
	if err != nil {
		return nil, err
	}
	if kind.Name != wfdir.KindPipeline {
		return enumeration, nil
	}
	// Skipped is filtered by the SAME predicate, and for the same reason it
	// exists at all. A skipped entry means "a file you might have expected to be
	// synced, and why it was not" — for a workflow that is every excluded path,
	// but here a .git/ directory, a .venv/, a symlinked fixture and a dot-file are
	// all things the loop was never going to send in the first place. Reporting
	// them on every single command is the noise the silent non-.sql rule exists to
	// avoid, and it trains the reader past the one line that matters: a `.sql`
	// file that was skipped.
	kept := enumeration.Skipped[:0]
	for _, skipped := range enumeration.Skipped {
		if pipelineSyncable(skipped.Path) {
			kept = append(kept, skipped)
		}
	}
	enumeration.Skipped = kept
	return enumeration, nil
}

// readPipelineFiles enumerates a pipeline folder and reads every .sql file, so
// the same set can be ordered, guarded and pushed without walking twice.
func readPipelineFiles(root string) (map[string]string, *wfdir.Enumeration, error) {
	enumeration, err := enumerateFolder(root, wfdir.PipelineKind)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]string, len(enumeration.Files))
	for path := range enumeration.Files {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		files[path] = string(body)
	}
	return files, enumeration, nil
}

// refusePositionalRefs refuses local SQL that still carries a positional
// `{{ ref('N') }}` marker.
//
// This is the one silently-destructive path in the whole design, so it is a hard
// refusal rather than a warning. A positional ref is an index into the row's
// stored `input_models`, and the CLI has no such list for a file on disk — the
// only list it can build is DeriveInputModels, which is derived from the
// id-shaped refs and SORTED. Sending the file would therefore hand the server a
// positional ref numbered against one list and a declared list that is a
// different one, and the server would resolve it against the second: the SQL
// silently starts reading a different table, the build succeeds, and nothing
// anywhere says so.
//
// Clone cannot produce such a file (it canonicalizes, and refuses what it cannot
// canonicalize), so reaching here means the file was hand-written or pasted out
// of the web builder. The fix is to write the id.
//
// Called with the files a command is about to SEND, never with the whole folder:
// a marker in a file nobody is pushing cannot mis-resolve anything, and refusing
// the run for it would let one unpushable file hold every other table hostage —
// including `push a.sql`, which named a file that is perfectly fine.
func refusePositionalRefs(files map[string]string) error {
	var offenders []string
	for _, path := range sortedPaths(files) {
		if refs := tablerefs.PositionalRefs(files[path]); len(refs) > 0 {
			offenders = append(offenders, fmt.Sprintf("%s (%s)", path, strings.Join(refs, ", ")))
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("these files carry positional refs, which cannot be pushed:\n    %s\n  A positional ref is an index into the table's stored input list, and a file on disk has no such list — the server would resolve it against the inputs this push declares, which are sorted and need not be in the same order. The SQL would then read a different table than you meant, and the build would succeed.\n  Replace each one with the table's id: {{ ref('table-…') }}",
		strings.Join(offenders, "\n    "))
}

// pipelineBaseline returns this folder's baseline for the bound instance,
// creating an empty one when the folder has never synced here.
//
// The returned pointer is the one stored in f.State, so callers mutate it and
// then SaveState — the same shape the workflow loop's baseline has, minus the
// wholesale replacement: a pipeline push lands file by file, and each file's
// entry has to survive the next one failing.
func pipelineBaseline(f *folder) *wfdir.InstanceState {
	inst := f.State.For(f.Key)
	if inst == nil {
		inst = &wfdir.InstanceState{SourceID: f.Binding.FeatureID}
		f.State.Set(f.Key, inst)
	}
	if inst.Files == nil {
		inst.Files = map[string]wfdir.FileState{}
	}
	if inst.Tables == nil {
		inst.Tables = map[string]wfdir.TableState{}
	}
	return inst
}

// The baseline writers.
//
// One per MOMENT this loop learns something, rather than one per field, because
// the invariant on wfdir.TableState — a hash is only ever compared against the
// row it was taken from — is only true if every writer knows which row it just
// looked at. A single "record everything" helper is what collapsed the three
// fingerprints into one in the first place: every caller passed the bytes it
// happened to be holding, and the guard could no longer tell whose they were.
//
// They all MUTATE the existing entry rather than replacing it, so a writer that
// means to move one fingerprint cannot silently erase the other two.

// mutateTable edits one file's table state in place.
func mutateTable(inst *wfdir.InstanceState, path string, mutate func(*wfdir.TableState)) {
	state := inst.Tables[path]
	mutate(&state)
	inst.Tables[path] = state
}

// interruptedMessage is what a Ctrl-C looks like from inside a pipeline command.
//
// Said once, and said in terms of what is still true afterwards: the interrupt
// stops this process listening, never the server. A build already dispatched
// runs to completion, a draft already written still holds what was written into
// it, and a commit already sent may well have landed. "context canceled",
// repeated once per remaining file, says none of that.
const interruptedMessage = "interrupted — anything already sent is still running on the server; run the command again to pick it up"

// pipelineErrorText renders an error for a per-file refusal, mapping the one
// whose bare text is not about anything the reader can act on.
func pipelineErrorText(err error) string {
	if errors.Is(err, context.Canceled) {
		return interruptedMessage
	}
	return err.Error()
}

// recordDraftPointer records which row a file is and which draft of it this
// checkout holds. An empty draftID means there is none any more (committed,
// discarded, or gone from under us).
//
// A draft id that is not the recorded one CLEARS BOTH draft fingerprints: the
// recorded bytes describe a row that is not this one, and keeping them would
// point leg (b) of the drift guard at a different draft's contents — and, worse
// for the wire one, would send the next PUT a baseCodeSha256 taken from a row
// that no longer exists, which the server would refuse as somebody else's edit.
func recordDraftPointer(inst *wfdir.InstanceState, path, tableID, draftID string) {
	mutateTable(inst, path, func(s *wfdir.TableState) {
		if s.DraftID != draftID {
			s.DraftSHA256 = ""
			s.DraftWireSHA256 = ""
		}
		s.TableID = tableID
		s.DraftID = draftID
	})
}

// recordDraftWrite records the SQL just written into a draft, in BOTH forms:
// `content` as it stands on disk, and `wire` as it went over the PUT.
//
// Two fingerprints of one write, because they are compared against different
// things and wfdir.TableState's invariant forbids folding them. `content` is
// what leg (b) of the drift guard compares (after de-aliasing the remote row),
// and `wire` is what the ROW now stores — so it, and only it, is what the next
// PUT may send as baseCodeSha256.
//
// Written after the PUT and NOT after the build, deliberately: what the draft
// holds is decided by the write, so a failed build must still advance this or
// the next push reads its own last attempt as somebody else's edit and refuses.
func recordDraftWrite(inst *wfdir.InstanceState, path, tableID, draftID, content, wire string) {
	mutateTable(inst, path, func(s *wfdir.TableState) {
		s.TableID = tableID
		s.DraftID = draftID
		s.DraftSHA256 = wfdir.HashString(content)
		// An empty `wire` means UNKNOWN, not "the row holds nothing", and it is
		// the CLONE's answer: a clone READ somebody's draft rather than writing
		// one, and the disk form it kept cannot be turned back into the exact
		// bytes that row stores (canonicalDisk is lossy — a positional ref and an
		// id ref both land on the same stem). Recording a fingerprint we cannot
		// vouch for would make the first push after a clone 409 against a
		// difference nobody made, so the field is left empty and that push writes
		// unconditionally, exactly as it does today.
		s.DraftWireSHA256 = ""
		if wire != "" {
			s.DraftWireSHA256 = wfdir.HashString(wire)
		}
	})
}

// liveHashes is where one pipeline folder's LIVE fingerprints live, which is
// not the same place for every folder — and routing that decision through one
// value is what keeps the fork from being spelled at every site that reads or
// writes one — the two legs of push's drift guard, status's, the three moments
// an agreement is recorded, and discard's re-point.
//
// A NAMED STACK keeps them in ronja.lock.json, committed. "The live table held
// these bytes when this folder last agreed with it" is a fact about the
// ENVIRONMENT, true for everybody, and committing it is the only thing that ever
// gave a fresh CI checkout something to compare against — which is why a push
// from CI has had to reach for --force.
//
// A LEGACY instances[] folder keeps them exactly where they were, in
// .ronja/state.json. That is the whole of "v1 stays v1": a folder nobody has
// named a stack in behaves, byte for byte, as it did before stacks existed.
//
// The two do not overlap: on a named stack the lock is the ONLY source, and a
// folder that migrates has its fingerprints carried across once, at the moment
// it is named (folder.adoptLiveHashes). A permanent read-through to the local
// baseline was the alternative and is worse — it would leave a committed file's
// answer quietly depending on a per-user one that a colleague does not have, so
// the guard would behave differently for the person who ran the migration than
// for everybody else.
type liveHashes struct {
	lock  *wfdir.Lock
	stack string
	inst  *wfdir.InstanceState
}

// live is this folder's live-fingerprint store for the stack it is acting on.
func (f *folder) live(inst *wfdir.InstanceState) liveHashes {
	return liveHashes{lock: f.Lock, stack: f.Stack, inst: inst}
}

func (h liveHashes) get(path string) string {
	if h.stack != "" {
		return h.lock.TableLive(h.stack, path)
	}
	return h.inst.TableStateFor(path).LiveSHA256
}

// getMeta and setMeta are the same fork for the DOCUMENTATION fingerprint —
// the third drift leg. They route exactly as the SQL pair does, and for the same
// reason: on a named stack the answer is a fact about the environment and lives
// in the committed lock, and a legacy instances[] folder keeps it where it kept
// everything before stacks existed.
func (h liveHashes) getMeta(path string) string {
	if h.stack != "" {
		return h.lock.TableMeta(h.stack, path)
	}
	return h.inst.TableStateFor(path).MetaSHA256
}

func (h liveHashes) setMeta(path, tableID, meta string) {
	if h.stack != "" {
		h.lock.SetTableMeta(h.stack, path, tableID, meta)
		return
	}
	mutateTable(h.inst, path, func(s *wfdir.TableState) {
		s.TableID = tableID
		s.MetaSHA256 = meta
	})
}

// docsSeen and setDocsSeen are the SIDECAR half — a table this folder documents
// but does not build, keyed by the sidecar's path rather than by a .sql file.
//
// The same lock/baseline fork, and it exists for the sidecar because a legacy
// folder has nowhere committed to put a recording: wfdir.Lock.SetTableDocsSeen
// refuses an empty stack name outright rather than writing `"stacks":{"":{…}}`
// into a committed file.
//
// ⚠️ A NIL inst IS THE ORDINARY CASE, not a defensive nicety, and both halves
// have to answer it. `.ronja/state.json` is gitignored, so `f.State.For(f.Key)`
// is nil on every fresh clone — and `pipeline status` passes that nil straight
// in, unlike push and publish which go through pipelineBaseline. Both siblings
// already guard it (wfdir.InstanceState.TableStateFor answers the zero value,
// wfdir.Lock.TableDocsSeen answers three empty strings); these two were the pair
// that did not, and the read runs inside a status worker goroutine, so the miss
// was a raw process panic rather than an error anybody could act on.
//
// Empty strings are the right answer for a folder that has never synced here:
// "no recording" disarms the drift guard, which is exactly what a fresh clone
// means. The write is a no-op for the same reason the lock refuses an empty
// stack — there is no baseline in memory to record into, and inventing one here
// would write a file that `status` promises never to write.
func (h liveHashes) docsSeen(path string) (tableID, meta, declared string) {
	if h.stack != "" {
		return h.lock.TableDocsSeen(h.stack, path)
	}
	if h.inst == nil {
		return "", "", ""
	}
	entry := h.inst.TableDocs[path]
	return entry.TableID, entry.MetaSHA256, entry.DeclaredSHA256
}

func (h liveHashes) setDocsSeen(path, tableID, meta, declared string) {
	if h.stack != "" {
		h.lock.SetTableDocsSeen(h.stack, path, tableID, meta, declared)
		return
	}
	if h.inst == nil {
		return
	}
	if h.inst.TableDocs == nil {
		h.inst.TableDocs = map[string]wfdir.TableDocsState{}
	}
	// A sidecar rebound to a DIFFERENT table drops the old row's fingerprint
	// with it, by the invariant on wfdir.LockTableDocs: a hash is only ever
	// compared against the row it was taken from.
	entry := h.inst.TableDocs[path]
	if entry.TableID != tableID {
		entry = wfdir.TableDocsState{}
	}
	entry.TableID, entry.MetaSHA256, entry.DeclaredSHA256 = tableID, meta, declared
	h.inst.TableDocs[path] = entry
}

func (h liveHashes) set(path, tableID, liveCode string) {
	if h.stack != "" {
		h.lock.SetTableLive(h.stack, path, tableID, wfdir.HashString(liveCode))
		return
	}
	mutateTable(h.inst, path, func(s *wfdir.TableState) {
		s.TableID = tableID
		s.LiveSHA256 = wfdir.HashString(liveCode)
	})
}

// recordLiveAgreement records the LIVE row's canonical SQL as the one this
// folder agrees with. The three moments that is true: a clone, the fork of a
// fresh draft, and a publish that just committed onto it.
//
// Never called with a draft's SQL. Live is the row leg (a) compares, and a draft
// hash recorded here would refuse every subsequent push as drift on a table that
// had not moved — the bug this split exists to fix.
//
// ⚠️ It writes the fingerprint to wherever liveHashes says, but the TABLE ID
// still goes to the local baseline in every case: the baseline has to say which
// row it describes (see wfdir.TableState.TableID), and that is a statement about
// this checkout's own records, not about the environment.
func recordLiveAgreement(live liveHashes, path, tableID, liveCode string) {
	mutateTable(live.inst, path, func(s *wfdir.TableState) { s.TableID = tableID })
	live.set(path, tableID, liveCode)
}

// recordMetaAgreement records the LIVE row's DOCUMENTATION as the one this
// folder agrees with — the third leg's counterpart to recordLiveAgreement.
//
// Called only where that is true: a live row this command just READ through
// GetTable (the only response that carries the column catalog), and a publish
// that just committed onto one. An empty meta CLEARS the recording, which is how
// a caller says "I can no longer honestly claim to have seen this row's prose"
// rather than leaving a fingerprint of a state that has since moved.
func recordMetaAgreement(live liveHashes, path, tableID, meta string) {
	mutateTable(live.inst, path, func(s *wfdir.TableState) { s.TableID = tableID })
	live.setMeta(path, tableID, meta)
}

// recordSynced records a COMPLETE push of one file: written to the draft, built
// AND documented. This is the "last sync" every local diff is measured against,
// so it advances only on success — a bare push after a failed build, or after a
// documentation write that was refused, has to still see the file as changed and
// try again.
func recordSynced(inst *wfdir.InstanceState, path, content string) {
	inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
}

// recordDiscarded is the baseline after a draft was thrown away: no draft, and
// the file's content baseline re-pointed at the LIVE table, which is now the
// only row it is synced with.
//
// Re-pointing rather than leaving the old value is what makes discard's own
// promise true — "push again to open fresh drafts". A baseline still holding the
// discarded draft's bytes would read the local file as unchanged, and the next
// push would report "Up to date" with no draft on the server at all.
//
// With no live baseline recorded (an old state file, or a clone that could not
// canonicalize the live row) the ENTRY GOES: no baseline means every local file
// reads as changed, which re-stages it just the same and is the honest answer.
func recordDiscarded(live liveHashes, path, tableID string) {
	if sha := live.get(path); sha == "" {
		delete(live.inst.Files, path)
	} else {
		live.inst.Files[path] = wfdir.FileState{SHA256: sha}
	}
	recordDraftPointer(live.inst, path, tableID, "")
}

// prunePhantoms drops baseline entries for files that are neither on disk nor
// named in the manifest binding, returning what it dropped.
//
// The pair is the point. A file gone from disk but STILL bound is reported and
// left alone (there is no delete verb here), so its baseline has to survive. One
// that has left the binding too is a file this folder no longer manages at all,
// and keeping its entry means `status` reports a deletion for ever and a push
// repeats the note on every run — with no way to clear it short of deleting the
// whole baseline.
func prunePhantoms(inst *wfdir.InstanceState, binding wfdir.Binding, onDisk map[string]string) []string {
	if inst == nil {
		return nil
	}
	recorded := map[string]bool{}
	for path := range inst.Files {
		recorded[path] = true
	}
	for path := range inst.Tables {
		recorded[path] = true
	}
	var pruned []string
	for path := range recorded {
		if _, bound := binding.Tables[path]; bound {
			continue
		}
		if _, present := onDisk[path]; present {
			continue
		}
		delete(inst.Files, path)
		delete(inst.Tables, path)
		pruned = append(pruned, path)
	}
	sort.Strings(pruned)
	return pruned
}

// tablePaths is the manifest binding read backwards: table id → the file that
// builds it.
//
// Push needs it to turn a file's `{{ ref }}` markers into edges between FILES,
// and status needs it to spot a bound id the feature no longer holds. Refs to
// ids outside the folder simply do not appear here, which is correct: they are
// ordinary inputs (a foundation table, a colleague's derived table), not edges.
func tablePaths(binding wfdir.Binding) map[string]string {
	out := make(map[string]string, len(binding.Tables))
	for path, id := range binding.Tables {
		if id != "" {
			out[id] = path
		}
	}
	return out
}

// topoOrder orders the paths in `want` so that a file is pushed AFTER every file
// in the folder it reads from.
//
// It matters because a sync materializes: a downstream table built before its
// upstream has landed reads the upstream's PREVIOUS data, so the confidence
// report describes a state that never existed. (Chaining a downstream draft onto
// an upstream DRAFT is a follow-up — this only orders the pushes.)
//
// The graph is built over the whole folder rather than over `want` alone, so a
// cycle is caught wherever it is: a cycle the server would refuse at build time
// with a message about one table is refused here naming the loop. `want` is then
// read out of that order, which keeps a narrowed push (`pipeline push a.sql
// b.sql`) in the same relative order a full one would use.
//
// Ties break on path, so a push is reproducible request-for-request.
// The edges come from pipelineCodec.folderUpstreams rather than from
// DeriveInputModels, so a ref spelled as a SIBLING'S STEM is an edge too. It has
// to be: on a first push the stem is the only spelling available — the id does
// not exist yet — so a graph built from ids alone would see no edges at all and
// order the folder alphabetically, which is the exact failure the paragraph above
// describes.
func topoOrder(c pipelineCodec, files map[string]string, want []string) ([]string, error) {
	// upstream[path] = the folder files it reads from.
	upstream := make(map[string][]string, len(files))
	for _, path := range sortedPaths(files) {
		var deps []string
		for _, dep := range c.folderUpstreams(files[path]) {
			if dep == path {
				continue
			}
			if _, present := files[dep]; present {
				deps = append(deps, dep)
			}
		}
		sort.Strings(deps)
		upstream[path] = deps
	}

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(files))
	var order []string
	var stack []string

	var visit func(path string) error
	visit = func(path string) error {
		switch state[path] {
		case done:
			return nil
		case onStack:
			// Name the loop, from where it re-entered to the top of the stack.
			start := 0
			for i, p := range stack {
				if p == path {
					start = i
					break
				}
			}
			loop := append(append([]string{}, stack[start:]...), path)
			return fmt.Errorf("these files reference each other in a loop, so there is no order that builds them: %s.\n  Break the cycle in the SQL — a derived table cannot read from something that reads from it",
				strings.Join(loop, " -> "))
		}
		state[path] = onStack
		stack = append(stack, path)
		for _, dep := range upstream[path] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[path] = done
		order = append(order, path)
		return nil
	}
	for _, path := range sortedPaths(files) {
		if err := visit(path); err != nil {
			return nil, err
		}
	}

	wanted := make(map[string]bool, len(want))
	for _, path := range want {
		wanted[path] = true
	}
	out := make([]string, 0, len(want))
	for _, path := range order {
		if wanted[path] {
			out = append(out, path)
		}
	}
	return out, nil
}

// resolveArgPaths turns command-line path arguments into folder-relative paths,
// refusing anything outside the syncable set.
//
// Arguments are resolved against the WORKING DIRECTORY, not the folder root:
// `pipeline push staging/orders.sql` typed from a subdirectory means the file
// the shell would have completed, and the commands run from anywhere inside the
// folder.
func resolveArgPaths(root string, args []string, files map[string]string) ([]string, error) {
	var out []string
	for _, arg := range args {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", arg, err)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return nil, fmt.Errorf("resolve %s against %s: %w", arg, root, err)
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "../") || rel == ".." {
			return nil, fmt.Errorf("%s is outside this pipeline folder (%s)", arg, root)
		}
		if !pipelineSyncable(rel) {
			return nil, fmt.Errorf("%s is not a .sql file — a pipeline folder syncs SQL and ignores everything else", arg)
		}
		if _, ok := files[rel]; !ok {
			return nil, fmt.Errorf("%s is not in this pipeline folder", arg)
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// describeVerdict renders a build verdict for a human, since the wire values are
// terse and two of them mean things their names do not say out loud.
func describeVerdict(verdict string) string {
	switch verdict {
	case api.BuildVerdictOK:
		return "built"
	case api.BuildVerdictOKPartial:
		return "built, with incomplete data (the build skipped corrupt source files)"
	case api.BuildVerdictFailedStale:
		return "FAILED — the table is still serving its previous data"
	case api.BuildVerdictFailed:
		return "FAILED"
	case api.BuildVerdictBuilding:
		return "still building"
	case api.BuildVerdictPending:
		return "queued"
	case api.BuildVerdictInvalidated:
		return "invalidated (an upstream changed)"
	case "":
		return "unknown"
	default:
		return verdict
	}
}

// describeRowCount renders a materialized row count, where -1 is the server's
// "not available" and printing it as a number would report a table that was
// never built as one that came back with minus one row.
func describeRowCount(n int64) string {
	if n < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d", n)
}

// sampleQueryTimeout bounds the confidence report's sample read.
//
// Shorter than the query default on purpose: the samples are garnish, and a
// LIMIT 5 off a table that has just finished building is either quick or not
// worth waiting two minutes for while the author watches a push that has already
// succeeded.
const sampleQueryTimeout = 45 * time.Second

// maxSampledRowCount is the size past which a push stops reading sample rows.
//
// A sample is not a cheap read. `SELECT * … LIMIT 5` goes through the same
// duckdb endpoint every other query does — an LLM routing call, and on anything
// large an AWS Batch job — and what it buys is five rows of garnish under a
// report whose verdict, schema delta and row counts are already complete. On a
// table of this size that is real money and up to 45 seconds of a person
// watching a push that has already succeeded.
//
// Measured against the DRAFT row count the confidence report already carries, so
// the guard costs no extra request. A count of -1 ("not available") is NOT
// treated as large: it is the answer for a table whose stats have not landed,
// which is exactly the small fresh table a sample is most useful for.
const maxSampledRowCount = 1_000_000

// sampleRows reads up to five rows out of a freshly built draft, as CSV.
//
// Only ever called AFTER a successful sync: a draft that has been checked out
// and never synced has no partitions of its own, and the read fails with a
// no-data error that says nothing about anything.
//
// Every failure degrades to "" — no samples, no note here, no non-zero exit. The
// caller decides what to say. Nothing about a push's outcome depends on this
// working.
func sampleRows(ctx context.Context, client *api.Client, draftID string) (string, error) {
	out, err := client.Query(ctx, api.QueryInput{
		SQL:     fmt.Sprintf("SELECT * FROM {{ ref('%s') }} LIMIT 5", draftID),
		MaxRows: 5,
	}, sampleQueryTimeout)
	if err != nil {
		return "", err
	}
	// The trap this endpoint is known for: a failed query is HTTP 200 with the
	// failure in a field.
	if out.Failed() {
		return "", fmt.Errorf("%s", out.Error)
	}
	return strings.TrimRight(out.Result, "\n"), nil
}

// printSample renders the CSV sample under a report, indented.
//
// ⚠️ These are five rows of the customer's own data on STDOUT, and stdout is
// wherever the caller pointed it — a terminal, a build log, an agent transcript.
// That is a deliberate choice rather than an oversight, on two grounds: the
// machine-readable path already omits it (pipelineFileResult.Sample is
// `json:"-"`, so `--json` — the documented CI and agent mode — never carries a
// row), and suppressing it on a non-TTY would make the human report say
// different things depending on whether it was piped, which is the surprise this
// loop's output rules exist to avoid. A caller who must not log rows uses
// --json, which is what they were already using.
func printSample(out *os.File, csv string) {
	if csv == "" {
		return
	}
	for _, line := range strings.Split(csv, "\n") {
		fmt.Fprintf(out, "      %s\n", line)
	}
}
