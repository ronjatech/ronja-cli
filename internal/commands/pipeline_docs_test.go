package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func writeSidecar(t *testing.T, root, alias, body string) {
	t.Helper()
	dir := filepath.Join(root, wfdir.TableDocsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, alias+wfdir.TableDocsExt), []byte(body), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// declareTableDependency adds an alias → id binding to a folder's manifest, the
// way `ronja bind` does.
func declareTableDependency(t *testing.T, root string, key wfdir.InstanceKey, alias, id string) {
	t.Helper()
	manifest := pipelineManifestOf(t, root)
	if manifest.Dependencies == nil {
		manifest.Dependencies = map[string]wfdir.Dependency{}
	}
	manifest.Dependencies[alias] = wfdir.Dependency{Kind: "table"}
	// The bind map lives on the entry, not the embedded Binding, so it is written
	// in place: SetBinding replaces an entry wholesale.
	found := false
	for i := range manifest.Instances {
		if !wfdir.Matches(manifest.Instances[i].Key(), key) {
			continue
		}
		if manifest.Instances[i].Bind == nil {
			manifest.Instances[i].Bind = map[string]string{}
		}
		manifest.Instances[i].Bind[alias] = id
		found = true
	}
	if !found {
		t.Fatalf("no manifest entry for %+v to bind %q on", key, alias)
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func TestPipelinePushSendsTheHeaderAsDocumentation(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// `margin` is deliberately not a measured column: the unmatched case.
	f.fields["table-revenue"] = []string{"id", "total"}
	editFile(t, root, "revenue.sql", strings.Join([]string{
		"-- @table One row per invoice line.",
		"-- Amounts are in SEK.",
		"-- @column id: the line's own id",
		"-- @column margin: not a column this table has",
		"SELECT 1 FROM {{ ref('table-orders') }}",
	}, "\n"))

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}

	// The docs PUT must follow the code PUT: prose attaches only to columns the
	// build has measured.
	if len(f.updates) != 2 {
		t.Fatalf("want a code PUT then a docs PUT, got %+v", f.updates)
	}
	code, docs := f.updates[0], f.updates[1]
	if !code.SetsCode || code.Fields != nil || code.Description != nil {
		t.Errorf("the code PUT must carry no documentation: %+v", code)
	}
	// The server's patch leaves an absent key alone, so a docs body sending
	// `"code": ""` would wipe the SQL this push just wrote.
	if docs.SetsCode || docs.SetsInputModels {
		t.Fatalf("the documentation PUT named code/inputModels: %+v", docs)
	}
	if docs.ID != code.ID {
		t.Errorf("the documentation went to %s, the SQL to %s — both must be the draft", docs.ID, code.ID)
	}
	if docs.Description == nil || *docs.Description != "One row per invoice line. Amounts are in SEK." {
		t.Errorf("description = %v", docs.Description)
	}
	if docs.DescriptionSource != api.DescriptionSourceUser {
		t.Errorf("descriptionSource = %q, want %q — a person committed these words", docs.DescriptionSource, api.DescriptionSourceUser)
	}
	want := []api.TableFieldInput{
		{Name: "id", Description: "the line's own id", DescriptionSource: api.DescriptionSourceUser},
		{Name: "margin", Description: "not a column this table has", DescriptionSource: api.DescriptionSourceUser},
	}
	if len(docs.Fields) != len(want) {
		t.Fatalf("fields = %+v", docs.Fields)
	}
	for i, field := range docs.Fields {
		if field != want[i] {
			t.Errorf("fields[%d] = %+v, want %+v (sorted by name, so two checkouts send identical bodies)", i, field, want[i])
		}
	}

	payload := decodeJSON(t, out)
	file := payload["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Fatalf("an unmatched name is INFORMATION, not a failure: %+v", file)
	}
	report := file["docs"].(map[string]any)
	if got := toStrings(report["attachedColumns"]); len(got) != 1 || got[0] != "id" {
		t.Errorf("attachedColumns = %v", got)
	}
	if got := toStrings(report["unmatchedColumns"]); len(got) != 1 || got[0] != "margin" {
		t.Errorf("unmatchedColumns = %v", got)
	}
	if report["description"] != true {
		t.Errorf("the report must say the description was written: %+v", report)
	}
	if !strings.Contains(stderr, "margin") {
		t.Errorf("the unmatched name must be said out loud: %s", stderr)
	}
}

// TestPipelinePushWithoutAHeaderSendsNoDocumentation: documentation is opt-in,
// so a folder without a header sends no docs fields and records no fingerprint.
func TestPipelinePushWithoutAHeaderSendsNoDocumentation(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A leading comment that does not open with @table is ordinary commentary.
	editFile(t, root, "revenue.sql", "-- joins orders to customers, watch the fanout\nSELECT 1 FROM {{ ref('table-orders') }}")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.updates) != 1 {
		t.Fatalf("want exactly one PUT, got %+v", f.updates)
	}
	if f.updates[0].Fields != nil || f.updates[0].Description != nil {
		t.Fatalf("a folder that did not opt in must send no documentation: %+v", f.updates[0])
	}
	if meta := lockTableMetaOf(t, root, f, "revenue.sql"); meta != "" {
		t.Errorf("a folder that documents nothing recorded a documentation fingerprint: %q", meta)
	}
}

// TestPipelinePushSendsTheHeaderOnCreate: a table must not exist, even briefly,
// with no description when its own file carries one.
func TestPipelinePushSendsTheHeaderOnCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "margins.sql", "-- @table Margin per invoice line.\n-- @column margin: total less cost\nSELECT 1")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("want one create, got %+v", f.created)
	}
	create := f.created[0]
	if create.Description != "Margin per invoice line." || create.DescriptionSource != api.DescriptionSourceUser {
		t.Errorf("create body = %+v", create)
	}
	if len(create.Fields) != 1 || create.Fields[0].Name != "margin" {
		t.Errorf("create fields = %+v", create.Fields)
	}
}

