package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// flagSyncDir is the directory `ronja sync` walks. Its own var rather than a
// root flag: it means nothing to any other command, and the three folder loops
// already answer "which folder" by where you are standing.
var flagSyncDir string

// `ronja sync` asks a question about a TREE of folders rather than the one you
// are standing in: is everything committed under here still what the
// organization holds?
//
// That is the question a repository has and a folder does not. The three folder
// loops each answer it for themselves, and answering it forty times by hand is
// how a broken edge sits in a repository for a month — which is the failure this
// group exists for.
//
// `status` and `check` are READ-ONLY, and that is a promise with a test behind
// it (see TestSyncStatusWritesNothing and TestSyncCheckWritesNothing): every
// path under those two reads three files and makes GETs. Neither may reach
// wfdir.SaveState, which also writes a .gitignore — a command called `status`
// creating files in a customer's repository, in a git workflow whose whole point
// is a clean tree, is the exact wrong failure. ⚠️ It also rules out openFolder,
// which calls adoptStack and rewrites ronja.json; both open through
// openFolderForStatusAt.
//
// `apply` is the one that acts, and it opens through openFolderForStatusAt too —
// for a different reason, and one worth knowing: adoptStack's rewrite is
// something apply REFUSES (syncReasonManifestRewrite), so opening through the
// acting opener would perform the migration and then report declining to.
func newSyncCmd() *cobra.Command {
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Check — and deploy — every Ronja folder in a tree at once",
		Long: `Check, and deploy, every Ronja folder in a tree at once.

Walks down from a directory for every ronja.json — workflow, data app,
automation and pipeline folders alike — and answers one question about all of
them:

  ronja sync status                is the committed content what the
                                   organization holds?
  ronja sync check                 does every reference that content makes
                                   still resolve?
  ronja sync apply                 push and publish all of it

status and check are read-only: nothing is written, not even a checkout, so they
are safe to run on a repository you only have read access to. apply writes, and
refuses by default to create anything that does not already exist.`,
	}
	sync.AddCommand(newSyncStatusCmd(), newSyncCheckCmd(), newSyncApplyCmd())
	sync.PersistentFlags().StringVar(&flagSyncDir, "dir", ".",
		"directory to walk for folders (default: the working directory)")
	addStackFlag(sync)
	return sync
}

// Exit codes for the tree commands.
//
// ⚠️ The one place this CLI EXTENDS rather than inherits, and the extension is
// the point. Every other command has a two-valued answer, so `run` mapping every
// error to 1 was the whole contract; a tree gate has three, and collapsing them
// makes the difference unreadable to the caller that most needs it. "Something
// drifted" is actionable — push, or pull the change back. "I could not tell" is
// a dead credential, a folder that would not open, or a --stack nothing here
// recognises, and a job that treated it as drift would push over work it never
// read.
const (
	syncExitClean   = 0
	syncExitDrifted = 1
	syncExitUnknown = 2
)

