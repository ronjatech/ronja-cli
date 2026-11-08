package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja copy` snapshots tables into a stack's feature through
// POST /feature/model/:id/copy and binds the folder's dependency to the copy in
// that stack. The route matches an earlier copy by its source, so a second run
// refreshes the same table and leaves ronja.json as it was.
func newCopyCmd() *cobra.Command {
	var toStack, fromStack, name string
	var noRows bool
	cmd := &cobra.Command{
		Use:   "copy <dependency|table-id>... --to-stack <name>",
		Short: "Copy tables into a stack's feature as snapshots, and bind this folder to the copies",
		Long: `Copy tables into a stack's feature as snapshots, and bind this folder to the copies.

  ronja copy erp_orders erp_items --to-stack dev        # dependencies, from the stack that binds them
  ronja copy erp_orders --to-stack dev --from-stack main
  ronja copy table-abc123 --to-stack dev                # a table by id
  ronja copy board_notes --no-rows --to-stack dev       # the columns only

A dependency is copied from the row another stack of this folder on the same
organization binds it to (--from-stack when several do); copies never cross
organizations. A table given by id is copied as it is, and bound under the
dependency the --to-stack stack already binds to that table or its copy, or
else a new dependency named after it.

Each copy is a dynamic table in the --to-stack stack's feature, holding the
source's rows as they are now, and its id is written into that stack's "bind".
Run it again to refresh: the same table is refilled and keeps its id, so
ronja.json does not change. --no-rows creates the table with the source's
columns and no rows (or empties the copy), for a table the code writes.

A copy is synchronous and is refused above 1 GB; one that does not finish
within about 80 seconds answers copy_timeout, and running it again is safe.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if toStack == "" {
				return fmt.Errorf("--to-stack is required: the stack whose feature the copies go into, and whose \"bind\" they are written to")
			}
			if name != "" && len(args) > 1 {
				return fmt.Errorf("--name names one copy, and %d tables were given — copy that one on its own", len(args))
			}
			flagStack = toStack
			resolved, err := resolveDeployInstance(false)
			if err != nil {
				return err
			}
			kind, err := folderKindHere()
			if err != nil {
				return err
			}
			if kind.Name == wfdir.KindModule {
				return fmt.Errorf("this is a %s folder, and `ronja copy` binds the copies to dependencies: %s", kind.Label, moduleDependenciesInert)
			}
			f, err := openFolder(cmd.Context(), resolved, kind)
			if err != nil {
				return err
			}
			report, err := runCopy(cmd.Context(), f, args, copyOptions{from: fromStack, name: name, noRows: noRows})
			if report != nil {
				if flagJSON {
					if jerr := emitJSON(report); jerr != nil && err == nil {
						err = jerr
					}
				} else {
					printCopyReport(report)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&toStack, "to-stack", "", "the stack whose feature the copies are made in, and whose bind they are written to")
	cmd.Flags().StringVar(&fromStack, "from-stack", "", "the stack whose bind names each dependency's source (default: the one other stack that binds it)")
	cmd.Flags().StringVar(&name, "name", "", "name for a copy that is created (one table only; default: the source's name)")
	cmd.Flags().BoolVar(&noRows, "no-rows", false, "create the table with the source's columns and no rows")
	return cmd
}

type copyOptions struct {
	from, name string
	noRows     bool
}

// copyReport is one run: what each argument became.
type copyReport struct {
	Stack    string       `json:"stack"`
	Space    string       `json:"space,omitempty"`
	Manifest string       `json:"manifest"`
	Copies   []copyResult `json:"copies"`
	// Bound lists the aliases whose bind this run wrote or changed.
	Bound   []string `json:"bound"`
	Written bool     `json:"written"`
}

