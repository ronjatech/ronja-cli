package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// rawIDLift is one raw id a folder's code names, lifted into a dependency
// bound to that same id in every stack the folder already declares.
type rawIDLift struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Alias  string `json:"alias"`
	Access string `json:"access"`
	// Literal is set for an output literal, which becomes {{ write('<alias>') }}.
	Literal bool `json:"literal,omitempty"`
	Line    int  `json:"line,omitempty"`

	root string
}

// planRawIDLifts finds the raw ids in each workflow, data app and pipeline
// folder's code. A marker's id becomes its alias, which every existing stack's
// codec turns back into the same id. An output literal is placed by the
// server's own scan (POST /workflow/validate), read in the existing stack on
// this organization; skipped says, per folder root, why a workflow's were not
// looked for.
func planRawIDLifts(ctx context.Context, resolved *config.Resolved, l *spaceLookup, found []discoveredFolder) (lifts []rawIDLift, skipped map[string]string, err error) {
	client := newClient(resolved.URL, resolved.Token)
	var out []rawIDLift
	skipped = map[string]string{}
	for _, d := range found {
		switch d.Kind.Name {
		case wfdir.KindWorkflow, wfdir.KindDataApp, wfdir.KindPipeline:
		default:
			continue
		}
		m := d.Manifest
		if len(m.Stacks) == 0 && len(m.Instances) == 0 {
			continue
		}
		files, _, err := readLocalFiles(d.Root, d.Kind)
		if err != nil {
			return nil, nil, err
		}
		lock, err := wfdir.LoadLock(d.Root)
		if err != nil {
			return nil, nil, err
		}
		own := map[string]bool{}
		for _, st := range lock.Stacks {
			for _, t := range st.Tables {
				own[t.TableID] = true
			}
		}
		taken := map[string]bool{}
		for alias := range m.Dependencies {
			taken[strings.ToLower(alias)] = true
		}
		if d.Kind.Name == wfdir.KindPipeline {
			for path := range files {
				taken[strings.ToLower(tableNameFor(path))] = true
			}
		}
		// A plain dependency every stack binds to the id, wherever it is bound,
		// is that id's alias, so the lift declares nothing beside it. One bound
		// elsewhere to another id is not: reusing it would move that stack off
		// the table its code names today.
		aliasOf := map[string]string{}
		for _, alias := range m.AliasNames() {
			dep := m.Dependencies[alias]
			if dep.ByAddress() || dep.Output {
				continue
			}
			binds := make([]map[string]string, 0, len(m.Stacks)+len(m.Instances))
			for _, name := range m.StackNames() {
				binds = append(binds, m.Stacks[name].Bind)
			}
			for _, inst := range m.Instances {
				binds = append(binds, inst.Bind)
			}
			bound := ""
			for _, bind := range binds {
				if id := bind[alias]; id != "" {
					if bound != "" && bound != id {
						bound = ""
						break
					}
					bound = id
				}
			}
			if bound != "" {
				if _, seen := aliasOf[targetKey(dep.Kind, bound)]; !seen {
					aliasOf[targetKey(dep.Kind, bound)] = alias
				}
			}
		}
		claim := func(kind, id string) string {
			key := targetKey(kind, id)
			if alias, ok := aliasOf[key]; ok {
				return alias
			}
			_, name, _, _ := l.rowFeature(kind, id)
			base := aliasCandidate(name, kind, id)
			alias := base
			for i := 2; taken[strings.ToLower(alias)]; i++ {
				alias = fmt.Sprintf("%s_%d", base, i)
			}
			taken[strings.ToLower(alias)] = true
			aliasOf[key] = alias
			return alias
		}
		add := func(lift rawIDLift) {
			lift.Folder, lift.root = d.Rel, d.Root
			for _, have := range out {
				if have.root == lift.root && have.Kind == lift.Kind && have.ID == lift.ID && have.Path == lift.Path && have.Literal == lift.Literal && have.Line == lift.Line {
					return
				}
			}
			lift.Alias = claim(lift.Kind, lift.ID)
			out = append(out, lift)
		}
		for _, path := range sortedPaths(files) {
			for _, o := range markers.Scan(files[path]) {
				kind := o.Family.Kind
				if o.Positional || !markers.IsResourceID(kind, o.Arg) || own[o.Arg] {
					continue
				}
				access := "read"
				switch kind {
				case markers.KindWorkflow, markers.KindAgent:
					access = "run"
				case markers.KindCodex:
					access = "write"
				}
				add(rawIDLift{Path: path, Kind: kind, ID: o.Arg, Access: access})
			}
			for _, o := range markers.ScanWrites(files[path]) {
				if markers.IsResourceID(markers.KindTable, o.Arg) && !own[o.Arg] {
					add(rawIDLift{Path: path, Kind: markers.KindTable, ID: o.Arg, Access: "write"})
				}
			}
		}
		if d.Kind.Name != wfdir.KindWorkflow {
			continue
		}
		stack, why := stackOnOrganization(m, resolved)
		if stack == "" {
			skipped[d.Root] = fmt.Sprintf("the server's scan reads them in the folder's one stack on this organization, and %s. An output literal left as an id writes that table in every stack, and a deploy that would write another Space's table is refused bind_space_mismatch: change it to {{ write('<name>') }} by hand", why)
			continue
		}
		in := api.ValidateInput{
			FeatureID:      m.Stacks[stack].FeatureID,
			Entrypoint:     m.Entrypoint,
			Files:          validateFilesOf(newAliasCodec(m.Dependencies, m.Stacks[stack].Bind), files),
			RuntimeVersion: m.RuntimeForValidate(nil),
			Language:       languageOnTheWire(m),
		}
		result, err := client.ValidateWorkflowFiles(ctx, in)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: find the output literals to lift (POST /workflow/validate): %w — nothing was written", d.Rel, err)
		}
		for _, t := range result.WriteTargets {
			if t.Position != api.WritePositionOutputLiteral || !markers.IsResourceID(markers.KindTable, t.Arg) || t.Arg != t.TargetID {
				continue
			}
			add(rawIDLift{Path: t.Path, Kind: markers.KindTable, ID: t.Arg, Access: "write", Literal: true, Line: t.Line})
		}
	}
	// A table both read and written in one folder is one write dependency.
	for i := range out {
		for _, other := range out {
			if other.root == out[i].root && other.Alias == out[i].Alias && other.Access == "write" {
				out[i].Access = "write"
			}
		}
	}
	return out, skipped, nil
}

