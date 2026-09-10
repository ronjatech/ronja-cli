package commands

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline status` answers the three questions you have before pushing:
// what have I changed, what state are the tables in over there, and has anything
// moved underneath me since I last synced.
//
// It mutates nothing — not even a checkout — so it is safe to run at any point,
// including against a feature you only have read access to.
func newPipelineStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local changes, table health and drift",
		Long: `Show local changes, table health and drift.

Four things, for the instance this folder is bound to:

  LOCAL    .sql files added, modified or deleted since the last sync
  NEW      files that are not bound to a table yet — a push would create them
  HEALTH   each bound table's build state, and whether it has a failure recorded
  DRIFT    tables whose SQL, and metrics whose definition, changed on the server
           since the last sync — chat and the web builder edit the same rows, so
           this is a real collision

Every metrics/*.json file gets a block of its own: the metric it defines and its
trust badge (unvetted, verified or retired), whether the file defines something
the metric does not hold yet, any draft you have staged and not published, and a
drift line when the definition moved on the server — which on a VERIFIED metric
adds that --force alone will not overwrite it. A file with no metric behind it
says so instead: a push is what creates one.

When you have a draft open, drift is reported for BOTH rows: your draft (the one
a push writes to) and the live table underneath it, which a colleague can commit
to while your draft sits there.

Read-only: no draft is created and nothing is written. Run it from anywhere
inside the folder.

With --json, one object carrying all of the above.

The exit code answers one question — has anything moved on the server? — which
makes this a CI gate that needs no parsing. Non-zero for drift, whether it is a
table's SQL, a table's documentation or a metric's definition that moved. And
equally non-zero for a table or a metric whose state could not be READ: an
unreachable instance, an expired token, a binding that no longer resolves, a
metrics/*.json file that does not parse or whose recipe.source names nothing
this folder can resolve, or one bound to a row that turns out to be a table
rather than a metric. "I could not look" is not the same answer as "nothing
moved", and a gate that conflated them would go green on a revoked credential.

Zero for everything local, and for a folder with nothing to compare against yet:
a .sql or metrics/*.json file you have only changed on disk, a file with no table
or metric behind it, and — in a folder that names no stacks — a fresh clone whose
.ronja/ baseline was correctly never committed. Run a push to record one.

A folder that names stacks is different, and a gate should expect it: the SQL
each table last held, and the definition each metric last held, are in the
committed ronja.lock.json, so a fresh clone still compares against the live rows
and can exit non-zero. Only your own open draft reads as unknown there, and the
report says so.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance: a signed-out caller can still be
			// told what changed locally, which is most of the value and costs
			// nothing. The remote half degrades to "not checked" with a reason.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			root, err := folderRootHere(wfdir.PipelineKind)
			if err != nil {
				return err
			}
			report, err := pipelineStatusReportAt(cmd.Context(), root, resolved)
			if err != nil {
				return err
			}

			// The report is emitted FIRST and the verdict decides only the exit
			// code: a caller reading --json gets its payload whatever the answer,
			// exactly as it does from a push that failed.
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printPipelineStatus(report)
			}
			return pipelineStatusVerdict(report)
		},
	}
	return cmd
}

// pipelineStatusReportAt computes the report for ONE pipeline folder at an
// explicit root, and prints nothing.
//
// Hoisted out of the RunE closure above so `ronja sync` can ask the same
// question of every folder in a tree. The split is between COMPUTING the answer
// and DECIDING what to do with it: this function returns the report, and the
// caller owns rendering and the exit code — which is why the tree command can
// apply its own verdict (see folderVerdict) without the per-folder command's
// behaviour changing at all.
func pipelineStatusReportAt(ctx context.Context, root string, resolved *config.Resolved) (*pipelineStatusReport, error) {
	f, err := openFolderForStatusAt(ctx, root, resolved, wfdir.PipelineKind)
	if err != nil {
		return nil, err
	}

	// Reported, never refused — see folder.noteAliases.
	f.noteAliases()

	report := &pipelineStatusReport{
		Root:      f.Root,
		URL:       resolved.URL,
		Title:     f.Manifest.Title,
		Stack:     f.declaredStack(),
		Bound:     f.Bound,
		Ambiguous: f.BindingErr != nil,
		FeatureID: f.Binding.FeatureID,
		Tables:    len(f.Binding.Tables),
	}

	enumeration, err := enumerateFolder(f.Root, wfdir.PipelineKind)
	if err != nil {
		return nil, err
	}
	baseline := f.State.For(f.Key)
	// In memory only — status writes nothing. It exists so that removing a
	// file's entry from ronja.json makes the phantom disappear from THIS
	// report rather than only from the one after the next push.
	prunePhantoms(baseline, f.Binding, enumeration.Files)
	report.Local = wfdir.DiffHashes(enumeration.Files, baseline.Hashes())
	report.Skipped = enumeration.Skipped
	report.WillCreate = []string{}
	for path := range enumeration.Files {
		if f.Binding.Tables[path] == "" {
			report.WillCreate = append(report.WillCreate, path)
		}
	}
	sort.Strings(report.WillCreate)

	// The stems come from the enumeration, whose KEYS are the folder's
	// files — see newPipelineCodec, which reads nothing else from the map.
	// A status that read every .sql file a second time to learn the same
	// thing would be paying for the alias layer on every folder.
	report.Remote = pipelineRemoteStatus(ctx, resolved, f, newPipelineCodec(f, enumeration.Files))
	return report, nil
}

// pipelineStatusReport is the --json shape, and the same struct the human
// renderer reads — so the two can never describe different things.
type pipelineStatusReport struct {
	Root  string `json:"root"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// Stack is the NAME of the environment this report is about, empty for a
	// folder still using the unnamed legacy "instances" shape. Reported because
	// a folder can name several and the answer to "which one am I looking at"
	// must not be inferred from the organization id.
	Stack string `json:"stack,omitempty"`
	Bound bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies — the signed-out case.
	// Distinct from Bound so a caller does not read "not bound" and conclude a
	// push would create everything again.
	Ambiguous bool   `json:"ambiguous,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
	// Tables is how many files this folder is BOUND to, which is not the same as
	// how many .sql files it holds — see WillCreate.
	Tables  int             `json:"tables"`
	Local   wfdir.Diff      `json:"local"`
	Skipped []wfdir.Skipped `json:"skipped,omitempty"`
	// WillCreate lists local .sql files with no table behind them yet.
	WillCreate []string              `json:"willCreate"`
	Remote     *pipelineRemoteReport `json:"remote"`
}

// pipelineRemoteReport is shaped so "we did not look" and "we looked and it is
// fine" are different states, the same way the workflow loop's is.
type pipelineRemoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Deliberately
	// NOT called "skipped": pipelineStatusReport.Skipped already means "a local
	// file that will never be synced", and one JSON document using one word for
	// two unrelated things is a trap for anything parsing it.
	NotCheckedReason string `json:"notCheckedReason,omitempty"`
	// Problem is a folder-level failure — the list read itself did not work.
	Problem string                `json:"problem,omitempty"`
	Tables  []pipelineTableReport `json:"tables,omitempty"`
	// Docs is one entry per DOCS SIDECAR — the tables this folder documents but
	// does not build. Absent for the folders that keep none.
	Docs []pipelineDocsStatus `json:"docs,omitempty"`
	// Metrics is one entry per METRIC FILE — the numbers this folder defines.
	// Absent for the folders that keep none.
	Metrics []pipelineMetricStatus `json:"metrics,omitempty"`
	Notes   []string               `json:"notes,omitempty"`
}

// pipelineMetricStatus is one metric file's remote state.
//
// The same two-question split pipelineDocsStatus makes, plus the two facts that
// only a metric has: whether a push would CREATE it (a metric file is the one
// carrier in this folder that can name a row which does not exist yet and is not
// a .sql file), and whether the row an overwrite would land on is VERIFIED.
type pipelineMetricStatus struct {
	Path     string `json:"path"`
	Alias    string `json:"alias,omitempty"`
	MetricID string `json:"metricID,omitempty"`
	SourceID string `json:"sourceID,omitempty"`
	Name     string `json:"name,omitempty"`
	// MetricStatus is unvetted | verified | retired, straight off the row. Read
	// here for one reason: `verified` is what makes forcing past drift cost a
	// second flag, and somebody deciding whether to force needs to see it before
	// they type the first one.
	MetricStatus string `json:"metricStatus,omitempty"`
	// DraftID is the caller's own staged draft, empty when there is none. It is
	// what `publish` acts on, and it is neither drift nor pending: it is work
	// this folder has done and not landed.
	DraftID string `json:"draftID,omitempty"`
	// Problem is why this file cannot be used as it stands — it does not parse,
	// its source is not a table this folder can name here, the row could not be
	// read, or the row is a table rather than a metric.
	Problem string `json:"problem,omitempty"`
	// WillCreate reports a file with no metric behind it yet: a push creates one.
	// Its own field rather than a drift verdict, because "nothing is there" is
	// not "something moved" and must not fail a gate asking about the server.
	WillCreate bool `json:"willCreate,omitempty"`
	// Drift is one of the drift* constants, measured on the LIVE metric's recipe.
	Drift string `json:"drift,omitempty"`
	// Pending reports that a push would write something: this folder has not
	// pushed this file's current content here.
	Pending bool `json:"pending,omitempty"`
}

// pipelineDocsStatus is one docs sidecar's remote state.
//
// Two questions, and they are different ones: Drift asks whether the ROW's
// documentation moved since this folder agreed with it (which a push refuses on),
// and Pending asks whether the FILE says something the row does not (which a push
// would send). A sidecar can legitimately be both, neither, or either.
type pipelineDocsStatus struct {
	Path    string `json:"path"`
	Alias   string `json:"alias,omitempty"`
	TableID string `json:"tableID,omitempty"`
	Name    string `json:"name,omitempty"`
	// Problem is why this file cannot be used as it stands — it does not parse,
	// its alias is not declared and bound here, or the table could not be read.
	Problem string `json:"problem,omitempty"`
	// Warning is something worth saying about a file that is nonetheless fine —
	// see tableDocsSidecar.Warning. Deliberately NOT part of the exit code:
	// pipelineStatusVerdict scores Problem and Drift, and a placeholder sidecar
	// must not fail a CI gate that is asking whether the server moved.
	Warning string `json:"warning,omitempty"`
	// Drift is one of the drift* constants, measured on the live row's prose.
	Drift string `json:"drift,omitempty"`
	// Pending reports that a push would write something: the file declares a
	// description or a column note the row does not hold. One-directional, by the
	// three-state rule — prose on a column this file does not name is not a
	// difference.
	Pending bool `json:"pending,omitempty"`
}

func (r *pipelineRemoteReport) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Per-table drift verdicts. A vocabulary rather than a bool, because "we could
// not tell" is a third answer and reporting it as "no drift" is the one mistake
// that gets someone's work overwritten.
const (
	driftNone       = "none"
	driftChanged    = "changed"
	driftNoBaseline = "no_baseline"
	driftUnreadable = "unreadable"
)

// reasonNothingBound is the one NotCheckedReason that is not a failure to look.
// The folder is bound here, it names a feature, and that feature simply holds
// nothing this folder has pushed yet — a conclusion, where every other reason in
// that field is the remote half being unreachable, unreadable or ambiguous.
//
// A const rather than a literal because the exit code turns on the difference,
// and a gate that read one of these as the other would go green on an instance
// it never reached.
const reasonNothingBound = "no tables exist on this instance yet — the first push will create them"

// pipelineTableReport is one bound file's remote state.
type pipelineTableReport struct {
	Path    string `json:"path"`
	TableID string `json:"tableID"`
	Name    string `json:"name,omitempty"`
	// Status is the LIST's build status, and HasError its cheap "something is
	// recorded against this row" flag. Both come free with the one list call.
	Status   string `json:"status,omitempty"`
	HasError bool   `json:"hasError,omitempty"`
	// Problem is set when this binding cannot be used as it stands — the id is
	// not in the feature any more, or its SQL could not be read.
	Problem string `json:"problem,omitempty"`
	// Drift is one of the drift* constants above, for the row a push would WRITE
	// TO: your draft when you have one, else the live table.
	Drift string `json:"drift,omitempty"`
	// ComparedAgainst names the row Drift was measured against.
	ComparedAgainst string `json:"comparedAgainst,omitempty"`
	// LiveDrift is the OTHER leg — the live table against the fingerprint taken
	// from it — and is present only when a draft exists, because that is the only
	// time it says something Drift does not.
	//
	// It exists because push refuses on either leg and then says "run status to
	// see the detail": a status that could only ever report the draft answered a
	// question the caller had not asked, and the live leg (a colleague committed
	// while your draft sat open) was invisible on the one surface built to show
	// it.
	LiveDrift string `json:"liveDrift,omitempty"`
	// DocsDrift is the THIRD leg — the live table's DOCUMENTATION against the
	// fingerprint taken from it — and it is present only for a file that carries
	// a `-- @table` header. A folder that documents nothing makes no claim about
	// anybody's prose, so there is nothing to report and this stays empty, which
	// is what keeps a status on a folder written before this feature unchanged
	// down to the line.
	DocsDrift string               `json:"docsDrift,omitempty"`
	Draft     *pipelineDraftReport `json:"draft,omitempty"`
	// URL is the table's frontend page as the SERVER stamped it, rendered by the
	// human report only — see pushResult.URL for why links stay out of --json.
	URL string `json:"-"`
}

type pipelineDraftReport struct {
	ID string `json:"id"`
	// Verdict is the draft's own build verdict, which is the single most useful
	// thing here: a draft that failed to build cannot be published, and nothing
	// else in this report would say so.
	Verdict   string `json:"verdict,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// pipelineStatusConcurrency bounds the per-table reads.
//
// A folder of thirty tables is an ordinary size, and issuing sixty requests one
// after another makes `status` take long enough that people stop running it.
// Bounded rather than unbounded because the other failure mode is a folder of
// two hundred tables opening two hundred connections at once.
const pipelineStatusConcurrency = 8

// fillConcurrently runs fill over each index in `indices`, on at most
// pipelineStatusConcurrency goroutines, and returns when every one has finished.
//
// ONE COPY, because there were two byte-identical ones — this pool and the docs
// sidecars' — differing only in the call inside the loop. The invariant that
// makes the pool safe is the same in both and is stated once here rather than
// twice: EACH CALL WRITES ONLY ITS OWN SLOT of a pre-sized results slice, so
// there is nothing to lock and the results keep the order of the input.
//
// `fill` must not itself close over a shared mutable value. That is the whole of
// the contract, and it is what a second copy of this loop would be free to
// forget.
func fillConcurrently(indices []int, fill func(i int)) {
	workers := pipelineStatusConcurrency
	if len(indices) < workers {
		workers = len(indices)
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				fill(i)
			}
		}()
	}
	for _, i := range indices {
		work <- i
	}
	close(work)
	wg.Wait()
}

// pipelineRemoteStatus gathers everything server-side in TWO TIERS, degrading
// rather than aborting.
//
// Tier one is a SINGLE list of the feature's tables, which carries every bound
// row's kind, status, shadow markers and hasError flag. That is the whole health
// leg, for any number of tables, at the cost of one request — and it is what a
// naive implementation would have paid a GET per table for.
//
// Tier two is the code, which the list deliberately omits, so drift costs a read
// per table: the caller's own draft, then the live row, and — when a draft is
// open — that draft by its own id, which is the only way to get its build
// verdict. Two requests per bound table, three where there is a draft to judge,
// run concurrently.
//
// Drift is remote-vs-BASELINE, never remote-vs-local: the question is whether
// anything moved underneath us, and a file the author edited locally is exactly
// what a push is for.
func pipelineRemoteStatus(ctx context.Context, resolved *config.Resolved, f *folder, codec pipelineCodec) *pipelineRemoteReport {
	out := &pipelineRemoteReport{}
	// The organization lookup failed, so this folder's binding cannot be trusted
	// to be the right one. Reported ahead of the ambiguity below because it is
	// the CAUSE of it whenever both are set — and it is a "could not look", so
	// the exit-code verdict below is non-zero, exactly as for an unreadable
	// table.
	if reason := f.orgNotCheckedReason(); reason != "" {
		out.NotCheckedReason = reason
		return out
	}
	// Bound to several organizations here, and signed out, so which binding
	// applies is genuinely unknown. Reported rather than guessed: picking one
	// would show another organization's tables as if they were this folder's.
	if f.BindingErr != nil {
		out.NotCheckedReason = fmt.Sprintf("%s on %s — sign in (`ronja login`), or pass --profile to say which one",
			f.BindingErr, resolved.URL)
		return out
	}
	if !f.Bound {
		out.NotCheckedReason = fmt.Sprintf("this folder is not bound to %s yet — the first push will create the binding", resolved.URL)
		return out
	}
	if resolved.Token == "" {
		out.NotCheckedReason = fmt.Sprintf("not signed in to %s — run `ronja login --url %s`", resolved.URL, resolved.URL)
		return out
	}
	if f.Binding.FeatureID == "" {
		out.NotCheckedReason = fmt.Sprintf("this folder names no feature — add \"featureID\" to the %s entry in %s",
			resolved.URL, wfdir.ManifestName)
		return out
	}
	// A metric is NOT in Binding.Tables — that map is keyed by .sql path, and a
	// metric's identity lives in the lock — so a folder defining only metrics has
	// an empty map and plenty to report. Taking the early return for it would
	// answer "no tables exist on this instance yet" about a folder whose metrics
	// are all deployed, and the tree gate reads that reason as a clean
	// never-deployed verdict.
	//
	// A read that FAILS is treated as no metric files: this is the "nothing is
	// bound" branch, and a folder whose metrics/ directory cannot be listed has a
	// problem the metric leg below reports per file rather than one that should
	// change what this branch says about its tables.
	metricsOnDisk, _ := readMetricFiles(f.Root)
	if len(f.Binding.Tables) == 0 && len(metricsOnDisk) == 0 {
		out.NotCheckedReason = reasonNothingBound
		return out
	}

	client := newClient(resolved.URL, resolved.Token)
	items, err := client.ListFeatureTables(ctx, f.Binding.FeatureID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			out.Checked = true
			out.Problem = fmt.Sprintf("binding broken: feature %s no longer exists on %s (or you lost access to it)",
				f.Binding.FeatureID, resolved.URL)
			return out
		}
		out.NotCheckedReason = fmt.Sprintf("could not list the tables of %s: %v", f.Binding.FeatureID, err)
		return out
	}
	out.Checked = true

	byID := make(map[string]*api.TableListItem, len(items))
	for _, item := range items {
		if item != nil {
			byID[item.ID] = item
		}
	}

	paths := make([]string, 0, len(f.Binding.Tables))
	for path := range f.Binding.Tables {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// The local files, read for their SPELLING and nothing else: a sibling's
	// table id has two legitimate disk forms, and which one a file uses is what
	// the remote code has to be de-aliased back into. See pipelineCodec.disk.
	//
	// Read HERE rather than in the caller, so a status whose remote half stops at
	// one of the early returns above pays for no second walk of the folder. A read
	// that fails degrades to no spellings — every declared alias still resolves,
	// and a folder using sibling stems reads as drifted, which the rest of this
	// report is about to explain properly.
	local, _, readErr := readPipelineFiles(f.Root)
	if readErr != nil {
		local = nil
	}
	baseline := f.State.For(f.Key)
	known := baseline.Hashes()
	reports := make([]pipelineTableReport, len(paths))
	var needCode []int

	for i, path := range paths {
		id := f.Binding.Tables[path]
		report := pipelineTableReport{Path: path, TableID: id}
		item, present := byID[id]
		if !present {
			// A bound id the feature no longer holds. Reported per table rather
			// than aborting the whole block: the other twenty-nine bindings are
			// still worth answering, and this one is the answer.
			report.Problem = "binding broken: this table is not in the feature any more (deleted, moved, or you lost access to it)"
			reports[i] = report
			continue
		}
		report.Name = item.Name
		report.Status = item.Status
		report.HasError = item.HasError
		needCode = append(needCode, i)
		reports[i] = report
	}

	// Tier two, concurrently. See fillConcurrently: each call writes only its own
	// slot, so no lock is needed for the results themselves.
	live := f.live(baseline)
	fillConcurrently(needCode, func(i int) {
		fillTableDrift(ctx, client, codec, local[reports[i].Path], &reports[i],
			baseline.TableStateFor(reports[i].Path), live.get(reports[i].Path), live.getMeta(reports[i].Path))
	})

	out.Tables = reports
	out.Docs = pipelineDocsStatuses(ctx, client, f, live)
	// The METRIC files, after the docs sidecars for the reason the push pass runs
	// last: a report reads as "here is what I build, here is what I document,
	// here is what I define". The codec is the one the caller built from this
	// folder's own files, so a metric whose source names a sibling .sql file
	// resolves to the same row a `{{ ref }}` to it would.
	out.Metrics = pipelineMetricStatuses(ctx, client, f, codec, live)
	if baseline == nil {
		// ⚠️ Say WHICH half is unknown. On a stack folder the committed lock still
		// carries the live fingerprints, so the live-drift line above is a real
		// answer even here — this note used to say "every table reads as unknown",
		// which read as "nothing was checked" and is the one reading a CI operator
		// must not take from a report that just exited non-zero on real drift.
		// Only the DRAFT half is per-user, and only that half is missing.
		if f.Stack != "" {
			out.note("no local baseline — your own draft reads as unknown (this folder was never synced from here); the live comparison comes from " + wfdir.LockName)
		} else {
			out.note("no local baseline — every table reads as unknown (this folder was never synced from here)")
		}
	}
	for _, path := range paths {
		// `known` is the BASELINE's hashes, not the folder's contents: a missing
		// entry means this file was never synced from here, which is why its drift
		// cannot be judged. Nothing here says anything about what is on disk.
		if _, hasBaseline := known[path]; !hasBaseline && baseline != nil {
			out.note("%s is bound to a table but has no baseline — its drift cannot be judged until the next push", path)
		}
	}
	return out
}

// fillTableDrift reads one bound table's SQL and compares it against the
// baseline fingerprint TAKEN FROM THAT ROW.
//
// BOTH legs of push's drift guard, not one, and that is the point of the extra
// read: push refuses when EITHER the live table or your own draft has moved, and
// then tells the caller to run this command for the detail. A status that
// resolved the draft and reported only that could not show the live leg at all —
// a colleague committing while your draft sat open — so the sentence push prints
// was false exactly when it mattered.
//
// Which fingerprint follows from which row, and the pairing is the whole
// invariant (see wfdir.TableState): the live table against LiveSHA256, the draft
// against DraftSHA256 — and only when the recorded draft id IS the open one,
// since a fingerprint from a draft that has since been committed describes a row
// that no longer exists. A draft with no usable fingerprint falls back to the
// live SQL: a draft nobody has edited is byte-identical to the parent it forked
// from, so that comparison still catches somebody else's edit sitting in your
// own draft.
//
// An empty fingerprint is reported as its own answer (driftNoBaseline) rather
// than as drift.
func fillTableDrift(ctx context.Context, client *api.Client, codec pipelineCodec, local string, report *pipelineTableReport, state wfdir.TableState, liveSHA, metaSHA string) {
	draft, err := client.GetTableDraft(ctx, report.TableID)
	if err != nil {
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not check for your open draft: %v", err)
		return
	}

	// The LIVE row is read whether or not a draft exists: it is leg (a), and it
	// is also the fallback the draft leg compares against.
	live, err := client.GetTable(ctx, report.TableID)
	if err != nil {
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not read %s: %v", report.TableID, err)
		return
	}
	// Canonicalized and then de-aliased, in that order — see
	// pipelineCodec.canonicalDisk. Both fingerprints this is compared against are
	// hashes of the bytes on disk, which are in name form.
	liveCode, liveUnresolved := codec.canonicalDisk(local, live)
	liveDrift, liveProblem := compareDrift(liveCode, liveUnresolved, liveSHA)
	// The third leg, and ONLY for a file that opts in. The header is what makes
	// this folder claim anything about the table's prose; without one there is
	// nothing to compare and nothing a push would overwrite.
	if docs, _ := tabledocs.ParseHeader(local); !docs.Empty() {
		switch {
		case metaSHA == "":
			report.DocsDrift = driftNoBaseline
		case metaOf(live) == metaSHA:
			report.DocsDrift = driftNone
		default:
			report.DocsDrift = driftChanged
		}
	}

	if draft == nil {
		report.URL = live.URL
		report.ComparedAgainst = live.ID
		report.Drift = liveDrift
		report.Problem = liveProblem
		return
	}

	// The draft is read by its OWN id rather than taken from the /draft route,
	// which projects a plain table view with no build verdict on it.
	staged, err := client.GetTable(ctx, draft.ID)
	if err != nil {
		report.LiveDrift = liveDrift
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not read your draft %s: %v", draft.ID, err)
		return
	}
	report.Draft = &pipelineDraftReport{
		ID:        draft.ID,
		Verdict:   staged.BuildVerdict,
		UpdatedAt: formatTime(&draft.UpdatedAt),
	}
	report.ComparedAgainst = staged.ID
	report.LiveDrift = liveDrift

	against := state.DraftSHA256
	if state.DraftID != draft.ID {
		against = ""
	}
	if against == "" && !liveUnresolved {
		against = wfdir.HashString(liveCode)
	}
	draftCode, draftUnresolved := codec.canonicalDisk(local, staged)
	report.Drift, report.Problem = compareDrift(draftCode, draftUnresolved, against)
	if report.Problem == "" && liveProblem != "" {
		report.Problem = liveProblem
	}
}

// compareDrift is one row's verdict: its canonical SQL against the fingerprint
// taken from that same row.
//
// A row that cannot be canonicalized is driftUnreadable with a reason, never
// agreement — the stored SQL and the stored input list disagree with each other,
// and answering "no drift" there is the silent case the guard exists for.
func compareDrift(code string, unresolved bool, baselineHash string) (string, string) {
	switch {
	case unresolved:
		return driftUnreadable, "this table's stored SQL carries a positional ref that cannot be resolved, so its content cannot be compared — open it in the web builder and replace the ref with the table's id"
	case baselineHash == "":
		return driftNoBaseline, ""
	case wfdir.HashString(code) == baselineHash:
		return driftNone, ""
	default:
		return driftChanged, ""
	}
}

func printPipelineStatus(r *pipelineStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	if r.Stack != "" {
		// Printed next to the instance because it answers the same question a
		// step further in: which of this folder's environments is being reported.
		fmt.Fprintf(out, "  Stack:      %s\n", r.Stack)
	}
	switch {
	case r.Bound && r.FeatureID != "":
		fmt.Fprintf(out, "  Feature:    %s\n", r.FeatureID)
	case r.Ambiguous:
		// "not bound" would flatly contradict the Remote line below, which is
		// about to say the folder is bound to several organizations here.
		fmt.Fprintf(out, "  Feature:    bound here, but to several organizations — pick one with --profile\n")
	case r.Bound:
		fmt.Fprintf(out, "  Feature:    none named — a push refuses until one is\n")
	default:
		fmt.Fprintf(out, "  Feature:    not bound to this instance\n")
	}
	fmt.Fprintf(out, "  Tables:     %d bound\n", r.Tables)

	fmt.Fprintf(out, "\n  Local changes\n")
	if !r.Local.Dirty() {
		fmt.Fprintf(out, "    clean (%d file(s) match the last sync)\n", r.Local.Unchanged)
	} else {
		printPathList(out, "new", r.Local.Added)
		printPathList(out, "modified", r.Local.Modified)
		printPathList(out, "deleted", r.Local.Deleted)
		fmt.Fprintf(out, "    %d unchanged\n", r.Local.Unchanged)
	}
	for _, path := range r.WillCreate {
		fmt.Fprintf(out, "    %-9s %s\n", "creates", path)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "    skipped  %s — %s\n", s.Path, s.Reason)
	}

	fmt.Fprintf(out, "\n  Remote\n")
	switch {
	case r.Remote.NotCheckedReason != "":
		fmt.Fprintf(out, "    not checked: %s\n", r.Remote.NotCheckedReason)
	case r.Remote.Problem != "":
		fmt.Fprintf(out, "    %s\n", r.Remote.Problem)
	default:
		for _, t := range r.Remote.Tables {
			printPipelineTableStatus(out, t)
		}
		for _, d := range r.Remote.Docs {
			printPipelineDocsStatus(out, d)
		}
		for _, m := range r.Remote.Metrics {
			printPipelineMetricStatus(out, m)
		}
	}
	for _, note := range r.Remote.Notes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
}

func printPipelineTableStatus(out *os.File, t pipelineTableReport) {
	fmt.Fprintf(out, "    %s\n", t.Path)
	if t.Problem != "" {
		fmt.Fprintf(out, "      %s\n", t.Problem)
		return
	}
	fmt.Fprintf(out, "      table:  %s (%s)\n", t.TableID, t.Status)
	if t.HasError {
		// The list's flag says only that SOMETHING is recorded; it never says
		// what, and never whether the table is nonetheless serving good data.
		fmt.Fprintf(out, "      health: a build failure is recorded — `ronja api /api/v2/feature/model/%s --jq .lastBuildError` for the reason\n", t.TableID)
	}
	if t.Draft != nil {
		fmt.Fprintf(out, "      draft:  %s (%s)\n", t.Draft.ID, describeVerdict(t.Draft.Verdict))
	}
	switch t.Drift {
	case driftChanged:
		fmt.Fprintf(out, "      drift:  the SQL of %s changed since your last sync — a push would overwrite it\n", t.ComparedAgainst)
	case driftNoBaseline:
		fmt.Fprintf(out, "      drift:  nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      drift:  not checked\n")
	}
	// The live leg, printed only when it is a second answer — i.e. when a draft
	// exists. A colleague's commit landing under an open draft is the case push
	// refuses on and this line is the only place it is visible.
	switch t.LiveDrift {
	case driftChanged:
		fmt.Fprintf(out, "      live:   the SQL of %s changed since your last sync — committing your draft would revert it\n", t.TableID)
	case driftNoBaseline:
		fmt.Fprintf(out, "      live:   nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      live:   not checked\n")
	}
	// The documentation leg, printed only for a file that carries a header —
	// an empty DocsDrift is "this folder claims nothing about this table's
	// prose", which is not an answer worth a line.
	switch t.DocsDrift {
	case driftChanged:
		fmt.Fprintf(out, "      docs:   the documentation of %s changed since your last sync — a push carrying this file's header would overwrite it\n", t.TableID)
	case driftNoBaseline:
		fmt.Fprintf(out, "      docs:   nothing to compare against yet — the next push records a baseline\n")
	}
	printResourceURL(out, statusKeyWidth, t.URL)
}

// printPipelineDocsStatus renders one docs sidecar.
func printPipelineDocsStatus(out *os.File, d pipelineDocsStatus) {
	fmt.Fprintf(out, "    %s\n", d.Path)
	if d.Problem != "" {
		fmt.Fprintf(out, "      %s\n", d.Problem)
		return
	}
	name := d.TableID
	if d.Name != "" {
		name = fmt.Sprintf("%s (%s)", d.TableID, d.Name)
	}
	fmt.Fprintf(out, "      table:  %s\n", name)
	switch {
	case d.Warning != "":
		// A file that declares nothing gets the note INSTEAD of a docs line, not
		// as well as one: "the table already says what this file says" is true
		// of a file that says nothing, and useless.
		fmt.Fprintf(out, "      note:   %s\n", d.Warning)
	case d.Pending:
		fmt.Fprintf(out, "      docs:   this file says something the table does not — a push would write it\n")
	default:
		fmt.Fprintf(out, "      docs:   the table already says what this file says\n")
	}
	switch d.Drift {
	case driftChanged:
		fmt.Fprintf(out, "      drift:  the documentation of %s changed since your last sync — a push would overwrite it\n", d.TableID)
	case driftNoBaseline:
		fmt.Fprintf(out, "      drift:  nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      drift:  not checked\n")
	}
}

// printPipelineMetricStatus renders one metric file.
func printPipelineMetricStatus(out *os.File, m pipelineMetricStatus) {
	fmt.Fprintf(out, "    %s\n", m.Path)
	if m.Problem != "" {
		fmt.Fprintf(out, "      %s\n", m.Problem)
		return
	}
	if m.WillCreate {
		// No metric line and no drift line: there is no row to name and nothing
		// to compare against. Said as what a push would DO, which is the only
		// fact this file has here.
		fmt.Fprintf(out, "      metric: none yet — a push creates one called %q reading %s\n", m.Alias, m.SourceID)
		return
	}
	name := m.MetricID
	if m.Name != "" {
		name = fmt.Sprintf("%s (%s)", m.MetricID, m.Name)
	}
	fmt.Fprintf(out, "      metric: %s\n", name)
	if m.MetricStatus != "" {
		fmt.Fprintf(out, "      status: %s\n", m.MetricStatus)
	}
	if m.Pending {
		fmt.Fprintf(out, "      local:  this file defines something the metric does not — a push would stage it\n")
	} else {
		fmt.Fprintf(out, "      local:  the metric already holds what this file defines\n")
	}
	if m.DraftID != "" {
		fmt.Fprintf(out, "      draft:  %s staged and not published — `ronja pipeline publish` commits it\n", m.DraftID)
	}
	switch m.Drift {
	case driftChanged:
		// The one drift line in this command that names a consequence rather than
		// only a fact: forcing past it overwrites the organization's definition of
		// a number, and on a VERIFIED metric it costs a second flag.
		fmt.Fprintf(out, "      drift:  the definition of %s changed since your last sync — a push would overwrite it\n", m.MetricID)
		if m.MetricStatus == api.MetricStatusVerified {
			fmt.Fprintf(out, "              this metric is VERIFIED, so --force alone will not overwrite it\n")
		}
	case driftNoBaseline:
		fmt.Fprintf(out, "      drift:  nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      drift:  not checked\n")
	}
}

// pipelineStatusVerdict turns the report into an exit code, answering ONE
// question: has anything moved on the server under this folder?
//
// The contract is `ronja db migrate status`'s — a CI gate that needs no parsing
// — and the reason it treats "could not check" as a failure is the same reason
// that command does: a status that exits zero is read as "nothing has moved",
// and a revoked token, an unreachable instance or a binding that no longer
// resolves says nothing of the kind. Exiting zero there would make the gate go
// green precisely when it stopped working.
//
// "Nothing to compare against YET" is the opposite case and exits zero. A folder
// cloned from git has no .ronja/ — it is correctly never committed — so every
// file reads as no-baseline, and a folder freshly bound to a feature has nothing
// on the server at all. Both are the normal way somebody joins a pipeline, which
// is why push's drift guard disarms for exactly the same state; failing here
// would mean a first checkout could not pass a gate until it had pushed.
//
// A LOCAL edit is not the gate either, and never has been: drift is measured
// remote-against-baseline, and a file the author changed is what a push is for.
func pipelineStatusVerdict(r *pipelineStatusReport) error {
	if r == nil || r.Remote == nil {
		return nil
	}
	if reason := r.Remote.NotCheckedReason; reason != "" {
		if reason == reasonNothingBound {
			return nil
		}
		return fmt.Errorf("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if r.Remote.Problem != "" {
		return fmt.Errorf("%s", r.Remote.Problem)
	}

	var drifted, unchecked driftBucket
	for _, t := range r.Remote.Tables {
		switch {
		case t.Drift == driftChanged || t.LiveDrift == driftChanged || t.DocsDrift == driftChanged:
			drifted.tables = append(drifted.tables, t.Path)
		// driftNoBaseline is deliberately absent: it is "not compared yet", not
		// "could not be compared", and the two rows above are what tell them
		// apart — an unreadable row is a row that was read and made no sense.
		case t.Problem != "", t.Drift == "", t.Drift == driftUnreadable,
			t.LiveDrift == driftUnreadable:
			unchecked.tables = append(unchecked.tables, t.Path)
		}
	}
	// The docs sidecars, on the same discipline: a row whose prose moved is
	// drift, and a file that could not be resolved or read at all is UNCHECKED —
	// never clean, because "I could not look" is not "nothing is there".
	//
	// A PENDING sidecar is deliberately NOT here. Drift asks whether the server
	// moved under this folder; a file that says something the table does not is
	// the ordinary state of a folder with work to push, exactly like a modified
	// .sql file, and that is what a push is for.
	//
	// Counted as TABLES in the summary below, which is what they are about: a
	// sidecar's drift is the prose on a table having moved.
	for _, d := range r.Remote.Docs {
		switch {
		case d.Drift == driftChanged:
			drifted.tables = append(drifted.tables, d.Path)
		case d.Problem != "", d.Drift == "", d.Drift == driftUnreadable:
			unchecked.tables = append(unchecked.tables, d.Path)
		}
	}
	// The METRIC files, on the same discipline. A file whose LIVE metric's
	// definition moved is drift; one that could not be resolved or read is
	// UNCHECKED — never clean, because "I could not look" is not "nothing is
	// there".
	//
	// A file with no metric yet (WillCreate) is deliberately NOT here, and
	// neither is a PENDING one: both are the ordinary state of a folder with
	// work to push, which is what a push is for. This gate asks whether the
	// server moved under the folder.
	for _, m := range r.Remote.Metrics {
		switch {
		case m.WillCreate:
		case m.Drift == driftChanged:
			drifted.metrics = append(drifted.metrics, m.Path)
		case m.Problem != "", m.Drift == "", m.Drift == driftUnreadable:
			unchecked.metrics = append(unchecked.metrics, m.Path)
		}
	}

	switch {
	case !drifted.empty() && !unchecked.empty():
		return fmt.Errorf("%s drifted on the server (%s), and %s could not be checked (%s) — see above",
			drifted.describe(), drifted.paths(), unchecked.describe(), unchecked.paths())
	case !drifted.empty():
		// The remedy is the whole answer for a table and not quite the whole
		// answer for a metric: pushOneMetric's destructive gate refuses --force
		// alone on a VERIFIED one, so a summary that named only --force would send
		// exactly those readers into a refusal. Said only where it can apply.
		force := "push --force overwrites"
		if len(drifted.metrics) > 0 {
			force = "push --force overwrites (a VERIFIED metric needs --force-verified-metric as well)"
		}
		return fmt.Errorf("%s changed on the server since your last sync: %s — %s, or pull the change back into your file",
			drifted.describe(), drifted.paths(), force)
	case !unchecked.empty():
		return fmt.Errorf("%s could not be checked for drift (%s) — see the reason above; this is not the same as no drift",
			unchecked.describe(), unchecked.paths())
	}
	return nil
}

// driftBucket is one side of the verdict — what moved, or what could not be
// looked at — with the two KINDS of file kept apart.
//
// Apart, because the summary quotes the paths it counted: a folder that defines
// metrics produced "1 table changed on the server (metrics/average_order.json)",
// where the sentence and its own parenthesis disagree and the reader goes
// looking for SQL that does not exist. A mixed run cannot be described honestly
// by either noun alone, which is why this counts rather than picks.
type driftBucket struct {
	// tables holds .sql paths AND docs sidecars, which are about a table.
	tables  []string
	metrics []string
}

func (b driftBucket) empty() bool { return len(b.tables)+len(b.metrics) == 0 }

// describe renders "2 tables and 1 metric" — the counts, in the order paths()
// lists them.
func (b driftBucket) describe() string {
	tables := fmt.Sprintf("%d %s", len(b.tables), plural(len(b.tables), "table"))
	metrics := fmt.Sprintf("%d %s", len(b.metrics), plural(len(b.metrics), "metric"))
	switch {
	case len(b.metrics) == 0:
		return tables
	case len(b.tables) == 0:
		return metrics
	default:
		return tables + " and " + metrics
	}
}

// paths lists every file in the bucket, tables first, so the order matches the
// order describe() names the two kinds in.
func (b driftBucket) paths() string {
	all := make([]string, 0, len(b.tables)+len(b.metrics))
	all = append(all, b.tables...)
	all = append(all, b.metrics...)
	return strings.Join(all, ", ")
}
