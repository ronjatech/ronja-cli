package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The alias layer, from the commands' side.
//
// One assertion carries this whole pass and is repeated once per folder kind: a
// folder that USES an alias, pushed and then pushed again with nothing changed,
// reports UP TO DATE. It is the acceptance test because a de-alias site that was
// missed anywhere — in the file listing, in the reconciling read, in the
// baseline, in a canonicalized SQL comparison — shows up as exactly one symptom:
// permanent drift. A failure here is a site nobody wired, not a test that wants
// adjusting.

// tableDep is the one-line declaration these tests use.
func tableDep() map[string]wfdir.Dependency {
	return map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}}
}

// --- workflows --------------------------------------------------------------

// aliasWorkflowFolder is a stack folder that declares `orders`, binds it to the
// instance's own table id, and writes the ALIAS in its source. Unbound to any
// workflow, so the first push creates one.
func aliasWorkflowFolder(t *testing.T, f *fakeInstance, source string) string {
	t.Helper()
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		Dependencies: tableDep(),
		Stacks: map[string]wfdir.Stack{
			"dev": {
				URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1",
				Bind: map[string]string{"orders": "table-orders"},
			},
		},
	}
	if err := wfdir.SaveFolder(root, manifest, &wfdir.Lock{}); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	writeLocal(t, root, "main.py", source)
	return root
}

// TestPushOfAnAliasWorkflowIsUpToDateOnTheSecondRun is the acceptance test for
// the workflow loop.
func TestPushOfAnAliasWorkflowIsUpToDateOnTheSecondRun(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// The SERVER got the id. Nothing about an alias reaches the wire — that is
	// the whole point of resolving in the client.
	created := f.FileContents(workflowIDOf(t, root))
	if got := created["main.py"]; got != "read(\"{{ ref('table-orders') }}\")\n" {
		t.Fatalf("the server was sent %q", got)
	}
	// And the file on disk is untouched: a push does not rewrite the author's
	// source into ids behind their back.
	if got := readLocal(t, root, "main.py"); got != "read(\"{{ ref('orders') }}\")\n" {
		t.Fatalf("the local file was rewritten to %q", got)
	}

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date — a de-alias site is missing:\n%s", out)
	}
}

// TestPushOfAFolderWithNoDependenciesSendsExactlyWhatIsOnDisk pins the property
// that makes this change inert for every folder in the field: with no
// dependencies declared, not one byte of a push differs from what it was before
// the alias layer existed.
func TestPushOfAFolderWithNoDependenciesSendsExactlyWhatIsOnDisk(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	// Source full of things a codec COULD have touched — an id-form ref, a
	// two-arity secret, a positional ref, doubled braces — and a folder that
	// declares nothing, so none of them is.
	source := "a = \"{{ ref('table-orders') }}\"\n" +
		"b = \"{{ secret('secret-1', 'key') }}\"\n" +
		"c = f\"{{{{ ref('0') }}}}\"\n"
	root := initFolder(t, f, map[string]string{"main.py": source})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := f.FileContents(workflowIDOf(t, root))["main.py"]; got != source {
		t.Fatalf("push rewrote a folder that declares no dependencies:\n got %q\nwant %q", got, source)
	}
}

// TestPushRefusesAnUnboundAlias: a declaration the selected stack answers
// nothing for. Answered from the manifest alone, so it fires before any request.
func TestPushRefusesAnUnboundAlias(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	unbindStack(t, root, "dev")
	f.Requests = nil

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), `"orders"`) || !strings.Contains(err.Error(), "ronja bind --stack dev") {
		t.Errorf("refusal does not name the alias and the fix: %v", err)
	}
	for _, req := range f.Requests {
		if strings.Contains(req, "PUT") || strings.Contains(req, "POST") {
			t.Errorf("the refusal came after a write: %v", f.Requests)
		}
	}
}

