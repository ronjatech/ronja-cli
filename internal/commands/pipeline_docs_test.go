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

// writeSidecar puts a docs sidecar into a pipeline folder.
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
// way `ronja bind` does. It is what makes one committed folder deployable to
// several organizations, and it is how a docs sidecar names its table.
func declareTableDependency(t *testing.T, root string, key wfdir.InstanceKey, alias, id string) {
	t.Helper()
	manifest := pipelineManifestOf(t, root)
	if manifest.Dependencies == nil {
		manifest.Dependencies = map[string]wfdir.Dependency{}
	}
	manifest.Dependencies[alias] = wfdir.Dependency{Kind: "table"}
	// The bind map lives on the ENTRY, not on the embedded Binding — see
	// wfdir.Instance.Bind — so it is written in place rather than through
	// SetBinding, which replaces an entry wholesale.
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

// TestPipelinePushSendsTheHeaderAsDocumentation is the whole wire contract for
// the .sql carrier: what goes on the CREATE, what goes on the draft PUT, and in
// what ORDER.
func TestPipelinePushSendsTheHeaderAsDocumentation(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// The columns the table has MEASURED. `margin` is deliberately not among
	// them — the join rule's right-hand side, and the case the report exists for.
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

	// TWO PUTs, and the SECOND is the documentation. The order is the rule: the
	// join rule attaches prose to the columns the table has MEASURED, so the
	// documentation must follow the build.
	if len(f.updates) != 2 {
		t.Fatalf("want a code PUT then a docs PUT, got %+v", f.updates)
	}
	code, docs := f.updates[0], f.updates[1]
	if !code.SetsCode || code.Fields != nil || code.Description != nil {
		t.Errorf("the code PUT must carry no documentation: %+v", code)
	}
	// THE DOCS PUT NAMES NO `code` AND NO `inputModels`. Every field of the
	// server's patch is an optional, so an absent key leaves the column alone —
	// and a body that sent `"code": ""` would wipe the SQL this push just wrote.
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

	// THE REPORT, read back from the server rather than predicted.
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

// TestPipelinePushWithoutAHeaderSendsNoDocumentation: opt-in is the whole design.
// A folder that has not adopted the header must make byte-identical requests to
// the ones it made before this feature existed.
func TestPipelinePushWithoutAHeaderSendsNoDocumentation(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A leading comment that does NOT open with @table. Ordinary commentary.
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
	// And nothing is recorded, so the guard it would arm does not exist for this
	// folder either.
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

// TestPipelinePushRefusesWhenTheDocumentationMoved is the third drift leg.
//
// It is a SEPARATE leg from the SQL fingerprint on purpose: the file's SQL is
// untouched here, so a guard that only watched `code` would let this push
// silently overwrite an admin's prose at the next publish.
func TestPipelinePushRefusesWhenTheDocumentationMoved(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id", "total"}
	documented := "-- @table Ours.\n-- @column id: our prose\nSELECT * FROM {{ ref('table-orders') }}"
	editFile(t, root, "revenue.sql", documented)

	// First push: records the agreement.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	if lockTableMetaOf(t, root, f, "revenue.sql") == "" {
		t.Fatal("the first documenting push recorded no fingerprint, so the guard is disarmed for ever")
	}

	// Somebody rewrites the LIVE table's description in the web app. The SQL is
	// untouched.
	f.mu.Lock()
	f.tables["table-revenue"].Description = "somebody else wrote this"
	f.mu.Unlock()

	// A push of the same file — edited, so there is something to push.
	editFile(t, root, "revenue.sql", documented+"\n-- trailing edit")
	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("want a refusal, got a clean push")
	}
	if !strings.Contains(stderr, "documentation") {
		t.Fatalf("the refusal must name what moved: %s", stderr)
	}
	// And it must not say the SQL moved. Nothing about the code changed on the
	// server, so a reader sent to look for a code difference finds none — and a
	// message that is wrong in a case people meet is one nobody reads in the
	// cases it is right.
	if strings.Contains(stderr, "the SQL of table-revenue changed") {
		t.Errorf("a documentation-only drift was announced as a SQL change: %s", stderr)
	}

	// --force overwrites, and RE-RECORDS the agreement — or the same difference
	// would be re-reported on every later push and --force would become a
	// permanent part of the command line.
	before := lockTableMetaOf(t, root, f, "revenue.sql")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force", "--json"); err != nil {
		t.Fatalf("forced push: %v\n%s", err, stderr)
	}
	if after := lockTableMetaOf(t, root, f, "revenue.sql"); after == before || after == "" {
		t.Fatalf("--force did not re-record the agreement (%q → %q)", before, after)
	}
}

