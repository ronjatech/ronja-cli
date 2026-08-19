package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja pipeline` init / clone / status / discard.
//
// The push and publish halves have files of their own; what is here is the
// folder's birth and its read-only surfaces, which is where the invariants that
// everything else depends on are established: canonical SQL on disk, a baseline
// that describes the rows it hashed, and a bindings map nothing else can invent.

// --- capture ----------------------------------------------------------------

// runPipelineCLI is runCLI with stderr captured as well.
//
// It exists because this loop says a great deal on stderr that is not
// decoration — the skipped-kind report, the drift refusals, the staleness
// warnings, the "no confidence report" degradation. Those are the messages a
// person acts on, and a test that could not read them would be asserting only
// that the command did not crash.
func runPipelineCLI(t *testing.T, workdir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, workdir, args...)
	})
	return stdout, stderr, err
}

// countRequest counts EXACT "METHOD /path" matches, which is what the two-tier
// assertions need: "GET /api/v2/feature/model/table-x" and
// "GET /api/v2/feature/model/table-x/draft" are different requests, and a
// substring count would fold them together.
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
	// Nothing server-side: init reads /me for the organization and stops there.
	if n := countRequest(f, "GET /api/v2/feature/model"); n != 0 {
		t.Errorf("init made %d table requests", n)
	}
}

// TestPipelineInitRefusesASecondTime: running init inside a folder that is
// already synced would overwrite a manifest holding real bindings.
func TestPipelineInitRefusesASecondTime(t *testing.T) {
	f := newFakePipelineInstance(t)
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

// TestPipelineInitChecksTheFeatureIDShape: --feature is written into a
// COMMITTED manifest and nothing reads it until the first push, so a typo is
// otherwise invisible for several files and possibly several days — and what it
// reports there is a create that failed, not a manifest that was wrong from the
// moment it was written.
//
// By shape only. Whether the feature exists is a question for the server, and
// asking it here would put a round trip in the one command that makes none.
func TestPipelineInitChecksTheFeatureIDShape(t *testing.T) {
	f := newFakePipelineInstance(t)
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
		// A refused init leaves the directory exactly as it found it.
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
			t.Errorf("a refused init wrote a manifest for %q", bad)
		}
	}
	// The legacy prefix is still minted nowhere and still held by real rows.
	dir := t.TempDir()
	if _, _, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "col-legacy"); err != nil {
		t.Errorf("the legacy feature prefix must be accepted: %v", err)
	}
	// And no --feature at all is a folder that refuses at its first push, which
	// is a state init is allowed to create.
	empty := t.TempDir()
	if _, _, err := runPipelineCLI(t, empty, "pipeline", "init"); err != nil {
		t.Errorf("init without --feature: %v", err)
	}
}

// TestPipelineInitCreatesNoDirectoryWhenItRefuses: the manifest check comes
// BEFORE the directory is created, so a mistyped path (or a second init) leaves
// no empty folder behind.
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

// seedFeature registers a feature and its tables for the clone tests.
func seedFeature(f *fakePipelineInstance, scope string) {
	f.AddFeature("collection-1", "Sales pipeline", scope)
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code: "SELECT * FROM {{ ref('table-orders') }}", InputModels: []string{"table-orders"}})
}

// TestPipelineCloneWritesOneFilePerDerivedTable: the shape of a cloned folder —
// a .sql per derived table, a bindings map, and a baseline that hashes exactly
// what was written.
func TestPipelineCloneWritesOneFilePerDerivedTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// Two rows that must NOT become files, for two different reasons.
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw orders", FeatureID: "collection-1", Kind: "integration"})
	f.AddTable(&api.Table{ID: "table-mrr", Name: "MRR", FeatureID: "collection-1", Kind: api.TableKindMetric})

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

	// The stem is the table's name VERBATIM, case included: it is what a push
	// stamped at create, and folding it here would write a second file beside
	// the one git already tracks.
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
		t.Error("a metric must not become a file")
	}

	binding := pipelineBindingOf(t, root, f.Key())
	if binding.FeatureID != "collection-1" {
		t.Errorf("featureID = %q", binding.FeatureID)
	}
	if binding.Tables["Orders.sql"] != "table-orders" || binding.Tables["Revenue.sql"] != "table-revenue" {
		t.Errorf("tables = %+v", binding.Tables)
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())

	// The two skip reasons are reported SEPARATELY and in different words: a
	// non-derived kind has no SQL a file could hold, while a metric runs the
	// identical draft flow and is out of scope for this loop. Claiming otherwise
	// would send somebody looking for a capability the product already has.
	if !strings.Contains(stderr, "kinds: integration") {
		t.Errorf("the non-derived skip was not reported by kind:\n%s", stderr)
	}
	if !strings.Contains(stderr, "1 metric(s) not cloned (out of scope for this loop)") {
		t.Errorf("the metric skip was not reported as out-of-scope:\n%s", stderr)
	}
	if strings.Contains(stderr, "kinds: metric") {
		t.Errorf("a metric must not be reported as an unsupported kind:\n%s", stderr)
	}
}

// TestPipelineCloneCanonicalizesPositionalRefs: the AI build path persists
// POSITIONAL refs, so any table a colleague has touched through chat holds them.
// A folder-sync loop hashes remote code, so without canonicalization the same
// SQL would hash two ways and every such table would read as permanently
// drifted.
func TestPipelineCloneCanonicalizesPositionalRefs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 1"})
	// Exactly what POST /:id/build leaves behind.
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code:        "SELECT o.* FROM {{ ref('0') }} o JOIN {{ ref('0') }} b ON TRUE",
		InputModels: []string{"table-orders"}})

	dir := t.TempDir()
	_, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	// Asserted against the DIRECTORY LISTING, not os.Stat: FileSlug preserves
	// the table name's case (Revenue -> Revenue.sql, see wfdir.FileSlug), and a
	// stat by the wrong case still succeeds on the case-insensitive filesystems
	// dev machines run — which is exactly how this test's stale lowercase
	// expectation passed 78 local runs while failing every Linux CI run.
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
	// The baseline has to hash the CANONICAL form, or the next status reports
	// drift against a table nobody touched.
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
}

// TestPipelineCloneRefusesAnUnresolvablePositionalRef: the one silently
// destructive path in the design.
//
// A ref this client cannot resolve means the stored SQL and the stored input
// list disagree. Written to disk as-is, the next push would send it back, the
// server would re-resolve it against the FRESHLY DERIVED (and sorted) input
// list, and the SQL would start reading a different table with the build
// succeeding and nothing anywhere saying so.
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
	// Naming both halves is the point: which table, and which marker.
	if !strings.Contains(err.Error(), "table-revenue") || !strings.Contains(err.Error(), "ref('3')") {
		t.Errorf("the refusal names neither the table nor the ref: %v", err)
	}
	// And nothing is written: a folder missing one file would have a baseline
	// that never mentions it, so the file would simply never appear.
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
	// The BINDING names the stable identity, never the draft: a draft's id dies
	// at commit while the binding lives in the customer's git.
	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["Orders.sql"] != "table-orders" {
		t.Errorf("the binding must name the live table, got %+v", binding.Tables)
	}
	// The BASELINE records the draft, so the next push resumes it instead of
	// trying to check out a second one (which the server refuses).
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["Orders.sql"].DraftID != "table-draft-9" {
		t.Errorf("the baseline did not record the draft: %+v", inst.Tables["Orders.sql"])
	}
}

// TestPipelineCloneRoundTripsFilenames: a clone has to write back the filenames
// the folder pushed, because a pipeline folder lives in the customer's git.
//
// A table's display name is the file stem push stamped at create, so the name
// makes a round trip through the server. wfdir.Slug folds `_` into `-`, which
// broke it: an author committing orders_clean.sql got orders-clean.sql back —
// two files for one table, and a phantom deletion the moment either is pushed.
// Collision suffixing is asserted in the same test because it runs on the same
// slug and is the thing a change here is most likely to break.
func TestPipelineCloneRoundTripsFilenames(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-clean", Name: "orders_clean", FeatureID: "collection-1", Code: "SELECT 1"})
	f.AddTable(&api.Table{ID: "table-region", Name: "Revenue By Region", FeatureID: "collection-1", Code: "SELECT 2"})
	// Two names that slugify to the same thing, which is an ordinary state for a
	// feature and must not lose a file.
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
	// Everything that is NOT an underscore still slugifies as before: a space is
	// not a character a filename should carry.
	if got := readFile(t, root, "Revenue-By-Region.sql"); got != "SELECT 2" {
		t.Errorf("Revenue-By-Region.sql = %q", got)
	}

	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["orders_clean.sql"] != "table-clean" {
		t.Errorf("the binding does not name the underscored file: %+v", binding.Tables)
	}
	// The collision is suffixed rather than dropped: a skipped file is a table
	// the folder silently does not manage.
	// Two names that differ only by case are ONE file on macOS and Windows, so
	// the second is suffixed even though the stems are now distinct strings —
	// CheckLocalPaths would otherwise refuse the whole clone.
	if binding.Tables["Orders.sql"] == "" || binding.Tables["orders-2.sql"] == "" {
		t.Errorf("the name collision lost a file: %+v", binding.Tables)
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
}

// TestPipelineCloneRefusesAFeatureWithNoDerivedTables: a folder with no files is
// not a useful thing to have created, and the reason is worth saying.
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
// disk and the server — the clean starting point most status/push tests want.
func seedBoundFolder(t *testing.T, f *fakePipelineInstance) string {
	t.Helper()
	seedFeature(f, "private")
	files := map[string]string{
		"orders.sql":  "SELECT * FROM raw",
		"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		// Not syncable, and deliberately present: a pipeline folder is an
		// ordinary repo directory.
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

// TestPipelineStatusReadsHealthFromOneListing: the two-tier rule.
//
// The list carries every bound row's kind, status and hasError, so the health
// leg costs ONE request for any number of tables. Only the code — which the list
// deliberately omits — is paid for per table. A regression here is invisible in
// the output and shows up as a folder of thirty tables taking a minute to
// report, so the request count is the assertion.
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
	// The non-syncable file is neither a local change nor a skip report: a
	// pipeline folder ignores it silently, which is the whole point of the rule.
	local := payload["local"].(map[string]any)
	if len(local["added"].([]any)) != 0 || len(local["modified"].([]any)) != 0 {
		t.Errorf("README.md leaked into the local diff: %+v", local)
	}
}

// TestPipelineStatusReportsRemoteDriftAndOpenDrafts: drift is remote-vs-BASELINE
// (never remote-vs-local), and it is measured against the row a push would write
// to — which is the draft when there is one.
func TestPipelineStatusReportsRemoteDriftAndOpenDrafts(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A colleague's chat edit, in the caller's own draft.
	f.AddDraft("table-orders", "table-draft-7", "SELECT something else")
	f.syncOutcome["table-draft-7"] = "build_failed"
	f.tables["table-draft-7"].Status = api.TableStatusBuildFailed
	f.buildErr["table-draft-7"] = &api.TableBuildError{Message: "boom", Type: "impossible"}

	// Drift exits NON-ZERO, and the payload is emitted first: the report is what
	// a caller reads, the exit code is what a CI gate branches on.
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
	// The verdict is the single most useful thing here, and it is the reason the
	// draft is read by its OWN id: GET :id/draft carries no buildVerdict.
	if draft["verdict"] != api.BuildVerdictFailed {
		t.Errorf("draft verdict = %v, want %s", draft["verdict"], api.BuildVerdictFailed)
	}
}

// TestPipelineStatusReportsABrokenBinding: a bound id the feature no longer
// holds is answered per table, and costs no further requests — the other
// bindings are still worth reporting.
func TestPipelineStatusReportsABrokenBinding(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	delete(f.tables, "table-revenue")

	// A binding that no longer resolves is a table whose drift COULD NOT be
	// judged, which is not the same answer as no drift — so it exits non-zero
	// while still reporting every other binding.
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

// TestPipelineStatusListsUnboundFilesAsWillCreate: a new .sql file is not drift
// and not an error — it is a table a push would create, and saying so is the
// difference between "keep editing" and "nothing is on the server yet".
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

// TestPipelineStatusOnlyReportsSkippedFilesItMightHaveSynced: Skipped means "a
// file you might have expected to be synced, and why it was not". In a folder
// whose rule is "`.sql` and nothing else, silently", a .git/ directory and a
// dot-file are not that — reporting them on every command is the noise the
// silent rule exists to avoid, and it trains the reader past the line that does
// matter: a `.sql` file that was skipped.
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
	// A .sql the walk cannot read as source: this one IS worth a line, because
	// somebody put it there expecting it to sync.
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

// TestPipelineStatusWorksSignedOut: the local half of a status is worth having
// on a plane, and "not checked" must be distinguishable from "fine".
func TestPipelineStatusWorksSignedOut(t *testing.T) {
	f := newFakePipelineInstance(t)
	root := seedBoundFolder(t, f)
	signOutFrom(t, f.URL())

	// Signed out, the local half is still answered in full — and the exit is
	// non-zero, because "I could not look" must never read as "nothing moved".
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
	// And the local half is genuinely there.
	if payload["local"] == nil {
		t.Error("the local diff was not reported")
	}
	if f.listCalls != 0 {
		t.Errorf("a signed-out status made %d list calls", f.listCalls)
	}
}

// TestPipelineStatusExitsZeroOnAFreshClone: .ronja/ is correctly never
// committed, so a colleague who has just cloned the repo has no baseline for any
// file — the normal way somebody joins a pipeline, and the state push's own
// drift guard deliberately disarms for.
//
// Exiting non-zero there would mean a fresh checkout could not pass a gate until
// it had pushed, which is the opposite of what the gate is for.
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
	// And it is reported honestly rather than as agreement: the exit code says
	// nothing has moved, the payload says nothing was comparable.
	for _, table := range remote["tables"].([]any) {
		if drift := table.(map[string]any)["drift"]; drift != driftNoBaseline {
			t.Errorf("drift = %v, want %s", drift, driftNoBaseline)
		}
	}
}

// TestPipelineStatusExitsZeroOnAFreshInit: a folder bound to a feature it has
// never pushed to. Nothing is missing and nothing failed — there is simply
// nothing over there yet.
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

// TestPipelineStatusExitsNonZeroOnAnUnreadableRow: the other half of the
// contract. A row that was READ and made no sense is not a row with nothing to
// compare against, and reporting it as clean is how a gate goes green over
// somebody's unsynced work.
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

// TestPipelineDiscardDropsStagedDrafts: discard operates on the sync loop's own
// artifact and leaves the live tables and the local files exactly as they were.
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
	// The live table is untouched, and so is the local file.
	if f.CodeOf("table-orders") != "SELECT * FROM raw" {
		t.Errorf("the live table was changed: %q", f.CodeOf("table-orders"))
	}
	if readFile(t, root, "orders.sql") != "SELECT * FROM raw" {
		t.Error("discard must not touch local files")
	}
	// The draft pointer goes; the CONTENT baseline stays, because it still
	// describes what the server held when the draft was forked.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 == "" {
		t.Error("discard dropped the content baseline")
	}
}

// TestPipelineDiscardReportsNothingStaged: "no draft at all" is a different
// answer from "the drafts were discarded", and a caller has to be able to tell.
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

// TestPipelineDiscardRePointsTheBaselineWhenTheDraftIsAlreadyGone: the
// DOCUMENTED normal case — somebody discarded the draft from the web UI — has to
// leave exactly the baseline a discard here would have.
//
// Recording only the draft POINTER left inst.Files describing the vanished
// draft's bytes, so the local file read as unchanged for ever: every signal went
// green while the SQL had never reached the server, and the next push answered
// "Up to date" with no draft anywhere.
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
	// The CONTENT baseline now describes the live table, which is the only row
	// this file is synced with.
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT * FROM raw") {
		t.Fatalf("the content baseline still describes the vanished draft: %+v", inst.Files["orders.sql"])
	}
	// Which is the whole point: the next push re-stages instead of reporting a
	// folder that is up to date with nothing on the server.
	out, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push after discard: %v\n%s", err, stderr)
	}
	if decodeJSON(t, out)["upToDate"] == true {
		t.Fatalf("push reported up to date with no draft on the server:\n%s", out)
	}
}

// TestPipelineDiscardKeepsTheBookkeepingOfWhatItAlreadyDropped: one table's
// failure is per-file data, and the baseline for the drafts that DID go is
// written as they go.
//
// A run that aborted on the first failure had already deleted drafts server-side
// and saved nothing, so those files kept both their draft pointer and their old
// content baseline — the stale-baseline state above, arrived at from the other
// direction.
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
	// The one that failed is untouched, so a retry finds its draft where it was.
	if inst.Tables["revenue.sql"].DraftID != "table-draft-4" {
		t.Errorf("revenue.sql lost a draft that is still on the server: %+v", inst.Tables["revenue.sql"])
	}
}

// TestPipelineDiscardFailsWhenTheBaselineCannotBeWritten: a baseline write that
// fails IS the stale-baseline state — the drafts are gone and the folder still
// describes them — so it exits non-zero rather than reporting a clean discard.
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

// TestPipelineDiscardRequiresYesUnderJSON: --json says there is a program on the
// other end, and a program handed a [y/N] prompt deadlocks.
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

// TestPipelineCommandsRefuseTheOtherFolderKinds: the folder types are
// indistinguishable by shape, so LoadManifest's kind gate is the only thing
// standing between `wf push` in a pipeline folder and a workflow made out of
// SQL. wfdir tests the refusal; this pins that the COMMANDS actually route
// through it, in both directions.
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
			// Naming the command that WOULD work is the whole value: a wrong kind
			// is almost always the right folder and the wrong verb.
			if !strings.Contains(err.Error(), tc.wantCmd) {
				t.Errorf("refusal should point at %q, got: %v", tc.wantCmd, err)
			}
		})
	}
}

// TestPipelineStatusReportsBothDriftLegs: push refuses when EITHER the live
// table or your own draft has moved, and then says "run status to see the
// detail" — so status has to be able to show both.
//
// It could not: it resolved the draft and reported only that row, which made the
// live leg (a colleague committing while your draft sat open) invisible on the
// one surface built to show it.
func TestPipelineStatusReportsBothDriftLegs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A draft holding exactly what this folder last wrote into it — leg (b) is
	// clean, and it is recorded as such.
	f.AddDraft("table-orders", "table-draft-5", "SELECT * FROM raw")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {
			TableID:     "table-orders",
			DraftID:     "table-draft-5",
			DraftSHA256: wfdir.HashString("SELECT * FROM raw"),
			LiveSHA256:  wfdir.HashString("SELECT * FROM raw"),
		}})
	// …and a colleague's commit underneath it — leg (a) is not.
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
	// The row a push would WRITE TO is the draft, and it is unchanged. Reporting
	// that as the whole answer is exactly the gap this fills.
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

// TestPipelineStatusOmitsTheLiveLegWithNoDraft: with no draft there is one row
// and one answer, and repeating it under a second key would invite a caller to
// treat two names for the same fact as two facts.
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
