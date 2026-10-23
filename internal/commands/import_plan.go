package commands

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The PLAN half of `ronja import feature`: which folder each
// live resource of the feature lands in, at which address, and — the
// portability pass — how each reference it makes is written, decided from the
// server's reference inventory and nothing else.
//
// The pass is a classification, one row per kind of reference:
//
//	a read of a table built by the same pipeline folder   → its sibling stem
//	a read or run of anything imported into another folder → an ADDRESS dependency
//	{{ write('table-X') }} where X is this workflow's own output → an OUTPUT dependency (the name form)
//	any other write position (write marker, output literal, codex, a reference-set
//	table, a reference-set feature other than its own)     → a WRITE-CAPABLE data source
//	a read of anything not imported (or claimed elsewhere)  → a READ data source
//	a secret, mailbox or MCP server                         → a SIDE-EFFECT data source
//	an agent or a workflow not imported                     → a RUN data source ("run": true)
//	an automation reference to its own feature             → "@feature"
//	an output literal or an app TSX literal                → left in place, and reported
//
// Every data source is bound in the importing stack only. The ALIAS form is
// written two ways, both of which round-trip: in a position the codec reads
// both ways (markers, optionsQuery, write markers) by declaring the dependency
// and de-aliasing the server's content through the folder's own codec — the
// same toDisk every later status runs — and in an outbound-only position (an
// app's access block, a metric's recipe source, an automation's fields) by
// writing the alias directly.

// importFolder is one folder the import writes.
type importFolder struct {
	Kind wfdir.Kind
	// Rel is the folder's path under the import root, slash-separated.
	Rel  string
	Root string
	rows []*importRow

	deps map[string]wfdir.Dependency
	// bind is the importing stack's bind map (data sources and outputs).
	bind map[string]string
	// addressIDs is what each address dependency resolves to NOW — used only to
	// build the codec the import writes through; never written anywhere (an
	// address resolves through the other folder's claim at every later run).
	addressIDs map[string]string
	// aliasOf names the alias already given to a target, keyed by
	// targetKey; taken holds every name in use (folded), stems included.
	aliasOf map[string]string
	taken   map[string]bool

	// What the folder was read as, for the write step.
	pipeline *pipelineCloneSet
	// pristine is the pipeline as the server answered it, before the
	// portability pass rewrote its SQL and recipes — what a second pass
	// (settle) starts from.
	pristine    *pipelineCloneSet
	workflow    *api.Workflow
	app         *api.DataApp
	module      *api.Module
	automations map[string]*api.Automation // address → row
	// automationBodies are the rendered automation files, address → body.
	automationBodies map[string]string
	access           *api.DataAppAccess

	FolderID string
	// withdrawn: the folder's one row was refused a claim, and the folder was
	// removed from the tree.
	withdrawn bool
}

// importRow is one imported resource.
type importRow struct {
	res     api.ReferenceResource
	folder  *importFolder
	address string
}

func newImportFolder(kind wfdir.Kind, rel, base string) *importFolder {
	return &importFolder{
		Kind: kind, Rel: rel, Root: filepath.Join(base, filepath.FromSlash(rel)),
		deps: map[string]wfdir.Dependency{}, bind: map[string]string{}, addressIDs: map[string]string{},
		aliasOf: map[string]string{}, taken: map[string]bool{},
	}
}

// seedTaken records the names the folder's own files already use — a
// pipeline's stems, an automation folder's file names — so no alias takes one.
func (f *importFolder) seedTaken() {
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		for _, s := range f.pipeline.sources {
			f.taken[strings.ToLower(tableNameFor(s.Path))] = true
		}
		for _, m := range f.pipeline.metrics {
			f.taken[strings.ToLower(tableNameFor(m.Path))] = true
		}
	case wfdir.KindAutomation:
		for address := range f.automations {
			f.taken[strings.ToLower(automationNameFor(address))] = true
		}
	}
}

// clearPlan forgets what a portability pass decided for the folder.
func (f *importFolder) clearPlan() {
	f.deps, f.bind, f.addressIDs = map[string]wfdir.Dependency{}, map[string]string{}, map[string]string{}
	f.aliasOf, f.taken = map[string]string{}, map[string]bool{}
	f.access, f.automationBodies = nil, nil
}

// resetPlan returns a WORKFLOW folder to where it stood before its
// portability pass, rows and all, for planPortability's second pass.
func (f *importFolder) resetPlan() {
	f.clearPlan()
	f.seedTaken()
}

