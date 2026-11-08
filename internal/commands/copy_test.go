package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// fakeCopyInstance is the bind fake of one organization with a Dev and a Main
// Space, plus POST /feature/model/:id/copy as PR 1's route answers it: a copy
// is matched by its source, so a second call refreshes the same id.
type fakeCopyInstance struct {
	*fakeSearchInstance
	copied   map[string]string
	requests []copyRequestSeen
	// refuse answers every copy with this status and body instead.
	refuse     int
	refuseBody string
}

type copyRequestSeen struct {
	source string
	body   map[string]any
}

func newFakeCopyInstance(t *testing.T) *fakeCopyInstance {
	t.Helper()
	search := &fakeSearchInstance{t: t, hits: map[string][]api.SearchHit{}, featureStatus: map[string]int{}}
	search.featureSpace = map[string]string{"col-main": "space-main", "col-dev": "space-dev"}
	search.spaceNames = map[string]string{"space-main": "Main", "space-dev": "Dev"}
	search.defaultSpace = "space-main"
	search.rows = map[string]fakeBindRow{
		"table-main-orders": {featureID: "col-main", name: "erp_orders"},
		"table-main-items":  {featureID: "col-main", name: "ERP Items"},
	}
	f := &fakeCopyInstance{fakeSearchInstance: search, copied: map[string]string{}}
	search.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echoSpace(w, r)
		if r.URL.Path == "/api/v2/workflow/validate" {
			serveOutputLiterals(w, r)
			return
		}
		source, isCopy := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/feature/model/"), "/copy")
		if !isCopy || r.Method != http.MethodPost {
			search.serve(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.requests = append(f.requests, copyRequestSeen{source: source, body: body})
		if f.refuse != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.refuse)
			_, _ = w.Write([]byte(f.refuseBody))
			return
		}
		outcome, id := "refreshed", f.copied[source]
		if id == "" {
			outcome, id = "created", fmt.Sprintf("table-dev-copy%d", len(f.copied)+1)
			f.copied[source] = id
		}
		rows := 12
		if body["rows"] == false {
			rows = 0
		}
		writeJSON(w, map[string]any{"table": map[string]any{"id": id, "name": search.rows[source].name, "featureID": body["featureID"]},
			"rowsCopied": rows, "outcome": outcome})
	}))
	t.Cleanup(search.server.Close)
	signInTo(t, search.URL())
	return f
}

// serveOutputLiterals answers POST /workflow/validate with the write targets
// the server's scan reports for the posted files, none of them readable.
func serveOutputLiterals(w http.ResponseWriter, r *http.Request) {
	var in api.ValidateInput
	_ = json.NewDecoder(r.Body).Decode(&in)
	writeJSON(w, map[string]any{"findings": []any{}, "resolved": map[string]any{}, "writeTargets": fakeWriteTargets(in.Files, nil)})
}

// copyFolder is a workflow folder deploying to Main and to Dev, which declares
// erp_orders and binds it in main only.
func copyFolder(t *testing.T, f *fakeCopyInstance) string {
	t.Helper()
	root := t.TempDir()
	manifest := fmt.Sprintf(`{
  "formatVersion": 9,
  "kind": "workflow",
  "title": "Region Report",
  "entrypoint": "main.py",
  "dependencies": {"erp_orders": {"kind": "table"}},
  "stacks": {
    "main": {"url": %[1]q, "tenantID": %[2]q, "featureID": "col-main", "bind": {"erp_orders": "table-main-orders"}},
    "dev": {"url": %[1]q, "tenantID": %[2]q, "featureID": "col-dev", "spaceID": "space-dev", "space": "Dev"}
  }
}
`, f.URL(), testTenantID)
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readManifestBytes(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, wfdir.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A dependency is copied from the row the other stack binds, into the target
// stack's feature, and the copy is bound there. A second run refreshes the
// same table, so ronja.json is left exactly as the first run wrote it.
func TestCopyCreatesThenRefreshesAndBindsTheCopy(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)

	out, err := runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev")
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	if len(f.requests) != 1 || f.requests[0].source != "table-main-orders" || f.requests[0].body["featureID"] != "col-dev" {
		t.Fatalf("copy requests = %+v, want main's erp_orders copied into col-dev", f.requests)
	}
	if _, sent := f.requests[0].body["rows"]; sent {
		t.Fatalf("rows was sent on a plain copy: %v", f.requests[0].body)
	}
	if got := stackOf(t, root, "dev").Bind["erp_orders"]; got != "table-dev-copy1" {
		t.Fatalf("dev binds erp_orders to %q, want the copy", got)
	}
	if got := stackOf(t, root, "main").Bind["erp_orders"]; got != "table-main-orders" {
		t.Fatalf("main's bind changed: %q", got)
	}
	for _, want := range []string{"stack dev · Space Dev", "erp_orders  → table-dev-copy1 created, 12 rows copied from stack main's erp_orders (table-main-orders)", "Bound in stacks.dev.bind: erp_orders"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}

	before := readManifestBytes(t, root)
	out, err = runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev")
	if err != nil {
		t.Fatalf("second copy: %v\n%s", err, out)
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("a refresh rewrote ronja.json:\n--- before\n%s\n--- after\n%s", before, after)
	}
	if !strings.Contains(out, "table-dev-copy1 refreshed") || strings.Contains(out, "Bound in") {
		t.Fatalf("second report:\n%s", out)
	}
}

// A table given by id that the folder does not declare becomes a dependency
// named after the table, bound to the copy in the target stack.
func TestCopyByIDDeclaresADependencyNamedAfterTheTable(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)

	if out, err := runCLI(t, root, "copy", "table-main-items", "--to-stack", "dev"); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if dep, ok := m.Dependencies["erp_items"]; !ok || dep.Kind != "table" {
		t.Fatalf("dependencies = %+v, want erp_items declared as a table", m.Dependencies)
	}
	if got := m.Stacks["dev"].Bind["erp_items"]; got != "table-dev-copy1" {
		t.Fatalf("dev binds erp_items to %q", got)
	}
	if _, bound := m.Stacks["main"].Bind["erp_items"]; bound {
		t.Fatal("main was bound too")
	}
}

