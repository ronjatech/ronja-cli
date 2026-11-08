package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja sync stack add` declares one stack in every folder of a tree: bind
// --feature, folder by folder, under one confirmation. It writes ronja.json
// files only and creates nothing on the server.
func newSyncStackCmd() *cobra.Command {
	stack := &cobra.Command{
		Use:   "stack",
		Short: "Declare a stack in every folder of the tree",
	}
	var featureID string
	var yes bool
	add := &cobra.Command{
		Use:   "add [name] --feature <id>",
		Short: "Declare a stack on a feature in every folder of the tree, and bind what it can",
		Long: `Declare a stack on a feature in every folder of the tree, and bind what it can.

  ronja sync stack add main --feature col-…
  ronja sync stack add --feature col-…        # named after the feature's Space (dev), or main in the default Space

Each folder gets the stack, with the feature (and, outside the organization's
default Space, the Space it is in), and the same proposals
` + "`ronja bind --feature`" + ` makes, folder by folder: a read
dependency is bound when exactly one row of that name is in the stack's Space,
and a table the code writes or a workflow or Saved Agent it runs when exactly
one is in the stack's own feature. A table another stack binds to a snapshot
copy is bound to the copy's source. Every name it cannot fill is reported per
folder, with the ` + "`ronja copy`" + ` line that makes a row for it where there is one.

In the same confirmation it lifts every raw id in a folder's code into a
declared dependency bound to that same id in the folder's other stacks, so
those stacks read and write the same rows and the new stack binds its own. A
stack that records a Space is bound to an id it writes, runs or uses as a
secret only when the row is in that Space; anywhere else the name is left
unbound there and reported. An
output literal (tools.replaceTable(df, "table-…")) becomes {{ write('<alias>') }}:
the existing stacks write the same table, and their stored code differs in that
argument, so their next push is a new version. Commit the lift and push them
after it, so an older checkout does not push the literal back.

Everything is planned first, and nothing is written until you confirm (--yes
without a terminal). It writes only ronja.json files and the code a lift
rewrites, and creates nothing on the server: ` + "`ronja sync apply --stack <name> --allow-create`" + `
does that. It exits 1 while any name is left unbound, in any stack. The stack
is named by the argument, not --stack.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runSyncStackAdd(cmd.Context(), flagSyncDir, name, featureID, yes)
		},
	}
	add.Flags().StringVar(&featureID, "feature", "", "the feature the stack creates its resources in (required)")
	add.Flags().BoolVar(&yes, "yes", false, "lift raw ids and write the declarations without the prompt (required when there is no terminal)")
	stack.AddCommand(add)
	return stack
}

// stackAddReport is one run of `sync stack add`.
type stackAddReport struct {
	Stack     string `json:"stack"`
	Space     string `json:"space,omitempty"`
	FeatureID string `json:"featureID"`
	Feature   string `json:"feature,omitempty"`
	// Lifts are the raw ids lifted into dependencies before declaring.
	Lifts   []rawIDLift       `json:"lifts"`
	Folders []stackAddFolder  `json:"folders"`
	Skipped []stackAddSkipped `json:"skipped"`
	Written bool              `json:"written"`
}

// stackAddFolder is one folder the stack is declared in.
type stackAddFolder struct {
	Path     string `json:"path"`
	Declares string `json:"declares,omitempty"`
	// SpaceRecorded is the Space recorded on a stack that already named the
	// feature and no Space.
	SpaceRecorded string           `json:"spaceRecorded,omitempty"`
	AlreadyBound  []string         `json:"alreadyBound"`
	Proposed      []bindProposal   `json:"proposed"`
	Unresolved    []bindUnresolved `json:"unresolved"`
	// LiftUnbound are the lifted aliases left unbound in this folder's other
	// stacks, since each names a row outside that stack's Space.
	LiftUnbound []liftUnbound `json:"liftUnbound,omitempty"`
	// LiteralsSkipped says why this workflow folder's output literals were not
	// looked for, so none of them is lifted.
	LiteralsSkipped string `json:"literalsSkipped,omitempty"`
}

