package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The METRIC half of the pipeline loop: `metrics/<name>.json`.
//
// What is under test here is not the draft lifecycle — that is the .sql half's
// and is shared byte for byte — but the four things only a metric has: the
// carrier's own parse rules, the shared name namespace, the pass ordering that
// replaces a dependency edge, and the destructive brake on overwriting a
// definition somebody verified.

// aovRecipe is a small, complete recipe. Its keys are spelled out of
// alphabetical order deliberately, so anything in the path that re-marshalled
// rather than passing bytes through shows up as a changed digest.
const aovRecipe = `{"value":"revenue / orders","source":"orders","base_measures":[{"name":"revenue","agg":"sum","column":"amount"}]}`

// metricFileBody wraps a recipe in the file that carries it.
func metricFileBody(recipe string) string {
	return `{"recipe":` + recipe + `}`
}

// seedMetricFolder lays out a folder that BUILDS one table and DEFINES one
// metric reading it — the main case, and the one the pass ordering exists for.
func seedMetricFolder(t *testing.T, f *fakePipelineInstance) string {
	t.Helper()
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables:    map[string]string{"orders.sql": "table-orders"},
	})
	files := map[string]string{
		"orders.sql":                 "SELECT * FROM raw",
		"metrics/average_order.json": metricFileBody(aovRecipe),
	}
	writePipelineFolder(t, root, manifest, files)
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": files["orders.sql"]},
		map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders"}})
	return root
}

// metricStateOf reads one metric file's recorded state out of a LEGACY folder's
// local baseline.
func metricStateOf(t *testing.T, root string, key wfdir.InstanceKey, path string) wfdir.MetricState {
	t.Helper()
	return pipelineStateOf(t, root, key).Metrics[path]
}

// --- the carrier ------------------------------------------------------------

// TestPipelineMetricPushRefusesAnUnknownTopLevelKey: the file's own rule,
// reaching the reader as a per-file refusal rather than as a dropped key.
//
// The .sql half of the same push still runs, which is the other half of the
// contract: one unusable metric must not stop a folder deploying.
func TestPipelineMetricPushRefusesAnUnknownTopLevelKey(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		`{"recipe":`+aovRecipe+`,"verified":true}`); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("an unknown top-level key must make the push exit non-zero")
	}
	payload := decodeJSON(t, out)
	metrics := payload["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("metrics = %v", metrics)
	}
	entry := metrics[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "verified") {
		t.Errorf("the refusal must name the key: %v", entry["error"])
	}
	if len(f.metricsCreated) != 0 {
		t.Errorf("a file that does not parse must not reach a create: %+v", f.metricsCreated)
	}
}

// TestPipelineMetricPushSendsUnknownRecipeKeysThrough is the OPPOSITE rule one
// level down: the recipe grammar is the server's, so a key this build has never
// heard of has to reach it intact rather than be dropped by a client with no
// entitlement to an opinion about it.
func TestPipelineMetricPushSendsUnknownRecipeKeysThrough(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue","filters":[{"column":"status","eq":"paid"}]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.metricsCreated) != 1 {
		t.Fatalf("creates = %+v", f.metricsCreated)
	}
	sent := string(f.metricsCreated[0].Recipe)
	if !strings.Contains(sent, `"filters"`) || !strings.Contains(sent, `"paid"`) {
		t.Errorf("an unknown recipe key was dropped on the way to the server: %s", sent)
	}
}

// --- the shared namespace ---------------------------------------------------

