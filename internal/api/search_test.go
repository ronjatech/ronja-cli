package api

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

const serverHitFile = "backend/lib/search/hit.go"

var serverJSONTag = regexp.MustCompile("`[^`]*json:\"([^\",]+)")

// cli/ does not import the backend, so this reads the server's SOURCE (as
// markers/parity_test.go does): a renamed `title` tag would decode as empty and
// make every `ronja bind` report "no match", which a hand-written decode fixture
// cannot catch. Only the tags SearchHit reads are asserted; it skips when the
// backend tree is absent and always runs in this repository.
func TestSearchHitMirrorsTheServerTags(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("backend tree not present — nothing to check the mirror against")
	}
	body, err := os.ReadFile(filepath.Join(root, serverHitFile))
	if err != nil {
		t.Fatalf("read %s: %v", serverHitFile, err)
	}
	server := map[string]bool{}
	for _, m := range serverJSONTag.FindAllStringSubmatch(string(body), -1) {
		server[m[1]] = true
	}
	if len(server) == 0 {
		t.Fatalf("found no json tags in %s — the extractor has drifted, not the shape", serverHitFile)
	}

	mirror := reflect.TypeOf(SearchHit{})
	for i := 0; i < mirror.NumField(); i++ {
		field := mirror.Field(i)
		tag := field.Tag.Get("json")
		if !server[tag] {
			t.Errorf("SearchHit.%s decodes %q, and %s no longer serializes a field by that name — the field would decode as its zero value, silently. Re-read search.Hit and follow the rename.",
				field.Name, tag, serverHitFile)
		}
	}
}

// repoRoot returns the monorepo root, or "" when there is none. It duplicates
// markers/parity_test.go's helper deliberately: cli/ has no test-support package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "backend", "lib", "search")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
