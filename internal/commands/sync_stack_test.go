package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// stackTreeFolder writes a workflow folder at root/rel deploying to Main as
// "main", declaring deps and binding bind there, with main.py holding code.
func stackTreeFolder(t *testing.T, f *fakeCopyInstance, root, rel, deps, bind, code string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "formatVersion": 7,
  "kind": "workflow",
  "title": %q,
  "entrypoint": "main.py",
  "dependencies": %s,
  "stacks": {
    "main": {"url": %q, "tenantID": %q, "featureID": "col-main", "bind": %s}
  }
}
`, filepath.Base(rel), deps, f.URL(), testTenantID, bind)
	if err := os.WriteFile(filepath.Join(dir, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// `sync stack add` with only --feature names the stack after the feature's
// Space and declares it in every folder of the tree, applying the picker: the
// one Dev secret of the name is bound, a read table with no row in Dev and a
// write table with none in the stack's feature are reported with the copy
// line that makes one. Main is left as it was.
func TestSyncStackAddDeclaresEveryFolderAndReportsWhatItCouldNotFill(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.rows["secret-main-hub"] = fakeBindRow{featureID: "col-main", name: "hubspot"}
	f.rows["secret-dev-hub"] = fakeBindRow{featureID: "col-dev", name: "hubspot"}
	f.rows["table-main-notes"] = fakeBindRow{featureID: "col-main", name: "board_notes"}
	f.hits["hubspot"] = []api.SearchHit{exactHit("secret", "secret-main-hub", "hubspot"), exactHit("secret", "secret-dev-hub", "hubspot")}
	f.hits["erp_orders"] = []api.SearchHit{exactHit("table", "table-main-orders", "erp_orders")}
	f.hits["board_notes"] = []api.SearchHit{exactHit("table", "table-main-notes", "board_notes")}
	tree := t.TempDir()
	report := stackTreeFolder(t, f, tree, "workflows/region-report", `{"hubspot": {"kind": "secret"}, "erp_orders": {"kind": "table"}}`,
		`{"hubspot": "secret-main-hub", "erp_orders": "table-main-orders"}`, "print('hi')\n")
	board := stackTreeFolder(t, f, tree, "workflows/board-writer", `{"board_notes": {"kind": "table", "write": true}}`,
		`{"board_notes": "table-main-notes"}`, "print('hi')\n")

	out, err := runCLI(t, tree, "sync", "stack", "add", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), `stack "dev" has 2 unbound dependencies`) {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	for _, root := range []string{report, board} {
		dev := stackOf(t, root, "dev")
		if dev.FeatureID != "col-dev" || dev.SpaceID != "space-dev" || dev.Space != "Dev" || dev.TenantID != testTenantID {
			t.Fatalf("%s: dev = %+v", root, dev)
		}
	}
	if got := stackOf(t, report, "dev").Bind; got["hubspot"] != "secret-dev-hub" || got["erp_orders"] != "" {
		t.Fatalf("region-report's dev bind = %v, want only hubspot bound, to Dev's secret", got)
	}
	if got := stackOf(t, board, "main"); got.Bind["board_notes"] != "table-main-notes" || got.FeatureID != "col-main" {
		t.Fatalf("main changed: %+v", got)
	}
	for _, want := range []string{
		"stack dev → Space Dev · feature Feature (col-dev) — 2 folders",
		"workflows/region-report  hubspot (secret) → secret-dev-hub",
		"`ronja copy erp_orders --to-stack dev`",
		"workflows/board-writer  board_notes (table) — ",
		"`ronja copy board_notes --no-rows --to-stack dev`",
		`Declared stack "dev" in 2 folders`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
}

// A default name another folder's stack already uses for another feature is
// refused, asking for a name, and nothing is written.
func TestSyncStackAddRefusesADefaultNameThatClashes(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.featureSpace["col-dev-other"] = "space-dev"
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "print('hi')\n")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev-other", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	before := readManifestBytes(t, root)

	_, err = runCLI(t, tree, "sync", "stack", "add", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), `already has a stack "dev"`) || !strings.Contains(err.Error(), "ronja sync stack add <name> --feature col-dev") {
		t.Fatalf("stack add over a clashing default: %v", err)
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("ronja.json changed:\n%s", after)
	}
}

// mainPayload is what a push of the folder to its main stack sends: every
// file through main's codec.
func mainPayload(t *testing.T, f *fakeCopyInstance, root string) map[string]string {
	t.Helper()
	previous := flagStack
	flagStack = "main"
	defer func() { flagStack = previous }()
	folder, err := openFolderLocallyAt(root, &config.Resolved{URL: f.URL(), TenantID: testTenantID}, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := readLocalFiles(root, folder.Kind)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, file := range validateFilesOf(folder.Codec, files) {
		out[file.Path] = file.Content
	}
	return out
}

// The lift rewrites each raw id in a marker into a declared dependency bound
// to that same id in main, so main's pushed files are byte-identical, and the
// new stack is declared over the lifted names.
func TestSyncStackAddLiftKeepsTheExistingStackPayloadByteIdentical(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.rows["workflow-main-sync"] = fakeBindRow{featureID: "col-main", name: "Nightly Sync"}
	tree := t.TempDir()
	code := "orders = {{ ref('table-main-orders') }}\nagain = {{ ref(\"table-main-orders\") }}\nrun = {{ workflow('workflow-main-sync') }}\n"
	root := stackTreeFolder(t, f, tree, "workflows/region-report", `{}`, `{}`, code)
	before := mainPayload(t, f, root)

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "unbound") {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if after := mainPayload(t, f, root); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("main's push changed:\n--- before\n%v\n--- after\n%v", before, after)
	}
	body, _ := os.ReadFile(filepath.Join(root, "main.py"))
	if string(body) != "orders = {{ ref('erp_orders') }}\nagain = {{ ref(\"erp_orders\") }}\nrun = {{ workflow('nightly_sync') }}\n" {
		t.Fatalf("main.py after the lift:\n%s", body)
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if dep := m.Dependencies["nightly_sync"]; dep.Kind != "workflow" || !dep.Run {
		t.Fatalf("nightly_sync = %+v, want a run dependency", dep)
	}
	if got := m.Stacks["main"].Bind; got["erp_orders"] != "table-main-orders" || got["nightly_sync"] != "workflow-main-sync" {
		t.Fatalf("main bind = %v", got)
	}
	if dev, ok := m.Stacks["dev"]; !ok || dev.Bind["erp_orders"] != "" {
		t.Fatalf("dev = %+v, want it declared with erp_orders unbound", dev)
	}
	if !strings.Contains(out, "table-main-orders → erp_orders (table, read)") {
		t.Fatalf("the lift was not listed:\n%s", out)
	}
}

// An output literal is placed by the server's scan and becomes
// {{ write('<alias>') }}, a write dependency bound in main to the same table:
// main's run still writes that table, and its code differs only in that call
// argument.
func TestSyncStackAddLiftsAnOutputLiteralIntoAWriteMarker(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.rows["table-main-out"] = fakeBindRow{featureID: "col-main", name: "region_totals"}
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/region-report", `{}`, `{}`, "df = load()\ntools.replaceTable(df, \"table-main-out\")\n")
	before := mainPayload(t, f, root)

	if out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes"); err == nil {
		t.Fatalf("stack add left nothing unbound:\n%s", out)
	}
	body, _ := os.ReadFile(filepath.Join(root, "main.py"))
	if string(body) != "df = load()\ntools.replaceTable(df, {{ write('region_totals') }})\n" {
		t.Fatalf("main.py after the lift:\n%s", body)
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if dep := m.Dependencies["region_totals"]; dep.Kind != "table" || !dep.Write {
		t.Fatalf("region_totals = %+v, want a write dependency", dep)
	}
	after := mainPayload(t, f, root)
	want := strings.Replace(before["main.py"], `"table-main-out"`, `{{ write('table-main-out') }}`, 1)
	if after["main.py"] != want {
		t.Fatalf("main's main.py:\n%s\nwant:\n%s", after["main.py"], want)
	}
}

// A folder that writes files is not declared on a stack outside the default
// Space: every deploy of it there would be refused, so it is reported instead.
func TestSyncStackAddReportsAFolderWriterOutsideTheDefaultSpace(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.defaultSpace = "space-main"
	tree := t.TempDir()
	writer := stackTreeFolder(t, f, tree, "workflows/exporter", `{"exports": {"kind": "folder", "write": true}}`, `{"exports": "/exports"}`, "print('hi')\n")
	plain := stackTreeFolder(t, f, tree, "workflows/plain", `{}`, `{}`, "print('hi')\n")

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "1 undeclared folder") {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if _, declared := loadStacks(t, writer)["dev"]; declared {
		t.Fatal("the file writer was declared on dev")
	}
	if _, declared := loadStacks(t, plain)["dev"]; !declared {
		t.Fatal("the plain folder was not declared")
	}
	if !strings.Contains(out, "workflows/exporter  not declared: it writes files") {
		t.Fatalf("report:\n%s", out)
	}
}

// folderBytes is every file directly in root, by name.
func folderBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func requireUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	if after := folderBytes(t, root); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("%s changed:\n--- before\n%v\n--- after\n%v", root, before, after)
	}
}

// An output literal the lift cannot place, in a file after one it can, refuses
// the command naming file:line before any file is written.
func TestSyncStackAddRefusesALiteralItCannotLiftBeforeWritingAnything(t *testing.T) {
	f := newFakeCopyInstance(t)
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "orders = {{ ref('table-main-orders') }}\n")
	if err := os.WriteFile(filepath.Join(root, "z.py"), []byte("tools.replaceTable(df, \"table-main-out\")  # \"table-main-out\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := folderBytes(t, root)

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "workflows/a/z.py:1:") || !strings.Contains(err.Error(), "edit it by hand") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	requireUnchanged(t, root, before)
}

// One folder whose declaration is refused stops every folder's lift too.
func TestSyncStackAddWritesNoLiftWhenAnotherFolderCannotBeDeclared(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.featureSpace["col-dev-other"] = "space-dev"
	tree := t.TempDir()
	lifts := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "orders = {{ ref('table-main-orders') }}\n")
	refused := stackTreeFolder(t, f, tree, "workflows/b", `{}`, `{}`, "print('hi')\n")
	m, err := wfdir.LoadManifest(refused, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev-other", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(refused, m); err != nil {
		t.Fatal(err)
	}
	beforeA, beforeB := folderBytes(t, lifts), folderBytes(t, refused)

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "workflows/b: stack \"dev\" already creates its resources in col-dev-other") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	requireUnchanged(t, lifts, beforeA)
	requireUnchanged(t, refused, beforeB)
}

// The lift and the declaration are one confirmation: declining it writes
// neither.
func TestSyncStackAddDeclinedWritesNothing(t *testing.T) {
	f := newFakeCopyInstance(t)
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "orders = {{ ref('table-main-orders') }}\n")
	before := folderBytes(t, root)
	restore := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = restore })
	withStdin(t, "n\n")

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev")
	if err != nil {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	requireUnchanged(t, root, before)
}

// The lift reuses a plain dependency only where every stack binds it to the
// lifted id: one bound elsewhere to a copy would move that stack off the table
// its code names today, so the lift declares its own alias instead.
func TestSyncStackAddLiftReusesAnAliasOnlyWhereEveryStackBindsTheID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devBind map[string]string
		alias   string
	}{
		{"only main binds it", nil, "erp_orders"},
		{"dev binds a copy", map[string]string{"erp_orders": "table-dev-copy1"}, "erp_orders_2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCopyInstance(t)
			f.featureSpace["col-staging"] = "space-staging"
			f.spaceNames["space-staging"] = "Staging"
			tree := t.TempDir()
			root := stackTreeFolder(t, f, tree, "workflows/a", `{"erp_orders": {"kind": "table", "write": true}}`,
				`{"erp_orders": "table-main-orders"}`, "orders = {{ ref('table-main-orders') }}\n")
			m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
			if err != nil {
				t.Fatal(err)
			}
			m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev", SpaceID: "space-dev", Space: "Dev",
				Bind: tc.devBind}
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}

			out, err := runCLI(t, tree, "sync", "stack", "add", "staging", "--feature", "col-staging", "--yes")
			if err == nil || !strings.Contains(err.Error(), "unbound") {
				t.Fatalf("stack add: %v\n%s", err, out)
			}
			if body, _ := os.ReadFile(filepath.Join(root, "main.py")); string(body) != "orders = {{ ref('"+tc.alias+"') }}\n" {
				t.Fatalf("main.py after the lift:\n%s", body)
			}
			m, err = wfdir.LoadManifest(root, wfdir.WorkflowKind)
			if err != nil {
				t.Fatal(err)
			}
			if !m.Dependencies["erp_orders"].Write {
				t.Fatalf("dependencies = %+v, want erp_orders still a write", m.Dependencies)
			}
			if main, dev := m.Stacks["main"].Bind[tc.alias], m.Stacks["dev"].Bind[tc.alias]; main != "table-main-orders" || dev != "table-main-orders" {
				t.Fatalf("%s: main binds %q, dev binds %q; both must keep the table the code names today", tc.alias, main, dev)
			}
			if tc.devBind != nil && m.Stacks["dev"].Bind["erp_orders"] != "table-dev-copy1" {
				t.Fatalf("dev's own erp_orders moved to %q", m.Stacks["dev"].Bind["erp_orders"])
			}
		})
	}
}

// A workflow folder with several stacks on this organization is not scanned
// for output literals, and the report says so per folder.
func TestSyncStackAddNotesAFolderWhoseOutputLiteralsWereNotLookedFor(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.featureSpace["col-staging"] = "space-staging"
	f.spaceNames["space-staging"] = "Staging"
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "tools.replaceTable(df, \"table-main-out\")\n")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "stack", "add", "staging", "--feature", "col-staging", "--yes")
	if err != nil {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflows/a  output literals not lifted: ") || !strings.Contains(out, "several (dev, main)") {
		t.Fatalf("report:\n%s", out)
	}
	if body, _ := os.ReadFile(filepath.Join(root, "main.py")); string(body) != "tools.replaceTable(df, \"table-main-out\")\n" {
		t.Fatalf("main.py changed:\n%s", body)
	}
	out, err = runCLI(t, tree, "--json", "sync", "stack", "add", "staging", "--feature", "col-staging", "--yes")
	if err != nil {
		t.Fatalf("stack add --json: %v\n%s", err, out)
	}
	var report stackAddReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(report.Folders) != 1 || !strings.Contains(report.Folders[0].LiteralsSkipped, "several (dev, main, staging)") {
		t.Fatalf("folders = %+v", report.Folders)
	}
}

// A legacy instances[] entry the new stack's name adopts is adopted by the
// write after the confirmation, never while the folders are planned: a refusal
// elsewhere leaves its ronja.json as it was, and the run that succeeds names it.
func TestSyncStackAddAdoptsALegacyEntryOnlyWhenItWrites(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.featureSpace["col-dev-other"] = "space-dev"
	tree := t.TempDir()
	legacy := filepath.Join(tree, "workflows", "legacy")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "kind": "workflow",
  "title": "Legacy",
  "entrypoint": "main.py",
  "instances": [{"url": %q, "tenantID": %q, "featureID": "col-dev", "workflowID": "wf-1"}]
}
`, f.URL(), testTenantID)
	if err := os.WriteFile(filepath.Join(legacy, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := stackTreeFolder(t, f, tree, "workflows/b", `{}`, `{}`, "print('hi')\n")
	m, err := wfdir.LoadManifest(refused, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev-other", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(refused, m); err != nil {
		t.Fatal(err)
	}
	before := folderBytes(t, legacy)

	if out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes"); err == nil {
		t.Fatalf("stack add over a folder declaring dev elsewhere:\n%s", out)
	}
	requireUnchanged(t, legacy, before)

	delete(m.Stacks, "dev")
	if err := wfdir.SaveManifest(refused, m); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if dev := stackOf(t, legacy, "dev"); dev.FeatureID != "col-dev" {
		t.Fatalf("legacy's dev = %+v, want the instances entry named dev", dev)
	}
}