// TestPipelineMetricPushRefusesAStemATableAlreadyClaims.
//
// The truth is the store: requireNameFreeTx has no `kind` predicate, so a metric
// and a table in one feature really do collide. The point of the LOCAL guard is
// that the refusal arrives before the first byte, naming the two FILES — rather
// than half way through a push that has already created things, naming a row.
func TestPipelineMetricPushRefusesAStemATableAlreadyClaims(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	if err := wfdir.WriteFile(root, "revenue.sql", "SELECT 1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Case-only, because that is exactly what the store folds and what a reader
	// looking at two filenames would not.
	if err := wfdir.WriteFile(root, "metrics/Revenue.json", metricFileBody(aovRecipe)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a metric and a table claiming one name must be refused")
	}
	for _, want := range []string{"metrics/Revenue.json", "revenue.sql", "Revenue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(f.created) != 0 || len(f.metricsCreated) != 0 {
		t.Error("the refusal must land before anything is created")
	}
}

// TestPipelineMetricStemCollisionRefusesTwoMetricsClaimingOneName is the OTHER
// pair in the same namespace, and the one no other check covers.
//
// `metrics/Revenue.json` and `metrics/revenue.json` are two names for one thing
// exactly as a metric and a table are — and unlike a pair of .sql files, they
// can really coexist in a committed folder: a pipeline folder's SyncExt is
// `.sql`, so a metric file is invisible to Enumerate and never reaches
// checkPushable's case-only PATH guard at all.
//
// THE FUNCTION IS CALLED DIRECTLY rather than through a folder, and that is the
// honest way to test this pair: macOS and Windows are case-insensitive, so the
// two files cannot both exist on the disk this test runs on. The folder that
// carries them is one committed from Linux and cloned here, which is precisely
// why the guard has to be about the committed SET rather than about what one
// filesystem can hold.
func TestPipelineMetricStemCollisionRefusesTwoMetricsClaimingOneName(t *testing.T) {
	err := checkMetricStemCollisions(
		map[string]string{"orders.sql": "SELECT 1"},
		[]pipelineMetricFile{
			{Path: "metrics/Revenue.json", Alias: "Revenue"},
			{Path: "metrics/revenue.json", Alias: "revenue"},
		})
	if err == nil {
		t.Fatal("two metric files claiming one name must be refused")
	}
	for _, want := range []string{"metrics/Revenue.json", "metrics/revenue.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// ONCE, not once per member: a pair reported twice reads as two problems and
	// sends the author looking for a second one.
	if n := strings.Count(err.Error(), "metrics/Revenue.json"); n != 1 {
		t.Errorf("the pair is reported %d times, not once:\n%v", n, err)
	}
	// A .sql file in the same fold joins the SAME group rather than earning a
	// second line — and the group is reported once however many files are in it.
	err = checkMetricStemCollisions(
		map[string]string{"Revenue.sql": "SELECT 1"},
		[]pipelineMetricFile{
			{Path: "metrics/Revenue.json", Alias: "Revenue"},
			{Path: "metrics/revenue.json", Alias: "revenue"},
		})
	if err == nil {
		t.Fatal("a table in the same fold must still be refused")
	}
	if n := strings.Count(err.Error(), "Revenue.sql"); n != 1 {
		t.Errorf("the .sql file is listed %d times, not once:\n%v", n, err)
	}
	for _, want := range []string{"metrics/Revenue.json", "metrics/revenue.json", "all of them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// Deterministic whichever order the files arrive in — `push` narrows the
	// metric set to the paths named on the command line, in the order they were
	// typed.
	reversed := checkMetricStemCollisions(
		map[string]string{"Revenue.sql": "SELECT 1"},
		[]pipelineMetricFile{
			{Path: "metrics/revenue.json", Alias: "revenue"},
			{Path: "metrics/Revenue.json", Alias: "Revenue"},
		})
	if reversed == nil || reversed.Error() != err.Error() {
		t.Errorf("the refusal depends on the order the files were given:\n%v\n%v", err, reversed)
	}
}

// TestPipelineMetricStemCollisionFoldsWhitespaceLikeTheServer.
//
// The server compares `lower(btrim())` and trims the name it stores, so
// `metrics/revenue .json` really does claim the name `revenue.sql`'s table
// holds. A guard that folded case alone would wave that pair through and let the
// push fail half way, server-side, naming a row rather than the two files.
func TestPipelineMetricStemCollisionFoldsWhitespaceLikeTheServer(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	if err := wfdir.WriteFile(root, "revenue.sql", "SELECT 1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// os.WriteFile rather than wfdir.WriteFile: that helper is the CLI's own
	// writer and refuses a space in a path outright. Nothing refuses one on the
	// way IN — readMetricFiles reads the directory as it finds it — so a file a
	// person named by hand reaches the guard exactly like this.
	if err := os.WriteFile(filepath.Join(root, "metrics", "revenue .json"),
		[]byte(metricFileBody(aovRecipe)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a stem that differs from a table's only by whitespace must be refused")
	}
	for _, want := range []string{"metrics/revenue .json", "revenue.sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(f.created) != 0 || len(f.metricsCreated) != 0 {
		t.Error("the refusal must land before anything is created")
	}
}

// --- ordering ---------------------------------------------------------------

// TestPipelineMetricPushRunsAfterTheSQLPass.
//
// A metric contributes no `{{ ref }}` edge, so it cannot join topoOrder — the
// ordering is bought by running the metric pass last instead. This is the case
// that makes it load-bearing rather than cosmetic: the metric reads a table this
// same push CREATES, so its source id does not exist until that create has
// returned.
func TestPipelineMetricPushRunsAfterTheSQLPass(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	// Bound to the feature and to NO tables: the .sql file is created by this
	// very push.
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	writePipelineFolder(t, root, manifest, map[string]string{
		"orders.sql":                 "SELECT * FROM raw",
		"metrics/average_order.json": metricFileBody(aovRecipe),
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.created) != 1 || len(f.metricsCreated) != 1 {
		t.Fatalf("created %d table(s) and %d metric(s)", len(f.created), len(f.metricsCreated))
	}
	// The source was resolved to the id the SQL pass had just minted, which is
	// only possible because the metric pass ran afterwards.
	binding := pipelineBindingOf(t, root, f.Key())
	wantSource := binding.Tables["orders.sql"]
	if wantSource == "" {
		t.Fatal("the .sql create recorded no binding")
	}
	if got := string(f.metricsCreated[0].Recipe); !strings.Contains(got, `"source":"`+wantSource+`"`) {
		t.Errorf("recipe = %s, want source %s", got, wantSource)
	}
	// And the request order says so directly, which is the assertion that
	// survives somebody making the resolution lazier.
	order := f.requestOrder()
	tableCreate := slices.Index(order, "POST /api/v2/feature/model")
	metricCreate := slices.Index(order, "POST /api/v2/feature/model/metric")
	if tableCreate < 0 || metricCreate < 0 || tableCreate > metricCreate {
		t.Errorf("the metric pass did not run after the .sql pass: %v", order)
	}
}

// TestPipelineMetricPushRefusesAnUnresolvableSource: a metric whose source
// resolved to "whichever table this name happens to mean" would compute a number
// from a row nobody chose.
func TestPipelineMetricPushRefusesAnUnresolvableSource(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"nowhere","value":"revenue"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("an unresolvable source must refuse the file")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "nowhere") {
		t.Errorf("the refusal must name the source: %v", entry["error"])
	}
}

// --- create, and the crash rule ---------------------------------------------

// TestPipelineMetricPushRecordsTheBindingBeforeAnythingElse.
//
// The create returns a LIVE row, so a push that died before recording it would
// leave a metric nobody can find and the next push would create a second one
// beside it — and this loop has no delete verb to undo that with. The assertion
// is that the recording is on disk even though everything AFTER the create was
// refused.
func TestPipelineMetricPushRecordsTheBindingBeforeAnythingElse(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	// The very next call after the create is the draft probe. Failing it proves
	// the recording had already been written.
	f.failDraftGet["table-metric-1"] = 500

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a failed draft probe must make the push exit non-zero")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if entry["created"] != true {
		t.Errorf("the created flag was lost: %v", entry)
	}
	recorded := metricStateOf(t, root, f.Key(), "metrics/average_order.json")
	if recorded.MetricID != "table-metric-1" {
		t.Fatalf("the binding was not recorded before the failure: %+v", recorded)
	}
	// The DECLARED fingerprint is deliberately NOT recorded: nothing built, so
	// the file is still unpushed and the next bare push must send it again.
	if recorded.DeclaredSHA256 != "" {
		t.Errorf("a create that never built recorded the file as pushed: %+v", recorded)
	}
}

// TestPipelineMetricPushAdoptsAMetricTheNameAlreadyHolds.
//
// The likely cause of a `name_taken` here is not a real collision — it is a 200
// this CLI failed to persist. The metric exists, the recording does not, and the
// next push tries to create the same name, which the server refuses with
// something the author cannot act on from the folder at all.
func TestPipelineMetricPushAdoptsAMetricTheNameAlreadyHolds(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	f.AddMetric("table-aov", "average_order", `{"source":"table-orders","value":"revenue","version":1}`)
	f.nameTakenOnMetricCreate = `a table or metric called "average_order" already exists in this feature`

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["metricID"] != "table-aov" {
		t.Errorf("metricID = %v, want the adopted row", entry["metricID"])
	}
	if !strings.Contains(stderr, "adopting it rather than refusing") {
		t.Errorf("the adoption was not said out loud:\n%s", stderr)
	}
	if got := metricStateOf(t, root, f.Key(), "metrics/average_order.json").MetricID; got != "table-aov" {
		t.Errorf("the adoption was not recorded: %q", got)
	}
}

// TestPipelineMetricPushRefusesWhenTheNameIsHeldByATable: the other half of the
// same branch, and the one that must NOT adopt. A metric file pointed at a
// derived table would push a recipe onto a row that has SQL, and the very next
// push would fight with whoever owns it.
func TestPipelineMetricPushRefusesWhenTheNameIsHeldByATable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	f.AddTable(&api.Table{ID: "table-clash", Name: "average_order", FeatureID: "collection-1",
		Code: "SELECT 1"})
	f.nameTakenOnMetricCreate = `a table or metric called "average_order" already exists in this feature`

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a name held by a TABLE is a real collision and must refuse")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "rather than a metric") {
		t.Errorf("the refusal must say what holds the name: %v", entry["error"])
	}
}

// --- the two drift legs -----------------------------------------------------

// seedStagedMetric puts a folder in the state a previous push left: the metric
// exists, the lock knows it, and the file agrees with what was staged.
func seedStagedMetric(t *testing.T, f *fakePipelineInstance, root string, status string) *api.Table {
	t.Helper()
	live := f.AddMetric("table-aov", "average_order", `{"source":"table-orders","value":"revenue / orders","version":1}`)
	live.MetricStatus = status

	parsed, err := metricfile.Parse([]byte(metricFileBody(aovRecipe)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolved, err := parsed.Resolve("table-orders")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	inst := state.For(f.Key())
	inst.Metrics = map[string]wfdir.MetricState{
		"metrics/average_order.json": {
			MetricID: "table-aov",
			// The fingerprint of what the LIVE row held when this folder last
			// agreed with it — deliberately NOT the live row's current recipe in
			// the drift tests, which move it.
			LiveSHA256:     metricfile.RecipeSHA256(live.MetricRecipe),
			DeclaredSHA256: metricfile.Fingerprint(resolved, parsed.Description, parsed.ReportingTimezone),
		},
	}
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return live
}

// TestPipelineMetricPushIsUpToDateWhenNothingMoved: the comparison cannot be
// file-against-row (the row holds the CANONICALIZED recipe), so it is
// file-against-what-this-folder-last-sent — and getting that wrong means
// re-staging and re-BUILDING every metric on every push.
func TestPipelineMetricPushIsUpToDateWhenNothingMoved(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	bindPipelineTable(t, root, f.Key(), "orders.sql", "table-orders")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeUpToDate {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if len(f.recipeWrites) != 0 {
		t.Errorf("an unchanged metric was re-staged: %+v", f.recipeWrites)
	}
}

// TestPipelineMetricPushRefusesWhenTheLiveDefinitionMoved is leg (a), and it is
// the leg with no server-side counterpart: no draft precondition speaks about
// the live row.
func TestPipelineMetricPushRefusesWhenTheLiveDefinitionMoved(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	live := seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	// A colleague committed a definition change while we were away.
	live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)
	// And the local file changed too, or the push would report up-to-date and
	// never reach the guard.
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a live definition that moved must refuse")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "changed on the server") {
		t.Errorf("error = %v", entry["error"])
	}
	if len(f.recipeWrites) != 0 {
		t.Errorf("nothing may be written when the guard fires: %+v", f.recipeWrites)
	}
}

// TestPipelineMetricPushForceRefusesAVerifiedMetric is THE destructive gate.
//
// Leg (a) firing on a metric an admin has verified means a colleague committed a
// change to the company's official definition of a number and somebody vetted
// it. --force alone must not be able to discard that: overwriting it costs a
// sentence you had to type.
func TestPipelineMetricPushForceRefusesAVerifiedMetric(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	live := seedStagedMetric(t, f, root, api.MetricStatusVerified)
	live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--force", "--json")
	if err == nil {
		t.Fatal("--force alone must not overwrite a verified metric")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	refusal := entry["error"].(string)
	if !strings.Contains(refusal, "--force-verified-metric") {
		t.Errorf("the refusal must name the second flag: %v", refusal)
	}
	// THE DIFF IS PRINTED WITH THE REFUSAL, not only with the overwrite. Somebody
	// deciding whether to type the second flag has to be able to see what they
	// would be discarding.
	if !strings.Contains(refusal, `"orders / revenue"`) || !strings.Contains(refusal, `"revenue * 2"`) {
		t.Errorf("the refusal did not show the difference:\n%s", refusal)
	}
	if len(f.recipeWrites) != 0 {
		t.Errorf("nothing may be written: %+v", f.recipeWrites)
	}
}

// TestPipelineMetricPushForceVerifiedMetricLetsItThrough: the second flag does
// what it says, and only in company with the first.
func TestPipelineMetricPushForceVerifiedMetricLetsItThrough(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	live := seedStagedMetric(t, f, root, api.MetricStatusVerified)
	live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push",
		"--force", "--force-verified-metric", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["outcome"] != metricOutcomePushed {
		t.Errorf("outcome = %v (%v)", entry["outcome"], entry["error"])
	}
	if len(f.recipeWrites) != 1 {
		t.Fatalf("recipe writes = %+v", f.recipeWrites)
	}
	// --force sends NO precondition: the flag exists to permit writing over what
	// is there, and sending the guard anyway would make it refuse the very thing
	// it authorizes.
	if f.recipeWrites[0].BaseRecipeSha256 != nil {
		t.Errorf("--force still sent a precondition: %v", *f.recipeWrites[0].BaseRecipeSha256)
	}
	// The overwrite is said out loud, with the difference.
	if !strings.Contains(stderr, "overwriting the definition of table-aov") {
		t.Errorf("the overwrite was not announced:\n%s", stderr)
	}
}

// TestPipelineMetricPushRefusesForceVerifiedMetricWithoutForce: the second flag
// only ever WIDENS what the first overwrites, so alone it is inert.
//
// Inert AND silent was the bug: an author who read "--force-verified-metric" as
// the stronger of the two flags typed it by itself, met the ordinary drift
// refusal, and was told nothing about the flag they had just passed. Refused
// before the folder is opened, so no request is made either.
func TestPipelineMetricPushRefusesForceVerifiedMetricWithoutForce(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	before := len(f.requestOrder())

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "--force-verified-metric", "--json")
	if err == nil {
		t.Fatal("--force-verified-metric without --force must be refused, not silently ignored")
	}
	for _, want := range []string{"--force-verified-metric", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if after := len(f.requestOrder()); after != before {
		t.Errorf("the refusal must land before any request: %v", f.requestOrder()[before:after])
	}
}

// TestPipelineMetricPushSendsThePreconditionOverTheReceivedBytes is leg (b),
// which is owned SERVER-side — and the assertion is about the one way a client
// can get it wrong invisibly.
//
// The digest must be taken over the bytes the server SENT, not over a re-marshal
// of them and not over the local file: the row holds the CANONICALIZED recipe
// (its version defaulted, its source a real id), so anything else is refused
// with a 409 describing a conflict nobody caused.
func TestPipelineMetricPushSendsThePreconditionOverTheReceivedBytes(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	// A draft already open, holding the parent's recipe — the resumed-draft case,
	// which is the one that carries a precondition at all.
	draft := f.AddDraft("table-aov", "table-draft-aov", "")
	draft.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"revenue / orders","version":1}`)
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.recipeWrites) != 1 {
		t.Fatalf("recipe writes = %+v", f.recipeWrites)
	}
	sent := f.recipeWrites[0].BaseRecipeSha256
	if sent == nil {
		t.Fatal("a resumed draft must carry a precondition")
	}
	want := recipeDigest(json.RawMessage(`{"source":"table-orders","value":"revenue / orders","version":1}`))
	if *sent != want {
		t.Errorf("baseRecipeSha256 = %q, want the digest of the bytes the server sent (%q)", *sent, want)
	}
}

// TestPipelineMetricPushReportsA409AsAConflict: a lost compare-and-swap is
// something the caller can DO something about, and a script must not have to
// read the prose to tell it from "you may not do this at all".
func TestPipelineMetricPushReportsA409AsAConflict(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	draft := f.AddDraft("table-aov", "table-draft-aov", "")
	draft.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"revenue / orders","version":1}`)
	f.failRecipePut["table-draft-aov"] = 409
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a refused precondition must exit non-zero")
	}
	entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if entry["conflict"] != true {
		t.Errorf("a 409 was not reported as a conflict: %v", entry)
	}
}

// --- publish ----------------------------------------------------------------

// TestPipelineMetricPublishWarnsThatAVerifiedMetricRefingerprints.
//
// Committing a definition change re-fingerprints the metric, so a previously
// verified one lands DRIFTED — "verified · review pending" — until an admin
// verifies it again. The author caused it and would otherwise meet it in the UI,
// days later, as a badge that changed by itself.
func TestPipelineMetricPublishWarnsThatAVerifiedMetricRefingerprints(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusVerified)
	draft := f.AddDraft("table-aov", "table-draft-aov", "")
	draft.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"revenue * 2","version":1}`)
	draft.Status = api.TableStatusReady
	f.synced["table-draft-aov"] = true

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	metrics := decodeJSON(t, out)["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("metrics = %v", metrics)
	}
	entry := metrics[0].(map[string]any)
	if entry["outcome"] != outcomePublished {
		t.Fatalf("outcome = %v (%v)", entry["outcome"], entry["error"])
	}
	if entry["reverified"] != true {
		t.Errorf("the re-verification consequence was not reported: %v", entry)
	}
	if !strings.Contains(stderr, "verified · review pending") {
		t.Errorf("the consequence was not said in words:\n%s", stderr)
	}
	// The live row really did move, and the baseline now describes it.
	if got := f.RecipeOf("table-aov"); !strings.Contains(got, `"revenue * 2"`) {
		t.Errorf("the commit did not land: %s", got)
	}
	recorded := metricStateOf(t, root, f.Key(), "metrics/average_order.json")
	if recorded.LiveSHA256 != metricfile.RecipeSHA256(json.RawMessage(f.RecipeOf("table-aov"))) {
		t.Errorf("the live fingerprint was not refreshed: %+v", recorded)
	}
}

// TestPipelineMetricPublishWithholdsTheBaselineWhenNothingLanded: GONE IS NOT
// LANDED. A draft stops existing on a discard and on a reviewer's rejection too,
// and banking the agreement there leaves the folder claiming a definition
// nobody ever committed.
func TestPipelineMetricPublishWithholdsTheBaselineWhenNothingLanded(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	draft := f.AddDraft("table-aov", "table-draft-aov", "")
	draft.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"revenue * 2","version":1}`)
	draft.Status = api.TableStatusReady
	f.synced["table-draft-aov"] = true
	// The draft is dropped without the parent being touched — a discard, or a
	// reviewer's rejection, racing this publish.
	f.rejectCommit["table-draft-aov"] = true

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "does not hold the definition that draft held") {
		t.Errorf("the divergence was not reported:\n%s", stderr)
	}
	if got := metricStateOf(t, root, f.Key(), "metrics/average_order.json").DeclaredSHA256; got != "" {
		t.Errorf("publish banked an agreement for a definition that never landed: %q", got)
	}
}