// --no-rows asks the route for the columns only.
func TestCopyNoRowsAsksForColumnsOnly(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)

	out, err := runCLI(t, root, "copy", "erp_orders", "--no-rows", "--to-stack", "dev")
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	if len(f.requests) != 1 || f.requests[0].body["rows"] != false {
		t.Fatalf("requests = %+v, want rows:false", f.requests)
	}
	if !strings.Contains(out, "table-dev-copy1 created, empty, columns from") {
		t.Fatalf("report:\n%s", out)
	}
}

// A same-named table in the target feature that is not a copy of this source
// is the route's 409 name_taken: the server's sentence, then how to pass a
// name from the CLI. Nothing is bound.
func TestCopyNameTakenPointsAtAName(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)
	before := readManifestBytes(t, root)
	f.refuse, f.refuseBody = http.StatusConflict,
		`{"error":"this feature already has a table named \"erp_orders\" (table-dev-own), and it is not a copy of this source — send \"name\" to copy it under another name","code":"name_taken"}`

	out, err := runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev")
	if err == nil {
		t.Fatalf("a name_taken copy exited 0:\n%s", out)
	}
	for _, want := range []string{"not copied (name_taken)", "table-dev-own", "ronja copy erp_orders --to-stack dev --name <name>"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("ronja.json changed on a refused copy:\n%s", after)
	}
}

// The route's 504 copy_timeout keeps its own advice, and adds that nothing was
// bound and a retry is safe.
func TestCopyTimeoutSaysARetryIsSafe(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)
	before := readManifestBytes(t, root)
	f.refuse, f.refuseBody = http.StatusGatewayTimeout,
		`{"error":"the copy of table-main-orders did not finish within 80 seconds, the longest a request may last, before its target table was settled. Running the copy again creates or refreshes it. For a large source, build a smaller derived table from it (only the rows and columns you need) and copy that first.","code":"copy_timeout"}`

	out, err := runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev")
	if err == nil {
		t.Fatalf("a timed-out copy exited 0:\n%s", out)
	}
	for _, want := range []string{"not copied (copy_timeout)", "build a smaller derived table", "running the same `ronja copy` again is safe"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("ronja.json changed on a timed-out copy:\n%s", after)
	}
}

// A table copied by id twice is bound under the dependency the first run
// declared: the second run refreshes the copy and leaves ronja.json as it was.
func TestCopyByIDTwiceKeepsItsDependency(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)
	if out, err := runCLI(t, root, "copy", "table-main-items", "--to-stack", "dev"); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	before := readManifestBytes(t, root)

	out, err := runCLI(t, root, "copy", "table-main-items", "--to-stack", "dev")
	if err != nil {
		t.Fatalf("second copy: %v\n%s", err, out)
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("a refresh by id rewrote ronja.json:\n--- before\n%s\n--- after\n%s", before, after)
	}
	if !strings.Contains(out, "erp_items  → table-dev-copy1 refreshed") {
		t.Fatalf("second report:\n%s", out)
	}
}

// A load balancer's 504 carries no code: it is reported as copy_timeout, with
// the advice to wait and run the same command again.
func TestCopyBareGatewayTimeoutSaysToWaitAndRunAgain(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)
	before := readManifestBytes(t, root)
	f.refuse, f.refuseBody = http.StatusGatewayTimeout, `<html><body>504 Gateway Time-out</body></html>`

	out, err := runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev")
	if err == nil {
		t.Fatalf("a 504 copy exited 0:\n%s", out)
	}
	for _, want := range []string{"not copied (copy_timeout)", "the copy is probably still writing; wait a minute, then run the same command", "never a second one"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
	if after := readManifestBytes(t, root); after != before {
		t.Fatalf("ronja.json changed on a 504:\n%s", after)
	}
}

// A stack of another organization is never a copy's source: --from-stack
// naming one is refused, and the implicit pick does not count it, both before
// any copy is asked for.
func TestCopyNeverCrossesOrganizations(t *testing.T) {
	f := newFakeCopyInstance(t)
	root := copyFolder(t, f)
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies["erp_lines"] = wfdir.Dependency{Kind: "table"}
	m.Stacks["prod"] = wfdir.Stack{URL: f.URL(), TenantID: "ten-other", FeatureID: "col-prod",
		Bind: map[string]string{"erp_orders": "table-prod-orders", "erp_lines": "table-prod-lines"}}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err = runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev", "--from-stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "copies never cross organizations") {
		t.Fatalf("--from-stack across organizations: %v", err)
	}
	_, err = runCLI(t, root, "copy", "erp_lines", "--to-stack", "dev")
	if err == nil || !strings.Contains(err.Error(), "only stacks of another organization (prod) bind \"erp_lines\"") {
		t.Fatalf("implicit pick across organizations: %v", err)
	}
	if _, err := runCLI(t, root, "copy", "erp_orders", "--to-stack", "dev"); err != nil {
		t.Fatalf("copy from main, the one stack of this organization that binds it: %v", err)
	}
	if len(f.requests) != 1 || f.requests[0].source != "table-main-orders" {
		t.Fatalf("copies asked for = %+v, want main's table only", f.requests)
	}
}