// TestStatusReportsAnUnboundAliasWithoutFailing is the other half of the same
// rule. A folder mid-promotion — declarations merged, binds not filled in — has
// to stay inspectable, so the command you run when you are already suspicious
// says what it found and still answers.
func TestStatusReportsAnUnboundAliasWithoutFailing(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	unbindStack(t, root, "dev")

	var out string
	stderr := captureStderr(t, func() {
		var err error
		out, err = runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(stderr, `"orders"`) {
		t.Errorf("status said nothing about the unbound alias:\n%s", stderr)
	}
	if decodeJSON(t, out)["root"] != root {
		t.Errorf("status did not report the folder:\n%s", out)
	}
}

// TestPushRefusesASourceThatWritesABoundIdLiterally.
//
// The refusal exists because the folder it describes can never be clean: the
// remote read de-aliases that id back to the alias, the local file keeps the id,
// and the two hashes differ for ever — so the file reads as drifted on every
// status with nothing the author can edit that fixes it except this.
func TestPushRefusesASourceThatWritesABoundIdLiterally(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('table-orders') }}\")\n")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "main.py") || !strings.Contains(err.Error(), "{{ ref('orders') }}") {
		t.Errorf("refusal does not name the file and the spelling to use: %v", err)
	}
}

// TestPushWarnsAboutADeclaredAliasNobodyUses: dead config, not a broken deploy,
// so it is said once and the push goes through.
func TestPushWarnsAboutADeclaredAliasNobodyUses(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "print('nothing to see')\n")

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("push: %v", err)
		}
	})
	if !strings.Contains(stderr, `"orders"`) || !strings.Contains(stderr, "no file in this folder uses it") {
		t.Errorf("no warning about the unused declaration:\n%s", stderr)
	}
}

// --- data apps --------------------------------------------------------------

// TestAppPushOfAnAliasFolderIsUpToDateOnTheSecondRun is the acceptance test for
// the data-app loop.
func TestAppPushOfAnAliasFolderIsUpToDateOnTheSecondRun(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer", Entrypoint: "App.tsx",
		Dependencies: tableDep(),
		Stacks: map[string]wfdir.Stack{
			"dev": {
				URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1",
				Bind: map[string]string{"orders": "table-orders"},
			},
		},
	}
	writeAppFolder(t, root, manifest, map[string]string{
		"App.tsx": "const q = \"{{ ref('orders') }}\";\n",
	})
	if err := wfdir.SaveLock(root, &wfdir.Lock{}); err != nil {
		t.Fatalf("save lock: %v", err)
	}

	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	sent := f.FileContents(appDraftIDOf(t, root, f))
	if got := sent["App.tsx"]; got != "const q = \"{{ ref('table-orders') }}\";\n" {
		t.Fatalf("the server was sent %q", got)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date — a de-alias site is missing:\n%s", out)
	}
}

// A data app's dependencies are not all in its content. The `access` block in
// ronja.json is a list of raw ids belonging to exactly one organization, so a
// folder that carried its code in alias form and its allowlist in id form would
// still be pushable to precisely one place — which is what the tests below are
// about, and why the acceptance one is repeated for an alias that appears ONLY
// in the access block and nowhere in any file.

// aliasAppFolder is a data-app stack folder that declares `deps`, binds them
// through stack "dev", and holds `access` in the folder's own alias vocabulary.
func aliasAppFolder(t *testing.T, f *fakeAppInstance, deps map[string]wfdir.Dependency, bind map[string]string, access api.DataAppAccess, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer", Entrypoint: "App.tsx",
		Dependencies: deps,
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1", Bind: bind},
		},
	}
	manifest.SetAccess(access)
	writeAppFolder(t, root, manifest, files)
	if err := wfdir.SaveLock(root, &wfdir.Lock{}); err != nil {
		t.Fatalf("save lock: %v", err)
	}
	return root
}