// --- discard ----------------------------------------------------------------

// TestPipelineMetricDiscardDropsTheDraftAndReopensTheFile.
//
// The LIVE fingerprint is kept — the live metric did not move — and the DECLARED
// one is cleared, because the row that held this file's content has just been
// deleted. Keeping it would let the next bare push skip the file and leave the
// folder with nothing staged anywhere, which is exactly the state this command's
// report promises it has not left behind.
func TestPipelineMetricDiscardDropsTheDraftAndReopensTheFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	live := seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	f.AddDraft("table-aov", "table-draft-aov", "")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json"); err != nil {
		t.Fatalf("discard: %v\n%s", err, stderr)
	}
	if !slices.Contains(f.discarded, "table-draft-aov") {
		t.Errorf("the metric draft was not discarded: %v", f.discarded)
	}
	recorded := metricStateOf(t, root, f.Key(), "metrics/average_order.json")
	if recorded.DeclaredSHA256 != "" {
		t.Errorf("the file was left recorded as pushed: %+v", recorded)
	}
	if recorded.LiveSHA256 != metricfile.RecipeSHA256(live.MetricRecipe) {
		t.Errorf("the live fingerprint was dropped by a discard that never touched the live row: %+v", recorded)
	}
}

// --- status -----------------------------------------------------------------

