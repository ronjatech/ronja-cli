package markers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// serverFiles is where each mirrored pattern really lives, relative to the
// repository root.
//
// A pattern is looked for in every file rather than in one named per family,
// because which file a marker's regex lives in is the server's business and has
// moved before (`ref` sits in esql, the rest in pymarkers). What must not move
// is the pattern.
var serverFiles = []string{
	"backend/engine/esql/esql.go",
	"backend/engine/pymarkers/markers.go",
	// The malformed-argument rule keeps pymarkers' OWN copy of the `{{ ref }}`
	// regex, because that package is a leaf and cannot import esql for it. Its
	// own comment says the two must stay identical, and listing the file here is
	// what makes something enforce that — see TestServerRefPatternCopiesAgree.
	"backend/engine/pymarkers/malformed.go",
}

// serverPatterns is every package-level `<name> = regexp.MustCompile("…")` in
// one server file, name → the pattern body the server actually compiles.
//
// PARSED rather than matched with a regex of our own, and the difference is a
// hole rather than a refinement. The regex this replaced pinned `^var name =`,
// which sees a single top-level declaration and nothing else — so a family moved
// into a grouped `var ( … )` block, which is an ordinary tidy-up nobody would
// think to mention, becomes INVISIBLE to both tests at once. Parity would stop
// checking it silently (the pattern is simply absent from the map), and
// TestEveryServerMarkerFamilyIsMirroredOrExcluded could not catch that either:
// its `found == 0` guard is about the extractor breaking COMPLETELY, and the
// other fourteen declarations keep it satisfied while one family goes unchecked.
//
// go/parser is stdlib, so this costs the module nothing, and it reads the same
// declarations the compiler does — grouped, single, `var` or `const` — while
// still ignoring anything inside a function body, which is where a pattern that
// is not a package-level family would live.
func serverPatterns(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.VAR && gen.Tok != token.CONST) {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				if body, ok := mustCompileArg(value.Values[i]); ok {
					out[name.Name] = body
				}
			}
		}
	}
	return out
}

// mustCompileArg reports the literal pattern a `regexp.MustCompile("…")` call
// compiles.
//
// Only a LITERAL argument counts. A pattern built by concatenation or read from
// a constant is one this mirror could not compare byte for byte anyway, and
// answering with half of it would be worse than not seeing it — the family would
// then be reported as drifted on every run, which is how a real drift stops being
// read.
func mustCompileArg(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fn.Sel.Name != "MustCompile" {
		return "", false
	}
	pkg, ok := fn.X.(*ast.Ident)
	if !ok || pkg.Name != "regexp" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	body, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return body, true
}

// TestPatternParity is the mechanical half of the promise the package doc makes:
// every pattern here is byte-for-byte the server's.
//
// It has to read the server's SOURCE because cli/ is a separate Go module that
// deliberately does not import the backend, so there is nothing to compare
// against in the type system. That makes this the one test in the module whose
// subject is outside its own tree — and the reason it is worth the oddity is
// that both drift directions are silent: a looser pattern rewrites text the
// server would have left alone, and a stricter one leaves an alias in the source
// where the server reads it as a literal id, which for the secret families is a
// warning rather than a refusal. Nothing else would notice either.
//
// SKIPS when the backend tree is not there — an installed module, or the CLI
// vendored on its own — rather than failing, because the absence of the file
// says nothing about whether the patterns agree. In this repository, and
// therefore in CI, it always runs.
func TestPatternParity(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("backend tree not present — nothing to check parity against")
	}

	server := map[string]string{}
	for _, rel := range serverFiles {
		for name, pattern := range serverPatterns(t, filepath.Join(root, rel)) {
			server[name] = pattern
		}
	}
	if len(server) == 0 {
		t.Fatalf("found no regexp declarations in %v — the extractor has drifted, not the patterns", serverFiles)
	}

	for _, fam := range families {
		want, ok := server[fam.serverVar]
		if !ok {
			t.Errorf("%s: server no longer declares %s — the family was renamed or removed there, and this mirror has not followed",
				fam.Family, fam.serverVar)
			continue
		}
		if got := fam.pattern.String(); got != want {
			t.Errorf("%s: pattern has drifted from %s\n  cli:    %s\n  server: %s",
				fam.Family, fam.serverVar, got, want)
		}
	}
}