// TestAppPushOfAnAliasAccessFolderIsUpToDateOnTheSecondRun is the acceptance
// test for the access half, and it carries the same weight as the content one:
// a conversion site missed anywhere — the create body, the diff that decides
// whether to patch, the patch itself — shows up as an allowlist that is re-sent
// on every push and a folder that never reports itself clean.
//
// The alias appears ONLY in `access`. No file mentions it, which is exactly the
// state a data app is in: nothing derives an allowlist from source.
func TestAppPushOfAnAliasAccessFolderIsUpToDateOnTheSecondRun(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := aliasAppFolder(t, f, tableDep(), map[string]string{"orders": "table-orders"},
		api.DataAppAccess{AllowedTableIDs: []string{"orders"}},
		map[string]string{"App.tsx": "const x = 1;\n"})

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
			t.Fatalf("first push: %v", err)
		}
	})
	// An alias a data app uses only in its allowlist IS used by the folder, so
	// the dead-config warning must not fire on it.
	if strings.Contains(stderr, "no file in this folder uses it") {
		t.Errorf("an alias used by the access block was reported as unused:\n%s", stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("want one create, got %+v", f.created)
	}
	// The SERVER got the id. Nothing about an alias reaches the wire, here or in
	// the content — the grant is on the row, and the row has an id.
	if got := f.created[0].AllowedTableIDs; len(got) != 1 || got[0] != "table-orders" {
		t.Fatalf("the create carried %v, want the resolved id", got)
	}
	// And the manifest is untouched: a push does not rewrite the author's
	// declared allowlist into ids behind their back.
	if got := appManifestOf(t, root).DeclaredAccess().AllowedTableIDs; len(got) != 1 || got[0] != "orders" {
		t.Fatalf("the manifest was rewritten to %v", got)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date — an access conversion site is missing:\n%s", out)
	}
	// The sharper form of the same claim: no PATCH was sent at all. `upToDate`
	// could in principle survive a patch that re-sent an identical allowlist;
	// a patch that re-grants the same privileges every push cannot.
	if len(f.patches) != 0 {
		t.Fatalf("the second push patched the allowlists: %+v", f.patches)
	}

	// The read-only report agrees, which is the leg push's own shortcut never
	// reaches: it diffs the LIVE row's ids against the manifest's names.
	out, err = runCLI(t, root, "app", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if changes := decodeJSON(t, out)["accessChanges"]; changes != nil {
		t.Fatalf("status reports an access change on a folder that just pushed: %v", changes)
	}
}

// TestAppPushOfAPromotedAliasAccessFolderSendsNoPatch is the EDIT cycle, and it
// is a different failure from the one above: the create carries the allowlist in
// its own body, so a conversion missed only at the DIFF site still looks right
// on a first push and then re-grants the app's privileges on every push after
// it, for ever. The state here is the one a promoted folder is actually in — a
// live row holding ids, a folder holding names — reached by binding the folder
// by hand rather than by creating the app.
func TestAppPushOfAPromotedAliasAccessFolderSendsNoPatch(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{
		ID:            "data_app-1",
		DataAppAccess: api.DataAppAccess{AllowedTableIDs: []string{"table-orders"}},
	}, api.DataAppFile{Path: "App.tsx", Content: "const x = 1;\n"})
	signInApp(t, f)

	root := aliasAppFolder(t, f, tableDep(), map[string]string{"orders": "table-orders"},
		api.DataAppAccess{AllowedTableIDs: []string{"orders"}},
		map[string]string{"App.tsx": "const x = 1;\n"})
	lock := &wfdir.Lock{}
	lock.SetBinding("dev", wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatalf("save lock: %v", err)
	}
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(aliasCodec{}, live, f.files[live.ID]))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("a folder whose names already resolve to the row's ids was not up to date:\n%s", out)
	}
	if len(f.patches) != 0 {
		t.Fatalf("the push re-granted the allowlists: %+v", f.patches)
	}
}