// TestPipelineMetricStatusReportsTheThreeAnswers: a file with no row yet, a
// staged draft, and a live definition that moved are three different states, and
// reporting any two of them as one is how somebody force-pushes over a colleague.
func TestPipelineMetricStatusReportsTheThreeAnswers(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	bindPipelineTable(t, root, f.Key(), "orders.sql", "table-orders")

	// (1) No row yet.
	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	entry := decodeJSON(t, out)["remote"].(map[string]any)["metrics"].([]any)[0].(map[string]any)
	if entry["willCreate"] != true || entry["pending"] != true {
		t.Errorf("an uncreated metric = %v", entry)
	}

	// (2) Created and agreed with.
	live := seedStagedMetric(t, f, root, api.MetricStatusVerified)
	out, _, err = runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	entry = decodeJSON(t, out)["remote"].(map[string]any)["metrics"].([]any)[0].(map[string]any)
	if entry["drift"] != driftNone || entry["pending"] == true {
		t.Errorf("an agreed metric = %v", entry)
	}
	if entry["metricStatus"] != api.MetricStatusVerified {
		t.Errorf("metricStatus = %v — somebody deciding whether to force has to see it", entry["metricStatus"])
	}

	// (3) The live definition moved.
	live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)
	out, _, err = runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("drift must make status exit non-zero")
	}
	entry = decodeJSON(t, out)["remote"].(map[string]any)["metrics"].([]any)[0].(map[string]any)
	if entry["drift"] != driftChanged {
		t.Errorf("a moved definition = %v", entry)
	}
}

