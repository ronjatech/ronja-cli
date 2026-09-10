package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// flagSyncAllowDropped is `sync check`'s one hatch, and it exists because
// `ronja wf push` has the same one for the same finding.
//
// A `{{ secret }}` marker naming a secret nobody can reach yet is the ordinary
// state of a repository mid-credential-setup, and without a hatch the only way
// to get a green tree there is not to run the command — which is how a gate
// stops being run at all. It is a FLAG rather than a TTY or --json test for the
// reason pushOptions gives: the exit code has to mean the same thing wherever it
// is read, and a flag nobody types by accident is how this CLI already spells "I
// know, and I mean it".
//
// ⚠️ It changes the SCORE and never the REPORT. The dropped edge stays in the
// human output and in --json, verdict and all; only its contribution to the exit
// code goes away. A flag that hid the evidence would be the failure mode here.
var flagSyncAllowDropped bool

// newSyncCheckCmd is `ronja sync check`.
func newSyncCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify every reference every folder beneath a directory makes",
		Long: `Verify every reference every folder beneath a directory makes.

Finds every ronja.json under --dir, works out what each folder's committed
content POINTS AT — the tables it reads, the secrets it injects, the agents and
workflows it runs, the rows a data app grants itself — and asks whether each of
those still resolves for the stack you name.

This is the question a repository has and a folder does not: a table renamed in
one feature breaks a marker in another folder that nobody opens for a month.

Each reference gets one of four answers:

  ok           it resolved, and this credential can read it
  unresolved   nothing here could turn it into an id at all — an alias this
               stack binds nothing to, a sibling file that is not there, an
               ambiguous name. Decided locally, and definite
  unreachable  a well-formed id the instance will not show us. Deleted,
               trashed, another organization's, or simply not yours — the read
               gate answers the same way for all four, on purpose, so that
               existence does not leak
  not_checked  nobody asked, with a reason

Read-only: nothing is written, no draft is created, and no folder is pushed.

The exit code is the tree's verdict, so this is a CI gate that needs no
parsing:

  0   every reference was checked, and every one resolved
  1   a definite finding — something is unresolved or unreachable
  2   could not tell — a folder was skipped, the credential is dead, a
      reference cannot be verified at all, or the walk found no folders

"Could not tell" WINS over a finding, exactly as it does for ` + "`sync status`" + `.
An ` + "`unreachable`" + ` scores 1 rather than 2 because the check RAN and the server
gave a definite negative; not_checked is the case where nothing was asked.

Two things it cannot verify, and says so rather than passing them:

  codex     no scoped token can read one by id — the route declares no access
            scope, so every scoped token is refused. (A workflow's codex
            markers ARE checked: the server resolves those itself.)
  mailbox   there is no read-one-mailbox route to ask, only a listing.

And two limits worth knowing. A data app's RUNTIME queries are not statically
enumerable, so what is checked is what the app DECLARES plus the ids written
literally in its source. A workflow's references are answered by the instance's
own validate endpoint, which needs a feature — a stack that has never been
pushed has none, and is reported as not checked rather than as broken.

With --json, the same answer as one object with every reference in it, carrying
a stable "verdict" field: exit codes do not survive a wrapper script.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance, for the reason `sync status` is
			// not: a signed-out caller still gets the walk, and still gets the one
			// leg that needs no network at all — every alias this folder declares
			// and this stack binds nothing to. Everything else is not_checked,
			// which is exactly what a dead credential should produce.
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
			report, err := syncCheckTree(cmd.Context(), flagSyncDir, resolved)
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printSyncCheck(report)
			}
			return report.exitError()
		},
	}
	cmd.Flags().BoolVar(&flagSyncAllowDropped, "allow-dropped-bindings", false,
		"exit zero even though a workflow save would drop a secret binding it cannot reach")
	return cmd
}

// syncCheckReport is the --json shape, and the same struct the human renderer
// reads — so the two can never describe different things.
type syncCheckReport struct {
	Dir   string `json:"dir"`
	URL   string `json:"url"`
	Stack string `json:"stack,omitempty"`
	// Verdict is the tree's answer — clean, broken or unknown — and the field
	// this command's contract lives in. Same word as the exit code, because exit
	// codes do not survive a wrapper script.
	Verdict string                  `json:"verdict"`
	Folders []syncCheckFolderReport `json:"folders"`
	Summary syncCheckSummary        `json:"summary"`
	// DroppedBindingsAllowed reports that --allow-dropped-bindings was given, so
	// the dropped edges below were accepted rather than counted. A field of its
	// own — the same shape pushResult uses — because without it the report would
	// tell a run that deliberately accepted them to go and fix them.
	DroppedBindingsAllowed bool `json:"droppedBindingsAllowed,omitempty"`
}

// syncCheckFolderReport is one folder's answer plus every reference it makes.
type syncCheckFolderReport struct {
	Path    string `json:"path"`
	Root    string `json:"root"`
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Verdict string `json:"verdict"`
	// Reason is a sync* constant, set ONLY when the folder as a whole was not
	// checked — a stack it does not declare, a manifest that will not load, a
	// stack with no feature to validate against. Machine contract; Detail is
	// prose.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Edges is empty rather than null for a folder that makes no references, so
	// a caller doing `.edges | length` never has to special-case one.
	Edges []syncEdgeReport `json:"edges"`
	// Findings are definite problems that are not about ONE reference: a data
	// app that reaches for a row it never granted itself, a workflow the
	// instance refused for a shape rather than for a binding. They score the
	// same as a broken edge.
	Findings []string `json:"findings,omitempty"`
	// Warnings are true, worth saying, and no reason to fail: a grant nothing
	// uses, an advisory from the instance.
	Warnings []string `json:"warnings,omitempty"`
}

type syncCheckSummary struct {
	Folders int `json:"folders"`
	Clean   int `json:"clean"`
	Broken  int `json:"broken"`
	Unknown int `json:"unknown"`
	// The reference counts, which are what a reader acts on: "3 unreachable" is
	// the number that sends somebody looking, and the folder count does not
	// carry it.
	Edges       int `json:"edges"`
	OK          int `json:"ok"`
	Unresolved  int `json:"unresolved"`
	Unreachable int `json:"unreachable"`
	NotChecked  int `json:"notChecked"`
	// Findings is how many folder-level findings there are — a data app reaching
	// for a row it never granted itself, a workflow the instance refused for a
	// shape rather than for a binding.
	//
	// ⚠️ They score exactly like a broken edge (see checkVerdictOf), so they have
	// to be COUNTED like one. They were not: a used-but-undeclared table exited 1
	// saying "(0 unresolved, 0 unreachable)", which reads as a bug in the tool
	// rather than a problem in the repository.
	Findings int `json:"findings,omitempty"`
	// DroppedBindings is how many of the unreachable ones are the server's soft
	// tier — the subset --allow-dropped-bindings accepts. Counted whether or not
	// the flag was given, so the flag can be NAMED in the refusal: a hatch nobody
	// can find is not a hatch.
	DroppedBindings int `json:"droppedBindings,omitempty"`
}

// exitError turns the tree verdict into the process exit code.
//
// ⚠️ `unreachable` scores 1 AND NOT 2, and that is the one judgement call in
// this command. It looks like a "could not tell" — the server declined to show
// us the row, and we genuinely cannot say whether it was deleted or merely
// invisible — but the distinction the exit codes draw is not about how much we
// know, it is about whether anything was ASKED. An unreachable edge was asked
// about and the instance gave a definite negative: under this credential, that
// reference does not resolve, and a run through it will fail. A `not_checked` is
// the other thing entirely — nobody looked, so nothing is known in either
// direction, and a job must not conclude anything from it.
func (r *syncCheckReport) exitError() error {
	switch r.Verdict {
	case verdictBroken:
		// The NOUN agrees with the total ("2 of 5 folders") and the VERB with the
		// subject ("1 of 1 folder makes"), which are two different counts — see
		// agree.
		return withExitCode(syncExitDrifted, fmt.Errorf("%d of %d %s %s references that do not resolve (%s) — see above%s",
			r.Summary.Broken, r.Summary.Folders, plural(r.Summary.Folders, "folder"),
			agree(r.Summary.Broken, "makes", "make"),
			r.Summary.countedBreakage(), r.droppedHatch()))
	case verdictUnknown:
		if r.Summary.Folders == 0 {
			return withExitCode(syncExitUnknown, fmt.Errorf("no %s found under %s — nothing was checked, which is not the same answer as nothing being wrong",
				wfdir.ManifestName, r.Dir))
		}
		return withExitCode(syncExitUnknown, fmt.Errorf("%d of %d %s could not be fully checked — see above; this is not the same answer as every reference resolving",
			r.Summary.Unknown, r.Summary.Folders, plural(r.Summary.Folders, "folder")))
	}
	return nil
}

// countedBreakage lists exactly what scored, so the number in the sentence and
// the numbers beside it cannot disagree.
//
// Findings are in it because they score: checkVerdictOf adds them to the broken
// count, and a sentence that named only the edge counts read as "1 folder makes
// references that do not resolve (0 unresolved, 0 unreachable)" — a self-
// contradicting line, on the one output a CI job prints.
func (s syncCheckSummary) countedBreakage() string {
	parts := []string{
		fmt.Sprintf("%d unresolved", s.Unresolved),
		fmt.Sprintf("%d unreachable", s.Unreachable),
	}
	if s.Findings > 0 {
		parts = append(parts, fmt.Sprintf("%d other %s", s.Findings, plural(s.Findings, "finding")))
	}
	return strings.Join(parts, ", ")
}

// droppedHatch names --allow-dropped-bindings when a dropped binding is part of
// what pushed this run to exit 1, and says nothing otherwise.
//
// `ronja wf push` names the same flag in the same place and for the same reason:
// a hatch nobody can find is not a hatch, and the reader is looking at an exit
// code rather than at this command's help. It is deliberately phrased as
// "accepts those" rather than "exits zero" — a tree with other broken edges
// stays non-zero, and promising otherwise would send somebody to run the command
// again for nothing.
func (r *syncCheckReport) droppedHatch() string {
	if r.Summary.DroppedBindings == 0 || r.DroppedBindingsAllowed {
		return ""
	}
	return fmt.Sprintf(". %d of them %s a secret binding a workflow save would DROP; `ronja sync check --allow-dropped-bindings` accepts those, for the \"I will bind it later\" flow",
		r.Summary.DroppedBindings, agree(r.Summary.DroppedBindings, "is", "are"))
}

// syncCheckTree walks the tree and verifies every folder's references.
//
// The shape mirrors syncStatusTree exactly — one walk, one organization lookup
// before the loop, one stack resolution per folder, all serial — so the two tree
// commands cannot disagree about which folders a repository contains or which
// of them a --stack applies to. The one addition is the edgeChecker, which is
// built ONCE for the whole run: that is what makes the (kind, id) dedupe span
// folders, which is where nearly all of the saving is.
func syncCheckTree(ctx context.Context, dir string, resolved *config.Resolved) (*syncCheckReport, error) {
	// A bad --stack is a wrong ARGUMENT, not something to degrade around. Same
	// rule, and same reasoning, as syncStatusTree's.
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

	// Resolved ONCE, before the loop, and swallowed — see syncStatusTree, where
	// the reason is that a lazy lookup would leave folder one compared on URL
	// alone and folder two compared on both.
	if resolved.Token != "" && resolved.TenantID == "" {
		_ = ensureTenant(ctx, resolved)
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}

	var client *api.Client
	if resolved.Token != "" {
		client = newClient(resolved.URL, resolved.Token)
	}
	checker := newEdgeChecker(client)

	report := &syncCheckReport{
		Dir:                    base,
		URL:                    resolved.URL,
		Stack:                  flagStack,
		Folders:                []syncCheckFolderReport{},
		DroppedBindingsAllowed: flagSyncAllowDropped,
	}
	verdicts := make([]string, 0, len(found))
	for _, folder := range found {
		line := syncCheckFolderReport{
			Path: folder.Rel, Root: folder.Root, Kind: folder.Kind.Name,
			Edges: []syncEdgeReport{},
		}
		switch {
		case folder.notChecked():
			line.Verdict, line.Reason, line.Detail = verdictUnknown, folder.Reason, folder.Detail
		default:
			line.Title = folder.Manifest.Title
			checkOneFolder(ctx, &line, folder, resolved, key, checker)
		}
		report.Folders = append(report.Folders, line)
		verdicts = append(verdicts, line.Verdict)
	}

	report.Summary = summarizeCheck(report.Folders)
	report.Verdict = worstVerdict(verdicts)
	// An EMPTY WALK is unknown, never clean — see syncStatusTree.
	if len(found) == 0 {
		report.Verdict = verdictUnknown
	}
	return report, nil
}

// checkOneFolder fills in one folder's line: its stack disposition, its edges,
// and the verdict they add up to.
func checkOneFolder(ctx context.Context, line *syncCheckFolderReport, folder discoveredFolder,
	resolved *config.Resolved, key wfdir.InstanceKey, checker *edgeChecker) {
	// The lock is read here rather than in the walk because only the stack
	// resolution needs it, and a folder ruled out earlier never pays for it.
	lock, lockErr := wfdir.LoadLock(folder.Root)
	if lockErr != nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnreadable, lockErr.Error()
		return
	}
	stackReason, stackDetail := resolveStackForFolder(folder.Manifest, lock, key, flagStack)
	// ⚠️ `stack_unverified` is the ONE not-checked reason this command does not
	// skip outright, and the split is worth spelling out because two review
	// findings collided here.
	//
	// It means: --stack X was given, this folder declares X, and the credential's
	// own organization could not be established — so nothing can confirm the two
	// are the same. Every REMOTE leg is then unsafe: an id lookup would ask OUR
	// organization about ANOTHER one's rows and report every one of them
	// `unreachable`, which is a confidently wrong answer.
	//
	// The DEPENDENCY leg is not. It asks whether the bind map under stack X
	// covers every alias this folder declares — a comparison of the committed
	// file against itself, with no organization in it anywhere. Skipping the
	// folder entirely would throw that away, and it is the one leg the docs
	// promise works signed out.
	//
	// So: the local leg runs, the remote legs do not, and the folder is
	// `not_checked` — never green either way.
	switch stackReason {
	case "":
	case syncReasonStackUnverified:
		checkFolderLocalOnly(ctx, line, folder, resolved, stackReason, stackDetail)
		return
	default:
		line.Verdict, line.Reason, line.Detail = verdictUnknown, stackReason, stackDetail
		return
	}

	// ⚠️ openFolderForStatusAt, NEVER openFolder. The acting opener calls
	// adoptStack, which WRITES ronja.json when a --stack names a legacy folder —
	// and this command is read-only with a test behind it. A tree command that
	// rewrote a committed file in forty folders as a side effect of a check is
	// the exact failure TestSyncCheckWritesNothing exists to catch.
	f, err := openFolderForStatusAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonFolderRefused, err.Error()
		return
	}
	// The NO-FLAG half of the one-stack-many-folders rule. resolveStackForFolder
	// above answers it for a NAMED stack and deliberately passes everything when
	// no name was given; this is the other direction, and without it a folder
	// belonging to another organization comes back `broken` rather than
	// `not_checked`. See syncReasonNotBoundHere for the mechanism and for the
	// case that must stay `broken`.
	if flagStack == "" && !f.Bound {
		line.Verdict, line.Reason = verdictUnknown, syncReasonNotBoundHere
		line.Detail = notBoundHereDetail(f)
		return
	}

	edges, findings, warnings, reason, detail := folderReferences(ctx, f, checker)
	line.Edges = append(line.Edges, edges...)
	line.Findings, line.Warnings = findings, warnings
	// A folder-level reason WINS over whatever the edges add up to, and the
	// edges are still reported. Both halves matter: the reason is why this
	// folder cannot be vouched for as a whole, and the edges are the part that
	// WAS decided — an unbound dependency is answered with no server at all, so
	// dropping it because the request that would have answered the rest failed
	// would throw away the one finding that never needed the network.
	if reason != "" {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, detail
		return
	}
	line.Verdict, line.Detail = checkVerdictOf(line.Edges, line.Findings, flagSyncAllowDropped)
}

// checkFolderLocalOnly reports the legs that need no server, for a folder whose
// remote half must not be asked about. See the switch in checkOneFolder.
func checkFolderLocalOnly(ctx context.Context, line *syncCheckFolderReport, folder discoveredFolder,
	resolved *config.Resolved, reason, detail string) {
	line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, detail
	f, err := openFolderForStatusAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil {
		return
	}
	_, aliases, err := folderSourceAndAliases(f)
	if err != nil {
		return
	}
	line.Edges = append(line.Edges, dependencyEdges(f.Manifest, aliases)...)
	line.Warnings = aliases.Warnings
}

// notBoundHereDetail says what the folder DOES name, so the reader can choose a
// --stack rather than being told only that this one did not match.
//
// It reads the manifest directly rather than borrowing describeBindSite, which
// is the function that produced the wrong message in the first place: that
// helper answers "where does a bind for the SELECTED entry go", and for an
// unmatched folder the selection is the unnamed legacy one — so it names an
// "instances" entry a stacks-only manifest does not have.
func notBoundHereDetail(f *folder) string {
	if names := f.Manifest.StackNames(); len(names) > 0 {
		return fmt.Sprintf("no --stack was given and none of this folder's stacks (%s) names the organization this credential reaches, so there is nothing here to check its references against — name one with --stack <name>",
			strings.Join(names, ", "))
	}
	return fmt.Sprintf("no --stack was given and nothing in %s names the organization this credential reaches, so there is nothing here to check its references against — push it once, or add a stack for this organization",
		wfdir.ManifestName)
}

// folderReferences enumerates and verifies one open folder's references,
// dispatching on kind.
//
// A non-empty reason means the folder as a WHOLE could not be checked. Whatever
// edges it managed to decide are returned alongside it rather than instead of
// it — see checkOneFolder.
func folderReferences(ctx context.Context, f *folder, checker *edgeChecker) (
	edges []syncEdgeReport, findings, warnings []string, reason, detail string) {
	switch f.Kind.Name {
	case wfdir.KindWorkflow:
		return workflowReferences(ctx, f, checker)
	case wfdir.KindDataApp:
		return dataAppReferences(ctx, f, checker)
	case wfdir.KindAutomation:
		return automationReferences(ctx, f, checker)
	default:
		return pipelineReferences(ctx, f, checker)
	}
}

// automationReferences resolves each automation file's id-bearing FIELDS and
// asks about every id.
//
// It cannot go through markerEdges, and the reason is the same one that shaped
// the loop: an automation's references are a structured field, not a marker, so
// markers.Scan over a .json file finds nothing at all. Falling through to
// pipelineReferences — which is what an unlisted kind does — therefore reported
// every automation folder as having no references whatever, which is `clean` for
// a folder whose note reference is dangling.
//
// The kinds with no alias (dataapp, mcpserver, feature) are reported as
// `unresolved` rather than looked up: resolveAutomationRefs refuses them at push
// time, so a check that answered `ok` for one would send somebody to a push that
// cannot succeed.
func automationReferences(ctx context.Context, f *folder, checker *edgeChecker) (
	edges []syncEdgeReport, findings, warnings []string, reason, detail string) {
	files, aliases, err := folderSourceAndAliases(f)
	if err != nil {
		return nil, nil, nil, syncReasonFolderRefused, err.Error()
	}
	edges = append(edges, dependencyEdges(f.Manifest, aliases)...)
	warnings = append(warnings, aliases.Warnings...)

	for _, path := range sortedPaths(files) {
		file, parseErr := parseAutomationFile(path, files[path])
		if parseErr != nil {
			// A file that will not parse has no references to check, and saying
			// so per file keeps the rest of the folder answerable — the same
			// split every other leg here makes between "this one" and "the folder".
			edges = append(edges, syncEdgeReport{
				Kind: "dependency", Where: path, Verdict: edgeUnresolved,
				Detail: fmt.Sprintf("%s is not a valid automation declaration, so its references could not be read: %v", path, parseErr),
			})
			continue
		}
		for _, field := range file.refFields() {
			edge := syncEdgeReport{Kind: field.Kind, Ref: field.Value, Where: path + " → " + field.Where}
			switch {
			case field.Kind == "":
				edge.Kind = "dependency"
				edge.Verdict = edgeUnresolved
				edge.Detail = "this reference kind has no alias, so it could only be named by one organization's raw id — a push refuses it"
			case markers.IsResourceID(field.Kind, field.Value):
				// Left undecided, so checker.apply asks the instance about it.
				edge.ID = field.Value
			default:
				id, bound := f.Codec.resolve(field.Kind, field.Value)
				if !bound {
					edge.Verdict = edgeUnresolved
					edge.Detail = fmt.Sprintf("%q is not a %s id and nothing here binds it to one", field.Value, field.Kind)
					break
				}
				edge.ID = id
			}
			edges = append(edges, edge)
		}
	}
	return checker.apply(ctx, edges), nil, warnings, "", ""
}

// workflowReferences asks the INSTANCE, because the answer already exists there
// and is better than anything this program could compute.
//
// POST /api/v2/workflow/validate takes candidate files pre-creation and reports
// per-marker findings against the caller's own reach. It is marker-derived, it
// is server-authoritative, and `ronja wf validate` already runs it — so the
// workflow half of edge verification shipped some time ago, and the right move
// is to call it rather than to build a second, weaker verifier beside it.
//
// ⚠️ It REFUSES without a featureID, and a stack with no featureID is the normal
// state before a folder's first push. That is not_checked, not a failure and not
// a silent skip — see errNoFeature.
func workflowReferences(ctx context.Context, f *folder, checker *edgeChecker) (
	edges []syncEdgeReport, findings, warnings []string, reason, detail string) {
	// ⚠️ THE LOCAL LEG RUNS FIRST, and specifically before the credential check
	// below. It used to run after, so a signed-out workflow folder returned
	// `no_credential` and reported NOTHING — while this file's own comment, the
	// README and the customer guide all promise that an alias this stack binds
	// nothing to is found with no network at all. The pipeline and data-app legs
	// already did it in this order; the workflow one was the odd leg out, and it
	// is the kind of folder a CI job under a rotated token hits first.
	aliases := aliasReport{}
	if _, local, readErr := folderSourceAndAliases(f); readErr == nil {
		aliases = local
	}
	if checker.client == nil {
		return append(edges, dependencyEdges(f.Manifest, aliases)...), nil, aliases.Warnings,
			syncReasonNoCredential,
			"there is no credential for this instance, and a workflow's REMOTE references are resolved by the instance itself — sign in with `ronja login`. The dependency names above needed no server"
	}
	validation, err := validateWorkflowFolder(ctx, checker.client, f)
	// The alias pre-flight needs no server, so it is reported EITHER WAY — losing
	// it because the request failed would throw away the half of the answer that
	// was already known, and would do so precisely when the other half is
	// missing.
	//
	// It is re-run rather than read off the validation when that came back nil,
	// because validateWorkflowFolder resolves the feature FIRST and a folder with
	// no feature never reaches the pre-flight at all — which is the exact case
	// this leg most needs to keep answering, since a folder before its first push
	// is also a folder whose binds nobody has filled in yet. `wf validate`'s own
	// ordering is deliberately left alone.
	// Prefer validate's own copy when it got that far; otherwise keep the one
	// read above. See the note at that read for why it happens first.
	if validation != nil {
		aliases = validation.Aliases
	}
	edges = append(edges, dependencyEdges(f.Manifest, aliases)...)
	warnings = append(warnings, aliases.Warnings...)
	switch {
	case errors.Is(err, errNoFeature):
		return edges, nil, warnings, syncReasonNoFeature, err.Error()
	case err != nil:
		return edges, nil, warnings, syncReasonLookupFailed, err.Error()
	}
	serverEdges, serverFindings, serverWarnings := edgesFromValidation(validation.Result)
	edges = append(edges, serverEdges...)
	return edges, append(findings, serverFindings...), append(warnings, serverWarnings...), "", ""
}

// pipelineReferences resolves a derived table's `{{ ref }}` markers locally and
// then asks about each distinct id.
//
// Locally first because a pipeline folder's references are not all remote: a ref
// naming a SIBLING .sql file in the same folder is answered by the folder, and
// on a first push the sibling's table does not exist yet — asking the server
// about it would report a folder that deploys perfectly as broken.
func pipelineReferences(ctx context.Context, f *folder, checker *edgeChecker) (
	edges []syncEdgeReport, findings, warnings []string, reason, detail string) {
	files, aliases, err := folderSourceAndAliases(f)
	if err != nil {
		return nil, nil, nil, syncReasonFolderRefused, err.Error()
	}
	pipe := newPipelineCodec(f, files)
	edges = append(edges, dependencyEdges(f.Manifest, aliases)...)
	edges = append(edges, markerEdges(f, files, &pipe)...)
	// The DOCS SIDECARS are references too, and they are the one kind written as
	// a FILE NAME rather than as a marker: `tables/orders.json` says this folder
	// documents whatever `orders` binds to here. Nothing above would look at
	// them — markers.Scan reads .sql and finds nothing, and the alias edges only
	// cover names ronja.json declares, which a sidecar written against a literal
	// id deliberately is not.
	docsEdges, docsFindings := tableDocsEdges(f)
	edges = append(edges, docsEdges...)
	// The METRIC FILES are references twice over — the stem names the row the
	// file is, and `recipe.source` names the table it reads — and neither is a
	// marker either. A metric whose source has stopped resolving is a folder that
	// no longer deploys, which is exactly the question this command asks.
	fromMetrics, metricFindings := metricEdges(f, pipe, f.live(f.State.For(f.Key)))
	edges = append(edges, fromMetrics...)
	return checker.apply(ctx, edges), append(docsFindings, metricFindings...), aliases.Warnings, "", ""
}

// dataAppReferences checks what a data app DECLARES, and compares it against
// what its source uses — in both directions.
//
// ⚠️ It deliberately does NOT call ValidateDataApp. That endpoint needs an
// existing DRAFT id, so reaching it means creating or forking a draft, which
// makes it a write-adjacent path — and this command is read-only.
func dataAppReferences(ctx context.Context, f *folder, checker *edgeChecker) (
	edges []syncEdgeReport, findings, warnings []string, reason, detail string) {
	files, aliases, err := folderSourceAndAliases(f)
	if err != nil {
		return nil, nil, nil, syncReasonFolderRefused, err.Error()
	}
	edges = append(edges, dependencyEdges(f.Manifest, aliases)...)
	warnings = append(warnings, aliases.Warnings...)
	// The markers a data app's own source writes — `{{ ref('table-…') }}` in a
	// query — are references like any other and are verified like any other.
	edges = append(edges, markerEdges(f, files, nil)...)

	if !f.Manifest.ManagesAccess() {
		// The allowlist lives on the server, so there is nothing declared to
		// compare the source against. Said out loud as a not_checked EDGE rather
		// than as a folder-level skip: the markers above WERE verified and are
		// worth reporting, and an app whose grants this folder does not own is
		// still one this command cannot fully vouch for — reporting it as clean
		// would be the overstatement the whole vocabulary exists to prevent.
		edges = append(edges, syncEdgeReport{
			Kind: "dependency", Where: wfdir.ManifestName,
			Verdict: edgeNotChecked, Reason: syncReasonAccessUnmanaged,
			Detail: fmt.Sprintf("%s declares no \"access\" block, so this folder does not own the app's allowlist and there is nothing here to check the source against — run `ronja app push` from a folder that manages access, or accept that the grants are managed in Ronja", wfdir.ManifestName),
		})
		return checker.apply(ctx, edges), nil, warnings, "", ""
	}
	access := f.declaredAccess()
	declared := accessEdges(access)
	findings = compareDeclaredAndUsed(declared, access, usedResourceIDs(f, files))
	edges = append(edges, declared...)
	return checker.apply(ctx, edges), findings, warnings, "", ""
}

// folderSourceAndAliases reads a folder's files once and runs the alias
// pre-flight over them.
//
// One read, not two: folder.aliasReport would walk the tree again to answer the
// same question, and both callers need the content anyway to scan it for
// markers.
func folderSourceAndAliases(f *folder) (map[string]string, aliasReport, error) {
	files, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return nil, aliasReport{}, err
	}
	return files, checkAliases(f.Manifest, f.selection(), f.Codec, files, folderStems(f.Kind, files), folderFieldRefs(f.Kind, f.Root, files)), nil
}

// checkVerdictOf folds one folder's edges into its verdict.
//
// UNKNOWN DOMINATES, exactly as it does in pushDelta.verdict and for the same
// reason:
// the strongest TRUE statement about a folder where one reference is broken and
// another could not be looked at is "I could not verify everything". Reporting
// it as a finding claims the unverifiable half was fine.
func checkVerdictOf(edges []syncEdgeReport, findings []string, allowDropped bool) (verdict, detail string) {
	var broken, unchecked, accepted int
	for _, edge := range edges {
		switch edge.Verdict {
		case edgeUnresolved, edgeUnreachable:
			// An accepted drop is EXCLUDED FROM THE SCORE and from nothing else:
			// the edge keeps its `unreachable` verdict, its detail and its place
			// in the list, because the caller asked to accept a broken reference,
			// not to stop being told about one.
			if allowDropped && edge.DroppedBinding {
				accepted++
				continue
			}
			broken++
		case edgeNotChecked:
			unchecked++
		}
	}
	broken += len(findings)
	switch {
	case unchecked > 0 && broken > 0:
		return verdictUnknown, fmt.Sprintf("%d %s could not be checked, and %d did not resolve",
			unchecked, plural(unchecked, "reference"), broken)
	case unchecked > 0:
		return verdictUnknown, fmt.Sprintf("%d %s could not be checked — this is not the same answer as everything resolving",
			unchecked, plural(unchecked, "reference"))
	case broken > 0:
		return verdictBroken, fmt.Sprintf("%d %s did not resolve", broken, plural(broken, "reference"))
	case accepted > 0:
		// Clean, and it says WHY it is clean. A folder that reports nothing here
		// would look identical to one with no dropped bindings at all, and the
		// whole point of leaving the edge in the report is that somebody still
		// has to go and create that secret.
		return verdictClean, fmt.Sprintf("%d dropped %s accepted (--allow-dropped-bindings) — runs that touch them will still fail",
			accepted, plural(accepted, "binding"))
	}
	return verdictClean, ""
}

func summarizeCheck(folders []syncCheckFolderReport) syncCheckSummary {
	summary := syncCheckSummary{Folders: len(folders)}
	for _, folder := range folders {
		summary.Findings += len(folder.Findings)
		switch folder.Verdict {
		case verdictClean:
			summary.Clean++
		case verdictBroken:
			summary.Broken++
		default:
			summary.Unknown++
		}
		for _, edge := range folder.Edges {
			summary.Edges++
			switch edge.Verdict {
			case edgeOK:
				summary.OK++
			case edgeUnresolved:
				summary.Unresolved++
			case edgeUnreachable:
				summary.Unreachable++
				if edge.DroppedBinding {
					summary.DroppedBindings++
				}
			default:
				summary.NotChecked++
			}
		}
	}
	return summary
}

// printSyncCheck renders the human report.
//
// The folders are listed before their verdicts for the reason printSyncStatus
// gives: a downward walk can pick up a vendored or example folder nobody meant
// to deploy, and the list is where a reader sees a surprise member.
//
// Within a folder, only the references that are NOT ok are printed. A clean
// folder's forty resolved edges are noise on the way to the one that is not —
// they are all in --json for anything that wants them.
func printSyncCheck(r *syncCheckReport) {
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
	for _, folder := range r.Folders {
		if folder.Kind != "" {
			fmt.Fprintf(out, "    %s (%s)\n", folder.Path, folder.Kind)
			continue
		}
		fmt.Fprintf(out, "    %s\n", folder.Path)
	}

	fmt.Fprintf(out, "\n  References\n")
	for _, folder := range r.Folders {
		fmt.Fprintf(out, "    %-8s %s\n", folder.Verdict, folder.Path)
		if folder.Detail != "" {
			fmt.Fprintf(out, "             %s\n", folder.Detail)
		}
		for _, edge := range folder.Edges {
			if edge.Verdict == edgeOK && !edge.Unused {
				continue
			}
			printSyncCheckEdge(out, edge, r.DroppedBindingsAllowed)
		}
		for _, finding := range folder.Findings {
			fmt.Fprintf(out, "             finding      %s\n", finding)
		}
		for _, warning := range folder.Warnings {
			fmt.Fprintf(out, "             note         %s\n", warning)
		}
	}

	fmt.Fprintf(out, "\n  %s — %d %s: %d ok, %d unresolved, %d unreachable, %d not checked\n",
		r.Verdict, r.Summary.Edges, plural(r.Summary.Edges, "reference"),
		r.Summary.OK, r.Summary.Unresolved, r.Summary.Unreachable, r.Summary.NotChecked)
	if r.Summary.Findings > 0 {
		fmt.Fprintf(out, "  %d other %s — see the `finding` %s above.\n",
			r.Summary.Findings, plural(r.Summary.Findings, "finding"),
			plural(r.Summary.Findings, "line"))
	}
	// The hatch is named in BOTH directions: when it would have helped, and when
	// it was used. The second line matters as much as the first — a green run
	// that accepted a broken reference must not read like a run that found none.
	switch {
	case r.Summary.DroppedBindings > 0 && r.DroppedBindingsAllowed:
		fmt.Fprintf(out, "  %d dropped %s accepted (--allow-dropped-bindings) — runs that touch them will still fail.\n",
			r.Summary.DroppedBindings, plural(r.Summary.DroppedBindings, "binding"))
	case r.Summary.DroppedBindings > 0:
		fmt.Fprintf(out, "  %d of those %s a secret binding a workflow save would DROP. Make them reachable, or run --allow-dropped-bindings to accept them.\n",
			r.Summary.DroppedBindings, agree(r.Summary.DroppedBindings, "is", "are"))
	}
}

// printSyncCheckEdge renders one reference line, naming what was written before
// what it resolved to — the reader searches their source for the former.
func printSyncCheckEdge(out *os.File, edge syncEdgeReport, droppedAllowed bool) {
	label := edge.Verdict
	switch {
	case edge.Verdict == edgeOK && edge.Unused:
		label = "unused"
	case edge.DroppedBinding && droppedAllowed:
		// The same word `ronja wf push` uses for the same acceptance. The verdict
		// in --json is untouched: this is the human column saying what was DONE
		// about the edge, not a different answer about it.
		label = "accepted"
	}
	what := edge.Ref
	if what == "" {
		what = edge.ID
	}
	if what != "" && edge.ID != "" && edge.ID != edge.Ref {
		what = fmt.Sprintf("%s (%s)", what, edge.ID)
	}
	fmt.Fprintf(out, "             %-12s %s %s\n", label, edge.Kind, what)
	if edge.Where != "" {
		fmt.Fprintf(out, "                          in %s\n", edge.Where)
	}
	if edge.Detail != "" {
		fmt.Fprintf(out, "                          %s\n", edge.Detail)
	}
	if edge.Unused && edge.Verdict == edgeOK {
		fmt.Fprintf(out, "                          granted by %s and used by no file in this folder\n", wfdir.ManifestName)
	}
}