// TestAppPushResolvesAMetricSlotThroughATableAlias pins the metric decision.
//
// A metric slot takes a `table` alias, because a metric is a kind='metric' row
// in models_v2 carrying a `table-` prefixed id — the same id namespace. There is
// no `metric` dependency kind and there will not be one: a dependency kind says
// which MARKER family may use an alias and no marker resolves a metric, and
// `ronja bind` would find nothing for it either, since GET /api/v2/search
// reports a metric as kind "table". See accessDependencies for the whole
// reasoning.
func TestAppPushResolvesAMetricSlotThroughATableAlias(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := aliasAppFolder(t, f,
		map[string]wfdir.Dependency{"revenue": {Kind: markers.KindTable}},
		map[string]string{"revenue": "table-revenue"},
		api.DataAppAccess{AllowedMetricIDs: []string{"revenue"}},
		map[string]string{"App.tsx": "const x = 1;\n"})

	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := f.created[0].AllowedMetricIDs; len(got) != 1 || got[0] != "table-revenue" {
		t.Fatalf("the metric slot carried %v, want the resolved id", got)
	}
	// And it did not leak into the table slot: the two lists are separate grants
	// and the alias was listed in exactly one of them.
	if got := f.created[0].AllowedTableIDs; len(got) != 0 {
		t.Fatalf("the table slot was granted %v", got)
	}
}

// TestAppPushLeavesAnAccessEntryOfTheWrongKindAlone: the access half of the rule
// content already follows. `hubspot` is declared a SECRET and listed as a
// TABLE — the codec is keyed by (kind, name), so it resolves nothing and the
// entry is sent exactly as written, for the server to refuse by name. Guessing
// here would grant a secret's id as a table.
func TestAppPushLeavesAnAccessEntryOfTheWrongKindAlone(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := aliasAppFolder(t, f,
		map[string]wfdir.Dependency{"hubspot": {Kind: markers.KindSecret}},
		map[string]string{"hubspot": "secret-1"},
		api.DataAppAccess{AllowedTableIDs: []string{"hubspot"}},
		map[string]string{"App.tsx": "const x = 1;\n"})

	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := f.created[0].AllowedTableIDs; len(got) != 1 || got[0] != "hubspot" {
		t.Fatalf("the entry was rewritten to %v — a name of the wrong kind must be left as found", got)
	}
	if got := f.created[0].AllowedSecretIDs; len(got) != 0 {
		t.Fatalf("the secret slot was granted %v", got)
	}
}

// TestAppPushRefusesAnAccessEntryNamingABoundIdLiterally.
//
// The access analogue of the source refusal, and the damage is WORSE rather than
// equal: an id compares equal to itself, so nothing drifts and nothing warns —
// the folder simply grants stack "dev"'s own row wherever it is pushed, and the
// first anyone knows of it is an app in the second organization that can read
// nothing.
func TestAppPushRefusesAnAccessEntryNamingABoundIdLiterally(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := aliasAppFolder(t, f, tableDep(), map[string]string{"orders": "table-orders"},
		api.DataAppAccess{AllowedTableIDs: []string{"table-orders"}},
		map[string]string{"App.tsx": "const x = 1;\n"})
	f.Requests = nil

	_, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "allowedTableIDs") || !strings.Contains(err.Error(), `"orders"`) {
		t.Errorf("refusal does not name the slot and the spelling to use: %v", err)
	}
	// Local, like every other alias refusal: nothing was created.
	for _, request := range f.Requests {
		if strings.Contains(request, "/dataapp") {
			t.Errorf("the refusal came after a request: %v", f.Requests)
			break
		}
	}
}

// TestAppPushOfAFolderWithNoDependenciesSendsTheAllowlistVerbatim is the access
// half of the inertness property, and it is the one that must hold for every
// data-app folder in the field: with no dependencies declared the codec short
// circuits before it looks at a single slot, so the create carries exactly the
// ids on disk — including one that a codec COULD have had an opinion about, had
// there been a declaration to give it one.
func TestAppPushOfAFolderWithNoDependenciesSendsTheAllowlistVerbatim(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	access := api.DataAppAccess{
		AllowedTableIDs:    []string{"table-orders"},
		AllowedSecretIDs:   []string{"secret-1"},
		AllowedAgentIDs:    []string{"agent-1"},
		AllowedWorkflowIDs: []string{"workflow-1"},
		AllowedCodexIDs:    []string{"cdx-1"},
		AllowedMetricIDs:   []string{"table-revenue"},
		Capabilities:       []string{"ai"},
	}
	root := aliasAppFolder(t, f, nil, nil, access,
		map[string]string{"App.tsx": "const q = \"{{ ref('table-orders') }}\";\n"})

	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("want one create, got %+v", f.created)
	}
	if !api.SameAccess(f.created[0].DataAppAccess, access) {
		t.Fatalf("the create carried %+v, want %+v", f.created[0].DataAppAccess, access)
	}
}