// TestPipelinePushRefusesWhenTheDocumentationMoved: documentation drift is its
// own leg, separate from the SQL fingerprint, because a guard watching only
// `code` would let a push overwrite an admin's prose while the SQL is untouched.
func TestPipelinePushRefusesWhenTheDocumentationMoved(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id", "total"}
	documented := "-- @table Ours.\n-- @column id: our prose\nSELECT * FROM {{ ref('table-orders') }}"
	editFile(t, root, "revenue.sql", documented)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	if lockTableMetaOf(t, root, f, "revenue.sql") == "" {
		t.Fatal("the first documenting push recorded no fingerprint, so the guard is disarmed for ever")
	}

	f.mu.Lock()
	f.tables["table-revenue"].Description = "somebody else wrote this"
	f.mu.Unlock()

	editFile(t, root, "revenue.sql", documented+"\n-- trailing edit")
	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("want a refusal, got a clean push")
	}
	if !strings.Contains(stderr, "documentation") {
		t.Fatalf("the refusal must name what moved: %s", stderr)
	}
	if strings.Contains(stderr, "the SQL of table-revenue changed") {
		t.Errorf("a documentation-only drift was announced as a SQL change: %s", stderr)
	}

	// --force must re-record the agreement, or every later push re-reports the
	// same difference.
	before := lockTableMetaOf(t, root, f, "revenue.sql")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force", "--json"); err != nil {
		t.Fatalf("forced push: %v\n%s", err, stderr)
	}
	if after := lockTableMetaOf(t, root, f, "revenue.sql"); after == before || after == "" {
		t.Fatalf("--force did not re-record the agreement (%q → %q)", before, after)
	}
}

// TestPipelinePushDocumentsASidecarTable: a sidecar documents a table this folder
// does not build, named through the alias layer.
func TestPipelinePushDocumentsASidecarTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Fortnox invoices", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"invoice_no", "amount"}
	declareTableDependency(t, root, f.Key(), "fortnox_invoices", "table-raw")
	writeSidecar(t, root, "fortnox_invoices", `{
	  "description": "One row per Fortnox invoice line.",
	  "columns": {
	    "amount": "line total, excluding VAT",
	    "renamed_last_week": "prose for a column that is not there"
	  }
	}`)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	payload := decodeJSON(t, out)
	docs, ok := payload["docs"].([]any)
	if !ok || len(docs) != 1 {
		t.Fatalf("want one docs entry, got %+v", payload["docs"])
	}
	entry := docs[0].(map[string]any)
	if entry["outcome"] != docsOutcomeWritten || entry["tableID"] != "table-raw" {
		t.Fatalf("docs entry = %+v", entry)
	}
	result := entry["result"].(map[string]any)
	if got := toStrings(result["attachedColumns"]); len(got) != 1 || got[0] != "amount" {
		t.Errorf("attachedColumns = %v", got)
	}
	// A name the data does not have is reported, never an error, so a committed
	// docs file survives a column rename.
	if got := toStrings(result["unmatchedColumns"]); len(got) != 1 || got[0] != "renamed_last_week" {
		t.Errorf("unmatchedColumns = %v", got)
	}

	// A column the file never names is left untouched.
	f.mu.Lock()
	stored := f.columnDocs["table-raw"]
	description := f.tables["table-raw"].Description
	f.mu.Unlock()
	if stored["invoice_no"] != "" {
		t.Errorf("a column the file never named was written: %q", stored["invoice_no"])
	}
	if stored["amount"] != "line total, excluding VAT" {
		t.Errorf("amount = %q", stored["amount"])
	}
	if description != "One row per Fortnox invoice line." {
		t.Errorf("description = %q", description)
	}

	updatesBefore := len(f.updates)
	out, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v\n%s", err, stderr)
	}
	if len(f.updates) != updatesBefore {
		t.Errorf("the second push wrote again: %+v", f.updates[updatesBefore:])
	}
	payload = decodeJSON(t, out)
	entry = payload["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeUpToDate {
		t.Errorf("outcome = %v, want %q", entry["outcome"], docsOutcomeUpToDate)
	}
	// `upToDate` is what a script watches, so it must be derived from the docs
	// outcomes too, not only from the no-work early return.
	if payload["upToDate"] != true {
		t.Errorf("a clean run over a sidecar folder reported upToDate = %v", payload["upToDate"])
	}
}

