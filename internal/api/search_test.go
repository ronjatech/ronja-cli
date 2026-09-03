package api

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// serverHitFile is where search.Hit really lives, relative to the repository
// root. It is the type SearchHit hand-mirrors four fields of.
const serverHitFile = "backend/lib/search/hit.go"

// serverJSONTag pulls the `json:"name"` out of a struct field declaration, so
// the comparison is against the tags the server actually serializes with rather
// than against a copy in a comment.
var serverJSONTag = regexp.MustCompile("`[^`]*json:\"([^\",]+)")

// TestSearchHitMirrorsTheServerTags is the mechanical half of the promise
// SearchHit's doc makes, on the pattern markers/parity_test.go established one
// package over: cli/ is a separate Go module that deliberately does not import
// the backend, so a hand-mirror has nothing to check itself against in the type
// system, and the check has to read the server's SOURCE.
//
// It is worth the oddity because the drift is SILENT and lands somewhere that
// looks like a data problem. `ronja bind` matches a hit on its TITLE — that
// check is load-bearing rather than defensive, since the endpoint's tag leg
// stamps a matching tag's score onto a resource whatever it is called — so a
// renamed `title` tag decodes as the empty string, matches no alias, and every
// bind reports "no match" for rows sitting right there. The decode test beside
// this one cannot see it: its JSON body is hand-written, so it renames right
// along with the mirror and stays green.
//
// Only the four tags SearchHit reads are asserted. The seven it leaves out are
// left out deliberately (see the type's doc), and requiring them here would turn
// every field the server adds into a failure in the CLI's test suite.
//
// SKIPS when the backend tree is not there — an installed module, or the CLI
// vendored on its own — exactly as the markers parity test does, because the
// absence of the file says nothing about whether the shapes agree. In this
// repository, and therefore in CI, it always runs.
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

// repoRoot walks up from the test's own directory looking for the monorepo root,
// identified by the backend tree this test reads. Returns "" when there is none.
//
// A second copy of markers/parity_test.go's helper rather than a shared one:
// cli/ has no test-support package, and inventing one so two files can agree on
// four lines of os.Stat would be the larger change. Both are test-only.
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