// TestPipelineStatusNamesMetricsAsMetricsInItsVerdict: the summary line counts
// what it found, and a metric file is not a table.
//
// The parenthesised path says `metrics/….json` while the sentence said "table",
// which is the kind of contradiction a reader trusts and then spends ten minutes
// on, looking for SQL that does not exist.
func TestPipelineStatusNamesMetricsAsMetricsInItsVerdict(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	live := seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	// A colleague committed a change to the definition since this folder synced.
	live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)

	_, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("a moved definition must make status exit non-zero")
	}
	if !strings.Contains(err.Error(), "1 metric changed on the server") {
		t.Errorf("verdict = %v", err)
	}
	if strings.Contains(err.Error(), "table") {
		t.Errorf("a metric was counted as a table: %v", err)
	}
	// And the remedy it names has to be one that works on a metric: --force alone
	// is refused on a VERIFIED one, so a summary naming only --force would send
	// exactly those readers into a refusal.
	if !strings.Contains(err.Error(), "--force-verified-metric") {
		t.Errorf("the remedy does not hold for a metric: %v", err)
	}
}

// TestPipelineStatusCountsTablesAndMetricsSeparately: the mixed run, which is
// the one a single noun cannot describe honestly whichever noun it picks.
func TestPipelineStatusCountsTablesAndMetricsSeparately(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	// One of each: a metric file nothing can read, and a bound table that could
	// not be read either. Both are UNCHECKED — "I could not look" is not "nothing
	// moved" — and the sentence has to name them apart.
	breakMetricFile(t, root)
	f.failGet["table-orders"] = 500

	_, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("a row that could not be read must make status exit non-zero")
	}
	want := "1 table and 1 metric could not be checked for drift (orders.sql, metrics/average_order.json)"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("verdict = %v\n  want it to contain %q", err, want)
	}
}