// --- pipelines --------------------------------------------------------------

// TestPipelinePushOfAnAliasFolderIsUpToDateOnTheSecondRun is the acceptance test
// for the pipeline loop, which is the one whose remote content is SQL read back
// through Canonicalize — so it exercises the order the two steps run in.
func TestPipelinePushOfAnAliasFolderIsUpToDateOnTheSecondRun(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1"})
	f.AddTable(&api.Table{
		ID: "table-revenue", Name: "revenue", Kind: api.TableKindDerived, FeatureID: "collection-1",
		Code: "SELECT 1 FROM {{ ref('table-orders') }}",
	})

	root := t.TempDir()
	local := "SELECT 1 FROM {{ ref('orders') }}"
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Sales pipeline",
		Dependencies: tableDep(),
		Stacks: map[string]wfdir.Stack{
			"dev": {
				URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1",
				Bind: map[string]string{"orders": "table-orders"},
			},
		},
	}
	writePipelineFolder(t, root, manifest, map[string]string{"revenue.sql": local})
	// Bound by hand, because the point of this test is the EDIT cycle rather than
	// the create: the local file is in ALIAS form and the live row is in ID form,
	// which is exactly the state a promoted folder is in.
	lock := &wfdir.Lock{}
	lock.SetBinding("dev", wfdir.Binding{Tables: map[string]string{"revenue.sql": "table-revenue"}})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatalf("save lock: %v", err)
	}
	// The baseline the last sync would have left: hashes of the file as it is on
	// DISK, alias form and all, which is the invariant this whole pass rests on.
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"revenue.sql": local},
		map[string]wfdir.TableState{"revenue.sql": {TableID: "table-revenue"}})
	// A local edit, so the first push has something to do.
	editFile(t, root, "revenue.sql", "SELECT 2 FROM {{ ref('orders') }}")

	if _, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want one PUT, got %+v", f.updates)
	}
	if f.updates[0].Code != "SELECT 2 FROM {{ ref('table-orders') }}" {
		t.Fatalf("the server was sent %q", f.updates[0].Code)
	}
	if len(f.updates[0].InputModels) != 1 || f.updates[0].InputModels[0] != "table-orders" {
		t.Fatalf("inputModels came from the disk form: %+v", f.updates[0].InputModels)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date — a de-alias site is missing:\n%s", out)
	}
	// And the read-only report agrees, which is the leg the push's own
	// up-to-date shortcut never reaches: it compares the LIVE row's canonical
	// SQL, in id form, against a baseline taken from a file in alias form.
	out, _, err = runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if drift := firstTableDrift(t, out); drift != driftNone {
		t.Fatalf("status reports drift %q on a folder that just pushed:\n%s", drift, out)
	}
}