// TestPipelinePushDocumentsASidecarTable is the second carrier: a table this
// folder does NOT build, named through the alias layer so one committed folder
// can deploy to several organizations.
func TestPipelinePushDocumentsASidecarTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// An INTEGRATION table: no .sql file anywhere, and never will be one.
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
	// THE JOIN RULE, and the reason a committed docs file is safe to keep in git:
	// a name the data does not have is reported, never an error.
	if got := toStrings(result["unmatchedColumns"]); len(got) != 1 || got[0] != "renamed_last_week" {
		t.Errorf("unmatchedColumns = %v", got)
	}

	// THREE-STATE: `invoice_no` was never named, so it must be untouched.
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

	// A second push writes nothing: the row already says what the file says.
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
	// And the RUN says so. `upToDate` is the one key a script watches to decide
	// whether a push changed the organization, and it is reported from the
	// outcomes rather than from the early return that only a folder with no
	// sidecars at all can now reach.
	if payload["upToDate"] != true {
		t.Errorf("a clean run over a sidecar folder reported upToDate = %v", payload["upToDate"])
	}
}

// TestPipelinePushRefusesASidecarWithNoBinding: the loop may only ever ask about
// names the folder already declares. A sidecar naming an alias nothing binds is
// refused where it is written rather than resolved by browsing the organization
// for a table with a similar name.
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

// TestPipelinePushRefusesASidecarThatDoesNotParse: an unknown key is an ERROR,
// not a dropped field. A dropped key is the failure this whole loop exists to
// remove — the file says one thing, the row keeps another, and nothing reports
// a difference.
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
	// And nothing was written: the refusal is local, before any request.
	if len(f.updates) != 0 {
		t.Errorf("an unparseable file still reached the server: %+v", f.updates)
	}
}

// TestPipelinePushRefusesASidecarWhoseTableMoved is the sidecar's drift guard,
// and --force overriding it.
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

	// Somebody documents another column in the web app, and edits the file here
	// so there is something to push.
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
	// The unmanaged column survives the overwrite: --force is permission to write
	// what this folder declares, not to blank what it does not.
	if stored["invoice_no"] != "somebody else's prose" {
		t.Errorf("--force cleared a column the file never named: %q", stored["invoice_no"])
	}
	// And the agreement is RE-RECORDED, which the next push is the only honest
	// test of: without it the same difference is reported for ever and --force
	// becomes a permanent part of the command line, which trains it past the one
	// time it means something.
	out, _, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("the push after a --force was refused again: %v", err)
	}
	entry = decodeJSON(t, out)["docs"].([]any)[0].(map[string]any)
	if entry["outcome"] != docsOutcomeUpToDate {
		t.Errorf("--force did not re-record the sidecar's agreement: %+v", entry)
	}
}

// TestASidecarThatDoesNotParseIsStillAUseOfItsDependency: one mistake, one
// message.
//
// The sidecar's FILENAME is what names the alias; its body is not consulted to
// decide that. A file whose JSON is broken already earns a refusal naming the
// file and the fix — and skipping it in the use-scan earns it a second warning
// saying the dependency it plainly names is dead config, which sends the author
// to delete a manifest entry that is perfectly correct.
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

// TestPipelineStatusReportsSidecars: `pipeline status` — and through it
// `ronja sync status`, which walks a whole tree of folders — has to understand
// the second carrier, or a repository-level gate would call a folder clean while
// a push from it would change the organization.
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
		// Non-zero is expected here only for drift; a pending sidecar is not
		// drift, so status must exit clean.
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

// TestPipelinePushNarrowsDocsToNamedPaths: `push tables/raw.json` is the obvious
// thing to type after editing one, and naming any path must narrow BOTH halves —
// a command that quietly re-pushed every other sidecar would be doing more than
// it was asked.
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
// reports every dependency a folder declares purely to document a table as dead
// config — on every push, every status and every `sync check`.
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

// toStrings decodes a --json string array.
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

// TestMetaSHA256MatchesTheRowItWasTakenFrom pins the fingerprint's one
// invariant at the boundary the CLI actually uses it across: it is computed from
// a GetTable answer's own fields, so a row read twice with nothing changed
// hashes the same, and a row whose prose moved does not.
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
	// A column with no prose contributes nothing, so a table nobody has
	// documented hashes the same however many columns it has.
	bare := &api.Table{Fields: []api.TableField{{Name: "a"}, {Name: "b"}}}
	if metaOf(bare) != tabledocs.MetaSHA256("", nil) {
		t.Fatal("undocumented columns must not move the fingerprint")
	}
}

