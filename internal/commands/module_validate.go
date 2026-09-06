package commands

import (
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module validate` checks the folder against everything a module save
// will refuse, and saves nothing.
//
// ⚠️ It is LOCAL-ONLY, and that is a fact about the primitive rather than a
// smaller version of `wf validate`. A workflow's validate endpoint exists to
// resolve `{{ ref }}` / `{{ secret }}` / `{{ agent }}` markers against the
// author's access before a save — the answer genuinely requires the server. A
// module refuses EVERY Ronja marker at save (plan §3.3): it receives its
// database handles, data and configuration as function arguments, so there is
// nothing to resolve, nothing to bind, and nothing an extra round trip could
// tell the author. What IS checkable is checkable here: the path grammar, the
// .py rule, the size and NUL limits, the case-collision rule, the presence of
// the __init__.py that makes the package importable — and the marker rule
// itself, which needs no server precisely BECAUSE it resolves nothing. It is a
// lexical scan of the source, ported from the server's own gate; see
// module_markers.go for the keep-in-sync obligation that comes with the copy.
//
// The honest consequence, said out loud in the report rather than implied: this
// does not prove the module SAVES. The two things it cannot see are the package
// name (unique among live modules, not a stdlib shadow) and the caller's write
// access to the feature, both of which are the server's to answer and both of
// which `module push` reports the moment it asks.
func newModuleValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check this folder locally without saving anything",
		Long: `Check this folder locally without saving anything.

Reports what a save would reject: a path the server's grammar refuses, a file
that is not .py, a file too large or holding binary content, two paths differing
only by capitalisation, a missing ` + "`__init__.py`" + ` (the file that makes the package
importable), and a file carrying a Ronja marker.

Modules are marker-free: ` + "`{{ ref }}`" + `, ` + "`{{ secret }}`" + `, ` + "`{{ workflow }}`" + ` and the rest
are refused at save, in any file. A module runs inside the calling workflow's
interpreter under that workflow's authority, so it takes its database handles,
data and configuration as function arguments — put the marker in the consumer
workflow and pass the result in.

Nothing is sent and nothing is persisted: there is no binding to resolve, so
unlike ` + "`ronja wf validate`" + ` there is no server-side pass to run. The two things
this cannot see are whether the package name is free and whether you may write to
the feature; ` + "`ronja module push`" + ` reports both.

Exits non-zero when there are errors. With --json, one object listing them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance, and this is the one acting-shaped
			// command in the loop that needs no credential at all: nothing here
			// talks to the server, so refusing a signed-out caller would be
			// refusing to do arithmetic because the network is down. `openFolder`
			// still needs a resolved instance to key the binding by, so the
			// folder is opened LOCALLY — the binding is not read.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			f, err := openFolderLocally(resolved, wfdir.ModuleKind)
			if err != nil {
				return err
			}

			files, enumeration, err := readLocalFiles(f.Root, wfdir.ModuleKind)
			if err != nil {
				return err
			}
			noteModuleSkipped(enumeration)

			report := &moduleValidateReport{
				Root:     f.Root,
				Name:     f.Manifest.Name,
				Title:    f.Manifest.Title,
				Files:    len(files),
				Skipped:  enumeration.Skipped,
				Problems: []string{},
			}
			// checkPushable is the shared refusal — case collisions, oversize
			// files, NUL bytes — and it is reused rather than reimplemented so a
			// module folder is refused for the same reasons and in the same words
			// as every other folder.
			if err := checkPushable(files, wfdir.ModuleKind, maxModuleFileBytes); err != nil {
				report.Problems = append(report.Problems, err.Error())
			}
			if _, ok := files[moduleInitFile]; !ok {
				report.Problems = append(report.Problems, fmt.Sprintf(
					"%s is missing — it is what makes this folder an importable package, and the server refuses to delete it, so a push without one cannot reconcile. Create it (an empty file is fine)",
					moduleInitFile))
			}
			// The ONE content rule a module save has, and the one an author walks
			// into while moving a helper OUT of a workflow folder: that helper may
			// well have carried a marker there. Every file is reported rather than
			// only the first, because a save refuses one at a time and finding them
			// one push at a time is the slowest possible way to learn the rule.
			for _, path := range sortedPaths(files) {
				verb := firstRonjaMarkerVerb(files[path])
				if verb == "" {
					continue
				}
				report.Problems = append(report.Problems, moduleMarkerProblem(path, verb))
			}
			if f.Manifest.Name == "" {
				report.Problems = append(report.Problems, fmt.Sprintf(
					"%s declares no \"name\" — a module's package name is the identifier consumers import, and a folder that does not declare one leaves it to whatever the row already holds",
					wfdir.ManifestName))
			} else if err := checkLocalModuleName(f.Manifest.Name); err != nil {
				report.Problems = append(report.Problems, err.Error())
			}
			report.OK = len(report.Problems) == 0

			// Emitted BEFORE the failure is returned, in both modes: the payload
			// is the answer to the question that was asked, and a caller that only
			// gets an exit code has to run the command again to see why.
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printModuleValidateReport(report)
			}
			if !report.OK {
				return fmt.Errorf("%d problem(s) — nothing was saved", len(report.Problems))
			}
			return nil
		},
	}
	return cmd
}

// moduleValidateReport is the --json shape and the human renderer's input, so
// the two cannot describe different things.
type moduleValidateReport struct {
	OK    bool   `json:"ok"`
	Root  string `json:"root"`
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
	Files int    `json:"files"`
	// Problems are prose, one per refusal, and always non-nil so a caller never
	// has to tell `null` from "clean".
	Problems []string        `json:"problems"`
	Skipped  []wfdir.Skipped `json:"skipped,omitempty"`
}

func printModuleValidateReport(r *moduleValidateReport) {
	out := os.Stdout
	for _, p := range r.Problems {
		fmt.Fprintf(out, "  error  %s\n", p)
	}
	if r.OK {
		fmt.Fprintf(out, "  Clean — %d .py file(s) ready to push.\n", r.Files)
	} else {
		fmt.Fprintf(out, "\n  %d problem(s) — a push would be refused.\n", len(r.Problems))
	}
	// Said on every run, clean or not: a validate that returned "clean" without
	// this line would be read as "the push will work", and the two things it
	// cannot see are exactly the two that stop a first push.
	fmt.Fprintf(out, "  Checked locally only — a module carries no markers to resolve. `ronja module push`\n")
	fmt.Fprintf(out, "  is what checks the package name is free and that you may write to the feature.\n")
}