// stackOnOrganization is the folder's stack on this credential's organization,
// or "" and why there is not exactly one.
func stackOnOrganization(m *wfdir.Manifest, resolved *config.Resolved) (string, string) {
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	var here []string
	for _, name := range m.StackNames() {
		if wfdir.Matches(m.Stacks[name].Key(), key) {
			here = append(here, name)
		}
	}
	switch len(here) {
	case 0:
		return "", "it declares none"
	case 1:
		return here[0], ""
	}
	return "", fmt.Sprintf("it declares several (%s)", strings.Join(here, ", "))
}

func printRawIDLifts(lifts []rawIDLift) {
	out := os.Stdout
	fmt.Fprintf(out, "  These raw ids would become dependencies, bound to the same id in each folder's other stacks (one written, run or used as a secret only where it is in that stack's Space):\n\n")
	for _, lift := range lifts {
		where := lift.Path
		if lift.Literal {
			where = fmt.Sprintf("%s:%d (output literal → {{ write('%s') }})", lift.Path, lift.Line, lift.Alias)
		}
		fmt.Fprintf(out, "    %s  %s  %s → %s (%s, %s)\n", lift.Folder, where, lift.ID, lift.Alias, lift.Kind, lift.Access)
	}
	fmt.Fprintln(out)
}

// distinctLiftIDs counts the rows the lifts name, each once however many
// places name it.
func distinctLiftIDs(lifts []rawIDLift) int {
	seen := map[string]bool{}
	for _, lift := range lifts {
		seen[targetKey(lift.Kind, lift.ID)] = true
	}
	return len(seen)
}

// liftsIn is the lifts in the folder at root.
func liftsIn(lifts []rawIDLift, root string) []rawIDLift {
	var mine []rawIDLift
	for _, lift := range lifts {
		if lift.root == root {
			mine = append(mine, lift)
		}
	}
	return mine
}

// liftedBodies is each folder's code as the lift rewrites it, by root and
// path, holding only the files it changes. Nothing is written.
func liftedBodies(found []discoveredFolder, lifts []rawIDLift) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, d := range found {
		mine := liftsIn(lifts, d.Root)
		if len(mine) == 0 {
			continue
		}
		files, _, err := readLocalFiles(d.Root, d.Kind)
		if err != nil {
			return nil, err
		}
		alias := map[aliasKey]string{}
		for _, lift := range mine {
			if !lift.Literal {
				alias[aliasKey{kind: lift.Kind, name: lift.ID}] = lift.Alias
			}
		}
		changed := map[string]string{}
		for _, path := range sortedPaths(files) {
			body := rewriteMarkers(files[path], func(o markers.Occurrence) string {
				if a, ok := alias[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
					return a
				}
				return o.Arg
			})
			body, err = markers.RewriteWrites(body, func(o markers.Occurrence) string {
				if a, ok := alias[aliasKey{kind: markers.KindTable, name: o.Arg}]; ok {
					return a
				}
				return o.Arg
			})
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w — nothing was written", d.Rel, path, err)
			}
			for _, lift := range mine {
				if lift.Literal && lift.Path == path {
					if body, err = liftOutputLiteral(body, lift); err != nil {
						return nil, fmt.Errorf("%s/%s:%d: %w — nothing was written", d.Rel, path, lift.Line, err)
					}
				}
			}
			if body != files[path] {
				changed[path] = body
			}
		}
		out[d.Root] = changed
	}
	return out, nil
}