// TestSidecarRoundTripsThroughTheLock: the recorded id is what makes the
// fingerprint comparable, and a sidecar repointed at a different table must drop
// it rather than compare one table's prose against a hash taken from another's.
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
	// And it survives a marshal/unmarshal, since the file is committed.
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

// TestPipelinePushDoesNotBankAFileWhoseDocumentationFailed is the whole reason
// the documentation write moved ABOVE the baseline.
//
// A docs_failed push has written and built the SQL, so the temptation is to call
// the file synced. Do that and the file's content hash is in the baseline, the
// bare-push selector never picks it again, and `status` reads green — the
// committed file and the row say different things for ever, silently, which is
// the one divergence this loop exists to remove. It bites hardest where it is
// most likely: a caller who may not write `fields` at all fails on EVERY
// documented file of the first push and is then never offered a retry.
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
	// The 400 arm: a bare "field not allowed: fields" says nothing about WHO may
	// do it, and that is the whole of the reader's problem.
	if !strings.Contains(file["error"].(string), "admin") {
		t.Errorf("the refusal must explain who can write column prose: %v", file["error"])
	}
	// The report must not claim what did not land. `description: true` beside
	// `error` is the report contradicting itself, and a script reading the first
	// key believes prose was written that was not.
	report := file["docs"].(map[string]any)
	if report["description"] == true {
		t.Errorf("a failed write reported a description as written: %+v", report)
	}
	// The SQL is publishable, and the summary has to say so rather than counting
	// this file among the ones that did not land.
	summary := payload["error"].(string)
	if strings.Contains(summary, "did not land,") || strings.Contains(summary, "1 of 1 file(s) did not land") {
		t.Errorf("a built, publishable file was counted as one that did not land: %q", summary)
	}
	if !strings.Contains(summary, "publishable") {
		t.Errorf("the summary must say the SQL landed: %q", summary)
	}

	// THE BASELINE IS UNMOVED, which is what makes the retry exist at all.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["revenue.sql"].SHA256 == wfdir.HashString(documented) {
		t.Fatal("the file was recorded as synced although its documentation never landed — the next bare push will skip it for ever")
	}

	// The instance is widened (or an admin runs it) and a BARE push — no paths,
	// no --force — picks the same file up and lands the prose.
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

// TestPipelinePushWritesNoDocumentationWhenTheBuildFails pins what is currently
// true by ORDER alone: the documentation write sits below the build, so a failed
// build returns before it. Unpinned, a later reshuffle would write prose onto a
// draft whose columns were never measured — every name unmatched, on a table
// whose real schema nobody has seen.
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

// TestPipelinePushRefusesASidecarForATableThisFolderBuilds: ONE TABLE, ONE
// CARRIER.
//
// The check is on the ID a sidecar resolves to, and these are the two spellings
// that reach it — neither of them collides with a filename, so the alias
// pre-flight's name check sees nothing. Documenting a table twice writes the
// header onto the draft and the sidecar onto the LIVE row; `publish` then copies
// the draft's rows over the sidecar's work, the sidecar's baseline is stale, and
// every later push refuses with drift the folder caused itself.
func TestPipelinePushRefusesASidecarForATableThisFolderBuilds(t *testing.T) {
	// A sidecar named after the literal id a sibling .sql file builds.
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

	// And the same table reached through an ALIAS, which is the spelling nothing
	// local can spot without resolving it: the name `raw` collides with no stem.
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

// TestPipelinePushDocumentsUnicodeColumnNames: a column name is matched by its
// EXACT name against a measured set, and nothing in either carrier is entitled
// to normalize one. A folded, trimmed or re-encoded name would be reported
// unmatched for ever while looking perfectly correct in the file.
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
	// Sorted by name, which is what makes two checkouts of the same folder send
	// byte-identical bodies — over code points, not over a locale's collation.
	if attached[0] != "année" || attached[1] != "belopp_kr" || attached[2] != "商品名" {
		t.Errorf("attachedColumns is not sorted by code point: %v", attached)
	}
	// And the header carrier, on the same names.
	f.mu.Lock()
	header := f.columnDocs["table-draft-1"]["année"]
	f.mu.Unlock()
	if header != "l'année" {
		t.Errorf("the header did not document a unicode column: %q", header)
	}
}

// TestPipelinePublishRecordsNoDocumentationFingerprintForAnUndocumentedFolder:
// the third drift leg arms ONLY for a folder that documents something, and that
// rule is about `publish` too.
//
// Without it every pipeline folder in existence grows a `metaSHA256` per table
// in its COMMITTED lock on its next publish — a diff in a file people review,
// for a feature nobody in that folder has adopted, arming a guard that then
// pauses a later push over prose that was never this folder's business.
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