// newSyncStatusCmd is `ronja sync status`.
func newSyncStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report drift for every folder beneath a directory",
		Long: `Report drift for every folder beneath a directory.

Finds every ronja.json under --dir and runs that folder's own status
computation against the stack you name, then reports one verdict per folder and
one for the tree:

  clean      the organization holds what this folder committed
  drifted    something changed on the server since the last sync
  unknown    it could not be checked — and that is NOT the same answer

Read-only: no draft is created and nothing is written.

The exit code is the tree's verdict, so this is a CI gate that needs no
parsing:

  0   every folder was checked, and every one is clean
  1   checked, and something drifted
  2   could not tell — a folder was skipped, a manifest would not load, the
      credential is dead, or the walk found no folders at all

"Could not tell" WINS over "drifted": the strongest true statement about a tree
where one folder drifted and another could not be read is that not everything
was verified. An empty walk is exit 2 for the same reason — a renamed directory
must not sail through the gate reporting success.

With --json, the same answer as one object, carrying a stable "verdict" field:
exit codes do not survive a wrapper script.

Folders are checked against ONE --stack. A folder that does not declare it, or
declares it pointing at another organization, is reported as not checked rather
than aborting the run — a repository legitimately holds folders belonging to
several organizations — and it is never counted as clean, so a mistyped stack
name cannot report a whole repository healthy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance, for the reason the three
			// per-folder status commands are not: a signed-out caller still gets
			// the walk and a named reason per folder. The verdict is `unknown`
			// either way, which is exactly what a dead credential should produce.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				// ⚠️ UNKNOWN, not the default 1. Every failure here — no profile,
				// a profile whose URL disagrees with --url, an ambiguous one, a
				// URL that will not parse — is the one class where NOTHING was
				// verified, and `run` scored it 1: the code this command's own
				// help defines as "checked, and something drifted". A job reading
				// that pushes.
				return withExitCode(syncExitUnknown, err)
			}
			report, err := syncStatusTree(cmd.Context(), flagSyncDir, resolved)
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printSyncStatus(report)
			}
			return report.exitError()
		},
	}
	return cmd
}

// syncStatusReport is the --json shape, and the same struct the human renderer
// reads — so the two can never describe different things.
type syncStatusReport struct {
	// Dir is the directory that was walked, absolute.
	Dir string `json:"dir"`
	URL string `json:"url"`
	// Stack is the name that was asked for, empty when no --stack was given.
	Stack string `json:"stack,omitempty"`
	// Verdict is the tree's answer — clean, drifted or unknown — and the field
	// this command's contract lives in. It carries the same word as the exit
	// code because exit codes do not survive a wrapper script.
	Verdict string             `json:"verdict"`
	Folders []syncFolderReport `json:"folders"`
	Summary syncSummary        `json:"summary"`
}

// syncFolderReport is one folder's line.
type syncFolderReport struct {
	// Path is relative to Dir, which is what a human recognises; Root is
	// absolute, which is what a script acts on. Both, because a report that
	// carried only the relative one would be unusable from any other directory.
	Path string `json:"path"`
	Root string `json:"root"`
	// Kind is the manifest's own kind name, empty for a folder ruled out before
	// its kind could be established.
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Verdict string `json:"verdict"`
	// Reason is a sync* constant and is set ONLY for a folder that was not
	// checked at all. It is separate from Detail because it is the machine
	// contract: Detail is prose and may be reworded, Reason may not.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type syncSummary struct {
	Folders int `json:"folders"`
	Clean   int `json:"clean"`
	Drifted int `json:"drifted"`
	// Unknown counts every folder that could not be vouched for, whether it was
	// skipped before the walk reached the network or checked and found
	// unreadable. One number, because the exit code makes one claim.
	Unknown int `json:"unknown"`
}

// exitError turns the tree verdict into the process exit code.
//
// Clean returns nil. Anything else returns an error carrying its own code —
// `run` reads it — so the reason is still printed on stderr like every other
// failure, and a caller that only reads $? gets the three-valued answer.
func (r *syncStatusReport) exitError() error {
	switch r.Verdict {
	case verdictDrifted:
		// "out of step with the organization" rather than "drifted on the
		// server", which this line used to say and which is now false for half
		// the folders that reach it: a folder that has never been DEPLOYED is
		// drifted too, and "on the server" asserts a comparison against
		// something that is not on the server at all. The group's own help
		// already asks the question in these words ("is everything here still in
		// step?"), and the per-folder detail lines say which of the two it is.
		return withExitCode(syncExitDrifted, fmt.Errorf("%d of %d %s %s out of step with the organization — see above",
			r.Summary.Drifted, r.Summary.Folders, plural(r.Summary.Folders, "folder"),
			agree(r.Summary.Drifted, "is", "are")))
	case verdictUnknown:
		if r.Summary.Folders == 0 {
			return withExitCode(syncExitUnknown, fmt.Errorf("no %s found under %s — nothing was checked, which is not the same answer as nothing being wrong",
				wfdir.ManifestName, r.Dir))
		}
		return withExitCode(syncExitUnknown, fmt.Errorf("%d of %d %s could not be checked — see above; this is not the same answer as no drift",
			r.Summary.Unknown, r.Summary.Folders, plural(r.Summary.Folders, "folder")))
	}
	return nil
}

// syncStatusTree walks the tree and computes one report.
//
// SERIAL, deliberately. api.Client is field-only and safe to share, so
// concurrency would work — but it would be stacked on top of the root threading
// this command needed in the first place, and on a stack resolution shared
// across folders, which is precisely where the subtle bugs would be. Add it
// when somebody reports a slow repository, with this landed and tested.
func syncStatusTree(ctx context.Context, dir string, resolved *config.Resolved) (*syncStatusReport, error) {
	// A bad --stack is the one thing this command does NOT degrade around, and
	// it follows openFolderForStatus's rule exactly. Degrading is for an
	// environment that will not answer; a name that cannot be a stack name at
	// all is a wrong ARGUMENT, and reporting every folder in the repository as
	// `stack_absent` would describe a problem the reader does not have.
	if flagStack != "" {
		if err := wfdir.ValidStackName(flagStack); err != nil {
			return nil, err
		}
	}
	found, err := discoverFolders(dir)
	if err != nil {
		return nil, err
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}

	// The organization is resolved ONCE, before the loop, and the failure is
	// swallowed exactly as openFolderForStatus swallows it.
	//
	// Once rather than per folder because it is not only a saved request: the
	// stack resolution below compares the folder's declared organization against
	// this one, and a lookup that happened lazily inside the first folder's open
	// would leave folder one compared on URL alone and folder two compared on
	// both. Which folders were checked would then depend on the walk order.
	if resolved.Token != "" && resolved.TenantID == "" {
		_ = ensureTenant(ctx, resolved)
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}

	// Folders is an empty LIST rather than null for an empty walk: a caller
	// doing `.folders | length` on the one answer that matters most — nothing
	// was found — must not have to special-case a null.
	report := &syncStatusReport{
		Dir:     base,
		URL:     resolved.URL,
		Stack:   flagStack,
		Folders: []syncFolderReport{},
	}
	verdicts := make([]string, 0, len(found))
	for _, folder := range found {
		line := syncFolderReport{Path: folder.Rel, Root: folder.Root, Kind: folder.Kind.Name}
		if folder.notChecked() {
			line.Verdict, line.Reason, line.Detail = verdictUnknown, folder.Reason, folder.Detail
			report.Folders = append(report.Folders, line)
			verdicts = append(verdicts, line.Verdict)
			continue
		}
		line.Title = folder.Manifest.Title
		// The lock is read here rather than in the walk because only the stack
		// resolution needs it, and a folder ruled out above never pays for it.
		lock, lockErr := wfdir.LoadLock(folder.Root)
		if lockErr != nil {
			line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnreadable, lockErr.Error()
			report.Folders = append(report.Folders, line)
			verdicts = append(verdicts, line.Verdict)
			continue
		}
		if reason, detail := resolveStackForFolder(folder.Manifest, lock, key, flagStack); reason != "" {
			line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, detail
			report.Folders = append(report.Folders, line)
			verdicts = append(verdicts, line.Verdict)
			continue
		}
		verdict := statusVerdictOfFolder(ctx, folder, resolved)
		line.Verdict, line.Detail = verdict.Verdict, verdict.Detail
		report.Folders = append(report.Folders, line)
		verdicts = append(verdicts, line.Verdict)
	}

	report.Summary = summarize(report.Folders)
	report.Verdict = worstVerdict(verdicts)
	// An EMPTY WALK is unknown, never clean. Nothing was checked, and a renamed
	// or moved directory would otherwise pass the gate reporting success —
	// which is the one way a gate fails without anybody noticing.
	if len(found) == 0 {
		report.Verdict = verdictUnknown
	}
	return report, nil
}

// statusVerdictOfFolder runs one folder's own status computation and reduces it
// to a verdict.
//
// The three per-folder reports are computed by the SAME functions the three
// `status` commands run — pipelineStatusReportAt and friends — rather than by a
// tree-shaped reimplementation. That is the whole reason those bodies were
// hoisted out of their RunE closures: a second copy of "is this folder clean"
// would drift from the one people actually run, and it would drift silently.
func statusVerdictOfFolder(ctx context.Context, folder discoveredFolder, resolved *config.Resolved) folderVerdict {
	// How many files this folder's OWN loop would push, read before any request
	// because it is what decides whether "nothing is deployed here yet" is a
	// clean answer or a drifted one — see neverDeployedVerdict, which exists
	// because that reason used to be read as clean unconditionally and reported
	// an undeployed table or workflow as a healthy repository.
	//
	// First, so a folder whose files cannot even be walked costs no round trip.
	enumeration, err := wfdir.Enumerate(folder.Root, folder.Kind)
	if err != nil {
		return unknownVerdict("%v", err)
	}
	local := len(enumeration.Files)
	switch folder.Kind.Name {
	case wfdir.KindPipeline:
		report, err := pipelineStatusReportAt(ctx, folder.Root, resolved)
		if err != nil {
			return unknownVerdict("%v", err)
		}
		return verdictOfPipelineStatus(report, local,
			undeployedAgainstLock(folder.Root, resolved, enumeration.Files))
	case wfdir.KindDataApp:
		report, err := dataAppStatusReportAt(ctx, folder.Root, resolved)
		if err != nil {
			return unknownVerdict("%v", err)
		}
		return verdictOfAppStatus(report, local)
	case wfdir.KindAutomation:
		report, err := automationStatusReportAt(ctx, folder.Root, resolved)
		if err != nil {
			return unknownVerdict("%v", err)
		}
		// No `undeployed` argument, and the asymmetry with the pipeline arm above
		// is the point: that one exists because a pipeline's fresh-clone answer
		// lives in the committed lock's fingerprints. An automation folder has no
		// local baseline at all, so its ordinary comparison IS the committed-file
		// one and needs no second leg to survive a clone.
		return verdictOfAutomationStatus(report, local)
	default:
		report, err := workflowStatusReportAt(ctx, folder.Root, resolved)
		if err != nil {
			return unknownVerdict("%v", err)
		}
		return verdictOfWorkflowStatus(report, local)
	}
}

// undeployedAgainstLock names the .sql files whose COMMITTED content differs
// from the SQL the COMMITTED lock says was last deployed.
//
// ⚠️ This is the one leg that works on a FRESH CLONE, and it is the reason
// ronja.lock.json holds a pipeline's live fingerprint at all. The lock file's own
// doc says why: that hash is "a fact about the ENVIRONMENT, not about a person…
// the first thing a fresh CI checkout has ever had to compare against". A clone
// has no .ronja/, so `Local` is empty and every other local signal is silent —
// while this one still answers the question the whole phase exists for: does
// this repository hold SQL that was never deployed?
//
// THE HASHES ARE COMPARABLE, and that is a property of the loop rather than a
// coincidence — pipeline_push.go states it where the fingerprint is written:
// "`content` is the DISK form, and the baseline has to be disk form… the two are
// the same fingerprint by construction, [because] what we sent is this file with
// its aliases substituted, so de-aliasing the row would return exactly these
// bytes." Every writer honours it: a create records the disk bytes directly, and
// an update or publish records codec.canonicalDisk of the row — canonicalized
// and then de-aliased back into THIS folder's spelling. A row whose SQL cannot
// be canonicalized records NO fingerprint at all, so it simply has nothing here
// to compare and is skipped rather than guessed at.
//
// It is computed HERE, in the tree command, from the lock the folder already
// carries — not added to pipelineStatusReport, which is `ronja pipeline status`'s
// published --json shape. A new key there would be a scripting-visible change to
// a shipped command for the sake of a verdict only this command computes.
//
// A LEGACY folder (no named stack) returns nothing, and that is honest rather
// than lazy: its live fingerprints live in .ronja/state.json, which is per-user
// and git-ignored, so a fresh clone of one has nothing committed to compare
// against. `Local` is the only answer such a folder has, and it needs a baseline.
//
// `files` is the enumeration's path → disk-hash map.
//
// ⚠️ KNOWN COST, accepted deliberately: this re-opens the manifest, lock and
// state that pipelineStatusReportAt has already read one call earlier, so a
// pipeline folder pays a second local open. It is three file reads against a
// verdict that must be right, and threading the opened folder out of the report
// path would change a shipped function's signature to save them. Worth doing if
// a large repository ever makes it measurable; not worth doing blind.
func undeployedAgainstLock(root string, resolved *config.Resolved, files map[string]string) []string {
	// Local only: no request, and no promise about anything but the manifest,
	// the lock and the resolved stack name.
	f, err := openFolderLocallyAt(root, resolved, wfdir.PipelineKind)
	if err != nil || f.Stack == "" {
		return nil
	}
	var out []string
	for path, hash := range files {
		// An empty fingerprint is "never recorded", not "empty file": the folder
		// has never agreed with a live row for this path, which the report's own
		// no-baseline / WillCreate legs already answer.
		if recorded := f.Lock.TableLive(f.Stack, path); recorded != "" && recorded != hash {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func summarize(folders []syncFolderReport) syncSummary {
	summary := syncSummary{Folders: len(folders)}
	for _, f := range folders {
		switch f.Verdict {
		case verdictClean:
			summary.Clean++
		case verdictDrifted:
			summary.Drifted++
		default:
			summary.Unknown++
		}
	}
	return summary
}

// printSyncStatus renders the human report.
//
// The folders are LISTED before their verdicts, and that is not decoration: a
// downward walk can pick up a vendored or example folder that nobody meant to
// deploy, and the list is where a reader sees a surprise member.
//
// ⚠️ Nothing here streams. The tree completes every round trip before this runs,
// so the list is not progress — a comment here used to say it was printed "before
// the network work starts", which would have somebody looking for output that
// cannot arrive while a slow repository is being read.
func printSyncStatus(r *syncStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Dir)
	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	if r.Stack != "" {
		fmt.Fprintf(out, "  Stack:      %s\n", r.Stack)
	}

	if len(r.Folders) == 0 {
		fmt.Fprintf(out, "\n  No %s found under this directory.\n", wfdir.ManifestName)
		return
	}

	fmt.Fprintf(out, "\n  %d %s found\n", len(r.Folders), plural(len(r.Folders), "folder"))
	for _, f := range r.Folders {
		if f.Kind != "" {
			fmt.Fprintf(out, "    %s (%s)\n", f.Path, f.Kind)
			continue
		}
		fmt.Fprintf(out, "    %s\n", f.Path)
	}

	fmt.Fprintf(out, "\n  Status\n")
	for _, f := range r.Folders {
		fmt.Fprintf(out, "    %-8s %s\n", f.Verdict, f.Path)
		if f.Detail != "" {
			fmt.Fprintf(out, "             %s\n", f.Detail)
		}
	}

	fmt.Fprintf(out, "\n  %s — %d clean, %d drifted, %d not checked\n",
		r.Verdict, r.Summary.Clean, r.Summary.Drifted, r.Summary.Unknown)
}
