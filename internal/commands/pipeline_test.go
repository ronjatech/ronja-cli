package commands

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// --- capture ----------------------------------------------------------------

// runPipelineCLI captures stderr too: this loop's skip reports, drift refusals
// and staleness warnings go there, and they are what a person acts on.
func runPipelineCLI(t *testing.T, workdir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, workdir, args...)
	})
	return stdout, stderr, err
}

// countRequest counts EXACT "METHOD /path" matches: a substring count would fold
// ".../table-x" and ".../table-x/draft" together.
func countRequest(f *fakePipelineInstance, want string) int {
	n := 0
	for _, got := range f.requestOrder() {
		if got == want {
			n++
		}
	}
	return n
}

// --- init -------------------------------------------------------------------

// TestPipelineInitWritesAManifestWithNoEntrypoint: a pipeline folder has no file
// the server runs, so the committed manifest must not carry an entrypoint key
// inviting somebody to fill one in.
func TestPipelineInitWritesAManifestWithNoEntrypoint(t *testing.T) {
	f := newFakePipelineInstance(t)
	f.AddFeature("collection-1", "Sales", "private")
	signInPipeline(t, f)
	dir := t.TempDir()

	out, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "collection-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["kind"] != wfdir.KindPipeline {
		t.Errorf("kind = %v", payload["kind"])
	}
	if payload["featureID"] != "collection-1" {
		t.Errorf("featureID = %v", payload["featureID"])
	}

	body := readFile(t, dir, wfdir.ManifestName)
	if strings.Contains(body, "entrypoint") {
		t.Errorf("a pipeline manifest must carry no entrypoint key:\n%s", body)
	}
	binding := pipelineBindingOf(t, dir, f.Key())
	if binding.FeatureID != "collection-1" {
		t.Errorf("featureID = %q", binding.FeatureID)
	}
	if len(binding.Tables) != 0 {
		t.Errorf("init must bind no tables, got %+v", binding.Tables)
	}
	// Nothing table-side: init reads /me for the organization and probes the
	// feature, and makes no table request.
	if n := countRequest(f, "GET /api/v2/feature/model"); n != 0 {
		t.Errorf("init made %d table requests", n)
	}
}

// TestPipelineInitRefusesASecondTime: running init inside a folder that is
// already synced would overwrite a manifest holding real bindings.
func TestPipelineInitRefusesASecondTime(t *testing.T) {
	f := newFakePipelineInstance(t)
	f.AddFeature("collection-1", "Sales", "private")
	signInPipeline(t, f)
	dir := t.TempDir()

	if _, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "collection-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	_, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "collection-1")
	if err == nil {
		t.Fatal("a second init must be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal = %v", err)
	}
}

// TestPipelineInitChecksTheFeatureIDShape: --feature lands in a committed
// manifest nothing reads until the first push, so a typo would otherwise surface
// days later as a failed create. The shape check runs before confirmFeatureIn's
// round trip and names the real prefix.
func TestPipelineInitChecksTheFeatureIDShape(t *testing.T) {
	f := newFakePipelineInstance(t)
	f.AddFeature("col-legacy", "Legacy", "private")
	signInPipeline(t, f)

	for _, bad := range []string{"table-orders", "Sales pipeline", "feat-1"} {
		dir := t.TempDir()
		_, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", bad)
		if err == nil {
			t.Errorf("init accepted %q as a feature id", bad)
			continue
		}
		if !strings.Contains(err.Error(), "collection-") {
			t.Errorf("the refusal does not name the real prefix: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
			t.Errorf("a refused init wrote a manifest for %q", bad)
		}
	}
	// The legacy prefix is minted nowhere but still held by real rows.
	dir := t.TempDir()
	if _, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "col-legacy"); err != nil {
		t.Errorf("the legacy feature prefix must be accepted: %v", err)
	}
	// No --feature at all is allowed: that folder refuses at its first push.
	empty := t.TempDir()
	if _, _, err := runPipelineCLI(t, empty, "pipeline", "init"); err != nil {
		t.Errorf("init without --feature: %v", err)
	}
}

func TestPipelineInitCreatesNoDirectoryWhenItRefuses(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	dir := t.TempDir()

	target := filepath.Join(dir, "sales")
	if _, _, err := runPipelineCLI(t, dir, "pipeline", "init", target, "--feature", "table-oops"); err == nil {
		t.Fatal("a bad feature id must refuse")
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("a refused init created %s", target)
	}
}

// --- clone ------------------------------------------------------------------

