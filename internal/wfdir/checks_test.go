package wfdir

import (
	"os"
	"strings"
	"testing"
)

func TestChecksStem(t *testing.T) {
	if stem, ok := ChecksStem("checks/gl_lines.json"); !ok || stem != "gl_lines" {
		t.Errorf("ChecksStem = %q, %v", stem, ok)
	}
	for _, p := range []string{"checks/a/b.json", "tables/gl_lines.json", "checks/.json", "checks/x.txt"} {
		if _, ok := ChecksStem(p); ok {
			t.Errorf("%s read as a checks file", p)
		}
	}
	if ChecksPath("gl_lines") != "checks/gl_lines.json" {
		t.Error("ChecksPath")
	}
}

// TestLockChecksPreservesUnknownKeys: the sidecars reach both new nesting levels
// of the committed lock — the checks file's entry and each managed check — and
// a table rebind keeps nothing.
func TestLockChecksPreservesUnknownKeys(t *testing.T) {
	root := t.TempDir()
	body := `{"stacks":{"dev":{"checks":{"checks/gl.json":{"tableID":"table-gl","future":1,"managed":{"unik rad":{"checkID":"tcheck-1","liveSHA256":"abc","later":true}}}}}}}` + "\n"
	write(t, root, LockName, body)
	lock, err := LoadLock(root)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	tableID, managed := lock.ChecksSeen("dev", "checks/gl.json")
	if tableID != "table-gl" || managed["unik rad"].CheckID != "tcheck-1" || managed["unik rad"].LiveSHA256 != "abc" {
		t.Fatalf("ChecksSeen = %q %+v", tableID, managed)
	}

	lock.SetChecksSeen("dev", "checks/gl.json", "table-gl", map[string]LockCheckEntry{
		"unik rad": {CheckID: "tcheck-1", LiveSHA256: "def"},
		"ny":       {CheckID: "tcheck-2", LiveSHA256: "ghi"},
	})
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, _ := os.ReadFile(LockPath(root))
	for _, want := range []string{`"future"`, `"later"`, `"liveSHA256": "def"`, `"tcheck-2"`} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("missing %s after a re-recording:\n%s", want, written)
		}
	}

	lock.SetChecksSeen("dev", "checks/gl.json", "table-other", nil)
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, _ = os.ReadFile(LockPath(root))
	if strings.Contains(string(written), `"future"`) || strings.Contains(string(written), `"tcheck-1"`) {
		t.Fatalf("a rebind kept the old table's recordings:\n%s", written)
	}

	empty := &Lock{}
	empty.SetChecksSeen("", "checks/gl.json", "table-gl", nil)
	if len(empty.Stacks) != 0 {
		t.Errorf("an empty stack name wrote %+v", empty.Stacks)
	}
	var none *Lock
	if id, m := none.ChecksSeen("dev", "checks/gl.json"); id != "" || m != nil {
		t.Error("a nil lock must answer nothing")
	}
}

// TestLockChecksPathsAndDrop: a push finds a deleted checks file's record by
// listing the stack's recorded paths, and drops the record once it owns nothing
// still enabled — without touching another stack's.
func TestLockChecksPathsAndDrop(t *testing.T) {
	lock := &Lock{}
	lock.SetChecksSeen("dev", "checks/b.json", "table-b", map[string]LockCheckEntry{"x": {CheckID: "c1"}})
	lock.SetChecksSeen("dev", "checks/a.json", "table-a", nil)
	lock.SetChecksSeen("prod", "checks/a.json", "table-pa", nil)
	if got := lock.ChecksPaths("dev"); strings.Join(got, ",") != "checks/a.json,checks/b.json" {
		t.Fatalf("ChecksPaths = %v", got)
	}
	lock.DropChecksSeen("dev", "checks/a.json")
	lock.DropChecksSeen("dev", "checks/b.json")
	if got := lock.ChecksPaths("dev"); len(got) != 0 || lock.Stacks["dev"].Checks != nil {
		t.Fatalf("after drop: %v %+v", got, lock.Stacks["dev"].Checks)
	}
	if id, _ := lock.ChecksSeen("prod", "checks/a.json"); id != "table-pa" {
		t.Error("a drop on one stack touched another")
	}
	var none *Lock
	none.DropChecksSeen("dev", "checks/a.json")
	if none.ChecksPaths("dev") != nil {
		t.Error("a nil lock must answer nothing")
	}
}

// TestStateChecksRoundTrip: a LEGACY folder keeps its checks record in
// .ronja/state.json, and it survives a save and a load intact.
func TestStateChecksRoundTrip(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://x.example", TenantID: "t1"}
	state := &State{}
	state.Set(key, &InstanceState{Checks: map[string]ChecksState{
		"checks/gl.json": {TableID: "table-gl", Managed: map[string]LockCheckEntry{"unik": {CheckID: "c1", LiveSHA256: "abc"}}},
	}})
	if err := SaveState(root, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.For(key).Checks["checks/gl.json"]
	if got.TableID != "table-gl" || got.Managed["unik"].CheckID != "c1" || got.Managed["unik"].LiveSHA256 != "abc" {
		t.Fatalf("round trip = %+v", got)
	}
}
