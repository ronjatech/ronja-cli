package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja import feature` turns what already runs in a feature into
// a repository tree of adopted folders — one per sync kind — whose references
// are written as sibling stems, addresses, declared outputs and declared data
// sources rather than one organization's ids, and whose rows are claimed for
// the importing stack, so the first `sync apply` of the unchanged tree writes
// nothing.
//
// Steps, in this order (claims are checked before anything is written):
//
//  1. read every live root resource from the feature's reference inventory
//     (paged) and each kind's own read route for its content;
//  2. read the feature's claims, and leave every row another folder already
//     manages where it is — references to it become data sources;
//  3. decide every folder and address, then run the portability pass
//     (import_plan.go) — still nothing written;
//  4. write the folders through the clone commands' own writers, minting each
//     folder's id and recording the anchors a clone records;
//  5. claim every written row in POST /managed/claim batches of at most 200,
//     each row reporting its own outcome; a row refused leaves the tree, and
//     every folder that held or referenced it is planned and written again
//     with that reference as a data source (settle), so the tree never holds
//     an entry the server refused nor an address into one;
//  6. report every literal left in place, every data source and what it is
//     bound to, every address, and every row claimed elsewhere.
//
// Single resources keep the four clone commands, which stay classic:
// they never mint an id and never claim.

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Write a feature's resources into a repository tree",
	}
	addStackFlag(cmd)
	cmd.AddCommand(newImportFeatureCmd())
	return cmd
}

func newImportFeatureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "feature <name|id> [dir]",
		Short: "Write every pipeline, workflow, app, module and automation of a feature into folders",
		Long: `Write a feature into a repository tree, ready to deploy with ronja sync.

  ronja import feature Sales
  ronja import feature collection-abc123 ./sales --stack main

The name is looked up among organization features; import a private feature by
its id. The destination (default: the current directory) must be empty — a
.git alone is fine, as just after git init. It
writes one folder per resource kind:

  pipelines/<feature>/     every derived table, its metrics and its enabled checks
  workflows/<name>/        one folder per workflow
  apps/<name>/             one folder per data app
  modules/<name>/          one folder per module
  automations/<feature>/   one .json file per automation

Each folder is adopted (it carries a folder id) and names one stack, --stack
(default "main"), pinned to the feature's id. Every row is claimed for that
stack, so ronja sync status is clean straight away and ronja sync apply --stack
main writes nothing.

References are rewritten rather than copied as ids: a table the same pipeline
builds becomes its file's stem, anything imported into another folder becomes an
address dependency, a workflow's own output becomes its name, and everything
else becomes a declared data source bound in this stack. A row another folder
already manages is left alone and becomes a data source. Two kinds of literal are
left in place and listed: output-table arguments (tools.replaceTable("table-…"))
and id literals in app source.

Managing a table or metric needs the Admin role. Run by anyone else, the import
writes no pipeline folder: derived tables and metrics are declared as data
sources wherever they are read, and the report lists them as not imported.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 2 {
				dir = args[1]
			}
			return runImportFeature(cmd.Context(), args[0], dir)
		},
	}
}

// featureImport is one run.
type featureImport struct {
	ctx      context.Context
	client   *api.Client
	resolved *config.Resolved
	stack    string
	base     string
	feature  *api.Feature

	folders []*importFolder
	byID    map[string]*importRow

	// admin is whether the caller may manage tables and metrics (the Admin
	// role floor of a table or metric claim). False plans every derived table
	// and metric as a data source rather than a pipeline row.
	admin bool

	claimedElsewhere []importClaimedElsewhere
	literals         []importLiteral
	sources          []importDataSource
	addresses        []importAddress
	outputs          []importOutput
	// writtenBy is every table a WORKFLOW folder of the tree writes — its own
	// outputs and its write sources — by id: the dependency it is written
	// through, so a folder that reads it names it by address in that folder
	// (`"from"` the workflow folder, `"address"` the dependency) rather than
	// binding one stack's copy.
	writtenBy   map[string]writtenTable
	claims      []importClaimOutcome
	notImported []importSkip
	notCarried  []importNotCarried
	problems    []string
}