func seedFeature(f *fakePipelineInstance, scope string) {
	f.AddFeature("collection-1", "Sales pipeline", scope)
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code: "SELECT * FROM {{ ref('table-orders') }}", InputModels: []string{"table-orders"}})
}

func TestPipelineCloneWritesOneFilePerDerivedTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// An integration table has no SQL a file could hold; a metric clones into
	// metrics/<name>.json, not .sql.
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw orders", FeatureID: "collection-1", Kind: "integration"})
	f.AddTable(&api.Table{ID: "table-mrr", Name: "MRR", FeatureID: "collection-1", Kind: api.TableKindMetric,
		MetricRecipe: json.RawMessage(`{"source":"table-orders","value":"revenue","version":1}`)})

	dir := t.TempDir()
	out, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--json")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	payload := decodeJSON(t, out)
	if payload["tables"] != float64(2) {
		t.Errorf("tables = %v", payload["tables"])
	}
	if payload["title"] != "Sales pipeline" {
		t.Errorf("title = %v", payload["title"])
	}

	// The stem is the table's name verbatim, case included: it is what a push
	// stamped at create, and folding it would write a second file beside the one
	// git tracks.
	if got := readFile(t, root, "Orders.sql"); got != "SELECT * FROM raw" {
		t.Errorf("Orders.sql = %q", got)
	}
	if got := readFile(t, root, "Revenue.sql"); got != "SELECT * FROM {{ ref('table-orders') }}" {
		t.Errorf("Revenue.sql = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "raw-orders.sql")); err == nil {
		t.Error("an integration table must not become a file")
	}
	if _, err := os.Stat(filepath.Join(root, "mrr.sql")); err == nil {
		t.Error("a metric must not become a .sql file")
	}
	// The recipe is written in id form, like the .sql refs: a cloned folder
	// declares no dependencies for an alias to resolve through.
	if got := readFile(t, root, "metrics/MRR.json"); !strings.Contains(got, `"source": "table-orders"`) {
		t.Errorf("metrics/MRR.json = %q", got)
	}

	binding := pipelineBindingOf(t, root, f.Key())
	if binding.FeatureID != "collection-1" {
		t.Errorf("featureID = %q", binding.FeatureID)
	}
	if binding.Tables["Orders.sql"] != "table-orders" || binding.Tables["Revenue.sql"] != "table-revenue" {
		t.Errorf("tables = %+v", binding.Tables)
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())

	// A metric's identity never goes in the manifest's `tables` map: the SQL half
	// of the loop walks that map, so a metric in it would be pushed as SQL.
	if _, inTables := binding.Tables["metrics/MRR.json"]; inTables {
		t.Errorf("a metric must not land in the manifest's tables map: %+v", binding.Tables)
	}

	if !strings.Contains(stderr, "kinds: integration") {
		t.Errorf("the non-derived skip was not reported by kind:\n%s", stderr)
	}
	if strings.Contains(stderr, "not cloned (out of scope") {
		t.Errorf("metrics are cloned now, so nothing may say otherwise:\n%s", stderr)
	}
	if strings.Contains(stderr, "kinds: metric") {
		t.Errorf("a metric must not be reported as an unsupported kind:\n%s", stderr)
	}
}

// TestPipelineCloneCanonicalizesPositionalRefs: tables the retired AI build
// touched still hold positional refs, and without canonicalization the same SQL
// would hash two ways and read as permanently drifted.
func TestPipelineCloneCanonicalizesPositionalRefs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 1"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code:        "SELECT o.* FROM {{ ref('0') }} o JOIN {{ ref('0') }} b ON TRUE",
		InputModels: []string{"table-orders"}})

	dir := t.TempDir()
	_, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	// Asserted against the directory listing, not os.Stat: wfdir.FileSlug keeps
	// the name's case, and a wrong-case stat still succeeds on the
	// case-insensitive filesystems dev machines run, while Linux CI fails.
	entries, dirErr := os.ReadDir(root)
	if dirErr != nil {
		t.Fatalf("read clone folder: %v\nstderr:\n%s", dirErr, stderr)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Contains(names, "Revenue.sql") {
		t.Fatalf("Revenue.sql missing after a clean clone\n  folder: %v\n  requests: %v\n  stderr:\n%s",
			names, f.requestOrder(), stderr)
	}

	got := readFile(t, root, "Revenue.sql")
	if strings.Contains(got, "ref('0')") {
		t.Errorf("a positional ref survived onto disk: %q", got)
	}
	if !strings.Contains(got, "{{ ref('table-orders') }}") {
		t.Errorf("the ref was not rewritten to id form: %q", got)
	}
	// The baseline must hash the canonical form, or the next status reports
	// drift against a table nobody touched.
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
}