// TestPipelinePushRefusesASidecarWithNoBinding: a sidecar naming an alias nothing
// binds is refused, never resolved by searching the organization for a similar
// table name.
func TestPipelinePushRefusesASidecarWithNoBinding(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	writeSidecar(t, root, "fortnox_invoices", `{"description":"x"}`)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatalf("want a non-zero exit, got a clean push\n%s", out)
	}
	entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeRefused {
		t.Fatalf("docs entry = %+v", entry)
	}
	if !strings.Contains(entry["error"].(string), "dependencies") {
		t.Errorf("the refusal must point at the declaration: %v", entry["error"])
	}
	if !strings.Contains(stderr, "fortnox_invoices") {
		t.Errorf("stderr must name the file: %s", stderr)
	}
}

// TestPipelinePushRefusesASidecarThatDoesNotParse: an unknown key is an error,
// never a dropped field, or the file and the row would disagree silently.
func TestPipelinePushRefusesASidecarThatDoesNotParse(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"description":"x","kind":"integration"}`)

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("want a non-zero exit for a file that cannot be read as documentation")
	}
	entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeRefused {
		t.Fatalf("docs entry = %+v", entry)
	}
	if len(f.updates) != 0 {
		t.Errorf("an unparseable file still reached the server: %+v", f.updates)
	}
}

func TestPipelinePushRefusesASidecarWhoseTableMoved(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"amount"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}

	f.mu.Lock()
	f.fields["table-raw"] = append(f.fields["table-raw"], "invoice_no")
	f.columnDocs["table-raw"]["invoice_no"] = "somebody else's prose"
	f.mu.Unlock()
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours, reworded"}}`)

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("want a refusal on a row whose documentation moved")
	}
	entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeRefused || !strings.Contains(entry["error"].(string), "documentation") {
		t.Fatalf("docs entry = %+v", entry)
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force", "--json"); err != nil {
		t.Fatalf("forced push: %v\n%s", err, stderr)
	}
	f.mu.Lock()
	stored := f.columnDocs["table-raw"]
	f.mu.Unlock()
	if stored["amount"] != "ours, reworded" {
		t.Errorf("--force did not write: %q", stored["amount"])
	}
	// --force writes what this folder declares; it never blanks what it does not.
	if stored["invoice_no"] != "somebody else's prose" {
		t.Errorf("--force cleared a column the file never named: %q", stored["invoice_no"])
	}
	// --force must re-record the agreement, or every later push re-reports the
	// same difference.
	out, _, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("the push after a --force was refused again: %v", err)
	}
	entry = decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeUpToDate {
		t.Errorf("--force did not re-record the sidecar's agreement: %+v", entry)
	}
}

