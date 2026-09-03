package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf validate` is the inner loop: it asks the server everything a save
// would check, and saves nothing.
//
// It needs no workflow to exist — only a feature to validate against — which is
// what makes converting an existing script an iterate-to-green loop instead of
// a create-then-fix one. It is also the check `wf push` runs first, so a clean
// validate means the push that follows will not be refused for a binding.
func newWorkflowValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check this folder server-side without saving anything",
		Long: `Check this folder server-side without saving anything.

Sends every file in the folder to the instance and reports what a save would
reject: unreachable {{ ref }} / {{ write }} / {{ agent }} / {{ codex }}
markers, markers written inside string literals, a missing entrypoint, bad
paths — plus the data and secret bindings the code would resolve to.

Nothing is persisted: no draft is opened, and the workflow does not have to
exist yet. A folder created by ` + "`ronja wf init`" + ` can be validated before its
first push.

Errors mean a save would be refused; warnings (an unreachable secret, say) do
not block one. Exits non-zero when there are errors.

The folder's own dependency names are checked too, before anything is sent: a
name no stack binds, or one used under the wrong kind of marker, is reported
here and exits non-zero, because ` + "`ronja wf push`" + ` refuses it.

With --json, the server's response verbatim plus an "ok" boolean and any local
"aliasRefusals".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			validation, err := validateWorkflowFolder(cmd.Context(), client, f)
			// The alias findings are printed BEFORE the error is returned, and
			// the partial return is what makes that possible: they were computed
			// before the request went out, and a validate that then failed on the
			// network used to print them anyway. Losing them on that path would
			// hide the one half of the answer that needed no server at all.
			if validation != nil {
				noteAliasErrors(validation.Aliases)
			}
			if err != nil {
				return err
			}
			result, aliases := validation.Result, validation.Aliases

			// Emitted BEFORE the failure is returned, in both modes: the payload
			// is the answer to the question that was asked, and a caller that
			// only gets an exit code has to run the command again to see why.
			if flagJSON {
				// A fresh err: assigning to the one above reads as if the
				// validate call could still be in play, and the next person to
				// add a branch here would have to work out that it cannot.
				if err := emitJSON(validateReport{
					OK:            result.OK() && len(aliases.Refusals) == 0,
					Findings:      nonNilFindings(result.Findings),
					Resolved:      result.Resolved,
					AliasRefusals: aliases.Refusals,
				}); err != nil {
					return err
				}
			} else {
				printValidateReport(result, len(aliases.Refusals))
			}
			if !result.OK() {
				return fmt.Errorf("%s", countErrors(result))
			}
			if len(aliases.Refusals) > 0 {
				// Non-zero so CI notices, after the payload has been emitted.
				// The refusals have already been printed by noteAliasErrors, so
				// this counts them rather than repeating them.
				return fmt.Errorf("%d alias %s in %s — nothing was saved, and `ronja wf push` would refuse this folder",
					len(aliases.Refusals), plural(len(aliases.Refusals), "problem"), wfdir.ManifestName)
			}
			return nil
		},
	}
	return cmd
}

// workflowValidation is everything `ronja wf validate` LEARNS about one folder:
// the server's verdict, and the local alias pre-flight that runs before the
// request.
//
// Both, never one, because they answer different questions — "would this save"
// and "can this folder be pushed at all" — and a caller that read only the
// server's half would call a folder clean that `push` refuses outright.
type workflowValidation struct {
	// Result is the server's response. Nil only when the request itself failed,
	// in which case Aliases is still populated — see the partial return below.
	Result  *api.ValidateResult
	Aliases aliasReport
}