// --- clone ------------------------------------------------------------------

// TestPipelineCloneWritesMetricFiles: the message that said metrics were "not
// cloned (out of scope for this loop)" is true by construction now, so it is
// gone — and what replaces it is a file a push can send straight back.
func TestPipelineCloneWritesMetricFiles(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	metric := f.AddMetric("table-aov", "average_order",
		`{"value":"revenue / orders","source":"table-orders","version":1}`)
	metric.Description = "Average order value"
	metric.DescriptionSource = api.DescriptionSourceUser

	dir := t.TempDir()
	out, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--json")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	body := readFile(t, root, "metrics/average_order.json")
	if !strings.Contains(body, `"source": "table-orders"`) {
		t.Errorf("the recipe was not written in id form: %s", body)
	}
	if !strings.Contains(body, `"description": "Average order value"`) {
		t.Errorf("a person-written description was not cloned: %s", body)
	}
	if payload := decodeJSON(t, out); payload["metrics"] != float64(1) {
		t.Errorf("metrics = %v", payload["metrics"])
	}

	// A CLONE MUST READ AS CLEAN. Both fingerprints are recorded, or the first
	// status after a clone proposes to re-stage every metric it just copied.
	recorded := metricStateOf(t, root, f.Key(), "metrics/average_order.json")
	if recorded.MetricID != "table-aov" || recorded.LiveSHA256 == "" || recorded.DeclaredSHA256 == "" {
		t.Fatalf("clone recorded %+v", recorded)
	}
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push after clone: %v", err)
	}
	if len(f.recipeWrites) != 0 || len(f.metricsCreated) != 0 {
		t.Errorf("a push straight after a clone changed something: writes=%+v creates=%+v",
			f.recipeWrites, f.metricsCreated)
	}
}

// TestPipelineCloneSkipsAMetricWithNoRecipe: a metric row cannot exist without a
// recipe, so an empty one means the INSTANCE does not serve the field. The clone
// degrades loudly rather than dying, because the derived tables beside it are
// perfectly cloneable.
func TestPipelineCloneSkipsAMetricWithNoRecipe(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	f.AddTable(&api.Table{ID: "table-old", Name: "legacy", FeatureID: "collection-1",
		Kind: api.TableKindMetric})

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--json"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "answered no recipe for it") {
		t.Errorf("the skip was not explained:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "metrics", "legacy.json")); err == nil {
		t.Error("a metric with no recipe must not become a file no push could send")
	}
	if got := readFile(t, dir, "out", "orders.sql"); got == "" {
		t.Error("the derived tables beside it must still be cloned")
	}
}