// TestASidecarThatDoesNotParseIsStillAUseOfItsDependency: the sidecar's filename,
// not its body, names the alias, so a broken file earns one refusal and no
// second "dead dependency" warning about a correct manifest entry.
func TestASidecarThatDoesNotParseIsStillAUseOfItsDependency(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"description":"x",`)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatalf("a file that is not JSON must be refused\n%s", out)
	}
	if strings.Contains(stderr, "no file in this folder uses it") {
		t.Errorf("one broken file earned two warnings — the second about a correct manifest entry: %s", stderr)
	}
}

// TestPipelineStatusReportsSidecars: status (and `ronja sync status` through it)
// must report pending sidecars, or a gate would call a folder clean that a push
// would still change.
func TestPipelineStatusReportsSidecars(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"amount"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)

	out, _, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		// A pending sidecar is not drift, so status must exit clean.
		t.Fatalf("status: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	docs, ok := remote["docs"].([]any)
	if !ok || len(docs) != 1 {
		t.Fatalf("status reported no sidecars: %+v", remote["docs"])
	}
	entry := docs[0].(map[string]any)
	if entry["tableID"] != "table-raw" || entry["pending"] != true {
		t.Fatalf("docs status = %+v", entry)
	}
	if entry["drift"] != driftNoBaseline {
		t.Errorf("drift = %v, want %q — nothing has been compared yet", entry["drift"], driftNoBaseline)
	}
}

// TestPipelinePushNarrowsDocsToNamedPaths: naming any path narrows the sidecars
// as well as the .sql files.
func TestPipelinePushNarrowsDocsToNamedPaths(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.AddTable(&api.Table{ID: "table-other", Name: "Other", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"amount"}
	f.fields["table-other"] = []string{"amount"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	declareTableDependency(t, root, f.Key(), "other", "table-other")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)
	writeSidecar(t, root, "other", `{"columns":{"amount":"also ours"}}`)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", wfdir.TableDocsPath("raw"), "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	docs := decodeJSON(t, out)["docs"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["path"] != wfdir.TableDocsPath("raw") {
		t.Fatalf("naming one sidecar pushed %+v", docs)
	}
	f.mu.Lock()
	other := f.columnDocs["table-other"]
	f.mu.Unlock()
	if len(other) != 0 {
		t.Errorf("a sidecar nobody named was pushed: %+v", other)
	}
}

// TestSidecarAliasIsSeenAsAUseOfTheDependency: without this the alias pre-flight
// reports a dependency declared only to document a table as dead config.
func TestSidecarAliasIsSeenAsAUseOfTheDependency(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"amount"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "no file in this folder uses it") {
		t.Fatalf("the dependency a sidecar names was reported as dead config: %s", stderr)
	}
}

// lockTableMetaOf reads one pipeline file's recorded DOCUMENTATION fingerprint
// out of the committed lock, or out of the local baseline for a legacy folder.
func lockTableMetaOf(t *testing.T, root string, f *fakePipelineInstance, path string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	for stack := range lock.Stacks {
		if meta := lock.TableMeta(stack, path); meta != "" {
			return meta
		}
	}
	return pipelineStateOf(t, root, f.Key()).TableStateFor(path).MetaSHA256
}

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}

func TestMetaSHA256MatchesTheRowItWasTakenFrom(t *testing.T) {
	row := &api.Table{
		Description: "One row per invoice.",
		Fields: []api.TableField{
			{Name: "amount", Description: "line total"},
			{Name: "id"},
		},
	}
	first := metaOf(row)
	if first != metaOf(row) {
		t.Fatal("the same row hashed twice must agree with itself")
	}
	row.Fields[1].Description = "the line's id"
	if metaOf(row) == first {
		t.Fatal("documenting a column must move the fingerprint")
	}
	// A column with no prose contributes nothing, so adding undocumented columns
	// never moves the fingerprint.
	bare := &api.Table{Fields: []api.TableField{{Name: "a"}, {Name: "b"}}}
	if metaOf(bare) != tabledocs.MetaSHA256("", nil) {
		t.Fatal("undocumented columns must not move the fingerprint")
	}
}

// TestSidecarRoundTripsThroughTheLock: a sidecar repointed at a different table
// must drop its fingerprint, never compare one table's prose to another's hash.
func TestSidecarRoundTripsThroughTheLock(t *testing.T) {
	lock := &wfdir.Lock{}
	lock.SetTableDocsSeen("prod", "tables/raw.json", "table-a", "hash-a", "declared-a")
	if id, meta, declared := lock.TableDocsSeen("prod", "tables/raw.json"); id != "table-a" || meta != "hash-a" || declared != "declared-a" {
		t.Fatalf("round trip = %q %q %q", id, meta, declared)
	}
	lock.SetTableDocsSeen("prod", "tables/raw.json", "table-b", "", "")
	if id, meta, declared := lock.TableDocsSeen("prod", "tables/raw.json"); id != "table-b" || meta != "" || declared != "" {
		t.Fatalf("a rebound sidecar kept the old fingerprint: %q %q %q", id, meta, declared)
	}
	body, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again wfdir.Lock
	if err := json.Unmarshal(body, &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if id, _, _ := again.TableDocsSeen("prod", "tables/raw.json"); id != "table-b" {
		t.Fatalf("lost through the file: %s", body)
	}
}

// TestPipelinePushDoesNotBankAFileWhoseDocumentationFailed: a docs_failed file
// has its SQL built but must not enter the baseline, or the bare push never
// picks it again and the committed header and the row disagree silently.
func TestPipelinePushDoesNotBankAFileWhoseDocumentationFailed(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id"}
	// The non-admin's 400: the SQL PUT lands, the documenting PUT does not.
	f.failDocsPut["table-draft-1"] = 400
	documented := "-- @table One row per invoice line.\n-- @column id: the line's own id\nSELECT * FROM {{ ref('table-orders') }}"
	editFile(t, root, "revenue.sql", documented)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatalf("a documentation write that did not land must exit non-zero\n%s", out)
	}
	payload := decodeJSON(t, out)
	file := payload["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeDocsFailed {
		t.Fatalf("outcome = %v, want %q: %+v", file["outcome"], pushOutcomeDocsFailed, file)
	}
	if !strings.Contains(file["error"].(string), "admin") {
		t.Errorf("the refusal must explain who can write column prose: %v", file["error"])
	}
	report := file["docs"].(map[string]any)
	if report["description"] == true {
		t.Errorf("a failed write reported a description as written: %+v", report)
	}
	summary := payload["error"].(string)
	if strings.Contains(summary, "did not land,") || strings.Contains(summary, "1 of 1 file(s) did not land") {
		t.Errorf("a built, publishable file was counted as one that did not land: %q", summary)
	}
	if !strings.Contains(summary, "publishable") {
		t.Errorf("the summary must say the SQL landed: %q", summary)
	}

	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["revenue.sql"].SHA256 == wfdir.HashString(documented) {
		t.Fatal("the file was recorded as synced although its documentation never landed — the next bare push will skip it for ever")
	}

	// A bare push, no paths and no --force, must pick the file up again.
	delete(f.failDocsPut, "table-draft-1")
	if _, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("retry: %v\n%s", err, stderr)
	}
	f.mu.Lock()
	stored := f.columnDocs["table-draft-1"]["id"]
	f.mu.Unlock()
	if stored != "the line's own id" {
		t.Fatalf("the retry did not document the column: %q", stored)
	}
	if got := pipelineStateOf(t, root, f.Key()).Files["revenue.sql"].SHA256; got != wfdir.HashString(documented) {
		t.Error("a push that documented the file did not advance the baseline")
	}
	_ = stderr
}

// TestPipelinePushWritesNoDocumentationWhenTheBuildFails pins an ordering: the
// docs write sits below the build, so prose never lands on a draft whose columns
// were never measured.
func TestPipelinePushWritesNoDocumentationWhenTheBuildFails(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.syncOutcome["table-draft-1"] = "build_failed"
	editFile(t, root, "revenue.sql", "-- @table One row per invoice line.\n-- @column id: the line's own id\nSELECT * FROM {{ ref('table-orders') }}")

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatalf("a failed build must exit non-zero\n%s", out)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeBuildFailed {
		t.Fatalf("outcome = %v: %+v", file["outcome"], file)
	}
	for _, update := range f.updates {
		if update.Fields != nil || update.Description != nil {
			t.Fatalf("a draft whose build failed was documented: %+v", update)
		}
	}
}

// TestPipelinePushRefusesASidecarForATableThisFolderBuilds: one table, one
// carrier. The check is on the resolved ID, since neither spelling here collides
// with a filename; a table documented twice gets the header on the draft and the
// sidecar on the live row, publish overwrites the sidecar's work, and every later
// push refuses with self-inflicted drift.
func TestPipelinePushRefusesASidecarForATableThisFolderBuilds(t *testing.T) {
	t.Run("literal id", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		root := seedBoundFolder(t, f)
		writeSidecar(t, root, "table-revenue", `{"description":"documented twice"}`)

		out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
		if err == nil {
			t.Fatalf("want a refusal\n%s", out)
		}
		entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
		if entry["outcome"] != docsOutcomeRefused {
			t.Fatalf("docs entry = %+v", entry)
		}
		if !strings.Contains(entry["error"].(string), "revenue.sql") {
			t.Errorf("the refusal must name the file that already builds it: %v", entry["error"])
		}
		if strings.Contains(stderr, "documented twice") {
			t.Errorf("the sidecar was still sent: %s", stderr)
		}
		f.mu.Lock()
		description := f.tables["table-revenue"].Description
		f.mu.Unlock()
		if description == "documented twice" {
			t.Error("a sidecar for a table this folder builds wrote onto the live row")
		}
	})

	t.Run("alias bound to a built table", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		root := seedBoundFolder(t, f)
		declareTableDependency(t, root, f.Key(), "raw", "table-revenue")
		writeSidecar(t, root, "raw", `{"description":"documented twice"}`)

		out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
		if err == nil {
			t.Fatalf("want a refusal\n%s", out)
		}
		entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
		if entry["outcome"] != docsOutcomeRefused || !strings.Contains(entry["error"].(string), "revenue.sql") {
			t.Fatalf("docs entry = %+v", entry)
		}
	})
}

// TestPipelinePushDocumentsUnicodeColumnNames: column names match exactly and no
// carrier may normalize one, or a correct-looking name stays unmatched for ever.
func TestPipelinePushDocumentsUnicodeColumnNames(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"belopp_kr", "année", "商品名"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"année":"l'année de la facture","商品名":"品目の名前","belopp_kr":"belopp i kronor"}}`)
	editFile(t, root, "revenue.sql", "-- @table Fakturarader.\n-- @column année: l'année\nSELECT * FROM {{ ref('table-orders') }}")
	f.fields["table-revenue"] = []string{"année"}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	f.mu.Lock()
	stored := f.columnDocs["table-raw"]
	f.mu.Unlock()
	if stored["année"] != "l'année de la facture" || stored["商品名"] != "品目の名前" {
		t.Fatalf("a unicode column name did not attach verbatim: %+v", stored)
	}
	entry := decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	attached := toStrings(entry["result"].(map[string]any)["attachedColumns"])
	if len(attached) != 3 {
		t.Fatalf("attachedColumns = %v", attached)
	}
	// Sorted by code point, not a locale's collation, so two checkouts send
	// byte-identical bodies.
	if attached[0] != "année" || attached[1] != "belopp_kr" || attached[2] != "商品名" {
		t.Errorf("attachedColumns is not sorted by code point: %v", attached)
	}
	f.mu.Lock()
	header := f.columnDocs["table-draft-1"]["année"]
	f.mu.Unlock()
	if header != "l'année" {
		t.Errorf("the header did not document a unicode column: %q", header)
	}
}