// TestPipelineCloneRefusesAnUnresolvablePositionalRef: an unresolvable ref
// means the stored SQL and input list disagree; written as-is, the next push
// would have the server re-resolve it against the freshly derived input list,
// silently reading a different table.
func TestPipelineCloneRefusesAnUnresolvablePositionalRef(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code: "SELECT * FROM {{ ref('3') }}", InputModels: []string{"table-orders"}})

	dir := t.TempDir()
	_, _, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out")
	if err == nil {
		t.Fatal("an unresolvable positional ref must refuse the clone")
	}
	if !strings.Contains(err.Error(), "table-revenue") || !strings.Contains(err.Error(), "ref('3')") {
		t.Errorf("the refusal names neither the table nor the ref: %v", err)
	}
	// Nothing is written: a folder missing one file would have a baseline that
	// never mentions it, so the file would never appear.
	assertNothingWritten(t, filepath.Join(dir, "out"))
}

// TestPipelineClonePrefersYourOwnDraft: cloning live over an open draft would
// look like the author's web-builder or chat edits had vanished.
func TestPipelineClonePrefersYourOwnDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT live"})
	f.AddDraft("table-orders", "table-draft-9", "SELECT draft")

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	if got := readFile(t, root, "Orders.sql"); got != "SELECT draft" {
		t.Errorf("the draft's SQL was not cloned: %q", got)
	}
	// The binding names the live table, never the draft: a draft's id dies at
	// commit while the binding lives in the customer's git.
	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["Orders.sql"] != "table-orders" {
		t.Errorf("the binding must name the live table, got %+v", binding.Tables)
	}
	// The baseline records the draft, so the next push resumes it instead of
	// checking out a second one, which the server refuses.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["Orders.sql"].DraftID != "table-draft-9" {
		t.Errorf("the baseline did not record the draft: %+v", inst.Tables["Orders.sql"])
	}
}

// TestPipelineCloneRoundTripsFilenames: a table's name is the file stem push
// stamped at create, so clone must write back exactly that stem (orders_clean,
// not orders-clean) or one table becomes two files in the customer's git.
func TestPipelineCloneRoundTripsFilenames(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-clean", Name: "orders_clean", FeatureID: "collection-1", Code: "SELECT 1"})
	f.AddTable(&api.Table{ID: "table-region", Name: "Revenue By Region", FeatureID: "collection-1", Code: "SELECT 2"})
	f.AddTable(&api.Table{ID: "table-dupe-a", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 3"})
	f.AddTable(&api.Table{ID: "table-dupe-b", Name: "orders", FeatureID: "collection-1", Code: "SELECT 4"})

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	if got := readFile(t, root, "orders_clean.sql"); got != "SELECT 1" {
		t.Errorf("orders_clean.sql = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "orders-clean.sql")); err == nil {
		t.Error("the underscore was folded into a dash — the filename no longer round-trips")
	}
	if got := readFile(t, root, "Revenue-By-Region.sql"); got != "SELECT 2" {
		t.Errorf("Revenue-By-Region.sql = %q", got)
	}

	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["orders_clean.sql"] != "table-clean" {
		t.Errorf("the binding does not name the underscored file: %+v", binding.Tables)
	}
	// Names differing only by case are one file on macOS and Windows, so the
	// second is suffixed rather than dropped; CheckLocalPaths would otherwise
	// refuse the whole clone.
	if binding.Tables["Orders.sql"] == "" || binding.Tables["orders-2.sql"] == "" {
		t.Errorf("the name collision lost a file: %+v", binding.Tables)
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
}

func TestPipelineCloneRefusesAFeatureWithNoDerivedTables(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Raw data", "private")
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})

	dir := t.TempDir()
	_, _, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out")
	if err == nil {
		t.Fatal("a feature with no derived tables must refuse")
	}
	if !strings.Contains(err.Error(), "no derived tables") {
		t.Errorf("refusal = %v", err)
	}
}

// --- status -----------------------------------------------------------------

// seedBoundFolder lays out a two-table folder whose baseline matches both the
// disk and the server.
func seedBoundFolder(t *testing.T, f *fakePipelineInstance) string {
	t.Helper()
	seedFeature(f, "private")
	files := map[string]string{
		"orders.sql":  "SELECT * FROM raw",
		"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		// Not syncable: a pipeline folder is an ordinary repo directory.
		"README.md": "# how this works\n",
	}
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables:    map[string]string{"orders.sql": "table-orders", "revenue.sql": "table-revenue"},
	})
	writePipelineFolder(t, root, manifest, files)
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  files["orders.sql"],
			"revenue.sql": files["revenue.sql"],
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders"},
			"revenue.sql": {TableID: "table-revenue"},
		})
	return root
}

