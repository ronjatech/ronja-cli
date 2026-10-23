package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The REPORT of `ronja import feature`: the --json shape and the human
// rendering of what was written, claimed, left out and left literal. Split from
// import.go.

// importClaimOutcome is one row's claim, as the report gives it.
type importClaimOutcome struct {
	Folder  string `json:"folder"`
	Address string `json:"address"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// importFolderView is one written folder in the report.
type importFolderView struct {
	Path string `json:"path"`
	// Withdrawn: the folder's row was refused a claim and the folder removed.
	Withdrawn bool               `json:"withdrawn,omitempty"`
	Kind      string             `json:"kind"`
	FolderID  string             `json:"folderID"`
	Resources []importRowSummary `json:"resources"`
}

type importRowSummary struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

// importSkip is a live resource the import did not write, and why.
type importSkip struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// importReport is the --json shape, and what the human report prints.
type importReport struct {
	Root             string                   `json:"root"`
	URL              string                   `json:"url"`
	Stack            string                   `json:"stack"`
	FeatureID        string                   `json:"featureID"`
	FeatureName      string                   `json:"featureName"`
	Verdict          string                   `json:"verdict"`
	Folders          []importFolderView       `json:"folders"`
	Claims           []importClaimOutcome     `json:"claims"`
	Literals         []importLiteral          `json:"literals"`
	DataSources      []importDataSource       `json:"dataSources"`
	Addresses        []importAddress          `json:"addresses"`
	Outputs          []importOutput           `json:"outputs"`
	ClaimedElsewhere []importClaimedElsewhere `json:"claimedElsewhere"`
	NotImported      []importSkip             `json:"notImported"`
	NotCarried       []importNotCarried       `json:"notCarried"`
	Problems         []string                 `json:"problems,omitempty"`
}

func (imp *featureImport) report() *importReport {
	r := &importReport{
		Root: imp.base, URL: imp.resolved.URL, Stack: imp.stack,
		FeatureID: imp.feature.ID, FeatureName: imp.feature.Name, Verdict: verdictApplied,
		Folders: []importFolderView{}, Claims: imp.claims, Literals: imp.literals,
		DataSources: imp.sources, Addresses: imp.addresses, Outputs: imp.outputs, ClaimedElsewhere: imp.claimedElsewhere,
		NotImported: imp.notImported, NotCarried: imp.notCarried, Problems: imp.problems,
	}
	for _, f := range imp.folders {
		view := importFolderView{Path: f.Rel, Kind: f.Kind.Name, FolderID: f.FolderID, Withdrawn: f.withdrawn}
		for _, row := range f.rows {
			view.Resources = append(view.Resources, importRowSummary{Kind: row.res.Kind, ID: row.res.ID, Name: row.res.Name, Address: row.address})
		}
		r.Folders = append(r.Folders, view)
	}
	if r.Claims == nil {
		r.Claims = []importClaimOutcome{}
	}
	if r.Literals == nil {
		r.Literals = []importLiteral{}
	}
	sort.SliceStable(r.Literals, func(i, j int) bool {
		a, b := r.Literals[i], r.Literals[j]
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	if r.DataSources == nil {
		r.DataSources = []importDataSource{}
	}
	if r.Addresses == nil {
		r.Addresses = []importAddress{}
	}
	if r.Outputs == nil {
		r.Outputs = []importOutput{}
	}
	if r.ClaimedElsewhere == nil {
		r.ClaimedElsewhere = []importClaimedElsewhere{}
	}
	if r.NotImported == nil {
		r.NotImported = []importSkip{}
	}
	if r.NotCarried == nil {
		r.NotCarried = []importNotCarried{}
	}
	if len(r.Problems) > 0 {
		r.Verdict = verdictRefused
	}
	return r
}

func printImportReport(r *importReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  Imported %q into %s, for stack %q\n\n", r.FeatureName, r.Root, r.Stack)
	for _, f := range r.Folders {
		fmt.Fprintf(out, "    %-40s %d %s\n", f.Path, len(f.Resources), plural(len(f.Resources), "row"))
	}
	refused := 0
	for _, c := range r.Claims {
		if c.Outcome != api.ManagedOutcomeClaimed && c.Outcome != api.ManagedOutcomeAlreadyClaimed {
			refused++
		}
	}
	fmt.Fprintf(out, "\n  Claimed: %d of %d %s for stack %q\n", len(r.Claims)-refused, len(r.Claims), plural(len(r.Claims), "row"), r.Stack)
	for _, c := range r.Claims {
		if c.Outcome != api.ManagedOutcomeClaimed && c.Outcome != api.ManagedOutcomeAlreadyClaimed {
			fmt.Fprintf(out, "    %s %s %s (%s): %s\n", strings.ToUpper(c.Outcome), c.Folder, describeAddressKey(c.Address), c.ID, c.Detail)
		}
	}
	if len(r.Literals) > 0 {
		fmt.Fprintf(out, "\n  Left in place (%d) — positions no alias reaches:\n", len(r.Literals))
		for _, l := range r.Literals {
			what := l.TargetID
			if what == "" {
				what = l.Arg
			}
			fmt.Fprintf(out, "    %s/%s:%d  %s %s", l.Folder, l.Path, l.Line, l.Position, what)
			if l.Alias != "" {
				fmt.Fprintf(out, " (declared as %q)", l.Alias)
			}
			fmt.Fprintln(out)
		}
	}
	if len(r.DataSources) > 0 {
		fmt.Fprintf(out, "\n  Data sources (%d) — bound in stack %q only; any other stack must bind each one:\n", len(r.DataSources), r.Stack)
		for _, d := range r.DataSources {
			fmt.Fprintf(out, "    %-30s %-10s %-11s %s → %s\n", d.Folder, d.Kind, d.Access, d.Alias, d.BoundTo)
		}
	}
	if len(r.Addresses) > 0 {
		fmt.Fprintf(out, "\n  Addresses (%d) — resolved through the other folder's claim, no bind needed:\n", len(r.Addresses))
		for _, a := range r.Addresses {
			fmt.Fprintf(out, "    %-30s %s → %s\n", a.Folder, a.Alias, describeAddressTarget(a.Kind, a.From, a.Address))
		}
	}
	if len(r.Outputs) > 0 {
		fmt.Fprintf(out, "\n  Outputs (%d) — written by name; bound in stack %q to the table it already holds, optional elsewhere:\n", len(r.Outputs), r.Stack)
		for _, o := range r.Outputs {
			fmt.Fprintf(out, "    %-30s %s → %s\n", o.Folder, o.Alias, o.BoundTo)
		}
	}
	if len(r.ClaimedElsewhere) > 0 {
		fmt.Fprintf(out, "\n  Managed by another folder, left there (%d):\n", len(r.ClaimedElsewhere))
		for _, c := range r.ClaimedElsewhere {
			fmt.Fprintf(out, "    %s %s (%s) — folder %s %s\n", c.Kind, c.ID, c.Name, c.FolderID, describeAddressKey(c.Address))
		}
	}
	if len(r.NotImported) > 0 {
		fmt.Fprintf(out, "\n  Not imported (%d) — no folder of the tree holds these:\n", len(r.NotImported))
		for _, s := range r.NotImported {
			fmt.Fprintf(out, "    %s %s (%s) — %s\n", s.Kind, s.ID, s.Name, s.Reason)
		}
	}
	if len(r.NotCarried) > 0 {
		fmt.Fprintf(out, "\n  Not carried (%d) — bindings set outside the code; deploying back leaves them as they are:\n", len(r.NotCarried))
		for _, c := range r.NotCarried {
			what := c.TargetID
			if what == "" {
				what = c.TargetPath
			}
			fmt.Fprintf(out, "    %s %s.%s %s %s — %s\n", c.Folder, c.ResourceID, c.Field, c.TargetKind, what, c.Effect)
		}
	}
	if len(r.Problems) > 0 {
		fmt.Fprintln(out)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(out, "  Problem: %s\n", p)
	}
	fmt.Fprintf(out, "\n  Next: commit the tree, then ronja sync status --stack %s\n", r.Stack)
}

// describeAddressTarget names what an address dependency resolves to, as the
// import report lists it: "the workflow in ../../workflows/report" for a
// folder's one row, `"orders.sql" in ../../pipelines/sales` for a file's.
func describeAddressTarget(kind, from, address string) string {
	if address != "" {
		return fmt.Sprintf("%q in %s", address, from)
	}
	label := kind
	if k, ok := wfdir.KindByName(kind); ok {
		label = k.Label
	}
	return fmt.Sprintf("the %s in %s", label, from)
}