// stackAddSkipped is a folder the stack is not declared in, and why.
type stackAddSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// syncReasonFolderWriteOutsideDefault — a folder declares a folder write
// scope, and the new stack's Space is not the organization's default, where a
// run's file writes land. Machine contract: add, never rename.
const syncReasonFolderWriteOutsideDefault = "folder_write_outside_default_space"

func runSyncStackAdd(ctx context.Context, dir, name, featureID string, yes bool) error {
	if flagStack != "" {
		return fmt.Errorf("name the stack as the argument, not with --stack: `ronja sync stack add %s --feature <id>`", flagStack)
	}
	if featureID == "" {
		return fmt.Errorf("--feature is required: the feature the stack creates its resources in, which is the one thing nothing can work out for you")
	}
	if name != "" {
		if err := wfdir.ValidStackName(name); err != nil {
			return err
		}
	}
	found, err := discoverFolders(dir)
	if err != nil {
		return err
	}
	var problems []string
	for _, d := range found {
		if d.notChecked() {
			problems = append(problems, fmt.Sprintf("%s: %s", d.Rel, d.Detail))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("these folders cannot be opened, so the stack would be declared in only part of the tree — nothing was written:\n    %s", strings.Join(problems, "\n    "))
	}
	if len(found) == 0 {
		return fmt.Errorf("no %s under %s — `sync stack add` declares a stack in the folders of a tree", wfdir.ManifestName, dir)
	}
	resolved, err := resolveDeployTreeInstance(dir, yes)
	if err != nil {
		return err
	}
	if err := ensureTenant(ctx, resolved); err != nil {
		return err
	}
	if err := confirmFeatureIn(ctx, featureID, resolved); err != nil {
		return err
	}
	l := newSpaceLookup(ctx, resolved)
	if name == "" {
		existing := map[string]wfdir.Stack{}
		for _, d := range found {
			for k, v := range d.Manifest.Stacks {
				if _, seen := existing[k]; !seen || v.FeatureID == featureID {
					existing[k] = v
				}
			}
		}
		if name, err = defaultStackName(l, featureID, existing); err != nil {
			return fmt.Errorf("%w (here the name is the first argument: `ronja sync stack add <name> --feature %s`)", err, featureID)
		}
	}

	feature, err := l.feature(featureID)
	if err != nil {
		return declareSpaceFailed(featureID, name, err)
	}
	report := &stackAddReport{Stack: name, FeatureID: featureID, Feature: feature.Name,
		Lifts: []rawIDLift{}, Folders: []stackAddFolder{}, Skipped: []stackAddSkipped{}}
	outsideDefault := false
	if feature.SpaceID != "" {
		space, err := l.space(feature.SpaceID)
		if err != nil {
			return declareSpaceFailed(featureID, name, err)
		}
		report.Space, outsideDefault = space.Name, !space.IsDefault
	}

	var declare []discoveredFolder
	for _, d := range found {
		if outsideDefault {
			// See BL-c4d8.
			if alias := folderWriteScope(d.Manifest); alias != "" {
				report.Skipped = append(report.Skipped, stackAddSkipped{Path: d.Rel, Reason: syncReasonFolderWriteOutsideDefault,
					Detail: fmt.Sprintf("not declared: it writes files (dependency %q, a folder write scope), and a run's file writes land in the organization's default Space, not in Space %s, so every deploy of this stack would be refused bind_space_mismatch. Write a table in the stack's own feature instead, then run this again",
						alias, orUnknown(report.Space))})
				continue
			}
		}
		declare = append(declare, d)
	}

	// Everything is planned before the one confirmation, and nothing is
	// written before it: the lift in memory, then each folder's declaration
	// over the dependencies the lift adds, so the new stack's picker answers
	// them.
	lifts, literalsSkipped, err := planRawIDLifts(ctx, resolved, l, declare)
	if err != nil {
		return err
	}
	bodies, err := liftedBodies(declare, lifts)
	if err != nil {
		return err
	}
	if len(lifts) > 0 {
		report.Lifts = lifts
	}

	previousStack, previousFeature := flagStack, declaringFeature
	flagStack, declaringFeature = name, featureID
	defer func() { flagStack, declaringFeature = previousStack, previousFeature }()
	var plans []*bindPlan
	var rels []string
	for _, d := range declare {
		f, err := openFolderToPlanAt(ctx, d.Root, resolved, d.Kind)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", d.Rel, err))
			continue
		}
		liftUnbound := liftIntoManifest(f, l, name, liftsIn(lifts, d.Root))
		plan, err := planBind(ctx, f, featureID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", d.Rel, err))
			continue
		}
		plans, rels = append(plans, plan), append(rels, d.Rel)
		report.Folders = append(report.Folders, stackAddFolder{Path: d.Rel, Declares: plan.result.DeclaresStack, SpaceRecorded: plan.spaceRecorded,
			AlreadyBound: plan.result.AlreadyBound, Proposed: plan.result.Proposed, Unresolved: plan.result.Unresolved,
			LiteralsSkipped: literalsSkipped[d.Root], LiftUnbound: liftUnbound})
	}
	if len(problems) > 0 {
		return fmt.Errorf("stack %q cannot be declared in every folder — nothing was written:\n    %s", name, strings.Join(problems, "\n    "))
	}

	writes := 0
	for _, p := range plans {
		if p.writes() {
			writes++
		}
	}
	if !flagJSON {
		if len(lifts) > 0 {
			printRawIDLifts(lifts)
		}
		printStackAddPlan(report)
	}
	if writes > 0 || len(lifts) > 0 {
		question := fmt.Sprintf("Declare stack %q in %d %s?", name, writes, plural(writes, "folder"))
		effect := fmt.Sprintf("writes stack %q into %d %s", name, writes, wfdir.ManifestName)
		if ids := distinctLiftIDs(lifts); ids > 0 {
			question = fmt.Sprintf("Lift %d raw %s into dependencies and declare stack %q in %d %s?", ids, plural(ids, "id"), name, writes, plural(writes, "folder"))
			effect = fmt.Sprintf("rewrites %d raw %s in this tree's code into dependencies and %s", ids, plural(ids, "id"), effect)
		}
		ok, err := confirm(question, effect, yes)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := writeStackAdd(plans, rels, bodies, lifts); err != nil {
			return withExitCode(1, err)
		}
		report.Written = true
	}
	if flagJSON {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else if report.Written && writes > 0 {
		fmt.Fprintf(os.Stdout, "  Declared stack %q in %d %s. %s is committed and shared, so commit it.\n", name, writes, plural(writes, "folder"), wfdir.ManifestName)
	}
	var unbound, others []string
	for _, folder := range report.Folders {
		for _, u := range folder.Unresolved {
			unbound = append(unbound, folder.Path+": "+u.Alias)
		}
		for _, u := range folder.LiftUnbound {
			others = append(others, fmt.Sprintf("%s: %s in stack %q", folder.Path, u.Alias, u.Stack))
		}
	}
	if len(unbound) > 0 || len(others) > 0 || len(report.Skipped) > 0 {
		deps := "dependencies"
		if len(unbound) == 1 {
			deps = "dependency"
		}
		also := ""
		if len(others) > 0 {
			also = fmt.Sprintf(", and the lift left %d %s unbound in other stacks (%s)", len(others), pluralVerb(len(others), "alias", "aliases"), strings.Join(others, ", "))
		}
		return withExitCode(1, fmt.Errorf("stack %q has %d unbound %s and %d undeclared %s%s — bind each by hand in its folder's %s, or run `ronja bind --stack <name>` there once the rows exist",
			name, len(unbound), deps, len(report.Skipped), plural(len(report.Skipped), "folder"), also, wfdir.ManifestName))
	}
	return nil
}

// folderWriteScope names a folder's first folder write dependency, "" for none.
func folderWriteScope(m *wfdir.Manifest) string {
	for _, alias := range m.AliasNames() {
		if dep := m.Dependencies[alias]; markers.IsPathKind(dep.Kind) && dep.Write {
			return alias
		}
	}
	return ""
}

func printStackAddPlan(r *stackAddReport) {
	out := os.Stdout
	where := r.FeatureID
	if r.Feature != "" {
		where = fmt.Sprintf("%s (%s)", r.Feature, r.FeatureID)
	}
	if r.Space != "" {
		fmt.Fprintf(out, "stack %s → Space %s · feature %s — %d %s\n", r.Stack, r.Space, where, len(r.Folders), plural(len(r.Folders), "folder"))
	} else {
		fmt.Fprintf(out, "stack %s → feature %s — %d %s\n", r.Stack, where, len(r.Folders), plural(len(r.Folders), "folder"))
	}
	for _, f := range r.Folders {
		if f.Declares == "" && len(f.Proposed) == 0 && len(f.Unresolved) == 0 && len(f.LiftUnbound) == 0 && f.LiteralsSkipped == "" {
			fmt.Fprintf(out, "  %s  already declared, nothing to bind\n", f.Path)
			continue
		}
		if f.SpaceRecorded != "" {
			fmt.Fprintf(out, "  %s  Space recorded: %s\n", f.Path, f.SpaceRecorded)
		}
		for _, p := range f.Proposed {
			how := ""
			if p.Source != "" {
				how = fmt.Sprintf(" (the source of %s, the copy another stack binds)", p.Source)
			}
			fmt.Fprintf(out, "  %s  %s (%s) → %s %q%s\n", f.Path, p.Alias, p.Kind, p.ID, p.Title, how)
		}
		for _, u := range f.Unresolved {
			fmt.Fprintf(out, "  %s  %s (%s) — %s\n", f.Path, u.Alias, u.Kind, u.Detail)
		}
		for _, u := range f.LiftUnbound {
			fmt.Fprintf(out, "  %s  %s (%s) left unbound in stack %q — %s; bind it there by hand\n", f.Path, u.Alias, u.Kind, u.Stack, u.Detail)
		}
		if f.LiteralsSkipped != "" {
			fmt.Fprintf(out, "  %s  output literals not lifted: %s\n", f.Path, f.LiteralsSkipped)
		}
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "  %s  %s\n", s.Path, s.Detail)
	}
	fmt.Fprintln(out)
}