// replanWithout drops the rows in gone from the folder and clears every
// decision the portability pass made, so it can run again over what is left:
// a pipeline starts again from the server's content, minus the dropped rows'
// files (a table's checks file with it).
func (f *importFolder) replanWithout(gone map[string]bool) {
	kept := f.rows[:0:0]
	for _, row := range f.rows {
		if !gone[row.res.ID] {
			kept = append(kept, row)
		}
	}
	f.rows = kept
	f.clearPlan()
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		set := &pipelineCloneSet{skipped: f.pristine.skipped}
		stems := map[string]bool{}
		for _, s := range f.pristine.sources {
			if !gone[s.TableID] {
				set.sources = append(set.sources, s)
				stems[wfdir.ChecksPath(tableNameFor(s.Path))] = true
			}
		}
		for _, m := range f.pristine.metrics {
			if !gone[m.MetricID] {
				set.metrics = append(set.metrics, m)
			}
		}
		for _, c := range f.pristine.checks {
			if stems[c.Path] {
				set.checks = append(set.checks, c)
			}
		}
		f.pipeline = set
	case wfdir.KindAutomation:
		for address, auto := range f.automations {
			if gone[auto.ID] {
				delete(f.automations, address)
			}
		}
	}
	f.seedTaken()
}

// codec is the folder's alias codec as the import writes it: its declarations,
// bound in the importing stack, with each address resolved to the row it names
// today.
func (f *importFolder) codec() aliasCodec {
	bind := make(map[string]string, len(f.bind)+len(f.addressIDs))
	for k, v := range f.bind {
		bind[k] = v
	}
	for k, v := range f.addressIDs {
		bind[k] = v
	}
	return newAliasCodec(f.deps, bind)
}

// hooks is what the shared clone writers need to lay this folder down.
func (f *importFolder) hooks() cloneHooks {
	return cloneHooks{codec: f.codec(), deps: f.deps, bind: f.bind, folderID: f.FolderID, access: f.access, recordTitle: true}
}