// TestPipelineStatusReadsHealthFromOneListing: health comes from one list call
// for any number of tables; only the code, which the list omits, costs a request
// per table. A regression is invisible in the output, so the count is asserted.
func TestPipelineStatusReadsHealthFromOneListing(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if f.listCalls != 1 {
		t.Errorf("status made %d list calls, want exactly 1", f.listCalls)
	}
	for _, id := range []string{"table-orders", "table-revenue"} {
		if n := countRequest(f, "GET /api/v2/feature/model/"+id+"/draft"); n != 1 {
			t.Errorf("%s: %d draft reads, want 1", id, n)
		}
		if n := countRequest(f, "GET /api/v2/feature/model/"+id); n != 1 {
			t.Errorf("%s: %d row reads, want 1", id, n)
		}
	}

	payload := decodeJSON(t, out)
	remote := payload["remote"].(map[string]any)
	if remote["checked"] != true {
		t.Fatalf("remote was not checked: %+v", remote)
	}
	tables := remote["tables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("want 2 tables, got %d", len(tables))
	}
	for _, entry := range tables {
		row := entry.(map[string]any)
		if row["drift"] != driftNone {
			t.Errorf("%v: drift = %v, want none", row["path"], row["drift"])
		}
	}
	local := payload["local"].(map[string]any)
	if len(local["added"].([]any)) != 0 || len(local["modified"].([]any)) != 0 {
		t.Errorf("README.md leaked into the local diff: %+v", local)
	}
}

// TestPipelineStatusReportsRemoteDriftAndOpenDrafts: drift is remote-vs-baseline
// (never remote-vs-local), measured against the row a push would write to: the
// draft when there is one.
func TestPipelineStatusReportsRemoteDriftAndOpenDrafts(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-7", "SELECT something else")
	f.syncOutcome["table-draft-7"] = "build_failed"
	f.tables["table-draft-7"].Status = api.TableStatusBuildFailed
	f.buildErr["table-draft-7"] = &api.TableBuildError{Message: "boom", Type: "impossible"}

	// Drift exits non-zero after the payload is emitted: a CI gate branches on
	// the exit code, a caller reads the report.
	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("drift must exit non-zero")
	}
	if !strings.Contains(err.Error(), "orders.sql") {
		t.Errorf("the failure must name the drifted file: %v", err)
	}
	payload := decodeJSON(t, out)
	tables := payload["remote"].(map[string]any)["tables"].([]any)

	var orders map[string]any
	for _, entry := range tables {
		row := entry.(map[string]any)
		if row["path"] == "orders.sql" {
			orders = row
		}
	}
	if orders == nil {
		t.Fatalf("orders.sql missing from the report: %+v", tables)
	}
	if orders["drift"] != driftChanged {
		t.Errorf("drift = %v, want changed", orders["drift"])
	}
	if orders["comparedAgainst"] != "table-draft-7" {
		t.Errorf("drift must be measured against the draft a push would write to, got %v",
			orders["comparedAgainst"])
	}
	draft := orders["draft"].(map[string]any)
	// The draft is read by its own id because GET :id/draft carries no
	// buildVerdict.
	if draft["verdict"] != api.BuildVerdictFailed {
		t.Errorf("draft verdict = %v, want %s", draft["verdict"], api.BuildVerdictFailed)
	}
}

// TestPipelineStatusReportsABrokenBinding: a bound id the feature no longer
// holds costs no further requests and exits non-zero (its drift could not be
// judged), while every other binding is still reported.
func TestPipelineStatusReportsABrokenBinding(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	delete(f.tables, "table-revenue")

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("a broken binding must exit non-zero")
	}
	payload := decodeJSON(t, out)
	tables := payload["remote"].(map[string]any)["tables"].([]any)
	found := false
	for _, entry := range tables {
		row := entry.(map[string]any)
		if row["path"] != "revenue.sql" {
			continue
		}
		found = true
		problem, _ := row["problem"].(string)
		if !strings.Contains(problem, "binding broken") {
			t.Errorf("problem = %q", problem)
		}
	}
	if !found {
		t.Fatalf("revenue.sql missing from the report: %+v", tables)
	}
	if n := countRequest(f, "GET /api/v2/feature/model/table-revenue"); n != 0 {
		t.Errorf("a broken binding cost %d row reads, want 0", n)
	}
}