// The other half of the same rule: a file that DOES carry a header must record
// the agreement, or the next documenting push reports this folder's own publish
// as somebody else's edit.
func TestPipelinePublishRecordsTheDocumentationFingerprintForADocumentedFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.fields["table-orders"] = []string{"id"}
	f.columnDocs["table-orders"] = map[string]string{"id": "the order's own id"}
	// The DRAFT carries the description, because that is where a push puts it and
	// the commit is what copies it onto the live row. The agreement recorded
	// below is an agreement with the row, so the fixture has to be a row that
	// really holds what the file declares — see docsLanded.
	f.tables["table-draft-5"].Description = "Orders."
	editFile(t, root, "orders.sql", "-- @table Orders.\n-- @column id: the order's own id\nSELECT staged FROM raw")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if meta := lockTableMetaOf(t, root, f, "orders.sql"); meta == "" {
		t.Fatal("a documented file recorded no agreement at the moment its prose landed on the live row")
	}
}

// TestPipelineStatusOnAFreshCloneWithASidecar is the FRESH CLONE, which is not
// an edge case: `.ronja/` is gitignored and correctly never committed, so
// `f.State.For(f.Key)` is nil for everybody who has not yet pushed from this
// checkout — and `pipeline status` hands that nil straight to liveHashes rather
// than going through pipelineBaseline the way push and publish do.
//
// The sidecar read then dereferenced it. Inside a status worker goroutine, so it
// was a raw process panic: no message, no exit code anybody could act on, and it
// fired on the first command a colleague ran after cloning a folder that keeps
// one docs file. The other two accessors on the same struct
// (InstanceState.TableStateFor, Lock.TableDocsSeen) have always answered the
// no-baseline case; these were the pair that did not.
func TestPipelineStatusOnAFreshCloneWithASidecar(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-invoices", Name: "invoices", FeatureID: "collection-1"})
	declareTableDependency(t, root, f.Key(), "invoices", "table-invoices")
	writeSidecar(t, root, "invoices", `{"description":"One row per invoice.","columns":{"amount":"SEK, ex VAT."}}`)

	// THE FRESH CLONE: everything committed is there, and nothing local is.
	if err := os.RemoveAll(filepath.Join(root, wfdir.StateDirName)); err != nil {
		t.Fatalf("remove local baseline: %v", err)
	}

	out, stderr, _ := runPipelineCLI(t, root, "pipeline", "status", "--json")
	// The exit code is deliberately not asserted — a folder with no baseline is
	// allowed to report whatever it reports. What must not happen is a panic,
	// which produces no JSON at all.
	if strings.Contains(stderr, "panic:") || strings.Contains(stderr, "nil pointer") {
		t.Fatalf("status panicked on a fresh clone carrying a docs sidecar:\n%s", stderr)
	}
	docs := decodeJSON(t, out)["remote"].(map[string]any)["docs"].([]any)
	if len(docs) != 1 {
		t.Fatalf("docs = %+v", docs)
	}
	// And it answers the right thing: no recording here yet, so nothing to
	// compare against — never "no drift", which is the answer that gets somebody
	// else's prose overwritten.
	if got := docs[0].(map[string]any)["drift"]; got != driftNoBaseline {
		t.Errorf("drift = %v, want %q on a checkout that has never synced", got, driftNoBaseline)
	}
}