// excludedServerPatterns is every marker-shaped pattern on the server that this
// package deliberately does NOT mirror, with the reason, so the enumeration
// below can tell "considered and excluded" from "nobody has looked at it".
//
// Each reason is the short form of one in the package doc; that doc is where the
// argument lives.
var excludedServerPatterns = map[string]string{
	"writeMarkerPattern": "a bare name is an auto-created OUTPUT table, not a reference",
	"fileMarkerPattern":  "the first argument is a store-relative folder PATH, never an id",
	"FileURLPattern":     "resolved only on the session runPython path; a workflow never inlines it",
	// Not a family at all — the two below are other readings of markers this
	// package already knows about, and mirroring either would double-report.
	"genericMarkerPattern": "the verb-agnostic scanner behind RejectMarkersInsideStrings, not a family",
	"refPattern":           "the POSITIONAL narrowing of tableIDRefPattern — Occurrence.Positional is this",
	"refMarkerPattern":     "pymarkers' own leaf-safe copy of tableIDRefPattern, which `ref` above already mirrors; TestServerRefPatternCopiesAgree pins the two server copies to each other",
}

// TestEveryServerMarkerFamilyIsMirroredOrExcluded is the direction
// TestPatternParity cannot see.
//
// Parity proves that each pattern we DO mirror still matches what the server
// matches. It says nothing about a family the server ADDS — which the package
// doc names as the silent direction, and which is the more dangerous one: a new
// alias-eligible family that nothing here scans is an id form this layer will
// never rewrite, so a folder using it deploys the first organization's id into
// the second and every check in the CLI stays green.
//
// So this enumerates the server's own marker-shaped patterns — identified
// MECHANICALLY, by a body that begins with the `\{\{` every marker starts with,
// rather than by a name convention that a new family need not follow — and
// requires each to be mirrored or named in excludedServerPatterns with a reason.
// Adding a family on the server therefore fails here, by name, with the two
// things to do about it.
func TestEveryServerMarkerFamilyIsMirroredOrExcluded(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("backend tree not present — nothing to enumerate")
	}

	mirrored := map[string]bool{}
	for _, fam := range families {
		mirrored[fam.serverVar] = true
	}

	found := 0
	for _, rel := range serverFiles {
		for name, pattern := range serverPatterns(t, filepath.Join(root, rel)) {
			// A marker pattern is one that matches `{{`. Everything else in
			// these files is about SQL text or S3 keys and has nothing to do
			// with this package.
			if !strings.HasPrefix(pattern, `\{\{`) {
				continue
			}
			found++
			if mirrored[name] || excludedServerPatterns[name] != "" {
				continue
			}
			t.Errorf("%s declares %s, a marker family this package neither mirrors nor excludes.\n"+
				"  If its first argument names a row, add it to `families` (and a dependency kind, and `ronja bind`'s searchKindOf).\n"+
				"  If it does not, add it to excludedServerPatterns with the reason and say so in the package doc.\n"+
				"  Pattern: %s", rel, name, pattern)
		}
	}
	if found == 0 {
		t.Fatalf("found no marker-shaped patterns in %v — the extractor has drifted, not the families", serverFiles)
	}
	// TestExcludedFamiliesStillExist pins each exclusion to a pattern the server
	// still declares. This catches the other way an exclusion goes wrong: a
	// family someone mirrored without deleting the reason it was left out, which
	// would leave the next reader with two answers.
	for name := range excludedServerPatterns {
		if mirrored[name] {
			t.Errorf("%s is both mirrored and excluded — one of the two is wrong", name)
		}
	}
}

