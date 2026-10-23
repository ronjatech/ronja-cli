package wfdir

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// loadDeclared writes a manifest of the given kind with the given extra keys
// and loads it, returning LoadManifest's answer.
func loadDeclared(t *testing.T, kind Kind, extra string) (*Manifest, error) {
	t.Helper()
	root := t.TempDir()
	body := `{"formatVersion": 5, "kind": "` + kind.Name + `", "title": "x", "instances": []` + extra + `}`
	write(t, root, ManifestName, body)
	return LoadManifest(root, kind)
}

// Every declaration that contradicts ITSELF is refused at load, with a sentence
// naming what is wrong — none of these can be applied by any stack.
func TestAddressDeclarationsRefuseWhatContradictsItself(t *testing.T) {
	cases := []struct {
		name  string
		kind  Kind
		extra string
		want  string
	}{
		{"unknown key in an entry", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql", "stack": "dev"}]`, `key "stack"`},
		{"entry not an object", PipelineKind, `, "moved": ["a.sql"]`, "is an object"},
		{"entry missing to", PipelineKind, `, "moved": [{"from": "a.sql"}]`, `no "to"`},
		{"entry path not a string", PipelineKind, `, "moved": [{"from": "a.sql", "to": 3}]`, "path string"},
		{"removed not strings", PipelineKind, `, "removed": [{"path": "a.sql"}]`, "removed"},
		{"a folder kind", WorkflowKind, `, "moved": [{"from": "a.py", "to": "b.py"}]`, "ONE resource"},
		{"removed in a data app", DataAppKind, `, "removed": ["App.tsx"]`, "ONE resource"},
		{"not an address", PipelineKind, `, "moved": [{"from": "README.md", "to": "b.sql"}]`, "not a file this pipeline folder builds"},
		{"a checks file", PipelineKind, `, "removed": ["checks/orders.json"]`, "not a file this pipeline folder builds"},
		{"not plain", PipelineKind, `, "moved": [{"from": "./a.sql", "to": "b.sql"}]`, "plain form"},
		{"escapes the folder", PipelineKind, `, "removed": ["../a.sql"]`, "plain form"},
		{"absolute", PipelineKind, `, "removed": ["/a.sql"]`, "absolute"},
		{"onto itself", PipelineKind, `, "moved": [{"from": "a.sql", "to": "a.sql"}]`, "onto itself"},
		{"across kinds", PipelineKind, `, "moved": [{"from": "a.sql", "to": "metrics/a.json"}]`, "cannot turn one kind"},
		{"two moves of one file", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql"}, {"from": "a.sql", "to": "c.sql"}]`, "both move"},
		{"two moves onto one file", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "c.sql"}, {"from": "b.sql", "to": "c.sql"}]`, "both move a file to"},
		{"a cycle", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql"}, {"from": "b.sql", "to": "a.sql"}]`, "cycle (a.sql → b.sql → a.sql)"},
		{"a cycle names the remedy", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql"}, {"from": "b.sql", "to": "a.sql"}]`,
			`If you renamed a file back or reused a path, delete entry 1 once ronja.json no longer binds "a.sql"`},
		{"a three-step cycle", AutomationKind,
			`, "moved": [{"from": "a.json", "to": "b.json"}, {"from": "b.json", "to": "c.json"}, {"from": "c.json", "to": "a.json"}]`, "cycle"},
		{"a chain listed backwards", PipelineKind,
			`, "moved": [{"from": "b.sql", "to": "c.sql"}, {"from": "a.sql", "to": "b.sql"}]`, "list them in the order they happened (entry 2 first)"},
		// A REUSED path reads as a backwards chain too, and the refusal says how
		// to reuse it: [{A,B},{C,A}] is "A was renamed to B, and later C took
		// A's old name" — listing entry 2 first would chain C → A → B instead.
		{"a reused path", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql"}, {"from": "c.sql", "to": "a.sql"}]`,
			`if entry 2 reuses the path for a different file, delete entry 1 once ronja.json no longer binds "a.sql"`},
		{"removed twice", PipelineKind, `, "removed": ["a.sql", "a.sql"]`, "twice"},
		{"moved and removed", PipelineKind,
			`, "moved": [{"from": "a.sql", "to": "b.sql"}], "removed": ["a.sql"]`, "both moved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadDeclared(t, tc.kind, tc.extra)
			if err == nil {
				t.Fatalf("loaded %s", tc.extra)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// A folder that names STACKS keeps its rows in ronja.lock.json, and one that
// names none keeps them in ronja.json: the reuse remedy names the file the
// folder actually has (the cases above are stackless).
func TestAddressReuseRemedyNamesTheLockOnAStack(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"formatVersion": 5, "kind": "pipeline", "title": "x",
		"stacks": {"prod": {"url": "https://ronja.example", "tenantID": "ten-1", "featureID": "collection-1"}},
		"moved": [{"from": "a.sql", "to": "b.sql"}, {"from": "b.sql", "to": "a.sql"}]}`)
	_, err := LoadManifest(root, PipelineKind)
	if err == nil || !strings.Contains(err.Error(), `delete entry 1 once no stack in ronja.lock.json still binds "a.sql"`) {
		t.Errorf("refusal = %v", err)
	}
}

// What a folder legitimately declares loads, unchanged — a chain in order, a
// metric move, nested .sql files, an automation move, removals, and the removal
// of a path an earlier rename moved a file to.
func TestAddressDeclarationsAcceptAWellFormedList(t *testing.T) {
	m, err := loadDeclared(t, PipelineKind, `, "moved": [
		{"from": "orders.sql", "to": "staging/orders.sql"},
		{"from": "staging/orders.sql", "to": "sales_orders.sql"},
		{"from": "metrics/aov.json", "to": "metrics/average_order.json"}
	], "removed": ["legacy_rollup.sql", "metrics/old.json"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Moved) != 3 || m.Moved[1].From != "staging/orders.sql" || len(m.Removed) != 2 {
		t.Errorf("decoded %+v / %+v", m.Moved, m.Removed)
	}
	if _, err := loadDeclared(t, AutomationKind, `, "moved": [{"from": "nightly.json", "to": "jobs/nightly.json"}], "removed": ["weekly.json"]`); err != nil {
		t.Errorf("automation: %v", err)
	}
	// Rename then retire: a move's destination removed later.
	if _, err := loadDeclared(t, PipelineKind, `, "moved": [{"from": "orders.sql", "to": "sales.sql"}], "removed": ["sales.sql"]`); err != nil {
		t.Errorf("rename then retire: %v", err)
	}
}

// `moved` makes the file version 5 — the stamp an older CLI needs to refuse it
// rather than create a second row — and in a pipeline folder `removed` alone
// does not, because an older CLI ignoring it does exactly what it asks.
func TestMovedStampsItsFormatVersionAndRemovedDoesNot(t *testing.T) {
	m := Manifest{Kind: KindPipeline, Title: "x", Moved: []MovedEntry{{From: "a.sql", To: "b.sql"}}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"formatVersion":5`) {
		t.Errorf("moved was written without format 5: %s", body)
	}
	if !strings.Contains(string(body), `"moved":[{"from":"a.sql","to":"b.sql"}]`) {
		t.Errorf("moved did not round-trip: %s", body)
	}
	m = Manifest{Kind: KindPipeline, Title: "x", Removed: []string{"a.sql"}}
	if body, _ = json.Marshal(m); strings.Contains(string(body), "formatVersion") {
		t.Errorf("removed alone stamped a version: %s", body)
	}

	hand := Manifest{Kind: KindPipeline, Title: "x", FormatVersion: 3, Moved: []MovedEntry{{From: "a.sql", To: "b.sql"}}}
	if err := hand.CheckAddressFormat(); err == nil || !strings.Contains(err.Error(), `Add "formatVersion": 5`) {
		t.Errorf("CheckAddressFormat = %v", err)
	}
	hand.FormatVersion = 5
	if err := hand.CheckAddressFormat(); err != nil {
		t.Errorf("CheckAddressFormat at 5 = %v", err)
	}
}

// In an AUTOMATION folder `removed` makes the file version 5 too. An older CLI
// ignoring it does not leave the row alone there: it refuses the bound file as
// an orphan and tells the reader to run `ronja automation push --prune`, which
// soft-deletes and pauses the very automation `removed` says to keep running.
// So the file has to make that CLI refuse it by version instead.
func TestRemovedInAnAutomationFolderNeedsTheMovesFormatVersion(t *testing.T) {
	m := Manifest{Kind: KindAutomation, Title: "x", Removed: []string{"nightly.json"}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"formatVersion":5`) {
		t.Errorf("removed in an automation folder was written without format 5: %s", body)
	}
	hand := Manifest{Kind: KindAutomation, Title: "x", FormatVersion: 2, Removed: []string{"nightly.json"}}
	if err := hand.CheckAddressFormat(); err == nil || !strings.Contains(err.Error(), `Add "formatVersion": 5`) ||
		!strings.Contains(err.Error(), "--prune") {
		t.Errorf("CheckAddressFormat = %v", err)
	}
	hand.FormatVersion = 5
	if err := hand.CheckAddressFormat(); err != nil {
		t.Errorf("CheckAddressFormat at 5 = %v", err)
	}
	pipeline := Manifest{Kind: KindPipeline, Title: "x", FormatVersion: 2, Removed: []string{"a.sql"}}
	if err := pipeline.CheckAddressFormat(); err != nil {
		t.Errorf("removed in a pipeline folder was refused: %v", err)
	}
}

// A CLI that reads only format 4 refuses a file carrying `moved` — the version
// gate is what stops it creating a second row. Exercised on this build by
// pretending to be one.
func TestAnOlderReaderRefusesAMovedFolder(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Kind: KindPipeline, Title: "x", Moved: []MovedEntry{{From: "a.sql", To: "b.sql"}}}
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkFormatVersion(ManifestPath(root), raw, manifestVersionLanguage); err == nil {
		t.Error("a format-4 reader would have opened a folder that declares a move")
	}
}

// RekeyPath carries EVERYTHING a stack records under the old path — id,
// fingerprints, anchor, recorded name, tags — and never overwrites; DropPath
// forgets it.
func TestLockRekeyAndDropPath(t *testing.T) {
	l := &Lock{}
	l.SetBinding("prod", Binding{Tables: map[string]string{"a.sql": "table-a"}})
	l.SetTableLive("prod", "a.sql", "table-a", "live-sha")
	l.SetTableMeta("prod", "a.sql", "table-a", "meta-sha")
	l.SetTableName("prod", "a.sql", "table-a", "Orders")
	l.SetMetricSeen("prod", "metrics/m.json", "metric-1", "l", "d")
	l.SetMetricTagsApplied("prod", "metrics/m.json", "metric-1", []AppliedTag{{ID: "tag-1", Name: "Finance"}})

	if !l.RekeyPath("prod", "a.sql", "b.sql") {
		t.Fatal("nothing moved")
	}
	got := l.Stacks["prod"].Tables["b.sql"]
	if got.TableID != "table-a" || got.LiveSHA256 != "live-sha" || got.MetaSHA256 != "meta-sha" || got.Name != "Orders" {
		t.Errorf("moved entry = %+v", got)
	}
	if _, left := l.Stacks["prod"].Tables["a.sql"]; left {
		t.Error("the old path is still recorded")
	}
	l.RekeyPath("prod", "metrics/m.json", "metrics/n.json")
	if id, applied, _ := l.MetricTags("prod", "metrics/n.json"); id != "metric-1" || len(applied) != 1 {
		t.Errorf("metric tags did not travel: %q %v", id, applied)
	}

	// Never onto an occupied address.
	l.SetBinding("prod", Binding{Tables: map[string]string{"b.sql": "table-a", "c.sql": "table-c"}})
	if l.RekeyPath("prod", "b.sql", "c.sql") {
		t.Error("RekeyPath overwrote an occupied address")
	}
	if l.Stacks["prod"].Tables["c.sql"].TableID != "table-c" {
		t.Error("the occupied address changed")
	}

	if !l.DropPath("prod", "b.sql") || l.Stacks["prod"].Tables["b.sql"].TableID != "" {
		t.Error("DropPath left the entry")
	}
	if l.DropPath("nope", "c.sql") {
		t.Error("DropPath on an unknown stack reported a drop")
	}
}

// checkAddressPath is the plain-form gate on its own, not leaning on the
// extension check after it: a bare ".." is refused here.
func TestCheckAddressPathRefusesTheParentDirectory(t *testing.T) {
	for _, p := range []string{"..", "../a.sql", ".", "./a.sql", "a//b.sql"} {
		if err := checkAddressPath(p); err == nil {
			t.Errorf("checkAddressPath(%q) accepted it", p)
		}
	}
	if err := checkAddressPath("staging/orders.sql"); err != nil {
		t.Errorf("checkAddressPath(staging/orders.sql) = %v", err)
	}
}