// TestPipelinePublishRecordsNoDocumentationFingerprintForAnUndocumentedFolder:
// publish, like push, arms the documentation leg only for a folder that documents
// something, or every folder's committed lock grows a guard it never opted into.
func TestPipelinePublishRecordsNoDocumentationFingerprintForAnUndocumentedFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.fields["table-orders"] = []string{"id"}
	f.columnDocs["table-orders"] = map[string]string{"id": "somebody else wrote this"}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if meta := lockTableMetaOf(t, root, f, "orders.sql"); meta != "" {
		t.Fatalf("a folder whose files carry no header recorded a documentation fingerprint on publish: %q", meta)
	}
}

// A file that carries a header must record the agreement on publish, or the next
// push reports this folder's own publish as somebody else's edit.
func TestPipelinePublishRecordsTheDocumentationFingerprintForADocumentedFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.fields["table-orders"] = []string{"id"}
	f.columnDocs["table-orders"] = map[string]string{"id": "the order's own id"}
	// The agreement is recorded only if the row really holds what the file
	// declares (docsLanded), so the draft the commit copies must carry it.
	f.tables["table-draft-5"].Description = "Orders."
	editFile(t, root, "orders.sql", "-- @table Orders.\n-- @column id: the order's own id\nSELECT staged FROM raw")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if meta := lockTableMetaOf(t, root, f, "orders.sql"); meta == "" {
		t.Fatal("a documented file recorded no agreement at the moment its prose landed on the live row")
	}
}