// TestServerRefPatternCopiesAgree pins the server's two copies of the
// `{{ ref }}` regex to each other.
//
// engine/pymarkers is a leaf (stdlib + lib/rrn + lib/rjerr), so the
// malformed-argument rule cannot import engine/esql for tableIDRefPattern and
// keeps refMarkerPattern instead. Its own comment says "the two must stay
// identical" and nothing enforced it — and both drift directions are silent: a
// looser copy in pymarkers refuses a marker the extractor never binds, a
// stricter one lets a handle through the rule and on to a reach query that
// reports it as unreachable, which is the answer the malformed_marker code
// exists to stop giving.
//
// It lives in this module for the same reason TestPatternParity does: this is
// already the one place that reads the server's source to compare two copies of
// a pattern across a boundary the type system cannot cross. It skips with the
// rest when the backend tree is absent.
func TestServerRefPatternCopiesAgree(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("backend tree not present — nothing to compare")
	}
	esql := serverPatterns(t, filepath.Join(root, "backend/engine/esql/esql.go"))["tableIDRefPattern"]
	leaf := serverPatterns(t, filepath.Join(root, "backend/engine/pymarkers/malformed.go"))["refMarkerPattern"]
	if esql == "" || leaf == "" {
		t.Fatalf("one of the two copies is gone: esql.tableIDRefPattern=%q pymarkers.refMarkerPattern=%q", esql, leaf)
	}
	if esql != leaf {
		t.Errorf("the server's two {{ ref }} patterns have drifted\n  esql.tableIDRefPattern:      %s\n  pymarkers.refMarkerPattern: %s", esql, leaf)
	}
}

// TestExcludedFamiliesStillExist pins every exclusion to something real.
//
// Each is excluded for a stated reason about a pattern the server actually
// declares — what `write`'s and `file`'s first argument MEANS, where
// `file_url` is and is not wired. If one disappears from the server, the
// exclusion is reasoning about a family that no longer exists and the doc has
// become fiction — which is worth a failing test, because the next person to
// read it would take it as current.
func TestExcludedFamiliesStillExist(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("backend tree not present")
	}
	found := map[string]bool{}
	for _, rel := range serverFiles {
		for name := range serverPatterns(t, filepath.Join(root, rel)) {
			found[name] = true
		}
	}
	for name, reason := range excludedServerPatterns {
		if !found[name] {
			t.Errorf("server no longer declares %s, which this package excludes (%q) — re-check the exclusion before deleting this entry", name, reason)
		}
	}
}

// repoRoot walks up from the test's own directory looking for the monorepo root,
// identified by the backend tree this test reads. Returns "" when there is none.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "backend", "engine", "pymarkers")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// TestServerPatternsReadsGroupedDeclarations pins the hole the regex extractor
// had: a family declared inside a `var ( … )` block was invisible to both parity
// tests, and neither could notice — parity simply stops checking a pattern it
// cannot find, and the enumeration's `found == 0` guard stays satisfied by the
// other declarations.
//
// A synthetic file rather than the server's own, because the point is the SHAPE
// the server does not use today: a test written against the current layout would
// pass before the fix as well as after it.
func TestServerPatternsReadsGroupedDeclarations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patterns.go")
	source := "package fake\n\n" +
		"import \"regexp\"\n\n" +
		"var single = regexp.MustCompile(`\\{\\{\\s*single\\(\\)\\s*\\}\\}`)\n\n" +
		"var (\n" +
		"\t// grouped, exactly as gofmt would leave it\n" +
		"\tgroupedOne = regexp.MustCompile(`\\{\\{\\s*one\\(\\)\\s*\\}\\}`)\n" +
		"\tgroupedTwo = regexp.MustCompile(\"\\\\{\\\\{\\\\s*two\\\\(\\\\)\\\\s*\\\\}\\\\}\")\n" +
		")\n\n" +
		"func notAFamily() { _ = regexp.MustCompile(`\\{\\{\\s*local\\(\\)\\s*\\}\\}`) }\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	got := serverPatterns(t, path)
	for name, want := range map[string]string{
		"single":     `\{\{\s*single\(\)\s*\}\}`,
		"groupedOne": `\{\{\s*one\(\)\s*\}\}`,
		// An interpreted string literal, unquoted to the same bytes a raw one
		// would give — the comparison is against what the server COMPILES, and
		// the quoting style it happens to use is not the mirror's business.
		"groupedTwo": `\{\{\s*two\(\)\s*\}\}`,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// A pattern inside a function body is not a package-level family, and a
	// mirror that demanded one be declared or excluded would be demanding it of
	// something that is not a marker family at all.
	if _, seen := got["notAFamily"]; seen {
		t.Error("a function-local pattern was read as a declaration")
	}
}
