package commands

import (
	"context"
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

With --json, the server's response verbatim plus an "ok" boolean.`,
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

			// The bound row's persisted parameters are checked alongside the
			// files by a real save, so a validate that left them out would pass
			// a folder whose first push then fails on a stale
			// parameters[N].optionsQuery — the exact case validate exists to
			// catch. Best-effort: validate persists nothing and is advisory, so
			// a row we cannot read costs the parameters, not the command.
			existing, err := inspectTarget(cmd.Context(), client, f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  Note: could not read %s on %s (%v) — validating the files alone, without its saved parameters.\n",
					f.Binding.WorkflowID, resolved.URL, err)
			}

			featureID, err := featureIDFor(cmd.Context(), client, f, existing.Workflow)
			if err != nil {
				return err
			}
			files, enumeration, err := readLocalFiles(f.Root, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			for _, s := range enumeration.Skipped {
				fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
			}
			// The same local guard `wf push` runs, and for the same reason: the
			// 200 MB CSV somebody dropped in the folder is diagnosed entirely
			// locally, by name, rather than as a 400 from somewhere deep in the
			// request path. Validate is the command people run FIRST, so it is
			// the worst place to skip it — and the server's own validate cap
			// (1 MiB per file) is the same number, so a folder this refuses is
			// one the server would refuse anyway, less helpfully.
			if err := checkPushable(files, maxFileBytes); err != nil {
				return err
			}

			// A folder that manages its parameters validates the set it DECLARES,
			// not the one the row currently has — otherwise a parameter added
			// locally is checked only after it has been pushed, which is the
			// wrong way round for a command whose job is to answer "would this
			// folder save cleanly". An unmanaged folder still borrows the row's,
			// so its optionsQuery markers keep being resolved.
			params := existing.Parameters()
			if f.Manifest.ManagesParameters() {
				params = f.Manifest.DeclaredParameters()
			}
			result, err := client.ValidateWorkflowFiles(cmd.Context(), api.ValidateInput{
				FeatureID:  featureID,
				Entrypoint: f.Manifest.Entrypoint,
				Parameters: params,
				Files:      validateFilesOf(files),
			})
			if err != nil {
				return err
			}

			// Emitted BEFORE the failure is returned, in both modes: the payload
			// is the answer to the question that was asked, and a caller that
			// only gets an exit code has to run the command again to see why.
			if flagJSON {
				// A fresh err: assigning to the one above reads as if the
				// validate call could still be in play, and the next person to
				// add a branch here would have to work out that it cannot.
				if err := emitJSON(validateReport{
					OK:       result.OK(),
					Findings: nonNilFindings(result.Findings),
					Resolved: result.Resolved,
				}); err != nil {
					return err
				}
			} else {
				printValidateReport(result)
			}
			if !result.OK() {
				return fmt.Errorf("%s", countErrors(result))
			}
			return nil
		},
	}
	return cmd
}

// validateReport is the --json shape: the server's response verbatim, plus the
// one derived fact a script would otherwise have to compute by scanning
// severities itself.
type validateReport struct {
	OK       bool                  `json:"ok"`
	Findings []api.ValidateFinding `json:"findings"`
	Resolved api.ValidateBindings  `json:"resolved"`
}

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
	return "", fmt.Errorf("no feature recorded for %s in %s — add \"featureID\" to that instance's binding, or recreate the folder with `ronja wf init --feature <id>`",
		f.Resolved.URL, wfdir.ManifestPath(f.Root))
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

func printValidateReport(result *api.ValidateResult) {
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
			label := "warning"
			if f.IsError() {
				label = "error"
			}
			fmt.Fprintf(out, "    %-7s %s: %s\n", label, f.Code, f.Message)
			if f.Marker != "" {
				fmt.Fprintf(out, "            %s\n", f.Marker)
			}
		}
	}
}