// --- the committed half, and the alias layer --------------------------------

// TestPipelineMetricPushOnAStackRecordsTheLock.
//
// The tests above run on a LEGACY folder, whose recordings live in the
// git-ignored .ronja/state.json. This is the half that matters for a repository:
// on a named stack the metric's identity and the live definition's fingerprint
// are COMMITTED, so a colleague's fresh clone knows which row the file is and
// has something to compare against — which is the whole reason a metric file can
// be kept in git at all.
//
// The metric's id must be in the LOCK and never in the manifest's `tables` map:
// that map is keyed by .sql path and is walked by the entire SQL half of the
// loop, so a metric in it would be enumerated, hashed and pushed as SQL.
func TestPipelineMetricPushOnAStackRecordsTheLock(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})

	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Sales pipeline",
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	writePipelineFolder(t, root, manifest, map[string]string{
		"metrics/average_order.json": metricFileBody(`{"source":"table-orders","value":"revenue"}`),
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	metricID, live, declared := lock.MetricSeen("dev", "metrics/average_order.json")
	if metricID == "" || live == "" || declared == "" {
		t.Fatalf("the lock recorded %q %q %q", metricID, live, declared)
	}
	// The live fingerprint is over the row's CANONICALIZED recipe — the bytes the
	// server sent, with its version defaulted in — never over the file, which
	// carries neither.
	if want := metricfile.RecipeSHA256(json.RawMessage(f.RecipeOf(metricID))); live != want {
		t.Errorf("liveSHA256 = %q, want the digest of the stored recipe (%q)", live, want)
	}
	body := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(body, `"metrics"`) {
		t.Errorf("the lock has no metrics section:\n%s", body)
	}
	if strings.Contains(body, `"tables"`) {
		t.Errorf("a metric landed in the lock's tables section:\n%s", body)
	}

	// The second push finds nothing to do, which is what makes the committed
	// recording worth having rather than merely present.
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Errorf("the second push was not up to date: %s", out)
	}
}

// TestPipelineMetricAliasesCountAsUses.
//
// A metric file names aliases in two places and NEITHER is a marker: the STEM is
// a filename and `recipe.source` is a JSON value, so markers.Scan finds nothing
// to scan in either. Without the alias-use leg, every dependency a folder
// declares purely for a metric is reported as dead config on every push, every
// status and every `sync check` — the warning crying wolf at exactly the folder
// shape that has no marker to point at.
func TestPipelineMetricAliasesCountAsUses(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	f.AddMetric("table-aov", "aov", `{"source":"table-orders","value":"revenue","version":1}`)

	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Sales pipeline",
		Dependencies: map[string]wfdir.Dependency{
			"orders":  {Kind: "table"},
			"the_aov": {Kind: "table"},
		},
		Stacks: map[string]wfdir.Stack{
			"dev": {
				URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1",
				Bind: map[string]string{"orders": "table-orders", "the_aov": "table-aov"},
			},
		},
	}
	// The folder holds NO .sql file, so both dependencies are named by the metric
	// file alone — the stem by `the_aov`, the source by `orders`.
	writePipelineFolder(t, root, manifest, map[string]string{
		"metrics/the_aov.json": metricFileBody(`{"source":"orders","value":"revenue * 2"}`),
	})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "no file in this folder uses it") {
		t.Errorf("a dependency a metric file names was reported as dead config:\n%s", stderr)
	}
	// And the stem's binding really did decide the row: no create, an edit of the
	// metric the stack points at.
	if len(f.metricsCreated) != 0 {
		t.Errorf("a bound stem must not create a second metric: %+v", f.metricsCreated)
	}
	if len(f.recipeWrites) != 1 || f.recipeWrites[0].ID == "" {
		t.Fatalf("recipe writes = %+v", f.recipeWrites)
	}
	if !strings.Contains(f.recipeWrites[0].Recipe, `"source":"table-orders"`) {
		t.Errorf("the source alias was not resolved: %s", f.recipeWrites[0].Recipe)
	}
}

// TestPipelineMetricsOnlyFolderIsUsableEndToEnd.
//
// A metric's identity lives in the LOCK, not in the manifest's `tables` map — so
// a folder that defines only metrics has an EMPTY Binding.Tables, and three
// commands used to read that as "this folder has nothing here yet" and refuse or
// report nothing. This walks the whole loop on that shape.
func TestPipelineMetricsOnlyFolderIsUsableEndToEnd(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Metrics", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})

	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Metrics",
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	writePipelineFolder(t, root, manifest, map[string]string{
		"metrics/aov.json": metricFileBody(`{"source":"table-orders","value":"revenue"}`),
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.metricsCreated) != 1 {
		t.Fatalf("creates = %+v", f.metricsCreated)
	}
	// No table binding anywhere — which is exactly the shape the three guards
	// below used to read as "this folder has nothing here yet".
	if body := readFile(t, wfdir.LockPath(root)); strings.Contains(body, `"tables"`) {
		t.Fatalf("this folder must have no table bindings at all:\n%s", body)
	}

	// STATUS must not answer "no tables exist on this instance yet". That reason
	// is read by the tree gate as a clean never-deployed verdict, so a folder
	// whose metrics are all deployed would score green for the wrong reason.
	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	if reason, said := remote["notCheckedReason"]; said {
		t.Fatalf("status refused to look: %v", reason)
	}
	metrics := remote["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("metrics = %v", metrics)
	}
	if metrics[0].(map[string]any)["draftID"] == "" {
		t.Errorf("the staged draft was not reported: %v", metrics[0])
	}

	// PUBLISH and DISCARD must both find the draft rather than refusing the
	// folder outright.
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	published := decodeJSON(t, out)["metrics"].([]any)
	if len(published) != 1 || published[0].(map[string]any)["outcome"] != outcomePublished {
		t.Fatalf("publish = %v", published)
	}
	if _, _, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json"); err != nil {
		t.Fatalf("discard: %v", err)
	}
}