// uniqueSegment returns a directory or file name not yet in used (folded), with
// a -2, -3 … suffix on a collision, and records it. Two rows with names that
// slug alike — "Orders" and "orders" — get two folders rather than one folder
// overwriting the other.
func uniqueSegment(used map[string]bool, base, ext string) string {
	name := base + ext
	for i := 2; used[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	used[strings.ToLower(name)] = true
	return name
}

// importDataSource is one data source the import declared, for the report.
type importDataSource struct {
	Folder string `json:"folder"`
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	// Access is read, write, run or side_effect — what the folder's code does
	// to the row, and what a stack binding it must decide deliberately.
	Access string `json:"access"`
	Stack  string `json:"stack"`
	// BoundTo is the id the importing stack binds the alias to.
	BoundTo string `json:"boundTo"`
	// Readable is false for a row this credential cannot read: bound to the
	// id the reference carries, and — for a write — treated as outside the
	// feature.
	Readable bool `json:"readable"`
}

// importAddress is one address dependency the import declared.
type importAddress struct {
	Folder  string `json:"folder"`
	Alias   string `json:"alias"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Address string `json:"address"`
}

// importOutput is one workflow's own output the import declared by its name.
type importOutput struct {
	Folder string `json:"folder"`
	Alias  string `json:"alias"`
	Stack  string `json:"stack"`
	// BoundTo is the row the importing stack already holds; another stack may
	// leave it unbound and send the name.
	BoundTo string `json:"boundTo"`
}

// importLiteral is one literal id (or folder path) the import LEFT IN PLACE —
// a position no codec rewrites — with where it is.
type importLiteral struct {
	Folder     string `json:"folder"`
	Path       string `json:"path"`
	Line       int    `json:"line,omitempty"`
	Position   string `json:"position"`
	Call       string `json:"call,omitempty"`
	TargetKind string `json:"targetKind"`
	TargetID   string `json:"targetID,omitempty"`
	Arg        string `json:"arg,omitempty"`
	// Alias is the write-capable data source declared for an output literal's
	// table, empty for the others.
	Alias string `json:"alias,omitempty"`
	Why   string `json:"why"`
}

// importNotCarried is a binding a resource holds in a STRUCTURED field — set
// outside its code, or left from code since changed — which no file of the
// folder carries, with what that means where the tree creates the resource anew.
// Deploying back to the same row leaves the binding as it is: a push never
// sends these fields.
type importNotCarried struct {
	Folder       string `json:"folder"`
	ResourceKind string `json:"resourceKind"`
	ResourceID   string `json:"resourceID"`
	Field        string `json:"field"`
	TargetKind   string `json:"targetKind"`
	TargetID     string `json:"targetID,omitempty"`
	TargetPath   string `json:"targetPath,omitempty"`
	Effect       string `json:"effect"`
}

// notCarriedFor describes one structured-field reference the folder does not
// carry.
func notCarriedFor(f *importFolder, row *importRow, ref api.ReferencePosition) importNotCarried {
	field, _, _ := strings.Cut(ref.Field, "[")
	var effect string
	switch field {
	case "explicitSecretIDs":
		effect = "a secret bound to the workflow outside its code; where the tree creates the workflow anew, it runs without that secret"
	case "inputModels":
		effect = "an input the table lists and its SQL no longer reads; where the tree creates the table anew, it does not list it"
	case "fileScopes":
		effect = "a folder the workflow was allowed to write outside its code; where the tree creates the workflow anew, it may not write there"
	case "secretID":
		effect = "a credential bound outside any file; where the tree creates the resource anew, it has none"
	default:
		effect = fmt.Sprintf("a %s binding no line of the current code makes (left from earlier code); where the tree creates the %s anew, it is not bound", field, row.res.Kind)
	}
	return importNotCarried{Folder: f.Rel, ResourceKind: row.res.Kind, ResourceID: row.res.ID, Field: ref.Field,
		TargetKind: ref.TargetKind, TargetID: ref.TargetID, TargetPath: ref.TargetPath, Effect: effect}
}

// importClaimedElsewhere is a row of the feature another folder already
// manages, so the import left it there and treated references to it as data
// sources.
type importClaimedElsewhere struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	FolderID string `json:"folderID"`
	Address  string `json:"address,omitempty"`
}

// depKindOf maps an inventory target kind onto the dependency kind an alias
// for it declares; "" for one no alias names (a folder path).
func depKindOf(targetKind string) string {
	switch targetKind {
	case api.RefKindTable, api.RefKindMetric:
		return markers.KindTable
	case api.RefKindWorkflow:
		return markers.KindWorkflow
	case api.RefKindDataApp:
		return markers.KindDataApp
	case api.RefKindModule:
		return markers.KindModule
	case api.RefKindSecret:
		return markers.KindSecret
	case api.RefKindAgent:
		return markers.KindAgent
	case api.RefKindCodex:
		return markers.KindCodex
	case api.RefKindMailbox:
		return markers.KindMailbox
	case api.RefKindNote:
		return markers.KindNote
	case api.RefKindMCPServer:
		return markers.KindMCPServer
	case api.RefKindFeature:
		return markers.KindFeature
	}
	return ""
}

// targetKey identifies one target within a folder: its dependency kind and id.
func targetKey(depKind, id string) string { return depKind + "\x00" + id }

// aliasCandidate turns a row's name into a legal alias (wfdir.ValidAliasName):
// letters, digits, `_`, `-` and `.`, starting with a letter or digit, never all
// digits and never shaped like an id. A row whose name yields nothing usable —
// or one this credential cannot read, which has no name — is named by its kind
// and id suffix.
func aliasCandidate(name, depKind, id string) string {
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
			underscore = false
		default:
			if !underscore && b.Len() > 0 {
				b.WriteByte('_')
				underscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_-.")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "_-.")
	}
	if out == "" || wfdir.ValidAliasName(out) != nil || markers.IsResourceID(depKind, out) {
		suffix := id
		if i := strings.LastIndex(id, "-"); i >= 0 {
			suffix = id[i+1:]
		}
		if len(suffix) > 12 {
			suffix = suffix[len(suffix)-12:]
		}
		out = depKind + "_" + strings.ToLower(suffix)
		if wfdir.ValidAliasName(out) != nil {
			out = depKind + "_ref"
		}
	}
	return out
}

// claimAlias gives a target its alias in this folder — the one it already has,
// or a new name free of every alias and stem in use.
func (f *importFolder) claimAlias(depKind, id, name string) string {
	if alias, ok := f.aliasOf[targetKey(depKind, id)]; ok {
		return alias
	}
	base := aliasCandidate(name, depKind, id)
	alias := base
	for i := 2; f.taken[strings.ToLower(alias)]; i++ {
		alias = fmt.Sprintf("%s_%d", base, i)
	}
	f.taken[strings.ToLower(alias)] = true
	f.aliasOf[targetKey(depKind, id)] = alias
	return alias
}

// relFrom is the slash path from one folder to another, which is what an
// address dependency's "from" holds.
func relFrom(from, to *importFolder) string {
	rel, err := filepath.Rel(from.Root, to.Root)
	if err != nil {
		return path.Join("..", to.Rel)
	}
	return filepath.ToSlash(rel)
}

// targetFacts is everything the folder's resources say about one target,
// gathered before any alias is chosen so one decision covers every position.
type targetFacts struct {
	depKind  string
	id       string
	name     string
	readable bool
	// write: some position may change it (and is not this workflow's own
	// output); output: a `{{ write() }}` naming this workflow's own output,
	// with its logical name.
	write       bool
	output      string
	direction   string
	featureSelf bool
}

// portability runs the pass over one folder: it decides, for every target its
// resources reference, the form it is written in, and records the literals it
// must leave. pipelineStems maps a table id the SAME pipeline folder builds to
// its stem; byID is every imported row.
func (imp *featureImport) portability(f *importFolder, byID map[string]*importRow) {
	facts := map[string]*targetFacts{}
	order := []string{}
	note := func(depKind string, ref api.ReferencePosition) *targetFacts {
		key := targetKey(depKind, ref.TargetID)
		t := facts[key]
		if t == nil {
			t = &targetFacts{depKind: depKind, id: ref.TargetID, direction: ref.Direction}
			facts[key] = t
			order = append(order, key)
		}
		if ref.Readable {
			t.readable = true
			if t.name == "" {
				t.name = ref.TargetName
			}
		}
		return t
	}
	for _, row := range f.rows {
		for _, ref := range row.res.References {
			switch {
			case ref.Position == api.RefPosStructuredField:
				// A binding no line of the folder's files makes: nothing the
				// folder writes carries it, so it is reported as not carried.
				imp.notCarried = append(imp.notCarried, notCarriedFor(f, row, ref))
				continue
			case ref.TargetKind == api.RefKindFolder:
				// A path, not a row: a read is left alone silently. A WRITE is
				// left in place and reported, and its path is declared as a
				// folder write source (`"kind": "folder", "write": true`) bound
				// in this stack: a folder is shared by every feature of the
				// organization, so the write gate admits a write into it only
				// for a stack that binds it — and a stack added later does not
				// inherit the bind.
				if ref.Writes() && ref.Path != "" {
					imp.literals = append(imp.literals, importLiteral{
						Folder: f.Rel, Path: ref.Path, Line: ref.Line, Position: ref.Position, Call: ref.Call,
						TargetKind: ref.TargetKind, Arg: ref.Arg,
						Why: "a folder write names a path every feature of the organization shares, not a row — left as written, and declared as a folder write source bound in this stack",
					})
					imp.declareFolderWrite(f, ref)
				}
				continue
			case ref.NameForm:
				// Already the portable form of an output.
				continue
			case ref.TargetID == "":
				continue
			}
			depKind := depKindOf(ref.TargetKind)
			if depKind == "" {
				continue
			}
			if depKind == markers.KindFeature && ref.TargetID == imp.feature.ID {
				note(depKind, ref).featureSelf = true
				continue
			}
			t := note(depKind, ref)
			switch {
			case ref.Position == api.RefPosWriteMarker && ref.OwnOutput && ref.Readable && ref.LogicalName != "" && row.res.Kind == api.RefKindWorkflow:
				t.output = ref.LogicalName
			case ref.Writes():
				t.write = true
			}
			if ref.Position == api.RefPosOutputLiteral {
				t.write = true
				imp.literals = append(imp.literals, importLiteral{
					Folder: f.Rel, Path: ref.Path, Line: ref.Line, Position: ref.Position, Call: ref.Call,
					TargetKind: ref.TargetKind, TargetID: ref.TargetID, Arg: ref.Arg,
					Why: "an output literal is a call argument no codec rewrites; its table is declared as a write-capable data source bound in this stack",
				})
			}
			if ref.Position == api.RefPosAppLiteral {
				imp.literals = append(imp.literals, importLiteral{
					Folder: f.Rel, Path: ref.Path, Line: ref.Line, Position: ref.Position, Call: ref.Call,
					TargetKind: ref.TargetKind, TargetID: ref.TargetID, Arg: ref.Arg,
					Why: "an id literal in app source is not a marker, so nothing resolves it per stack — edit it by hand to deploy elsewhere",
				})
			}
		}
	}

	// Own outputs first: an output is written by its logical name, so that name
	// must be free when it is chosen — a foreign table that happens to be called
	// the same and sorts earlier would otherwise take it, and degrade the
	// workflow's own output into a write-capable data source bound per stack.
	ownOutput := func(t *targetFacts) bool { return t.output != "" && !t.write }
	sort.Slice(order, func(i, j int) bool {
		a, b := facts[order[i]], facts[order[j]]
		if ownOutput(a) != ownOutput(b) {
			return ownOutput(a)
		}
		return order[i] < order[j]
	})
	for _, key := range order {
		t := facts[key]
		if t.featureSelf && t.id == imp.feature.ID {
			continue
		}
		target := byID[t.id]
		switch {
		case t.output != "" && !t.write && validOutputAlias(f, t.output):
			// The workflow's own output, written by its logical name; bound here
			// so the id the live row carries round-trips.
			f.taken[strings.ToLower(t.output)] = true
			f.aliasOf[key] = t.output
			f.deps[t.output] = wfdir.Dependency{Kind: markers.KindTable, Output: true}
			f.bind[t.output] = t.id
			imp.outputs = append(imp.outputs, importOutput{Folder: f.Rel, Alias: t.output, Stack: imp.stack, BoundTo: t.id})
			imp.recordWritten(f, t, t.output)
		case t.write || t.output != "":
			alias := f.claimAlias(t.depKind, t.id, t.name)
			f.deps[alias] = wfdir.Dependency{Kind: t.depKind, Write: true}
			f.bind[alias] = t.id
			imp.sources = append(imp.sources, importDataSource{Folder: f.Rel, Alias: alias, Kind: t.depKind,
				Access: "write", Stack: imp.stack, BoundTo: t.id, Readable: t.readable})
			imp.recordWritten(f, t, alias)
		case target != nil && target.folder == f && f.Kind.Name == wfdir.KindPipeline:
			// A sibling: written as its stem by the pipeline rewrite.
		case target != nil && target.folder != f && addressDepKindFits(t.depKind, target):
			alias := f.claimAlias(t.depKind, t.id, target.res.Name)
			f.deps[alias] = wfdir.Dependency{Kind: t.depKind, From: relFrom(f, target.folder), Address: target.address}
			f.addressIDs[alias] = t.id
			imp.addresses = append(imp.addresses, importAddress{Folder: f.Rel, Alias: alias, Kind: t.depKind,
				From: relFrom(f, target.folder), Address: target.address})
		case t.depKind == markers.KindTable && imp.writtenBy[t.id].folder != nil && imp.writtenBy[t.id].folder != f:
			// A table a workflow folder of the tree writes: named by that
			// folder's dependency, so every stack reads the table that
			// workflow writes THERE — never this stack's copy bound everywhere.
			w := imp.writtenBy[t.id]
			alias := f.claimAlias(t.depKind, t.id, t.name)
			f.deps[alias] = wfdir.Dependency{Kind: t.depKind, From: relFrom(f, w.folder), Address: w.alias}
			f.addressIDs[alias] = t.id
			imp.addresses = append(imp.addresses, importAddress{Folder: f.Rel, Alias: alias, Kind: t.depKind,
				From: relFrom(f, w.folder), Address: w.alias})
		default:
			alias := f.claimAlias(t.depKind, t.id, t.name)
			dep := wfdir.Dependency{Kind: t.depKind}
			access := "read"
			switch t.depKind {
			case markers.KindSecret, markers.KindMailbox, markers.KindMCPServer:
				access = "side_effect"
			case markers.KindWorkflow, markers.KindAgent:
				// A RUN EDGE out of the folder: declared "run": true
				// and bound in the importing stack, so the tree still deploys to
				// the feature it came from with nothing to change — while a stack
				// added later binds it, or another row, on purpose.
				access = "run"
				dep.Run = true
			}
			f.deps[alias] = dep
			f.bind[alias] = t.id
			imp.sources = append(imp.sources, importDataSource{Folder: f.Rel, Alias: alias, Kind: t.depKind,
				Access: access, Stack: imp.stack, BoundTo: t.id, Readable: t.readable})
		}
	}
	// Literals left in place name the alias their table was declared under.
	for i := range imp.literals {
		lit := &imp.literals[i]
		if lit.Folder == f.Rel && lit.Position == api.RefPosOutputLiteral && lit.Alias == "" {
			lit.Alias = f.aliasOf[targetKey(depKindOf(lit.TargetKind), lit.TargetID)]
		}
	}
}

// writtenTable is a table a workflow folder of the tree writes, and the
// dependency it writes it through.
type writtenTable struct {
	folder *importFolder
	alias  string
}

// recordWritten notes a table a WORKFLOW folder writes (featureImport.writtenBy).
func (imp *featureImport) recordWritten(f *importFolder, t *targetFacts, alias string) {
	if f.Kind.Name != wfdir.KindWorkflow || t.depKind != markers.KindTable {
		return
	}
	if imp.writtenBy == nil {
		imp.writtenBy = map[string]writtenTable{}
	}
	if _, taken := imp.writtenBy[t.id]; !taken {
		imp.writtenBy[t.id] = writtenTable{folder: f, alias: alias}
	}
}

// planPortability runs the portability pass over folders. A folder reading a
// table a workflow folder of the tree writes names it by that folder's
// dependency, which the writer's own pass records (writtenBy) — so the
// workflow folders are planned once first, to fill writtenBy whatever their
// order, and then every folder is planned with it whole: a workflow that reads
// another's table, two that read each other's included, names it by address
// whichever sorts first. A workflow folder's second pass records what it
// writes under the same aliases — they do not depend on writtenBy, only on its
// own references, taken in the same order.
func (imp *featureImport) planPortability(folders []*importFolder, byID map[string]*importRow) {
	ordered := workflowFoldersFirst(folders)
	for _, f := range ordered {
		if f.Kind.Name == wfdir.KindWorkflow {
			imp.portability(f, byID)
		}
	}
	for _, f := range ordered {
		if f.Kind.Name == wfdir.KindWorkflow {
			imp.forgetFolder(f.Rel)
			f.resetPlan()
		}
		imp.portability(f, byID)
	}
}

// workflowFoldersFirst is folders with the workflow folders first, each half in
// its order (see planPortability).
func workflowFoldersFirst(folders []*importFolder) []*importFolder {
	out := make([]*importFolder, 0, len(folders))
	for _, workflows := range []bool{true, false} {
		for _, f := range folders {
			if (f.Kind.Name == wfdir.KindWorkflow) == workflows {
				out = append(out, f)
			}
		}
	}
	return out
}

// validOutputAlias reports whether an output's logical name can be its alias
// here: legal as a name, and not already a stem or another alias.
func validOutputAlias(f *importFolder, name string) bool {
	if wfdir.ValidAliasName(name) != nil || markers.IsResourceID(markers.KindTable, name) {
		return false
	}
	return !f.taken[strings.ToLower(name)]
}

// addressDepKindFits reports whether an imported target can be named by
// address with the declared kind — the target's folder holds it.
func addressDepKindFits(depKind string, target *importRow) bool {
	return addressKindHeldBy(depKind, target.folder.Kind, target.address)
}

// declareFolderWrite declares the folder a write position writes into as a
// folder write source, bound in the importing stack to its normalized path. One
// declaration per path, however many lines write it.
func (imp *featureImport) declareFolderWrite(f *importFolder, ref api.ReferencePosition) {
	raw := ref.TargetPath
	if raw == "" {
		raw = ref.Arg
	}
	folderPath, ok := wfdir.NormalizeFolderPath(raw)
	if !ok || folderPath == "" {
		return
	}
	if _, seen := f.aliasOf[targetKey(markers.KindFolder, folderPath)]; seen {
		return
	}
	alias := f.claimAlias(markers.KindFolder, folderPath, "folder_"+strings.ReplaceAll(strings.TrimPrefix(folderPath, "/"), "/", "_"))
	f.deps[alias] = wfdir.Dependency{Kind: markers.KindFolder, Write: true}
	f.bind[alias] = folderPath
	imp.sources = append(imp.sources, importDataSource{Folder: f.Rel, Alias: alias, Kind: markers.KindFolder,
		Access: "write", Stack: imp.stack, BoundTo: folderPath, Readable: true})
}
