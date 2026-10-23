package wfdir

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestNewFolderIDIsTheServersCanonicalForm(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		id, err := NewFolderID()
		if err != nil {
			t.Fatal(err)
		}
		if !folderIDRE.MatchString(id) || seen[id] {
			t.Fatalf("minted %q", id)
		}
		seen[id] = true
	}
}

// A folder id is read lowercased (the server's spelling — two spellings would
// be two addresses), and anything that is not a UUID refuses the folder. So does
// an id in a file that declares a format below 6, naming the line to add.
func TestLoadManifestNormalizesAndValidatesTheFolderID(t *testing.T) {
	root := t.TempDir()
	write := func(version, id string) {
		body := `{"formatVersion": ` + version + `, "kind": "workflow", "id": "` + id + `", "title": "x", "instances": []}` + "\n"
		if err := os.WriteFile(ManifestPath(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("6", "0B6F7C2E-3A41-4F0E-9D4C-7A5E2B9C1D08")
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08" || !m.Adopted() {
		t.Fatalf("id = %q", m.ID)
	}
	write("5", "not-a-uuid")
	if _, err := LoadManifest(root, WorkflowKind); err == nil || !strings.Contains(err.Error(), `"id"`) {
		t.Fatalf("a malformed id loaded: %v", err)
	}
	write("5", "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08")
	if _, err := LoadManifest(root, WorkflowKind); err == nil || !strings.Contains(err.Error(), `Add "formatVersion": 6`) {
		t.Fatalf("an id under format 5 loaded: %v", err)
	} else if strings.Contains(err.Error(), "sync adopt") {
		// `sync adopt` loads this same file and is refused by this same
		// check, so naming it as the remedy would loop.
		t.Fatalf("the refusal names a command it also refuses: %v", err)
	}
}

// The id makes the file format version 6 — an older CLI would run an adopted
// folder off a lock the team stopped committing and create second rows — and
// `sync adopt` writing it into an existing file stamps the version and puts the
// id beside the kind, every other key where it was. A folder with no id is
// untouched by any of it.
func TestFolderIDStampsItsFormatVersionAndSitsBesideTheKind(t *testing.T) {
	root := t.TempDir()
	body := "{\n  \"kind\": \"workflow\",\n  \"title\": \"Monthly\",\n  \"entrypoint\": \"main.py\",\n  \"instances\": []\n}\n"
	if err := os.WriteFile(ManifestPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(ManifestPath(root)); string(raw) != body {
		t.Fatalf("a folder with no id was rewritten:\n%s", raw)
	}
	m.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(ManifestPath(root))
	want := "{\n  \"formatVersion\": 6,\n  \"kind\": \"workflow\",\n  \"id\": \"0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08\",\n  \"title\": \"Monthly\",\n  \"entrypoint\": \"main.py\",\n  \"instances\": []\n}\n"
	if string(raw) != want {
		t.Fatalf("manifest =\n%s\nwant\n%s", raw, want)
	}
	m, err = LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("the stamped file does not load: %v", err)
	}
	// Taking the id out never lowers the number.
	m.ID = ""
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(ManifestPath(root)); !strings.Contains(string(raw), `"formatVersion": 6`) {
		t.Fatalf("removing the id lowered the version:\n%s", raw)
	}
}

// Anchor and SetManaged are one translation in two directions: what the lock
// records about a row is what `applied` carries, and writing it back is the
// same entry.
func TestAnchorRoundTripsThroughApplied(t *testing.T) {
	l := &Lock{}
	l.SetTableLive("prod", "orders.sql", "table-1", "sha-live")
	l.SetTableMeta("prod", "orders.sql", "table-1", "sha-meta")
	l.SetTableName("prod", "orders.sql", "table-1", "Sales orders")
	anchor := l.Anchor("prod", PipelineKind, "orders.sql")
	raw, _ := json.Marshal(anchor)
	if string(raw) != `{"liveSHA256":"sha-live","metaSHA256":"sha-meta","name":"Sales orders"}` {
		t.Fatalf("anchor = %s", raw)
	}
	fresh := &Lock{}
	if err := fresh.SetManaged("prod", PipelineKind, "orders.sql", "table-1", anchor); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Stacks["prod"].Tables["orders.sql"]; got.TableID != "table-1" || got.LiveSHA256 != "sha-live" || got.Name != "Sales orders" {
		t.Fatalf("entry = %+v", got)
	}
	// A different row clears the anchor (nil keeps nothing of another row's).
	if err := fresh.SetManaged("prod", PipelineKind, "orders.sql", "table-2", nil); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Stacks["prod"].Tables["orders.sql"]; got.TableID != "table-2" || got.LiveSHA256 != "" {
		t.Fatalf("a rebound entry kept the old row's anchor: %+v", got)
	}
	if got := fresh.ManagedAddresses("prod", PipelineKind); len(got) != 1 || got[0].Kind != ManagedTable {
		t.Fatalf("addresses = %+v", got)
	}
	if ManagedKindAt(PipelineKind, "metrics/aov.json") != ManagedMetric || ManagedKindAt(WorkflowKind, "x.sql") != "" {
		t.Fatal("ManagedKindAt")
	}
}

// A table's claim anchor carries the checks record of its stem; the docs
// sidecars ride on NO claim — they are the folder anchor's — and a stale lock
// meets the server's records path by path.
func TestTheFolderAnchorCarriesTheChecksAndTheDocsSidecars(t *testing.T) {
	l := &Lock{}
	l.SetTableLive("prod", "orders.sql", "table-1", "sha-live")
	l.SetTableLive("prod", "refunds.sql", "table-2", "sha-r")
	l.SetChecksSeen("prod", "checks/orders.json", "table-1", map[string]LockCheckEntry{"unik": {CheckID: "tcheck-1", LiveSHA256: "sha-check"}})
	l.SetTableDocsSeen("prod", "tables/raw.json", "table-raw", "sha-meta", "sha-declared")

	for _, addr := range []string{"orders.sql", "refunds.sql"} {
		if a := l.Anchor("prod", PipelineKind, addr); a[LegacyDocsAnchorKey] != nil {
			t.Fatalf("a claim anchor carried the docs sidecars: %s %v", addr, a)
		}
	}
	for _, addr := range []string{"orders.sql", "refunds.sql"} {
		if a := l.Anchor("prod", PipelineKind, addr); a[LegacyChecksAnchorKey] != nil {
			t.Fatalf("a claim anchor carried a checks record: %s %v", addr, a)
		}
	}
	folder := l.FolderAnchor("prod")
	if folder["tableDocs"] == nil || folder["checks"] == nil {
		t.Fatalf("folder anchor = %v", folder)
	}
	fresh := &Lock{}
	for addr, id := range map[string]string{"orders.sql": "table-1", "refunds.sql": "table-2"} {
		if err := fresh.SetManaged("prod", PipelineKind, addr, id, l.Anchor("prod", PipelineKind, addr)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fresh.SetFolderAnchor("prod", folder); err != nil {
		t.Fatal(err)
	}
	if id, managed := fresh.ChecksSeen("prod", "checks/orders.json"); id != "table-1" || managed["unik"].LiveSHA256 != "sha-check" {
		t.Fatalf("checks = %s %+v", id, managed)
	}
	if id, meta, declared := fresh.TableDocsSeen("prod", "tables/raw.json"); id != "table-raw" || meta != "sha-meta" || declared != "sha-declared" {
		t.Fatalf("docs = %s %s %s", id, meta, declared)
	}
	// A claim anchor with no checks record leaves the lock's alone: the record
	// lives on the folder anchor, and one the server lacks is carried up by the
	// next anchor write, as a sidecar's is.
	if err := fresh.SetManaged("prod", PipelineKind, "orders.sql", "table-1", map[string]any{"liveSHA256": "sha-live"}); err != nil {
		t.Fatal(err)
	}
	if id, _ := fresh.ChecksSeen("prod", "checks/orders.json"); id != "table-1" {
		t.Fatalf("a claim anchor without checks dropped the lock's record: %q", id)
	}
	// An older build's claim anchor carried the record itself: read as a
	// fallback, and the folder anchor's record for the same file wins over it.
	older := &Lock{}
	legacyClaim := map[string]any{"liveSHA256": "sha-live", LegacyChecksAnchorKey: map[string]any{
		"tableID": "table-1", "managed": map[string]any{"unik": map[string]any{"checkID": "tcheck-1", "liveSHA256": "sha-old"}}}}
	if err := older.SetManaged("prod", PipelineKind, "orders.sql", "table-1", legacyClaim); err != nil {
		t.Fatal(err)
	}
	if _, managed := older.ChecksSeen("prod", "checks/orders.json"); managed["unik"].LiveSHA256 != "sha-old" {
		t.Fatalf("the older claim anchor's checks record was not read: %+v", managed)
	}
	if err := older.SetFolderAnchor("prod", folder); err != nil {
		t.Fatal(err)
	}
	if _, managed := older.ChecksSeen("prod", "checks/orders.json"); managed["unik"].LiveSHA256 != "sha-check" {
		t.Fatalf("the folder anchor's checks record lost to the claim's: %+v", managed)
	}
	// A stale lock: the server wins for the path it names, and a path only the
	// lock records is kept for the next anchor write to carry up.
	stale := &Lock{}
	stale.SetTableDocsSeen("prod", "tables/raw.json", "table-raw", "sha-old", "sha-old-declared")
	stale.SetTableDocsSeen("prod", "tables/only-here.json", "table-x", "sha-x", "sha-x-declared")
	if err := stale.SetFolderAnchor("prod", folder); err != nil {
		t.Fatal(err)
	}
	if _, meta, _ := stale.TableDocsSeen("prod", "tables/raw.json"); meta != "sha-meta" {
		t.Fatalf("the server's record lost to a stale lock: %s", meta)
	}
	if id, _, _ := stale.TableDocsSeen("prod", "tables/only-here.json"); id != "table-x" {
		t.Fatal("a record the server lacks was dropped from the lock")
	}
	// An older claim anchor's records still decode, for the one-time fallback.
	legacy, err := TableDocsIn(map[string]any{"liveSHA256": "x", LegacyDocsAnchorKey: folder["tableDocs"]})
	if err != nil || legacy["tables/raw.json"].MetaSHA256 != "sha-meta" {
		t.Fatalf("legacy = %+v, %v", legacy, err)
	}
	if got := RetiredAnchorKeys(ManagedTable); len(got) != 2 || got[0] != LegacyChecksAnchorKey || got[1] != LegacyDocsAnchorKey {
		t.Fatalf("retired = %v", got)
	}
}