func TestPipelineStatusListsUnboundFilesAsWillCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := wfdir.WriteFile(root, "margins.sql", "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	payload := decodeJSON(t, out)
	willCreate := payload["willCreate"].([]any)
	if len(willCreate) != 1 || willCreate[0] != "margins.sql" {
		t.Errorf("willCreate = %+v", willCreate)
	}
}

// TestPipelineStatusOnlyReportsSkippedFilesItMightHaveSynced: Skipped lists only
// files someone might have expected to sync; reporting .git/ and dot-files on
// every command would train the reader past a skipped `.sql` file.
func TestPipelineStatusOnlyReportsSkippedFilesItMightHaveSynced(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .sql the walk cannot read as source: somebody expected it to sync.
	if err := os.Symlink(filepath.Join(root, "orders.sql"), filepath.Join(root, "linked.sql")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	skipped, _ := decodeJSON(t, out)["skipped"].([]any)
	paths := map[string]bool{}
	for _, entry := range skipped {
		paths[entry.(map[string]any)["path"].(string)] = true
	}
	if !paths["linked.sql"] {
		t.Errorf("a skipped .sql file must still be reported: %+v", skipped)
	}
	for _, quiet := range []string{".git/", ".env"} {
		if paths[quiet] {
			t.Errorf("%s was reported as skipped; a pipeline folder is an ordinary repo directory: %+v", quiet, skipped)
		}
	}
}

// TestPipelineStatusWorksSignedOut: signed out, the local half is answered in
// full and the exit is non-zero, because "could not look" must never read as
// "nothing moved".
func TestPipelineStatusWorksSignedOut(t *testing.T) {
	f := newFakePipelineInstance(t)
	root := seedBoundFolder(t, f)
	signOutFrom(t, f.URL())

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("an unchecked remote must exit non-zero")
	}
	payload := decodeJSON(t, out)
	remote := payload["remote"].(map[string]any)
	if remote["checked"] == true {
		t.Error("remote must not report itself as checked when signed out")
	}
	reason, _ := remote["notCheckedReason"].(string)
	if !strings.Contains(reason, "not signed in") {
		t.Errorf("notCheckedReason = %q", reason)
	}
	if payload["local"] == nil {
		t.Error("the local diff was not reported")
	}
	if f.listCalls != 0 {
		t.Errorf("a signed-out status made %d list calls", f.listCalls)
	}
}

// TestPipelinePushNamesOtherOrganizationsOnlyWhenUnbound: the "entries name
// another organization" wording is only for a folder with no entry here; a
// folder bound here but missing featureID must be told to add that field.
func TestPipelinePushNamesOtherOrganizationsOnlyWhenUnbound(t *testing.T) {
	seed := func(t *testing.T, f *fakePipelineInstance, bindHere bool) string {
		manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
		if bindHere {
			manifest.SetBinding(f.Key(), wfdir.Binding{Tables: map[string]string{"orders.sql": "table-orders"}})
		}
		manifest.SetBinding(wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
			wfdir.Binding{FeatureID: "collection-other"})
		return writePipelineFolder(t, t.TempDir(), manifest, map[string]string{"new.sql": "SELECT 1"})
	}

	t.Run("unbound", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		_, _, err := runPipelineCLI(t, seed(t, f, false), "pipeline", "push", "--json")
		if err == nil {
			t.Fatal("push accepted a folder with no feature anywhere")
		}
		if !strings.Contains(err.Error(), "ten-other") {
			t.Errorf("error = %v, want the organization the entries DO name", err)
		}
	})
	t.Run("bound but missing the field", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		_, _, err := runPipelineCLI(t, seed(t, f, true), "pipeline", "push", "--json")
		if err == nil {
			t.Fatal("push accepted a binding with no featureID")
		}
		if strings.Contains(err.Error(), "ten-other") {
			t.Errorf("error = %v — a bound folder was told its entries name another organization", err)
		}
		if !strings.Contains(err.Error(), "featureID") {
			t.Errorf("error = %v, want the field it is missing named", err)
		}
	})
}

// TestPipelineStatusDegradesWhenTheOrganizationLookupFails: a failed
// organization lookup (rotated token, 5xx) must still print the local diff and
// a reason, and exit non-zero, rather than abort.
func TestPipelineStatusDegradesWhenTheOrganizationLookupFails(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.failMe = 401

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("an unchecked remote must exit non-zero")
	}
	payload := decodeJSON(t, out)
	if payload["local"] == nil {
		t.Error("the local diff was not reported")
	}
	remote := payload["remote"].(map[string]any)
	if remote["checked"] == true {
		t.Error("remote reported itself as checked without an organization")
	}
	reason, _ := remote["notCheckedReason"].(string)
	if !strings.Contains(reason, "which organization") {
		t.Errorf("notCheckedReason = %q, want the failed lookup named", reason)
	}
	if f.listCalls != 0 {
		t.Errorf("read %d listings under a credential whose organization is unknown", f.listCalls)
	}
}