// liftIntoManifest declares a folder's lifted dependencies in memory, each
// bound to its id in every stack and legacy entry that does not bind it yet,
// except the stack being declared, whose picker answers it. A dependency the
// lift reuses keeps its own fields and gains the access the code needs.
//
// A stack that records a Space binds an id it would write, run or use as a
// secret only when the row is in that Space: the Space rule refuses anything
// else at its next deploy, so it is left unbound there and returned, to bind
// by hand. A read is bound wherever it is, as the code reads it today.
func liftIntoManifest(f *folder, l *spaceLookup, declaring string, mine []rawIDLift) []liftUnbound {
	if len(mine) == 0 {
		return nil
	}
	m := f.Manifest
	if m.Dependencies == nil {
		m.Dependencies = map[string]wfdir.Dependency{}
	}
	var unbound []liftUnbound
	for _, lift := range mine {
		dep := m.Dependencies[lift.Alias]
		dep.Kind = lift.Kind
		dep.Write = dep.Write || lift.Access == "write"
		dep.Run = dep.Run || lift.Access == "run"
		m.Dependencies[lift.Alias] = dep
		bound := []bindProposal{{Alias: lift.Alias, ID: lift.ID}}
		for _, name := range m.StackNames() {
			st := m.Stacks[name]
			if name == declaring || st.Bind[lift.Alias] != "" {
				continue
			}
			if why := liftOutsideSpace(l, st, lift); why != "" {
				if !containsLiftUnbound(unbound, name, lift.Alias) {
					unbound = append(unbound, liftUnbound{Stack: name, Alias: lift.Alias, Kind: lift.Kind, Detail: why})
				}
				continue
			}
			st.Bind = mergedBind(st.Bind, bound)
			m.Stacks[name] = st
		}
		for i := range m.Instances {
			if m.Instances[i].Bind[lift.Alias] == "" {
				m.Instances[i].Bind = mergedBind(m.Instances[i].Bind, bound)
			}
		}
	}
	if st, declared := m.Stacks[f.Stack]; declared {
		f.Bind = st.Bind
	}
	f.Codec = newAliasCodec(m.Dependencies, f.codecBind())
	return unbound
}

// liftUnbound is a lifted alias left unbound in an existing stack.
type liftUnbound struct {
	Stack  string `json:"stack"`
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

func containsLiftUnbound(list []liftUnbound, stack, alias string) bool {
	for _, u := range list {
		if u.Stack == stack && u.Alias == alias {
			return true
		}
	}
	return false
}

// liftOutsideSpace says why st, a stack that records a Space, may not be
// bound to the lifted id, or "" when it may.
func liftOutsideSpace(l *spaceLookup, st wfdir.Stack, lift rawIDLift) string {
	if st.SpaceID == "" || lift.Access == "read" && lift.Kind != markers.KindSecret {
		return ""
	}
	in, featureID, _, ok, err := l.rowSpace(lift.Kind, lift.ID)
	switch {
	case !ok:
		return fmt.Sprintf("%s is a %s this ronja cannot place in a Space", lift.ID, lift.Kind)
	case err != nil:
		return fmt.Sprintf("which Space %s is in could not be read (%v)", lift.ID, err)
	case featureID == "":
		return fmt.Sprintf("%s is in no feature and so in no Space", lift.ID)
	case in != st.SpaceID:
		return fmt.Sprintf("%s is in %s, and this stack writes, runs and uses secrets only in %s", lift.ID, l.describeSpace(in), l.describeSpace(st.SpaceID))
	}
	return ""
}

// liftOutputLiteral replaces the quoted id on the literal's line with the
// write marker. The line must hold the quoted id exactly once, or nothing can
// say which one the server meant.
func liftOutputLiteral(body string, lift rawIDLift) (string, error) {
	lines := strings.SplitAfter(body, "\n")
	if lift.Line < 1 || lift.Line > len(lines) {
		return "", fmt.Errorf("the server placed the output literal %s on a line the file does not have, so it cannot be lifted — edit it by hand", lift.ID)
	}
	line := lines[lift.Line-1]
	marker := fmt.Sprintf("{{ write('%s') }}", lift.Alias)
	var quoted string
	for _, q := range []string{`"`, `'`} {
		if strings.Count(line, q+lift.ID+q) == 1 {
			if quoted != "" {
				quoted = ""
				break
			}
			quoted = q + lift.ID + q
		}
	}
	if quoted == "" {
		return "", fmt.Errorf("the line does not hold the quoted id %s exactly once, so the output literal cannot be lifted — edit it by hand", lift.ID)
	}
	lines[lift.Line-1] = strings.Replace(line, quoted, marker, 1)
	return strings.Join(lines, ""), nil
}