// Re-run over folders that already declare the stack, the lift binds the
// lifted id into the other stacks and leaves the declared one to its picker,
// so Dev is never handed Main's row by the lift.
func TestSyncStackAddReRunLeavesTheDeclaredStackToItsPicker(t *testing.T) {
	f := newFakeCopyInstance(t)
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "orders = {{ ref('table-main-orders') }}\n")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), `stack "dev" has 1 unbound dependency`) {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if main, dev := stackOf(t, root, "main").Bind["erp_orders"], stackOf(t, root, "dev").Bind["erp_orders"]; main != "table-main-orders" || dev != "" {
		t.Fatalf("main binds %q, dev binds %q: want main on its row and dev left to the picker", main, dev)
	}
	if !strings.Contains(out, "`ronja copy erp_orders --to-stack dev`") {
		t.Fatalf("the picker's copy line is missing:\n%s", out)
	}
}

// A write lift is not bound into another stack that records a Space other
// than the row's: it is left unbound there, reported per folder and stack, and
// counted in the exit code. A stack with no Space binds it as before.
func TestSyncStackAddLeavesAWriteLiftUnboundInAnotherSpacesStack(t *testing.T) {
	f := newFakeCopyInstance(t)
	f.featureSpace["col-staging"] = "space-staging"
	f.spaceNames["space-staging"] = "Staging"
	tree := t.TempDir()
	root := stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "x = 1\n{{ write('table-main-orders') }}\n")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Stacks["dev"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev", SpaceID: "space-dev", Space: "Dev"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "stack", "add", "staging", "--feature", "col-staging", "--yes", "--json")
	if err == nil || !strings.Contains(err.Error(), `erp_orders in stack "dev"`) {
		t.Fatalf("stack add: %v\n%s", err, out)
	}
	if main, dev := stackOf(t, root, "main").Bind["erp_orders"], stackOf(t, root, "dev").Bind["erp_orders"]; main != "table-main-orders" || dev != "" {
		t.Fatalf("main binds %q, dev binds %q: want main on its row and dev left unbound", main, dev)
	}
	var report stackAddReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got := report.Folders[0].LiftUnbound; len(got) != 1 || got[0].Stack != "dev" || got[0].Alias != "erp_orders" || !strings.Contains(got[0].Detail, "Space Main") {
		t.Fatalf("liftUnbound = %+v", got)
	}
}