// --- a file three verbs disagreed about --------------------------------------

// breakMetricFile makes the committed metric file unreadable, the way a merge or
// a hand edit does: the metric it defines is real, was pushed from here, and its
// id is in the baseline — but nothing can parse the file that names it any more.
func breakMetricFile(t *testing.T, root string) {
	t.Helper()
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		`{"recipe":`+aovRecipe+`,"verified":true}`); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestPipelineMetricPublishRefusesAFileItCannotRead.
//
// `push` refuses such a file by name and `status` reports it as this file's own
// problem. `publish` used to answer NOTHING: resolveMetricIdentities skips a file
// that already carries a Problem, so its id was never filled in, and
// publishOneMetric's empty-id arm dropped it from the report — exit 0, no line,
// and the draft it staged still on the server.
func TestPipelineMetricPublishRefusesAFileItCannotRead(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	f.AddDraft("table-aov", "table-draft-aov", "")
	breakMetricFile(t, root)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatalf("a metric file that cannot be read must make publish exit non-zero:\n%s\n%s", out, stderr)
	}
	metrics, ok := decodeJSON(t, out)["metrics"].([]any)
	if !ok || len(metrics) != 1 {
		t.Fatalf("the broken file was left out of the report: %s", out)
	}
	entry := metrics[0].(map[string]any)
	if entry["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "verified") {
		t.Errorf("the refusal must carry the file's own problem: %v", entry["error"])
	}
	if len(f.committed) != 0 {
		t.Errorf("a file this run could not read must not commit anything: %v", f.committed)
	}
}

// TestPipelineMetricDiscardRefusesAFileItCannotRead: the same hole, where the
// consequence is worse — a draft the person asked to delete, was not told about,
// and which is still there.
func TestPipelineMetricDiscardRefusesAFileItCannotRead(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	f.AddDraft("table-aov", "table-draft-aov", "")
	breakMetricFile(t, root)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json")
	if err == nil {
		t.Fatalf("a metric file that cannot be read must make discard exit non-zero:\n%s\n%s", out, stderr)
	}
	metrics, ok := decodeJSON(t, out)["metrics"].([]any)
	if !ok || len(metrics) != 1 {
		t.Fatalf("the broken file was left out of the report: %s", out)
	}
	entry := metrics[0].(map[string]any)
	if entry["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v", entry["outcome"])
	}
	if !strings.Contains(entry["error"].(string), "verified") {
		t.Errorf("the refusal must carry the file's own problem: %v", entry["error"])
	}
	// The whole point: the draft really is still there, and the person is told so
	// rather than left with a clean exit.
	if slices.Contains(f.discarded, "table-draft-aov") {
		t.Errorf("a draft was discarded for a file nothing could identify: %v", f.discarded)
	}
	if !strings.Contains(stderr, "still on the server") {
		t.Errorf("the surviving draft was not mentioned:\n%s", stderr)
	}
}

// TestPipelineMetricDiscardDoesNotGateOnDraftsItCannotHave: the confirmation is
// in front of a DELETION, so a run with nothing to delete must not ask for one.
//
// The count came from every metric file on disk rather than from the ones that
// name a row here, so a folder whose metrics have never been pushed offered to
// discard drafts of all of them — and under --json, where there is nobody to
// prompt, refused the command outright demanding --yes for a run that was never
// going to write.
func TestPipelineMetricDiscardDoesNotGateOnDraftsItCannotHave(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--json")
	if err != nil {
		t.Fatalf("discard: %v\n%s", err, stderr)
	}
	if decodeJSON(t, out)["nothing"] != true {
		t.Errorf("payload = %s", out)
	}
	if len(f.discarded) != 0 {
		t.Errorf("nothing was staged, but %v was discarded", f.discarded)
	}
}

// TestMetricDraftCandidatesCountsOnlyFilesThatNameARow is the count the
// confirmation quotes. A file with no metric behind it certainly has no draft of
// one, and neither does one this run cannot identify.
func TestMetricDraftCandidatesCountsOnlyFilesThatNameARow(t *testing.T) {
	got := metricDraftCandidates([]pipelineMetricFile{
		{Path: "metrics/average_order.json", MetricID: "table-aov"},
		{Path: "metrics/never_pushed.json"},
		{Path: "metrics/broken.json", Problem: "this is not a valid metric file"},
	})
	if got != 1 {
		t.Errorf("metricDraftCandidates = %d, want 1 — only the file that names a row can have a draft", got)
	}
}