// writeStackAdd writes each folder's ronja.json, carrying its lift and its
// declaration together, then the code its lift rewrote: a manifest whose code
// still names the raw ids deploys the same rows, since each new dependency is
// bound to the id the code names.
func writeStackAdd(plans []*bindPlan, rels []string, bodies map[string]map[string]string, lifts []rawIDLift) error {
	lifted := map[string]bool{}
	for _, lift := range lifts {
		lifted[lift.root] = true
	}
	type step struct {
		name  string
		write func() error
	}
	var steps []step
	for i, p := range plans {
		root := p.f.Root
		if !p.writes() && !lifted[root] {
			continue
		}
		steps = append(steps, step{name: filepath.Join(rels[i], wfdir.ManifestName), write: p.write})
		for _, path := range sortedPaths(bodies[root]) {
			body := bodies[root][path]
			steps = append(steps, step{name: filepath.Join(rels[i], filepath.FromSlash(path)), write: func() error {
				return os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(body), 0o644)
			}})
		}
	}
	for i, st := range steps {
		if err := st.write(); err != nil {
			pending := make([]string, 0, len(steps)-i)
			for _, rest := range steps[i:] {
				pending = append(pending, rest.name)
			}
			return fmt.Errorf("%s: %w — these are not written yet, so write them by hand or run this again: %s", st.name, err, strings.Join(pending, ", "))
		}
	}
	return nil
}