// validateWorkflowFolder runs everything `ronja wf validate` asks of ONE open
// folder and returns the answer, deciding nothing about how to report it.
//
// Hoisted out of the RunE closure above so `ronja sync check` can ask the same
// question of every workflow folder in a tree, for the reason
// pipelineStatusReportAt was: a second copy of "does this folder's source still
// resolve" would drift from the one people actually run, and it would drift
// silently.
//
// ⚠️ It takes an OPEN FOLDER rather than a root, which is the one place the tree
// commands' hoisting pattern differs — and the difference is load-bearing.
// `wf validate` opens through openFolder, which calls adoptStack and therefore
// WRITES ronja.json when a --stack names a legacy folder. `ronja sync` is
// read-only with a test behind it, so it opens through openFolderForStatusAt
// instead. Taking a root here would have forced one opener on both, and the
// only opener that suits `wf validate` is the one that writes.
//
// It returns a NON-NIL validation alongside an error whenever the alias
// pre-flight got far enough to run, so a caller can still report the local half
// of the answer when the request is what failed.
func validateWorkflowFolder(ctx context.Context, client *api.Client, f *folder) (*workflowValidation, error) {
	// The bound row's persisted parameters are checked alongside the files by a
	// real save, so a validate that left them out would pass a folder whose
	// first push then fails on a stale parameters[N].optionsQuery — the exact
	// case validate exists to catch. Best-effort: validate persists nothing and
	// is advisory, so a row we cannot read costs the parameters, not the command.
	existing, err := inspectTarget(ctx, client, f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not read %s on %s (%v) — validating the files alone, without its saved parameters.\n",
			f.Binding.WorkflowID, f.Resolved.URL, err)
	}

	featureID, err := featureIDFor(ctx, client, f, existing.Workflow)
	if err != nil {
		return nil, err
	}
	files, enumeration, err := readLocalFiles(f.Root, wfdir.WorkflowKind)
	if err != nil {
		return nil, err
	}
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
	// The same local guard `wf push` runs, and for the same reason: the 200 MB
	// CSV somebody dropped in the folder is diagnosed entirely locally, by name,
	// rather than as a 400 from somewhere deep in the request path. Validate is
	// the command people run FIRST, so it is the worst place to skip it — and
	// the server's own validate cap (1 MiB per file) is the same number, so a
	// folder this refuses is one the server would refuse anyway, less helpfully.
	if err := checkPushable(files, wfdir.WorkflowKind, maxFileBytes); err != nil {
		return nil, err
	}
	// The alias pre-flight, and its refusals are part of the VERDICT rather than
	// a note beside it. `validate` is the command a CI job gates on, so a green
	// verdict on a folder `push` refuses outright is the one output that costs
	// somebody the deploy it was run to protect — and an unbound secret is
	// doubly invisible there, since the server's own finding for it is only a
	// WARNING.
	//
	// The server's own findings arrive alongside these rather than instead of
	// them: this answers "can this folder be pushed", the server answers "would
	// this save", and a folder mid-promotion wants to see both.
	//
	// What crosses the wire is the RESOLVED source — validateFilesOf runs the
	// same codec a push does, below, because validating the disk form would ask
	// the server about names it has never heard of and report a wall of
	// unresolvable references for a folder that pushes perfectly. So the server
	// sees ids for every alias this stack binds, and only a name nothing here
	// could resolve reaches it verbatim — which is exactly the name its own
	// refusal is good at.
	out := &workflowValidation{Aliases: checkAliases(f.Manifest, f.selection(), f.Codec, files, nil, nil)}

	// A folder that manages its parameters validates the set it DECLARES, not
	// the one the row currently has — otherwise a parameter added locally is
	// checked only after it has been pushed, which is the wrong way round for a
	// command whose job is to answer "would this folder save cleanly". An
	// unmanaged folder still borrows the row's, so its optionsQuery markers keep
	// being resolved.
	params := existing.Parameters()
	if f.Manifest.ManagesParameters() {
		params = f.Manifest.DeclaredParameters()
	}
	result, err := client.ValidateWorkflowFiles(ctx, api.ValidateInput{
		FeatureID:  featureID,
		Entrypoint: f.Manifest.Entrypoint,
		Parameters: params,
		Files:      validateFilesOf(f.Codec, files),
		// The runtime the FOLDER declares, so a candidate is checked against the
		// rules its author wrote for — and, when it declares none, the runtime
		// the workflow this folder would create or push to actually runs on. See
		// Manifest.RuntimeForValidate.
		RuntimeVersion: f.Manifest.RuntimeForValidate(existing.Workflow),
	})
	if err != nil {
		if message, ok := explainFeatureUnreachable(err, featureID, f.Resolved); ok {
			return out, fmt.Errorf("%s\n  %s", message, f.featureFixAdvice())
		}
		// The instance never delivered a verdict. This command saves nothing, so
		// nothing is at stake in the folder — but the bare error ("Internal
		// server error (HTTP 500)") reads as an answer ABOUT the folder, which is
		// the one thing it is not. Same sentence as `wf push`'s, minus the words
		// about a push: see the arm in workflow_push.go.
		if api.Unanswered(err) {
			return out, fmt.Errorf(
				"validate: the instance did not answer (%w) — nothing was checked, "+
					"and this is not a verdict on your files. Try again in a moment.", err)
		}
		return out, err
	}
	out.Result = result
	return out, nil
}