// TestPipelineStatusExitsZeroOnAFreshClone: .ronja/ is never committed, so a
// fresh git checkout has no baseline; exiting non-zero would mean it could not
// pass a gate until it had pushed.
func TestPipelineStatusExitsZeroOnAFreshClone(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
		t.Fatalf("remove baseline: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("a fresh clone has nothing to compare against, which is not drift: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	if remote["checked"] != true {
		t.Fatalf("remote = %+v", remote)
	}
	// Reported as not comparable, not as agreement.
	for _, table := range remote["tables"].([]any) {
		if drift := table.(map[string]any)["drift"]; drift != driftNoBaseline {
			t.Errorf("drift = %v, want %s", drift, driftNoBaseline)
		}
	}
}

func TestPipelineStatusExitsZeroOnAFreshInit(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	writePipelineFolder(t, root, manifest, map[string]string{"orders.sql": "SELECT 1"})

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("a folder with nothing pushed yet has nothing to check: %v", err)
	}
	payload := decodeJSON(t, out)
	if create := payload["willCreate"].([]any); len(create) != 1 {
		t.Errorf("willCreate = %+v", create)
	}
	reason, _ := payload["remote"].(map[string]any)["notCheckedReason"].(string)
	if !strings.Contains(reason, "no tables exist") {
		t.Errorf("notCheckedReason = %q", reason)
	}
}

// TestPipelineStatusExitsNonZeroOnAnUnreadableRow: a row that could not be read
// is not a row with nothing to compare against; reporting it clean would let a
// gate go green over unsynced work.
func TestPipelineStatusExitsNonZeroOnAnUnreadableRow(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.failGet["table-orders"] = 500

	_, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("a bound row that could not be read must exit non-zero")
	}
	if !strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("verdict = %v", err)
	}
}

// --- discard ----------------------------------------------------------------

func TestPipelineDiscardDropsStagedDrafts(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-3", "SELECT staged")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders", DraftID: "table-draft-3"},
			"revenue.sql": {TableID: "table-revenue"},
		})

	out, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	payload := decodeJSON(t, out)
	files := payload["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("want one discarded file, got %+v", files)
	}
	entry := files[0].(map[string]any)
	if entry["outcome"] != outcomeDiscarded || entry["draftID"] != "table-draft-3" {
		t.Errorf("entry = %+v", entry)
	}
	if len(f.discarded) != 1 || f.discarded[0] != "table-draft-3" {
		t.Errorf("discarded = %+v", f.discarded)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw" {
		t.Errorf("the live table was changed: %q", f.CodeOf("table-orders"))
	}
	if readFile(t, root, "orders.sql") != "SELECT * FROM raw" {
		t.Error("discard must not touch local files")
	}
	// The content baseline stays: it still describes what the server held when
	// the draft was forked.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 == "" {
		t.Error("discard dropped the content baseline")
	}
}

// TestPipelineDiscardTreatsAnUnreachableTableAsNoDraft: GET :id/draft answers
// 404 when the table itself is out of reach (trashed, no access); that is the
// no-draft outcome, not a refusal that wedges the folder.
func TestPipelineDiscardTreatsAnUnreachableTableAsNoDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-3", "SELECT staged")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders", DraftID: "table-draft-3"},
			"revenue.sql": {TableID: "table-revenue"},
		})
	f.failDraftGet["table-orders"] = http.StatusNotFound

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("an unreachable table must not refuse the discard: %v\n%s", err, stderr)
	}
	files := decodeJSON(t, out)["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["outcome"] != outcomeNoDraft {
		t.Fatalf("files = %+v", files)
	}
	if len(f.discarded) != 0 {
		t.Errorf("nothing was reachable to discard, but %v was", f.discarded)
	}
	if !strings.Contains(stderr, "no longer reachable") {
		t.Errorf("the note naming why is missing: %q", stderr)
	}
	if inst := pipelineStateOf(t, root, f.Key()); inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived: %+v", inst.Tables["orders.sql"])
	}
}

func TestPipelineDiscardReportsNothingStaged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	out, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["nothing"] != true {
		t.Errorf("payload = %+v", payload)
	}
	if len(f.discarded) != 0 {
		t.Errorf("nothing was staged, but %v was discarded", f.discarded)
	}
}