// TestPipelineStatusOnAFreshCloneWithASidecar: `.ronja/` is gitignored, so on a
// fresh clone the instance state is nil and status, unlike push and publish,
// does not go through pipelineBaseline; the sidecar read must not dereference it.
func TestPipelineStatusOnAFreshCloneWithASidecar(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-invoices", Name: "invoices", FeatureID: "collection-1"})
	declareTableDependency(t, root, f.Key(), "invoices", "table-invoices")
	writeSidecar(t, root, "invoices", `{"description":"One row per invoice.","columns":{"amount":"SEK, ex VAT."}}`)

	if err := os.RemoveAll(filepath.Join(root, wfdir.StateDirName)); err != nil {
		t.Fatalf("remove local baseline: %v", err)
	}

	out, stderr, _ := runPipelineCLI(t, root, "pipeline", "status", "--json")
	// The exit code is deliberately not asserted; only a panic is wrong here.
	if strings.Contains(stderr, "panic:") || strings.Contains(stderr, "nil pointer") {
		t.Fatalf("status panicked on a fresh clone carrying a docs sidecar:\n%s", stderr)
	}
	docs := decodeJSON(t, out)["remote"].(map[string]any)["docs"].([]any)
	if len(docs) != 1 {
		t.Fatalf("docs = %+v", docs)
	}
	// Never "no drift": that answer gets somebody else's prose overwritten.
	if got := docs[0].(map[string]any)["drift"]; got != driftNoBaseline {
		t.Errorf("drift = %v, want %q on a checkout that has never synced", got, driftNoBaseline)
	}
}