type copyResult struct {
	Arg   string `json:"arg"`
	Alias string `json:"alias,omitempty"`
	// From is the stack whose bind named the source, empty for a table id.
	From       string `json:"from,omitempty"`
	SourceID   string `json:"sourceID,omitempty"`
	SourceName string `json:"sourceName,omitempty"`
	TableID    string `json:"tableID,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	RowsCopied int    `json:"rowsCopied"`
	NoRows     bool   `json:"noRows,omitempty"`
	// Declared is true when this run added the dependency to the folder.
	Declared bool `json:"declared,omitempty"`
	// Reason / Detail are set when the copy failed: copy_timeout, name_taken,
	// or the instance's own refusal.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// copySource is what one argument names.
type copySource struct {
	alias, from, id string
}

func runCopy(ctx context.Context, f *folder, args []string, opts copyOptions) (*copyReport, error) {
	stack, declared := f.Manifest.Stacks[f.Stack]
	if f.Stack == "" || !declared || stack.FeatureID == "" {
		return nil, fmt.Errorf("%s declares no stack %q with a feature to copy into — declare it first with `ronja sync stack add` or `ronja bind --stack <name> --feature <id>`",
			wfdir.ManifestPath(f.Root), flagStack)
	}
	if opts.from != "" {
		from, ok := f.Manifest.Stacks[opts.from]
		if !ok || opts.from == f.Stack {
			return nil, fmt.Errorf("--from-stack %q is not another stack of %s (it declares %s)", opts.from, wfdir.ManifestName, strings.Join(f.Manifest.StackNames(), ", "))
		}
		if !wfdir.Matches(from.Key(), stack.Key()) {
			return nil, fmt.Errorf("stack %q is organization %s on %s and stack %q is organization %s on %s, and copies never cross organizations — copy from a stack on %s's organization, or pass a table id there",
				opts.from, from.TenantID, from.URL, f.Stack, stack.TenantID, stack.URL, f.Stack)
		}
	}
	client := f.client()
	sources := make([]copySource, 0, len(args))
	for _, arg := range args {
		src, err := resolveCopySource(ctx, client, f, arg, opts.from)
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}

	report := &copyReport{Stack: f.Stack, Space: spaceLabel(stack), Manifest: wfdir.ManifestPath(f.Root), Copies: []copyResult{}, Bound: []string{}}
	deps := maps.Clone(f.Manifest.Dependencies)
	if deps == nil {
		deps = map[string]wfdir.Dependency{}
	}
	bind := maps.Clone(stack.Bind)
	if bind == nil {
		bind = map[string]string{}
	}
	failed := 0
	for i, src := range sources {
		res := copyResult{Arg: args[i], Alias: src.alias, From: src.from, SourceID: src.id, NoRows: opts.noRows}
		if source, err := client.GetTable(ctx, src.id); err == nil {
			res.SourceName = source.Name
		}
		in := api.CopyTableInput{FeatureID: stack.FeatureID, Name: opts.name}
		if opts.noRows {
			rows := false
			in.Rows = &rows
		}
		out, err := client.CopyTable(ctx, src.id, in)
		if err != nil {
			res.Reason, res.Detail = copyFailure(err, args[i], f.Stack)
			report.Copies = append(report.Copies, res)
			failed++
			continue
		}
		res.TableID, res.Outcome, res.RowsCopied = out.Table.ID, out.Outcome, out.RowsCopied
		if src.alias == "" {
			src.alias = boundCopyAlias(deps, bind, out.Table.ID, src.id)
			if src.alias == "" {
				src.alias = copyAliasFor(deps, out.Table.Name, src.id)
			}
			res.Alias = src.alias
		}
		if _, ok := deps[src.alias]; !ok {
			deps[src.alias] = wfdir.Dependency{Kind: markers.KindTable}
			res.Declared = true
		}
		if bind[src.alias] != out.Table.ID {
			bind[src.alias] = out.Table.ID
			report.Bound = append(report.Bound, src.alias)
		}
		report.Copies = append(report.Copies, res)
	}

	if len(report.Bound) > 0 {
		f.Manifest.Dependencies = deps
		stack.Bind = bind
		f.Manifest.Stacks[f.Stack] = stack
		if err := f.saveFolder(); err != nil {
			return report, fmt.Errorf("write %s: %w", wfdir.ManifestPath(f.Root), err)
		}
		report.Written = true
	}
	if failed > 0 {
		return report, withExitCode(1, fmt.Errorf("%d of %d %s could not be copied — see above", failed, len(sources), plural(len(sources), "table")))
	}
	return report, nil
}

// resolveCopySource reads what one argument names: a table by id, or a
// declared table dependency's row in the stack it is copied from.
func resolveCopySource(ctx context.Context, client *api.Client, f *folder, arg, from string) (copySource, error) {
	m := f.Manifest
	target := m.Stacks[f.Stack].Key()
	// A source is another stack of the target's organization.
	sources := []string{}
	elsewhere := []string{}
	for _, name := range m.StackNames() {
		switch {
		case name == f.Stack:
		case wfdir.Matches(m.Stacks[name].Key(), target):
			sources = append(sources, name)
		default:
			elsewhere = append(elsewhere, name)
		}
	}
	if markers.IsResourceID(markers.KindTable, arg) {
		// A table id another stack already binds is that dependency's source.
		for _, alias := range m.AliasNames() {
			if dep := m.Dependencies[alias]; dep.Kind != markers.KindTable || dep.ByAddress() || dep.Output {
				continue
			}
			for _, name := range sources {
				if m.Stacks[name].Bind[alias] == arg {
					return copySource{alias: alias, from: name, id: arg}, nil
				}
			}
		}
		return copySource{id: arg}, nil
	}
	dep, ok := m.Dependencies[arg]
	switch {
	case !ok:
		return copySource{}, fmt.Errorf("%q is neither a table id nor a dependency %s declares — name a declared table dependency, or pass the table's id", arg, wfdir.ManifestName)
	case dep.Kind != markers.KindTable:
		return copySource{}, fmt.Errorf("%q is a %s dependency, and only tables are copied", arg, dep.Kind)
	case dep.ByAddress() || dep.Output:
		return copySource{}, fmt.Errorf("%q resolves through another folder or this folder's own output, not a bind, so each stack already gets its own — there is nothing to copy", arg)
	}
	if from != "" {
		id := m.Stacks[from].Bind[arg]
		if id == "" {
			return copySource{}, fmt.Errorf("stack %q does not bind %q, so there is no source to copy it from — bind it there first, or pass the table's id", from, arg)
		}
		return copySource{alias: arg, from: from, id: id}, nil
	}
	var binding, bindingElsewhere []string
	for _, name := range sources {
		if m.Stacks[name].Bind[arg] != "" {
			binding = append(binding, name)
		}
	}
	for _, name := range elsewhere {
		if m.Stacks[name].Bind[arg] != "" {
			bindingElsewhere = append(bindingElsewhere, name)
		}
	}
	switch {
	case len(binding) == 0 && len(bindingElsewhere) > 0:
		return copySource{}, fmt.Errorf("only stacks of another organization (%s) bind %q, and copies never cross organizations — bind it in a stack on %s's organization, or pass the table's id", strings.Join(bindingElsewhere, ", "), arg, f.Stack)
	case len(binding) == 0:
		return copySource{}, fmt.Errorf("no other stack of %s binds %q, so there is no source to copy it from — bind it in the stack that has it, or pass the table's id", wfdir.ManifestName, arg)
	case len(binding) == 1:
		return copySource{alias: arg, from: binding[0], id: m.Stacks[binding[0]].Bind[arg]}, nil
	}
	return copySource{}, fmt.Errorf("stacks %s all bind %q — say which one to copy it from with --from-stack", strings.Join(binding, ", "), arg)
}

// boundCopyAlias is the plain table dependency the target stack already binds
// to the copy, or else to the source, "" for none.
func boundCopyAlias(deps map[string]wfdir.Dependency, bind map[string]string, copyID, sourceID string) string {
	for _, id := range []string{copyID, sourceID} {
		for _, alias := range slices.Sorted(maps.Keys(deps)) {
			if dep := deps[alias]; dep.Kind == markers.KindTable && !dep.ByAddress() && !dep.Output && bind[alias] == id {
				return alias
			}
		}
	}
	return ""
}

// copyAliasFor names the dependency a table copied by id is declared under:
// after the table, free of every alias already declared.
func copyAliasFor(deps map[string]wfdir.Dependency, tableName, id string) string {
	base := aliasCandidate(tableName, markers.KindTable, id)
	alias := base
	for i := 2; ; i++ {
		taken := false
		for existing := range deps {
			if strings.EqualFold(existing, alias) {
				taken = true
				break
			}
		}
		if !taken {
			return alias
		}
		alias = fmt.Sprintf("%s_%d", base, i)
	}
}

// copyFailure is a failed copy's reason and the sentence a person reads.
func copyFailure(err error, arg, stack string) (reason, detail string) {
	if msg, ok := api.AsNameTaken(err); ok {
		return api.NameTakenCode, fmt.Sprintf("%s. Copy it under another name: `ronja copy %s --to-stack %s --name <name>`", strings.TrimSuffix(msg, "."), arg, stack)
	}
	if api.WireCodeOf(err) == api.CopyTimeoutCode {
		return api.CopyTimeoutCode, fmt.Sprintf("%s Nothing was bound in %s; running the same `ronja copy` again is safe.", serverMessage(err), wfdir.ManifestName)
	}
	if api.IsTimeout(err) {
		return api.CopyTimeoutCode, fmt.Sprintf("the instance did not answer the copy within 120 seconds, so it may still finish. Nothing was bound in %s; run the same `ronja copy` again, which refreshes the same table", wfdir.ManifestName)
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusGatewayTimeout && api.WireCodeOf(err) == "" {
		// A load balancer's own 504: it gave up before the route's limit, and the
		// copy runs on.
		return api.CopyTimeoutCode, fmt.Sprintf("the copy is probably still writing; wait a minute, then run the same command (a re-run refreshes the same table, never a second one). Nothing was bound in %s (HTTP 504)", wfdir.ManifestName)
	}
	if errors.As(err, &apiErr) {
		return api.WireCodeOf(err), fmt.Sprintf("%s (HTTP %d)", serverMessage(err), apiErr.Status)
	}
	return "", err.Error()
}

func printCopyReport(r *copyReport) {
	out := os.Stdout
	if r.Space != "" {
		fmt.Fprintf(out, "stack %s · Space %s\n", r.Stack, r.Space)
	} else {
		fmt.Fprintf(out, "stack %s\n", r.Stack)
	}
	width := 0
	for _, c := range r.Copies {
		width = max(width, len(cmp.Or(c.Alias, c.Arg)))
	}
	for _, c := range r.Copies {
		label := cmp.Or(c.Alias, c.Arg)
		from := c.SourceID
		if c.SourceName != "" {
			from = fmt.Sprintf("%s (%s)", c.SourceName, c.SourceID)
		}
		if c.From != "" {
			from = fmt.Sprintf("stack %s's %s", c.From, from)
		}
		switch {
		case c.Reason != "" || c.Detail != "":
			fmt.Fprintf(out, "  %-*s  not copied (%s): %s\n", width, label, orUnknown(c.Reason), c.Detail)
		case c.NoRows:
			fmt.Fprintf(out, "  %-*s  → %s %s, empty, columns from %s\n", width, label, c.TableID, c.Outcome, from)
		default:
			fmt.Fprintf(out, "  %-*s  → %s %s, %d %s copied from %s\n", width, label, c.TableID, c.Outcome, c.RowsCopied, plural(c.RowsCopied, "row"), from)
		}
	}
	if r.Written {
		fmt.Fprintf(out, "Bound in stacks.%s.bind: %s\n", r.Stack, strings.Join(r.Bound, ", "))
	}
}