// TestPipelineFirstPushResolvesASiblingStemAfterCreatingIt is the case a stem
// cannot be resolved up front: on a FIRST push the sibling's table does not
// exist, so the id only arrives when its own create returns.
//
// Two assertions, and both matter. `a.sql` is created FIRST — topoOrder has to
// see the stem ref as an edge, or the two are ordered alphabetically and the
// downstream builds against a table that is not there. And `b.sql` is sent with
// a's REAL id, resolved inside the loop.
func TestPipelineFirstPushResolvesASiblingStemAfterCreatingIt(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	// Named so the ALPHABET disagrees with the dependency order: `b` reads from
	// `a`, and a graph that saw no edge would push `a` first by luck. `zz` reads
	// from `a` too and sorts last either way, so the ordering assertion below is
	// about the edge and not about the names.
	writePipelineFolder(t, root, manifest, map[string]string{
		"zz.sql": "SELECT * FROM {{ ref('a') }}",
		"a.sql":  "SELECT 1",
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.created) != 2 {
		t.Fatalf("want two creates, got %+v", f.created)
	}
	if f.created[0].Name != "a" {
		t.Fatalf("the upstream was not created first: %v then %v", f.created[0].Name, f.created[1].Name)
	}
	binding, _, err := pipelineManifestOf(t, root).Binding(f.Key())
	if err != nil {
		t.Fatalf("read the binding back: %v", err)
	}
	aID := binding.Tables["a.sql"]
	if aID == "" {
		t.Fatal("a.sql was not bound")
	}
	// The downstream's create carries the id, not the stem — resolved inside the
	// loop, from a binding that did not exist when the push started.
	if want := "SELECT * FROM {{ ref('" + aID + "') }}"; f.created[1].Code != want {
		t.Fatalf("downstream sent %q, want %q", f.created[1].Code, want)
	}
	if len(f.created[1].InputModels) != 1 || f.created[1].InputModels[0] != aID {
		t.Fatalf("inputModels = %+v", f.created[1].InputModels)
	}

	// And it round-trips: the second push has nothing to do.
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date:\n%s", out)
	}
}

// TestPipelinePushLeavesALiteralSiblingIdAlone is the other half of the stem
// rule, and the reason the de-alias is per FILE rather than per folder: writing
// a sibling's table id literally is the spelling every pipeline folder written
// before this slice uses, and the command help still documents it. A folder-wide
// id → stem map turned all of them into names and reported drift on tables
// nobody had touched.
func TestPipelinePushLeavesALiteralSiblingIdAlone(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// revenue.sql reads orders.sql BY ID, which is the seeded folder's shape.
	editFile(t, root, "revenue.sql", "SELECT 2 FROM {{ ref('table-orders') }}")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if f.updates[0].Code != "SELECT 2 FROM {{ ref('table-orders') }}" {
		t.Fatalf("the id form was rewritten on the way out: %q", f.updates[0].Code)
	}
	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if drift := firstTableDrift(t, out); drift != driftNone {
		t.Fatalf("status reports drift %q on an id-form folder:\n%s", drift, out)
	}
}

// TestPipelinePushRefusesAFileThatSpellsOneSiblingTwoWays is the seam between
// the two tests above: both spellings of a sibling are legal, and a file that
// uses BOTH for the SAME sibling can never be clean again.
//
// toWire collapses the pair to one id, so the server stores one spelling for
// two occurrences; coming back, the de-alias has one answer per id and rewrites
// both to it. The literal one then reads back as a word the file does not
// contain — drift on every status, reproduced by every push, with nothing the
// author can edit that fixes it except the change this refusal asks for.
//
// It has to be its own refusal because literalBindTargets, which refuses the
// same shape for a DECLARED alias, cannot see this one: a sibling's stem is not
// a bind target and nothing in `bind` ever names it.
func TestPipelinePushRefusesAFileThatSpellsOneSiblingTwoWays(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// Both spellings of orders.sql, in one file. Each is legal on its own — the
	// two tests above are exactly those two cases.
	editFile(t, root, "revenue.sql",
		"SELECT * FROM {{ ref('table-orders') }} JOIN {{ ref('orders') }} USING (id)")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatalf("push accepted a file that spells one sibling two ways:\n%s", out)
	}
	if !strings.Contains(stderr+out, "orders.sql") || !strings.Contains(stderr+out, "drifted for ever") {
		t.Fatalf("the refusal does not name the sibling or say what breaks:\n%s\n%s", stderr, out)
	}
	// Nothing was sent for it. A refusal that still wrote the SQL would have
	// established the permanent drift it exists to prevent.
	for _, u := range f.updates {
		if strings.Contains(u.Code, "orders") {
			t.Fatalf("the refused file was written anyway: %+v", u)
		}
	}
}

// --- the two failures that are silent everywhere else -----------------------