// TestPipelinePublishDoesNotBankDocumentationItDidNotAchieve is the other half
// of TestPipelinePushDoesNotBankAFileWhoseDocumentationFailed, and the hole that
// one left open.
//
// Push withholds recordSynced when the prose is refused, precisely so the file
// stays re-pushable. Publish then ran in a SEPARATE PROCESS with no memory of
// that, decided the file was "documented" by parsing its header, and banked both
// the content baseline and the documentation agreement — so the file hashed
// clean, the bare push skipped it, `status` read driftNone, and the committed
// header and the row disagreed permanently. That is exactly what the drift guard
// exists to prevent, defeated by the command it is meant to protect.
func TestPipelinePublishDoesNotBankDocumentationItDidNotAchieve(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.fields["table-revenue"] = []string{"id"}
	// The non-admin's 400 on `fields`: the SQL PUT lands and builds, the
	// documenting PUT does not.
	f.failDocsPut["table-draft-1"] = 400
	documented := "-- @table One row per invoice line.\n-- @column id: the line's own id\nSELECT * FROM {{ ref('table-orders') }}"
	editFile(t, root, "revenue.sql", documented)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err == nil {
		t.Fatalf("a documentation write that did not land must exit non-zero\n%s", stderr)
	}
	// The draft holds the SQL and is publishable — which is what the push report
	// says, and what the author does next.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}

	// The commit landed. The prose never did, and the folder still has to say so.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["revenue.sql"].SHA256 == wfdir.HashString(documented) {
		t.Fatal("publish banked a file whose documentation was refused — the next bare push skips it for ever, and the committed header and the row disagree in silence")
	}
	// And the divergence is REPORTED, not merely retryable: `status` must still
	// show the file as locally changed. That is the signal a human reads, and it
	// is the one the banked baseline erased.
	//
	// (The meta fingerprint is deliberately not asserted here. Leg (c) asks "did
	// the ROW move since we agreed", and it did not — nothing landed on it — so
	// the fork-time recording push adopted is still an honest answer. The signal
	// for a .sql file's own prose is its CONTENT hash, which is what a header
	// lives inside.)
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	local := decodeJSON(t, out)["local"].(map[string]any)
	if !slices.Contains(toStrings(local["modified"]), "revenue.sql") {
		t.Fatalf("status reports nothing to do for a file whose documentation never landed: %+v", local)
	}

	// The retry is what the withheld baseline buys, so it is asserted rather
	// than assumed: the instance is widened, a BARE push — no paths, no --force
	// — picks the same file up, and the prose lands.
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

// TestPipelinePushWarnsAboutAnEmptySidecarRatherThanRefusing: a placeholder
// somebody committed and has not filled in must not break the folder.
//
// As a Problem it was a refusal, and one refused sidecar makes the whole run
// exit non-zero — so an empty `{}` failed EVERY push in that folder from then
// on, including `push one-unrelated-file.sql`. Nothing is at stake: the file
// declares nothing, so nothing is sent and nothing is overwritten.
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
	// Said out loud, which is the whole reason it is not silently skipped: a
	// committed file that does nothing is nearly always one somebody meant to
	// fill in.
	if doc["warning"] == nil || !strings.Contains(doc["warning"].(string), "documents nothing") {
		t.Errorf("warning = %v", doc["warning"])
	}
	// Answered before the network — there is nothing to compare and nothing to
	// send, so it must not cost a read of the row.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.Requests {
		if strings.Contains(request, "table-invoices") {
			t.Errorf("an empty sidecar reached the network: %v", f.Requests)
			break
		}
	}
}

// TestPipelineStatusDoesNotFailOnAnEmptySidecar is the same rule on the read
// side, and it is the one a CI gate feels: pipelineStatusVerdict scores Problem
// and Drift, so an empty sidecar reported as a Problem failed a gate that was
// asking whether the SERVER had moved.
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
	// A file that says nothing can never say something the table does not.
	if doc["pending"] == true {
		t.Errorf("an empty sidecar reported as pending: %+v", doc)
	}
	if doc["warning"] == nil {
		t.Errorf("the placeholder was not reported at all: %+v", doc)
	}
	// And the human report says it too — --json above suppresses the printer, so
	// the rendering is exercised on its own run.
	human, stderr, err := runPipelineCLI(t, root, "pipeline", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, stderr)
	}
	if !strings.Contains(human, "documents nothing") {
		t.Errorf("the human report says nothing about it: %s", human)
	}
}

// TestPipelinePushClearsTheDraftPointerWhenTheDocsPutIs404: a draft stops
// existing the moment somebody commits or discards it from the web app, and a
// 404 on the documentation PUT is that, seen through the second write.
//
// awaitBuild's 404 arm has always cleared the recorded pointer. This one did
// not, so the next `publish` went on to commit a row that is gone — and reported
// a failure about a draft id rather than about what happened.
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

// TestDocsResultTellsNoReportFromNoColumnsSent: the two empty answers are
// different facts and were reported as the same one.
//
// api.UpdateTableDocs folds an absent join report into nil — an instance older
// than the report, or a body that carried no `fields` — so a header documenting
// only columns (no `-- @table`) produced an all-zero result: no human line at
// all, and `"docs":{}` in --json, on a push that really did write prose.
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
	// A report that DID come back is authoritative; the count must not be
	// invented beside it, or the report would claim work twice.
	withReport := docsResultFrom(columnsOnly, &api.ColumnDocReport{AttachedColumns: []string{"id"}}, nil)
	if withReport.ColumnsWritten != 0 {
		t.Errorf("columnsWritten was set although the server reported: %+v", withReport)
	}
	// And a write that carried nothing still says nothing.
	if line := describeDocsResult(docsResultFrom(tabledocs.Docs{}, nil, nil)); line != "" {
		t.Errorf("an empty carrier printed %q", line)
	}
}