// TestPipelinePublishDoesNotBankDocumentationItDidNotAchieve: publish runs in a
// separate process from the push that withheld recordSynced, so it must check
// the row, not the header, before banking the file.
func TestPipelinePublishDoesNotBankDocumentationItDidNotAchieve(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id"}
	f.failDocsPut["table-draft-1"] = 400
	documented := "-- @table One row per invoice line.\n-- @column id: the line's own id\nSELECT * FROM {{ ref('table-orders') }}"
	editFile(t, root, "revenue.sql", documented)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err == nil {
		t.Fatalf("a documentation write that did not land must exit non-zero\n%s", stderr)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}

	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["revenue.sql"].SHA256 == wfdir.HashString(documented) {
		t.Fatal("publish banked a file whose documentation was refused — the next bare push skips it for ever, and the committed header and the row disagree in silence")
	}
	// The meta fingerprint is deliberately not asserted: the row did not move, so
	// the existing recording stays honest; a header's own signal is the content hash.
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	local := decodeJSON(t, out)["local"].(map[string]any)
	if !slices.Contains(toStrings(local["modified"]), "revenue.sql") {
		t.Fatalf("status reports nothing to do for a file whose documentation never landed: %+v", local)
	}

	delete(f.failDocsPut, "table-draft-2")
	f.failDocsPut = map[string]int{}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("retry: %v\n%s", err, stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var landed bool
	for _, docs := range f.columnDocs {
		if docs["id"] == "the line's own id" {
			landed = true
		}
	}
	if !landed {
		t.Fatalf("the retry did not send the documentation: %+v", f.columnDocs)
	}
}

// TestPipelinePushWarnsAboutAnEmptySidecarRatherThanRefusing: an empty sidecar
// declares nothing, so it warns; a refusal would fail every push in the folder.
func TestPipelinePushWarnsAboutAnEmptySidecarRatherThanRefusing(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-invoices", Name: "invoices", FeatureID: "collection-1"})
	declareTableDependency(t, root, f.Key(), "invoices", "table-invoices")
	writeSidecar(t, root, "invoices", `{}`)
	editFile(t, root, "revenue.sql", "SELECT 2 FROM {{ ref('table-orders') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("an empty sidecar must not fail a push that has nothing to do with it: %v\n%s", err, stderr)
	}
	docs := decodeJSON(t, out)["docs"].([]any)
	if len(docs) != 1 {
		t.Fatalf("docs = %+v", docs)
	}
	doc := docs[0].(map[string]any)
	if doc["outcome"] != docsOutcomeNothing {
		t.Errorf("outcome = %v, want %q", doc["outcome"], docsOutcomeNothing)
	}
	if doc["warning"] == nil || !strings.Contains(doc["warning"].(string), "documents nothing") {
		t.Errorf("warning = %v", doc["warning"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.Requests {
		if strings.Contains(request, "table-invoices") {
			t.Errorf("an empty sidecar reached the network: %v", f.Requests)
			break
		}
	}
}

// TestPipelinePushWarnsAboutOrphanedColumnDirectives: a bare `--` deliberately
// ends the header, so directives below it are not read and must be reported, in
// ONE note per file however many there are.
func TestPipelinePushWarnsAboutOrphanedColumnDirectives(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "revenue.sql", "-- @table One row per invoice line.\n"+
		"--\n"+
		"-- the numbers below are the ones finance signed off on\n"+
		"-- @column id: the line's own id\n"+
		"-- @column total: SEK, ex VAT\n"+
		"SELECT * FROM {{ ref('table-orders') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("a comment must never fail a push: %v\n%s", err, stderr)
	}
	if got := strings.Count(stderr, "Note: revenue.sql — a bare `--` ends the header"); got != 1 {
		t.Fatalf("want exactly one Note line for the file, got %d:\n%s", got, stderr)
	}
	if !strings.Contains(stderr, "2 documentation directives below it are not read") {
		t.Errorf("the note does not name the count:\n%s", stderr)
	}
	// Selected by content, never position or length: `notes[]` is shared with
	// other warnings, such as upstream validation's line.
	var matched []string
	for _, entry := range decodeJSON(t, out)["files"].([]any) {
		file := entry.(map[string]any)
		if file["path"] != "revenue.sql" {
			continue
		}
		notes, _ := file["notes"].([]any)
		for _, note := range notes {
			if text, ok := note.(string); ok && strings.Contains(text, "a bare `--` ends the header") {
				matched = append(matched, text)
			}
		}
	}
	if len(matched) != 1 {
		t.Fatalf("want exactly one machine-readable note about the terminator, got %v\n%s", matched, out)
	}
	if !strings.Contains(matched[0], "2 documentation directives below it are not read") {
		t.Errorf("the machine-readable note does not name the count: %q", matched[0])
	}
}

// TestPipelineStatusDoesNotFailOnAnEmptySidecar: pipelineStatusVerdict scores
// Problem and Drift, so an empty sidecar must be a warning, not a Problem.
func TestPipelineStatusDoesNotFailOnAnEmptySidecar(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-invoices", Name: "invoices", FeatureID: "collection-1"})
	declareTableDependency(t, root, f.Key(), "invoices", "table-invoices")
	writeSidecar(t, root, "invoices", `{}`)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	docs := decodeJSON(t, out)["remote"].(map[string]any)["docs"].([]any)
	doc := docs[0].(map[string]any)
	if doc["problem"] != nil {
		t.Errorf("an empty sidecar is a warning, not a problem: %v", doc["problem"])
	}
	if doc["pending"] == true {
		t.Errorf("an empty sidecar reported as pending: %+v", doc)
	}
	if doc["warning"] == nil {
		t.Errorf("the placeholder was not reported at all: %+v", doc)
	}
	// --json suppresses the human printer, so it needs its own run.
	human, stderr, err := runPipelineCLI(t, root, "pipeline", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	if !strings.Contains(human, "documents nothing") {
		t.Errorf("the human report says nothing about it: %s", human)
	}
}

// TestPipelinePushClearsTheDraftPointerWhenTheDocsPutIs404: a 404 on the docs PUT
// means the draft was committed or discarded elsewhere, so the pointer must be
// cleared, as the build-wait 404 does, or the next publish commits a gone row.
func TestPipelinePushClearsTheDraftPointerWhenTheDocsPutIs404(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id"}
	f.failDocsPut["table-draft-1"] = 404
	editFile(t, root, "revenue.sql", "-- @table One row per invoice line.\nSELECT * FROM {{ ref('table-orders') }}")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err == nil {
		t.Fatalf("a documentation write that did not land must exit non-zero\n%s", stderr)
	}
	if draft := pipelineStateOf(t, root, f.Key()).Tables["revenue.sql"].DraftID; draft != "" {
		t.Fatalf("the pointer still names a draft the server 404'd: %q — the next publish commits a row that is gone", draft)
	}
}

// TestDocsResultTellsNoReportFromNoColumnsSent: api.UpdateTableDocs returns a nil
// join report both for an older instance and for a body with no `fields`, so the
// column count must come from the file when no report came back.
func TestDocsResultTellsNoReportFromNoColumnsSent(t *testing.T) {
	columnsOnly := tabledocs.Docs{Columns: map[string]string{
		"id": "the line's own id", "total": "SEK, ex VAT",
	}}
	got := docsResultFrom(columnsOnly, nil, nil)
	if got.ColumnsWritten != 2 {
		t.Fatalf("columnsWritten = %d, want 2 — the file is the only thing that still knows", got.ColumnsWritten)
	}
	line := describeDocsResult(got)
	if !strings.Contains(line, "2 column(s) written") {
		t.Errorf("a push that wrote prose printed %q", line)
	}
	// A report that came back is authoritative; no count is invented beside it.
	withReport := docsResultFrom(columnsOnly, &api.ColumnDocReport{AttachedColumns: []string{"id"}}, nil)
	if withReport.ColumnsWritten != 0 {
		t.Errorf("columnsWritten was set although the server reported: %+v", withReport)
	}
	if line := describeDocsResult(docsResultFrom(tabledocs.Docs{}, nil, nil)); line != "" {
		t.Errorf("an empty carrier printed %q", line)
	}
}