// TestPushRefusesDependenciesDeclaredBelowVersionThree.
//
// The pair is one this build cannot write, so it only ever arrives by hand —
// which is exactly what a reader copying the `dependencies` JSON out of a guide
// into an existing manifest produces. It has to be refused because it defeats
// the gate that exists for it: the file MEANS version 3 (its markers hold names,
// and the ids are nowhere in the source) while DECLARING that a version-1 CLI
// may read it, so a colleague on last week's build opens it, passes the version
// check, resolves nothing, and sends the alias to the server as a literal secret
// id — which the secret family soft-fails into a warning. Green push, workflow
// deployed with nothing bound, failure at run time.
func TestPushRefusesDependenciesDeclaredBelowVersionThree(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	declareFormatVersion(t, root, 2)
	f.Requests = nil

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// The fix is one line, and the message has to quote it — a refusal that only
	// names the problem sends the reader to the version constant.
	if !strings.Contains(err.Error(), `"formatVersion": 3`) {
		t.Errorf("refusal does not name the line to add: %v", err)
	}
	for _, req := range f.Requests {
		if strings.Contains(req, "PUT") || strings.Contains(req, "POST") {
			t.Errorf("the refusal came after a write: %v", f.Requests)
		}
	}
}

// TestStatusStillOpensAFolderDeclaredBelowVersionThree is the other half, and
// the reason the refusal is at acceptance rather than at load: the folder stays
// inspectable. Refusing it in LoadManifest would make `status` — the command you
// run when you are already suspicious — the one command that cannot explain what
// is wrong.
func TestStatusStillOpensAFolderDeclaredBelowVersionThree(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	declareFormatVersion(t, root, 2)

	var out string
	stderr := captureStderr(t, func() {
		var err error
		out, err = runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(stderr, "formatVersion") {
		t.Errorf("status said nothing about the version:\n%s", stderr)
	}
	if decodeJSON(t, out)["root"] != root {
		t.Errorf("status did not report the folder:\n%s", out)
	}
}

// TestPushRefusesAnAliasUsedUnderTheWrongMarkerFamily.
//
// `orders` is declared as a table and bound, and the code uses it under BOTH a
// ref and a secret. Every other check in the layer passes: CheckBind sees a
// declared alias bound to a table id, literalBindTargets sees no literal id, and
// the dead-config warning stays quiet because the table-kind key IS used — by
// the ref that also names it. The secret marker ships the literal string
// `orders`, the server soft-fails it into a warning, and the workflow deploys
// with no secret bound at all.
func TestPushRefusesAnAliasUsedUnderTheWrongMarkerFamily(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f,
		"read(\"{{ ref('orders') }}\")\nkey = \"{{ secret('orders', 'token') }}\"\n")
	f.Requests = nil

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// Both kinds, because "orders is wrong here" is not actionable without
	// saying which of the two declarations the reader meant.
	if !strings.Contains(err.Error(), "as a table, not a secret") {
		t.Errorf("refusal does not name both kinds: %v", err)
	}
	for _, req := range f.Requests {
		if strings.Contains(req, "PUT") || strings.Contains(req, "POST") {
			t.Errorf("the refusal came after a write: %v", f.Requests)
		}
	}
}

// TestPushRefusesAnUnresolvableSecretMarker: the same silence through the other
// door, and the one family it is worth refusing on.
//
// `hubspot` is declared nowhere. Left alone it reaches the server as a literal
// secret id — and the secret legs are the ONLY ones the server answers with a
// WARNING rather than an error (rworkflow's secretIDs / querySecretIDs are
// SeverityWarning; every other family is SeverityError). So this is the one
// unresolvable name that pushes green.
func TestPushRefusesAnUnresolvableSecretMarker(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f,
		"read(\"{{ ref('orders') }}\")\nkey = \"{{ secret('hubspot', 'token') }}\"\n")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), `"hubspot"`) || !strings.Contains(err.Error(), "SOFT-FAILS") {
		t.Errorf("refusal does not name the secret and why it is worth stopping for: %v", err)
	}
}