// The stack is named by the argument; an inherited --stack is refused rather
// than ignored.
func TestSyncStackAddRefusesTheStackFlag(t *testing.T) {
	f := newFakeCopyInstance(t)
	tree := t.TempDir()
	stackTreeFolder(t, f, tree, "workflows/a", `{}`, `{}`, "print('hi')\n")
	_, err := runCLI(t, tree, "sync", "stack", "add", "--stack", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "name the stack as the argument") {
		t.Fatalf("stack add --stack: %v", err)
	}
}

// Only a stack on the folder's own organization is a source for `ronja copy`,
// since a copy never crosses organizations.
func TestOtherStackBindsCountsOnlyThisOrganization(t *testing.T) {
	here := wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "ten-a"}
	f := &folder{Stack: "dev", Key: here, Manifest: &wfdir.Manifest{Stacks: map[string]wfdir.Stack{
		"dev":   {URL: here.URL, TenantID: here.TenantID},
		"other": {URL: here.URL, TenantID: "ten-b", Bind: map[string]string{"orders": "table-b"}},
	}}}
	if otherStackBinds(f, "orders") {
		t.Fatal("another organization's stack was offered as a copy source")
	}
	f.Manifest.Stacks["main"] = wfdir.Stack{URL: here.URL, TenantID: here.TenantID, Bind: map[string]string{"orders": "table-a"}}
	if !otherStackBinds(f, "orders") {
		t.Fatal("this organization's main stack was not offered")
	}
}