// TestPipelineDiscardNamesADocsSidecarForWhatItIs: the sidecar guard must see
// the folder's sidecars, or a real one is refused as "is not a docs file in this
// folder".
func TestPipelineDiscardNamesADocsSidecarForWhatItIs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)

	_, _, err := runPipelineCLI(t, root, "pipeline", "discard", wfdir.TableDocsPath("raw"), "--yes", "--json")
	if err == nil {
		t.Fatal("naming a docs sidecar must be refused — there is no draft in that path to discard")
	}
	if !strings.Contains(err.Error(), "no draft to discard") {
		t.Errorf("a sidecar that IS in this folder was refused for the wrong reason: %v", err)
	}

	_, _, err = runPipelineCLI(t, root, "pipeline", "discard", wfdir.TableDocsPath("nope"), "--yes", "--json")
	if err == nil {
		t.Fatal("a docs path that is not in the folder must be refused by name")
	}
	if !strings.Contains(err.Error(), "is not a docs file in this folder") {
		t.Errorf("verdict = %v", err)
	}
}

// TestPipelineDiscardRePointsTheBaselineWhenTheDraftIsAlreadyGone: a draft
// discarded from the web UI must leave the baseline a local discard would; a
// baseline still hashing the vanished draft makes the next push "Up to date"
// with nothing on the server.
func TestPipelineDiscardRePointsTheBaselineWhenTheDraftIsAlreadyGone(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// The draft's bytes are on disk and in the baseline; the server has no draft.
	editFile(t, root, "orders.sql", "SELECT staged FROM raw")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT staged FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {
			TableID:    "table-orders",
			DraftID:    "table-draft-3",
			LiveSHA256: wfdir.HashString("SELECT * FROM raw"),
		}})

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard: %v\n%s", err, stderr)
	}
	entry := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if entry["outcome"] != outcomeNoDraft {
		t.Fatalf("entry = %+v", entry)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT * FROM raw") {
		t.Fatalf("the content baseline still describes the vanished draft: %+v", inst.Files["orders.sql"])
	}
	out, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push after discard: %v\n%s", err, stderr)
	}
	if decodeJSON(t, out)["upToDate"] == true {
		t.Fatalf("push reported up to date with no draft on the server:\n%s", out)
	}
}

// TestPipelineDiscardKeepsTheBookkeepingOfWhatItAlreadyDropped: the baseline is
// written per dropped draft, so a later table's failure cannot leave the already
// deleted drafts with stale pointers and content baselines.
func TestPipelineDiscardKeepsTheBookkeepingOfWhatItAlreadyDropped(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-3", "SELECT staged")
	f.AddDraft("table-revenue", "table-draft-4", "SELECT staged downstream")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders", DraftID: "table-draft-3"},
			"revenue.sql": {TableID: "table-revenue", DraftID: "table-draft-4"},
		})
	// orders.sql is discarded first (paths are sorted); revenue.sql then fails.
	f.failDraftGet["table-revenue"] = 500

	out, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err == nil {
		t.Fatal("a discard that could not drop every draft must exit non-zero")
	}
	payload := decodeJSON(t, out)
	files := payload["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("the partial result must report every file it attempted: %+v", files)
	}
	if files[0].(map[string]any)["outcome"] != outcomeDiscarded ||
		files[1].(map[string]any)["outcome"] != pushOutcomeRefused {
		t.Fatalf("files = %+v", files)
	}
	if len(f.discarded) != 1 || f.discarded[0] != "table-draft-3" {
		t.Fatalf("discarded = %+v", f.discarded)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the baseline still points at a draft that was deleted: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT * FROM raw") {
		t.Errorf("the content baseline of the dropped draft was not re-pointed: %+v", inst.Files["orders.sql"])
	}
	if inst.Tables["revenue.sql"].DraftID != "table-draft-4" {
		t.Errorf("revenue.sql lost a draft that is still on the server: %+v", inst.Tables["revenue.sql"])
	}
}

// TestPipelineDiscardFailsWhenTheBaselineCannotBeWritten: a failed baseline
// write leaves the folder describing drafts that are gone, so it exits non-zero.
func TestPipelineDiscardFailsWhenTheBaselineCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test depends on")
	}
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-3", "SELECT staged")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders", DraftID: "table-draft-3"}})

	stateDir := filepath.Dir(wfdir.StatePath(root))
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", stateDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	out, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err == nil {
		t.Fatal("a discard whose baseline could not be written must exit non-zero")
	}
	if payload := decodeJSON(t, out); payload["error"] == nil || payload["error"] == "" {
		t.Errorf("the partial result must name the failure: %+v", payload)
	}
}