// validateReport is the --json shape: the server's response verbatim, plus the
// one derived fact a script would otherwise have to compute by scanning
// severities itself.
type validateReport struct {
	OK       bool                  `json:"ok"`
	Findings []api.ValidateFinding `json:"findings"`
	Resolved api.ValidateBindings  `json:"resolved"`
	// AliasRefusals are the LOCAL findings — what the folder's own declarations
	// and bindings say, answered before any request. A separate key rather than
	// entries folded into `findings`, because `findings` is the server's verdict
	// verbatim and a reader that treats one of these as having come from the
	// instance would go looking for a row that was never asked about.
	//
	// They count towards `ok`, which is the point of the field: an alias the
	// selected stack binds nothing to is a folder `push` refuses, and an
	// unresolvable secret is one the server would only WARN about — so `ok`
	// computed from the server alone is green on a folder that cannot deploy.
	//
	// omitempty, so the payload of a folder that declares no dependencies is
	// byte-identical to the one it had before this existed.
	AliasRefusals []string `json:"aliasRefusals,omitempty"`
}

// errNoFeature marks the two refusals featureIDFor can give, so a caller can
// tell "this folder names no feature yet" from every other way the lookup can
// fail.
//
// It exists for `ronja sync check`, and for one specific reason:
// POST /api/v2/workflow/validate REFUSES without a featureID, before it does any
// work. A stack that has no featureID is the normal state before a folder's
// first push — the onboarding case for a repository being adopted — so a tree
// check that read this as a failure would report every new folder as broken,
// and one that read it as a skip would report it as fine. It is neither: it is
// `not_checked`, which is never green.
//
// The message text is unchanged on both paths; only the wrapping is new.
var errNoFeature = errors.New("no feature recorded")

// featureIDFor works out which feature to validate against.
//
// The manifest's binding is the normal source — `wf init` records it, `wf
// clone` records it. The remote lookup covers a hand-written manifest that
// names a workflow but no feature: the workflow itself knows which feature it
// lives in, and failing over something we can simply ask about would be
// gratuitous.
//
// known is that workflow when the caller already fetched it, so the lookup is
// not paid for twice.
func featureIDFor(ctx context.Context, client *api.Client, f *folder, known *api.Workflow) (string, error) {
	if f.Binding.FeatureID != "" {
		return f.Binding.FeatureID, nil
	}
	if known != nil && known.FeatureID != "" {
		return known.FeatureID, nil
	}
	if known == nil && f.Binding.WorkflowID != "" {
		wf, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
		if err == nil && wf.FeatureID != "" {
			return wf.FeatureID, nil
		}
	}
	// The ORGANIZATION is part of the answer whenever this folder has NO entry
	// here and names another organization instead. "Add featureID to that
	// instance's binding" then sends the reader to a line that already has one —
	// which is exactly what they see, and exactly why they do not believe the
	// message.
	//
	// Not bound is half the condition: a folder that IS bound here and merely
	// left "featureID" out has a line to add the field to, and telling it "the
	// entries there name other organizations instead" would be the same species
	// of wrong advice one case over.
	if others := f.otherOrganizationsOn(); !f.Bound && len(others) > 0 {
		return "", fmt.Errorf("%w for %s in %s — this folder is bound to %s instead, and a workflow's ids belong to the organization that holds them, so nothing recorded there can be pushed under this credential.\n  %s",
			errNoFeature, describeTarget(f.Resolved), wfdir.ManifestPath(f.Root),
			f.describeOrganizationIDs(others), f.featureAdviceForAnotherOrganization())
	}
	return "", fmt.Errorf("%w for %s — %s, or recreate the folder with `ronja wf init --feature <id>`",
		errNoFeature, describeTarget(f.Resolved), f.featureAdvice())
}

