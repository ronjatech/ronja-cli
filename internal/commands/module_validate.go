package commands

import (
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// Local-only because a module refuses every marker, so there is nothing for a
// server pass to resolve. It cannot see package-name uniqueness or write
// access to the feature, and the report says so.
func newModuleValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check this folder locally without saving anything",
		Long: `Check this folder locally without saving anything.

Reports what a save would reject: a path the server's grammar refuses, a file
that is not .py, a file too large or holding binary content, two paths differing
only by capitalisation, a missing ` + "`__init__.py`" + ` (the file that makes the package
importable), a file or folder name that is not letters, digits and underscores, or starts
with a digit (` + "`my-helpers.py`" + `, ` + "`calc.v2.py`" + `, ` + "`2fast.py`" + `; the message suggests
one that works), and a file carrying a Ronja marker.

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
			// Deliberately not resolveInstance: nothing here talks to the
			// server, so a signed-out caller is not refused, and the binding is
			// not read.
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
			if err := checkPushable(files, wfdir.ModuleKind, maxModuleFileBytes); err != nil {
				report.Problems = append(report.Problems, err.Error())
			}
			if _, ok := files[moduleInitFile]; !ok {
				report.Problems = append(report.Problems, fmt.Sprintf(
					"%s is missing — it is what makes this folder an importable package, and the server refuses to delete it, so a push without one cannot reconcile. Create it (an empty file is fine)",
					moduleInitFile))
			}
			// Every file, not just the first: a push refuses one at a time.
			for _, path := range sortedPaths(files) {
				verb := firstRonjaMarkerVerb(files[path])
				if verb == "" {
					continue
				}
				report.Problems = append(report.Problems, moduleMarkerProblem(path, verb))
			}
			for _, path := range sortedPaths(files) {
				if problem := moduleFileNameProblem(path); problem != "" {
					report.Problems = append(report.Problems, problem)
				}
			}
			if f.Manifest.Name == "" {
				report.Problems = append(report.Problems, fmt.Sprintf(
					"%s declares no \"name\" — a module's package name is the identifier consumers import, and a folder that does not declare one leaves it to whatever the row already holds",
					wfdir.ManifestName))
			} else if err := checkLocalModuleName(f.Manifest.Name); err != nil {
				report.Problems = append(report.Problems, err.Error())
			}
			report.OK = len(report.Problems) == 0

			// Emitted before the failure is returned, in both modes.
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

type moduleValidateReport struct {
	OK    bool   `json:"ok"`
	Root  string `json:"root"`
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
	Files int    `json:"files"`
	// Always non-nil so a caller never has to tell `null` from "clean".
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
	// Every run, clean or not: without it "clean" reads as "the push will work".
	fmt.Fprintf(out, "  Checked locally only — a module carries no markers to resolve. `ronja module push`\n")
	fmt.Fprintf(out, "  is what checks the package name is free and that you may write to the feature.\n")
}