func runImportFeature(ctx context.Context, arg, dir string) error {
	if flagStack == "" {
		flagStack = "main"
	}
	if err := wfdir.ValidStackName(flagStack); err != nil {
		return err
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	// Refused before a single request, the precedent every clone has: the tree
	// is written whole or not at all, and import never re-imports into a tree
	// (`sync status` answers "what changed on the server").
	if err := importDestinationEmpty(base); err != nil {
		return err
	}
	resolved, err := resolveInstance()
	if err != nil {
		return err
	}
	if err := ensureTenant(ctx, resolved); err != nil {
		return err
	}
	imp := &featureImport{ctx: ctx, client: newClient(resolved.URL, resolved.Token), resolved: resolved,
		stack: flagStack, base: base, byID: map[string]*importRow{}}
	if imp.feature, err = imp.resolveFeature(arg); err != nil {
		return err
	}
	if err := imp.plan(); err != nil {
		return err
	}
	if err := imp.write(); err != nil {
		return err
	}
	imp.claimAll()
	report := imp.report()
	if flagJSON {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else {
		printImportReport(report)
	}
	if report.Verdict != verdictApplied {
		return withExitCode(1, fmt.Errorf("the tree was written, and the report names %d %s with its claims", len(report.Problems), plural(len(report.Problems), "problem")))
	}
	return nil
}

// resolveFeature finds the feature: by id, or by EXACT name among the
// organization's features (a private feature is listed in every Space, so a
// name could mean somebody's private one — those are imported by id).
func (imp *featureImport) resolveFeature(arg string) (*api.Feature, error) {
	arg = strings.TrimSpace(arg)
	if markers.IsResourceID(markers.KindFeature, arg) {
		feat, err := imp.client.GetFeature(imp.ctx, arg)
		if err != nil {
			if api.StatusOf(err) == 404 || api.StatusOf(err) == 400 {
				return nil, fmt.Errorf("no feature %s on %s that you can read", arg, imp.resolved.URL)
			}
			return nil, err
		}
		return feat, nil
	}
	if len(arg) < api.MinSearchTermLen {
		return nil, fmt.Errorf("%q is too short to look up by name — pass the feature's id", arg)
	}
	hits, err := imp.client.Search(imp.ctx, arg, api.MaxSearchLimit)
	if err != nil {
		return nil, fmt.Errorf("look up the feature %q: %w", arg, err)
	}
	var found []*api.Feature
	for _, hit := range hits {
		if hit.Kind != api.SearchKindFeature || hit.Title != arg {
			continue
		}
		feat, err := imp.client.GetFeature(imp.ctx, hit.ID)
		if err != nil {
			continue
		}
		if feat.Scope == scopeOrganization && feat.Name == arg {
			found = append(found, feat)
		}
	}
	switch {
	case len(found) == 1 && len(hits) < api.MaxSearchLimit:
		return found[0], nil
	case len(found) > 1:
		ids := make([]string, 0, len(found))
		for _, f := range found {
			ids = append(ids, f.ID)
		}
		return nil, fmt.Errorf("%d organization features are called %q (%s) — pass the id of the one to import", len(found), arg, strings.Join(ids, ", "))
	case len(hits) >= api.MaxSearchLimit:
		return nil, fmt.Errorf("the search for %q returned a full page, so whether one organization feature is called exactly that cannot be told — pass the feature's id", arg)
	}
	return nil, fmt.Errorf("no organization feature is called %q on %s — a private feature is imported by its id", arg, imp.resolved.URL)
}

// plan is steps 1–3: read, check claims, decide every folder, address and
// reference form. Nothing is written.
func (imp *featureImport) plan() error {
	imp.admin = imp.callerMayManageTables()
	inv, err := imp.client.FeatureReferences(imp.ctx, imp.feature.ID, 0)
	if err != nil {
		if noManagedRoute(err) {
			// The router's plain-text 404: no such route on this instance.
			return fmt.Errorf("%s does not serve a feature's reference inventory (GET /api/v2/feature/:id/references) — it is older than this CLI, and an import rewrites every reference from that inventory. Import against an instance that serves it, or use a `ronja` that matches this one; nothing was written", imp.resolved.URL)
		}
		return fmt.Errorf("read the reference inventory of %s: %w", imp.feature.ID, err)
	}
	if len(inv.OmittedKinds) > 0 {
		var missing []string
		for _, o := range inv.OmittedKinds {
			missing = append(missing, fmt.Sprintf("%s (needs %s:read)", o.Kind, o.Scope))
		}
		return fmt.Errorf("this credential cannot read every kind in the feature — %s — so an import would leave those resources out and their references unrewritten. Use a credential with those scopes", strings.Join(missing, ", "))
	}

	// 2. Claims FIRST: a row another folder manages is never imported.
	claims, err := imp.client.ListManaged(imp.ctx, imp.feature.ID, "")
	if err != nil {
		return fmt.Errorf("read which rows of %s are already managed (GET /managed): %w", imp.feature.ID, err)
	}
	claimed := map[string]api.ManagedClaim{}
	for _, c := range claims {
		if c.ResourceID != "" {
			claimed[c.ResourceID] = c
		}
	}

	featureSlug := wfdir.Slug(imp.feature.Name, "feature")
	pipeline := newImportFolder(wfdir.PipelineKind, "pipelines/"+featureSlug, imp.base)
	automations := newImportFolder(wfdir.AutomationKind, "automations/"+featureSlug, imp.base)
	used := map[string]map[string]bool{"workflows": {}, "apps": {}, "modules": {}}
	automationFiles := map[string]bool{}
	var tableItems []*api.TableListItem
	var single []*importFolder

	for _, res := range inv.Resources {
		if c, ok := claimed[res.ID]; ok {
			imp.claimedElsewhere = append(imp.claimedElsewhere, importClaimedElsewhere{
				Kind: res.Kind, ID: res.ID, Name: res.Name, FolderID: c.FolderID, Address: c.Address})
			continue
		}
		switch res.Kind {
		case api.RefKindTable, api.RefKindMetric:
			kind := res.TableKind
			if res.Kind == api.RefKindMetric {
				kind = api.TableKindMetric
			}
			if kind != api.TableKindDerived && kind != api.TableKindMetric {
				// No SQL a file could hold: a connector, foundation, manual or
				// dynamic table is a data source wherever it is read.
				imp.notImported = append(imp.notImported, importSkip{Kind: res.Kind, ID: res.ID, Name: res.Name, Reason: notImportedTableReason(res)})
				continue
			}
			if !imp.admin {
				// A table or metric claim needs the Admin role, so a pipeline
				// folder this caller writes could never be claimed — and every
				// address into it would dangle. Planned as a data source
				// instead, wherever it is referenced.
				imp.notImported = append(imp.notImported, importSkip{Kind: res.Kind, ID: res.ID, Name: res.Name, Reason: importReasonNeedsAdmin})
				continue
			}
			pipeline.rows = append(pipeline.rows, &importRow{res: res, folder: pipeline})
			tableItems = append(tableItems, &api.TableListItem{ID: res.ID, Name: res.Name, Kind: kind})
		case api.RefKindWorkflow, api.RefKindDataApp, api.RefKindModule:
			kind, parent, slug := wfdir.WorkflowKind, "workflows", wfdir.Slug(res.Name, "workflow")
			switch res.Kind {
			case api.RefKindDataApp:
				kind, parent, slug = wfdir.DataAppKind, "apps", wfdir.Slug(res.Name, "app")
			case api.RefKindModule:
				kind, parent, slug = wfdir.ModuleKind, "modules", wfdir.FileSlug(res.Name, "module")
			}
			f := newImportFolder(kind, parent+"/"+uniqueSegment(used[parent], slug, ""), imp.base)
			f.rows = []*importRow{{res: res, folder: f}}
			single = append(single, f)
		case api.RefKindAutomation:
			address := uniqueSegment(automationFiles, wfdir.Slug(res.Name, "automation"), ".json")
			automations.rows = append(automations.rows, &importRow{res: res, folder: automations, address: address})
		default:
			imp.notImported = append(imp.notImported, importSkip{Kind: res.Kind, ID: res.ID, Name: res.Name, Reason: notImportedKindReason(res.Kind)})
		}
	}

	// 1. Each kind's content, read through the clone readers.
	if len(pipeline.rows) > 0 {
		cloneable, metricRows, skipped := partitionCloneable(tableItems)
		set, err := fetchPipelineClone(imp.ctx, imp.client, cloneable, metricRows, skipped, true)
		if err != nil {
			return err
		}
		pipeline.pipeline = set
		pipeline.pristine = &pipelineCloneSet{sources: slices.Clone(set.sources), metrics: slices.Clone(set.metrics),
			checks: slices.Clone(set.checks), skipped: set.skipped}
		pipeline.seedTaken()
		at := map[string]string{}
		for _, s := range set.sources {
			at[s.TableID] = s.Path
		}
		for _, m := range set.metrics {
			at[m.MetricID] = m.Path
		}
		kept := pipeline.rows[:0]
		for _, row := range pipeline.rows {
			if p, ok := at[row.res.ID]; ok {
				row.address = p
				kept = append(kept, row)
				continue
			}
			imp.notImported = append(imp.notImported, importSkip{Kind: row.res.Kind, ID: row.res.ID, Name: row.res.Name,
				Reason: "the instance answered no recipe for this metric, so there is nothing a file could hold"})
		}
		pipeline.rows = kept
		if len(pipeline.rows) > 0 {
			imp.folders = append(imp.folders, pipeline)
		}
	}
	for _, f := range single {
		if err := imp.fetchSingle(f); err != nil {
			return err
		}
		imp.folders = append(imp.folders, f)
	}
	if len(automations.rows) > 0 {
		automations.automations = map[string]*api.Automation{}
		for _, row := range automations.rows {
			auto, err := imp.client.GetAutomation(imp.ctx, row.res.ID)
			if err != nil {
				return fmt.Errorf("read automation %s (%s): %w", row.res.ID, row.res.Name, err)
			}
			automations.automations[row.address] = auto
		}
		automations.seedTaken()
		imp.folders = append(imp.folders, automations)
	}
	if len(imp.folders) == 0 {
		return fmt.Errorf("%q holds nothing a folder can carry — no derived table, metric, workflow, data app, module or automation that is not already managed by another folder", imp.feature.Name)
	}

	// 3. The address map, then the portability pass over every folder.
	for _, f := range imp.folders {
		for _, row := range f.rows {
			imp.byID[row.res.ID] = row
		}
	}
	imp.planPortability(imp.folders, imp.byID)
	for _, f := range imp.folders {
		if err := imp.rewrite(f); err != nil {
			return err
		}
		id, err := wfdir.NewFolderID()
		if err != nil {
			return err
		}
		f.FolderID = id
	}
	return nil
}

// notImportedTableReason says why a table the feature holds is not a file of
// the tree.
func notImportedTableReason(res api.ReferenceResource) string {
	if res.OwnerWorkflowID != "" {
		return fmt.Sprintf("the output of workflow %s, which creates it by name when it runs — the workflow's folder declares it as an output, so no file holds it", res.OwnerWorkflowID)
	}
	kind := res.TableKind
	if kind == "" {
		kind = "non-derived"
	}
	return fmt.Sprintf("a table of kind %s has no SQL a file could hold — where the tree's code reads it, it is a data source bound in this stack", kind)
}

// notImportedKindReason says why a resource of a kind no folder holds was left
// out.
func notImportedKindReason(kind string) string {
	switch kind {
	case api.RefKindAgent:
		return "a saved agent is not a kind the tree carries (no folder holds one) — where the tree's code names it, it is a data source bound in this stack"
	case api.RefKindNote:
		return "a note is not a kind the tree carries (no folder holds one) — where the tree's code names it, it is a data source bound in this stack"
	case api.RefKindSecret:
		return "a secret's value never goes into a repository — where the tree's code names it, it is a data source bound in this stack"
	case api.RefKindCodex:
		return "a codex is not a kind the tree carries (no folder holds one) — where the tree's code names it, it is a data source bound in this stack"
	case api.RefKindMCPServer:
		return "an MCP server connection is not a kind the tree carries (no folder holds one) — where the tree's code names it, it is a data source bound in this stack"
	}
	return fmt.Sprintf("a %s is not a kind this build's tree carries", kind)
}

// importReasonNeedsAdmin is why a non-Admin's import leaves derived tables and
// metrics out of the tree.
const importReasonNeedsAdmin = "tables and metrics need an Admin to manage; they were declared as data sources"

// callerMayManageTables reads the caller's role. A credential that cannot say
// (a scoped token cannot call /me) is planned as one that may — a claim the
// server then refuses is withdrawn and its dependents rewritten (settle), so a
// wrong guess costs a pipeline folder, never a dangling reference.
func (imp *featureImport) callerMayManageTables() bool {
	me, err := imp.client.Me(imp.ctx)
	if err != nil {
		return true
	}
	return isAdmin(me)
}

// fetchSingle reads one workflow, app or module — the LIVE row, since that is
// what gets claimed and anchored.
func (imp *featureImport) fetchSingle(f *importFolder) error {
	id := f.rows[0].res.ID
	switch f.Kind.Name {
	case wfdir.KindWorkflow:
		wf, err := imp.client.GetWorkflow(imp.ctx, id)
		if err != nil {
			return fmt.Errorf("read workflow %s: %w", id, err)
		}
		if err := refuseUnclonable(wf); err != nil {
			return err
		}
		f.workflow = wf
	case wfdir.KindDataApp:
		app, err := imp.client.GetDataApp(imp.ctx, id)
		if err != nil {
			return fmt.Errorf("read data app %s: %w", id, err)
		}
		if err := refuseUnworkableApp(app); err != nil {
			return err
		}
		f.app = app
	case wfdir.KindModule:
		mod, err := imp.client.GetModule(imp.ctx, id)
		if err != nil {
			return fmt.Errorf("read module %s: %w", id, err)
		}
		if err := refuseUnclonableModule(mod); err != nil {
			return err
		}
		f.module = mod
	}
	return nil
}

// write is step 4: every folder through its kind's shared writer, into a
// hidden staging directory INSIDE the destination, whose top-level entries are
// moved into the destination only once every folder is written. The writers
// read each workflow's, app's and module's files and head version as they
// write, so a failure part way (a network error, a path that cannot be
// written) must leave the destination as it was — empty.
//
// The destination itself is never removed or replaced: it is the current
// directory by default, and a shell standing in it must not be left in a
// deleted directory. Staging inside it also needs nothing writable beside it.
func (imp *featureImport) write() (err error) {
	created := false
	if _, statErr := os.Stat(imp.base); os.IsNotExist(statErr) {
		if err := os.MkdirAll(imp.base, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", imp.base, err)
		}
		created = true
	}
	staging, err := os.MkdirTemp(imp.base, importStagingPrefix)
	if err != nil {
		return fmt.Errorf("create a staging directory in %s: %w", imp.base, err)
	}
	var moved []string
	defer func() {
		if err == nil {
			return
		}
		for _, name := range moved {
			os.RemoveAll(filepath.Join(imp.base, name))
		}
		os.RemoveAll(staging)
		if created {
			// Made by this run: put back as it was, absent.
			os.Remove(imp.base)
		}
	}()
	for _, f := range imp.folders {
		if werr := imp.writeFolder(f, filepath.Join(staging, filepath.FromSlash(f.Rel))); werr != nil {
			return fmt.Errorf("write %s: %w (nothing was written to %s)", f.Rel, werr, imp.base)
		}
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("read the staging directory %s: %w", staging, err)
	}
	for _, e := range entries {
		if err := importRename(filepath.Join(staging, e.Name()), filepath.Join(imp.base, e.Name())); err != nil {
			return fmt.Errorf("move the imported %s into %s: %w (nothing was left in %s)", e.Name(), imp.base, err, imp.base)
		}
		moved = append(moved, e.Name())
	}
	if err := os.Remove(staging); err != nil {
		return fmt.Errorf("remove the staging directory %s: %w", staging, err)
	}
	return nil
}

// importRename moves one written entry into place; a seam for the test that
// fails a move part way.
var importRename = os.Rename

// importDestinationEmpty refuses a destination that holds anything — the
// precedent every clone has — except a `.git`: a directory just `git init`ed,
// or a clone of an empty repository, is where a repository is started, and
// import writes nothing into `.git` (the tree is moved in beside it). A staging
// directory a previous import left behind (killed while writing) is named on
// its own, and NOT removed: nothing tells it from the staging directory of an
// import still running into the same place, and removing that one would
// destroy another run's tree as it is written. Removing it is one command for
// the person who knows which it is.
func importDestinationEmpty(base string) error {
	all, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", base, err)
	}
	entries := slices.DeleteFunc(all, func(e os.DirEntry) bool { return e.Name() == ".git" })
	if len(entries) == 0 {
		return nil
	}
	var stale []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), importStagingPrefix) {
			stale = append(stale, e.Name())
		}
	}
	if len(stale) == len(entries) {
		return fmt.Errorf("%s holds %s, left by an import that did not finish (it was interrupted, or is still running) — when none is running, remove %s and run the import again",
			base, strings.Join(stale, ", "), pluralVerb(len(stale), "it", "them"))
	}
	return fmt.Errorf("%s is not empty — import into an empty directory (a `.git` alone is fine)", base)
}

// writeFolder lays one folder down at root through its kind's shared writer.
func (imp *featureImport) writeFolder(f *importFolder, root string) error {
	hooks := f.hooks()
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		return writePipelineClone(imp.ctx, imp.resolved, imp.feature.ID, imp.feature.Name, root, f.pipeline, hooks)
	case wfdir.KindWorkflow:
		_, _, err := writeWorkflowClone(imp.ctx, imp.client, imp.resolved, f.workflow, f.workflow, root, hooks)
		return err
	case wfdir.KindDataApp:
		_, _, err := writeDataAppClone(imp.ctx, imp.client, imp.resolved, f.app, f.app, root, hooks)
		return err
	case wfdir.KindModule:
		_, err := writeModuleClone(imp.ctx, imp.client, imp.resolved, f.module, f.module, root, hooks)
		return err
	case wfdir.KindAutomation:
		return imp.writeAutomations(f, root, hooks)
	}
	return fmt.Errorf("no writer for a %s folder", f.Kind.Label)
}

// importStagingPrefix names the hidden directories an import writes into
// inside the destination before anything is moved into place.
const importStagingPrefix = ".ronja-import-"