// TestPipelineDiscardRequiresYesUnderJSON: a program on the other end of --json
// handed a [y/N] prompt deadlocks.
func TestPipelineDiscardRequiresYesUnderJSON(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-orders", "table-draft-3", "SELECT staged")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders", DraftID: "table-draft-3"}})

	_, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--json")
	if err == nil {
		t.Fatal("--json without --yes must refuse rather than prompt")
	}
	if len(f.discarded) != 0 {
		t.Errorf("a refused discard still deleted %v", f.discarded)
	}
}

// --- the kind refusal matrix ------------------------------------------------

// TestPipelineCommandsRefuseTheOtherFolderKinds: folder kinds are
// indistinguishable by shape, so LoadManifest's kind gate alone stops `wf push`
// in a pipeline folder making a workflow out of SQL; this pins that the commands
// route through it.
func TestPipelineCommandsRefuseTheOtherFolderKinds(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)

	pipelineRoot := t.TempDir()
	writePipelineFolder(t, pipelineRoot, &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "P"},
		map[string]string{"orders.sql": "SELECT 1"})

	workflowRoot := t.TempDir()
	if err := wfdir.SaveManifest(workflowRoot, &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "W", Entrypoint: "main.py"}); err != nil {
		t.Fatal(err)
	}
	appRoot := t.TempDir()
	if err := wfdir.SaveManifest(appRoot, &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "A", Entrypoint: "App.tsx"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		root    string
		args    []string
		wantCmd string
	}{
		{"wf status in a pipeline folder", pipelineRoot, []string{"wf", "status"}, "ronja pipeline"},
		{"app status in a pipeline folder", pipelineRoot, []string{"app", "status"}, "ronja pipeline"},
		{"pipeline status in a workflow folder", workflowRoot, []string{"pipeline", "status"}, "ronja wf"},
		{"pipeline status in a data-app folder", appRoot, []string{"pipeline", "status"}, "ronja app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runPipelineCLI(t, tc.root, tc.args...)
			if err == nil {
				t.Fatal("the wrong folder kind must be refused")
			}
			// A wrong kind is almost always the right folder and the wrong verb.
			if !strings.Contains(err.Error(), tc.wantCmd) {
				t.Errorf("refusal should point at %q, got: %v", tc.wantCmd, err)
			}
		})
	}
}

// TestPipelineStatusReportsBothDriftLegs: push refuses when either the live
// table or your own draft has moved and points at status, so status must show
// both legs.
func TestPipelineStatusReportsBothDriftLegs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// The draft holds exactly what this folder last wrote into it.
	f.AddDraft("table-orders", "table-draft-5", "SELECT * FROM raw")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {
			TableID:     "table-orders",
			DraftID:     "table-draft-5",
			DraftSHA256: wfdir.HashString("SELECT * FROM raw"),
			LiveSHA256:  wfdir.HashString("SELECT * FROM raw"),
		}})
	// A colleague's commit moves the live table underneath it.
	f.tables["table-orders"].Code = "SELECT theirs FROM raw"

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("drift on the live table must exit non-zero")
	}
	var orders map[string]any
	for _, entry := range decodeJSON(t, out)["remote"].(map[string]any)["tables"].([]any) {
		if row := entry.(map[string]any); row["path"] == "orders.sql" {
			orders = row
		}
	}
	if orders == nil {
		t.Fatal("orders.sql missing from the report")
	}
	if orders["drift"] != driftNone {
		t.Errorf("draft drift = %v, want none", orders["drift"])
	}
	if orders["comparedAgainst"] != "table-draft-5" {
		t.Errorf("comparedAgainst = %v, want the draft", orders["comparedAgainst"])
	}
	if orders["liveDrift"] != driftChanged {
		t.Errorf("liveDrift = %v, want changed — the live leg is the one that moved", orders["liveDrift"])
	}
}

// TestPipelineStatusOmitsTheLiveLegWithNoDraft: with no draft, liveDrift would
// repeat drift under a second key, inviting a caller to read one fact as two.
func TestPipelineStatusOmitsTheLiveLegWithNoDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("a clean, fully checked folder must exit zero: %v", err)
	}
	for _, entry := range decodeJSON(t, out)["remote"].(map[string]any)["tables"].([]any) {
		row := entry.(map[string]any)
		if row["drift"] != driftNone {
			t.Errorf("%v: drift = %v", row["path"], row["drift"])
		}
		if _, present := row["liveDrift"]; present {
			t.Errorf("%v: liveDrift is present with no draft open: %+v", row["path"], row)
		}
	}
}