// TestPushDoesNotRefuseANonIdSecretInAFolderThatDeclaresNothing pins the
// narrowing, and it is the reason the two legs above are gated on the folder
// declaring dependencies at all.
//
// The alias layer is INERT for every folder in the field — that is the property
// the whole branch rests on. A folder that declares nothing has no alias for a
// marker to be confused with, so refusing its `{{ secret('name') }}` would be a
// new opinion about source the server has always taken (with a warning), on
// folders this feature was never about.
func TestPushDoesNotRefuseANonIdSecretInAFolderThatDeclaresNothing(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{
		"main.py": "key = \"{{ secret('hubspot', 'token') }}\"\n",
	})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("a folder that declares no dependencies was refused: %v", err)
	}
}

// TestValidateFailsOnAnAliasRefusal.
//
// `validate` is what a CI job gates on, and its verdict is the one people trust
// instead of running the push. Alias findings printed only on stderr made it
// report `ok: true` for a folder `push` refuses outright — and misled a second
// time over, since the server's own finding for an unbound secret is a WARNING.
func TestValidateFailsOnAnAliasRefusal(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	unbindStack(t, root, "dev")

	out, err := runCLI(t, root, "wf", "validate", "--json")
	if err == nil {
		t.Fatal("validate exited zero on a folder push refuses")
	}
	payload := decodeJSON(t, out)
	if payload["ok"].(bool) {
		t.Error("ok = true with an unbound alias")
	}
	refusals, ok := payload["aliasRefusals"].([]any)
	if !ok || len(refusals) == 0 {
		t.Fatalf("the refusals are not in the payload:\n%s", out)
	}
	if !strings.Contains(refusals[0].(string), `"orders"`) {
		t.Errorf("aliasRefusals does not name the alias: %v", refusals)
	}
}

// --- helpers ----------------------------------------------------------------

// declareFormatVersion rewrites the manifest's stamped version DOWN, textually,
// because nothing in this CLI will write the file that way: MarshalJSON raises
// the stamp to what the content requires and never lowers it. Hand-editing is
// the only way the state under test is reached, which is the point.
func declareFormatVersion(t *testing.T, root string, version int) {
	t.Helper()
	path := filepath.Join(root, wfdir.ManifestName)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	patched := strings.Replace(string(body), `"formatVersion": 3`, fmt.Sprintf(`"formatVersion": %d`, version), 1)
	if patched == string(body) {
		t.Fatalf("the manifest was not stamped at 3 to begin with:\n%s", body)
	}
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func readLocal(t *testing.T, root, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// unbindStack drops a stack's whole bind map, which is what a folder looks like
// between "the declarations were merged" and "somebody ran ronja bind".
func unbindStack(t *testing.T, root, name string) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	stack := m.Stacks[name]
	stack.Bind = nil
	m.Stacks[name] = stack
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// workflowIDOf reads back the id the first push recorded — from the lock for a
// stack folder, from the manifest for a legacy instances[] one, since both
// shapes are used here.
func workflowIDOf(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	for _, stack := range lock.Stacks {
		if stack.WorkflowID != "" {
			return stack.WorkflowID
		}
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, inst := range m.Instances {
		if inst.Binding.WorkflowID != "" {
			return inst.Binding.WorkflowID
		}
	}
	t.Fatal("no workflow id recorded")
	return ""
}

// appDraftIDOf is the row a data-app push actually wrote to: the draft when
// there is one, else the app itself.
func appDraftIDOf(t *testing.T, root string, f *fakeAppInstance) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	for _, stack := range lock.Stacks {
		if stack.DataAppID == "" {
			continue
		}
		if draft, open := f.draftOf[stack.DataAppID]; open {
			return draft
		}
		return stack.DataAppID
	}
	t.Fatal("no data app id recorded")
	return ""
}

// firstTableDrift reads the drift verdict of the first table in a
// `pipeline status --json` payload.
func firstTableDrift(t *testing.T, out string) string {
	t.Helper()
	payload := decodeJSON(t, out)
	remote, ok := payload["remote"].(map[string]any)
	if !ok {
		t.Fatalf("no remote block:\n%s", out)
	}
	tables, ok := remote["tables"].([]any)
	if !ok || len(tables) == 0 {
		t.Fatalf("no tables reported:\n%s", out)
	}
	drift, _ := tables[0].(map[string]any)["drift"].(string)
	return drift
}