// nonNilFindings keeps the JSON shape stable: the server always sends an array,
// and a client should never have to distinguish `null` from "clean".
func nonNilFindings(findings []api.ValidateFinding) []api.ValidateFinding {
	if findings == nil {
		return []api.ValidateFinding{}
	}
	return findings
}

func countErrors(result *api.ValidateResult) string {
	errs := len(result.Errors())
	warnings := len(result.Findings) - errs
	msg := fmt.Sprintf("%d %s", errs, plural(errs, "error"))
	if warnings > 0 {
		msg += fmt.Sprintf(" and %d %s", warnings, plural(warnings, "warning"))
	}
	return msg + " — nothing was saved"
}

// printValidateReport renders the server's verdict.
//
// aliasRefusals is the count of LOCAL problems already printed on stderr, and it
// is here only so the summary line cannot say "Clean" about a folder this
// command is about to fail. The server genuinely found nothing — it was sent
// unresolved names and had no opinion about them — so the two are reported as
// the two separate answers they are rather than added together.
func printValidateReport(result *api.ValidateResult, aliasRefusals int) {
	printFindings(os.Stdout, result.Findings)
	out := os.Stdout
	errs := len(result.Errors())
	warnings := len(result.Findings) - errs
	switch {
	case errs > 0:
		fmt.Fprintf(out, "\n  %d %s, %d %s — a save would be refused\n",
			errs, plural(errs, "error"), warnings, plural(warnings, "warning"))
	case warnings > 0:
		fmt.Fprintf(out, "\n  Clean, with %d %s — a save would succeed\n",
			warnings, plural(warnings, "warning"))
	default:
		fmt.Fprintf(out, "  Clean.\n")
	}
	if aliasRefusals > 0 {
		fmt.Fprintf(out, "  %d alias %s above — `ronja wf push` would refuse this folder\n",
			aliasRefusals, plural(aliasRefusals, "problem"))
	}
	fmt.Fprintf(out, "  Resolves %s\n", describeBindings(result.Resolved))
}

// printFindings renders findings grouped by the file they belong to, errors
// before warnings within each group.
//
// Grouping by file is the whole reason findings carry a path: the author fixes
// one file at a time, and an ID referenced from three files is three edits.
// Files are ordered with the ones carrying errors first, so the thing that has
// to be fixed is at the top rather than wherever it fell alphabetically.
func printFindings(out *os.File, findings []api.ValidateFinding) {
	if len(findings) == 0 {
		return
	}
	byPath := map[string][]api.ValidateFinding{}
	for _, f := range findings {
		path := f.Path
		if path == "" {
			path = "(no file)"
		}
		byPath[path] = append(byPath[path], f)
	}
	paths := make([]string, 0, len(byPath))
	hasError := map[string]bool{}
	for path, group := range byPath {
		paths = append(paths, path)
		for _, f := range group {
			if f.IsError() {
				hasError[path] = true
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		if hasError[paths[i]] != hasError[paths[j]] {
			return hasError[paths[i]]
		}
		return paths[i] < paths[j]
	})

	for _, path := range paths {
		fmt.Fprintf(out, "\n  %s\n", path)
		group := byPath[path]
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].IsError() && !group[j].IsError()
		})
		for _, f := range group {
			// The server's own word, so a tier the CLI has never heard of prints
			// as itself. It grew an `info` tier for the durable-workflow
			// advisories, and labelling those "warning" would report a note about
			// a shape as a problem with it — while hard-coding the new word here
			// would only move the same bug to the next tier.
			label := f.Severity
			if label == "" {
				label = "warning"
			}
			fmt.Fprintf(out, "    %-7s %s: %s\n", label, f.Code, f.Message)
			if f.Marker != "" {
				fmt.Fprintf(out, "            %s\n", f.Marker)
			}
		}
	}
}
