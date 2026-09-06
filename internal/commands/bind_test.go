package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja bind` gets its own fake rather than fakeInstance's, and the reason is
// the assertion in serve(): this command must talk to exactly three routes —
// the organization lookup every folder command pays for, the search, and the
// single GET /feature/:id that `--feature` checks the id it was handed against.
// An unexpected path fails the test, which is what pins "bind is not a way to
// browse resources" as a property of the code rather than a promise in a
// comment. None of the three is a listing: two answer a term or an id the
// caller typed, and the third answers "who am I".
type fakeSearchInstance struct {
	t *testing.T
	// hits is what the search answers, keyed by the term asked for. A term with
	// no entry answers an empty result, which is what "nothing is called that"
	// looks like on the wire.
	hits map[string][]api.SearchHit
	// terms records every q the CLI asked for, in order — the only way to assert
	// that an already-bound alias costs no request.
	terms []string
	// fail is the status the search answers with instead of a result; 0 is off.
	fail int
	// featureStatus is the status GET /feature/:id answers with, per id — the
	// existence check `bind --feature` makes before it declares a stack. Absent
	// (0) is 200, since a feature the caller can reach is the ordinary case.
	featureStatus map[string]int
	server        *httptest.Server
}

func newFakeSearchInstance(t *testing.T) *fakeSearchInstance {
	t.Helper()
	f := &fakeSearchInstance{t: t, hits: map[string][]api.SearchHit{}, featureStatus: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSearchInstance) URL() string { return f.server.URL }

func (f *fakeSearchInstance) serve(w http.ResponseWriter, r *http.Request) {
	// GET /feature/:id — the one read `bind --feature` makes, and the only
	// route here that is not the organization lookup or the search. It is not a
	// way to browse: the id is one the caller typed, and the answer is whether
	// they can reach it.
	//
	// One segment only, or the unexpected-request assertion below stops being
	// one: `/api/v2/feature/` prefixes routes that are not a feature read
	// (`/feature/model/...`, `/feature/query`), and answering those here would
	// let bind grow a browse call without a single test noticing.
	if featureID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/"); ok && !strings.Contains(featureID, "/") {
		switch status := f.featureStatus[featureID]; status {
		case 0:
			writeJSON(w, map[string]any{"id": featureID, "name": "Feature", "scope": "private"})
		case http.StatusBadRequest:
			// The OLDER backend's answer for an id the caller cannot reach: the
			// raw table.ErrNoRows sentinel. A current one retyped that miss to
			// the 404 below, and this binary ships against both.
			http.Error(w, `{"error":"no rows"}`, status)
		case http.StatusNotFound:
			http.Error(w, `{"error":"feature not found"}`, status)
		default:
			http.Error(w, `{"error":"Internal server error"}`, status)
		}
		return
	}
	switch r.URL.Path {
	case "/api/v2/authentication/me":
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": 50},
			"tenant": map[string]any{"id": testTenantID, "name": "Test Org"},
		})
	case "/api/v2/search":
		term := r.URL.Query().Get("q")
		f.terms = append(f.terms, term)
		if f.fail != 0 {
			http.Error(w, `{"error":"boom"}`, f.fail)
			return
		}
		hits := f.hits[term]
		if hits == nil {
			hits = []api.SearchHit{}
		}
		writeJSON(w, map[string]any{"result": hits})
	default:
		f.t.Errorf("bind made an unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
	}
}

// exactHit is a hit the server would score as an exact name match.
func exactHit(kind, id, title string) api.SearchHit {
	return api.SearchHit{Kind: kind, ID: id, Title: title, Score: api.SearchScoreExact}
}

// bindCmdFolder writes a workflow folder declaring `deps` and binding `bind` on a
// stack called "prod" pointing at the fake.
//
// Raw JSON rather than a marshalled wfdir.Manifest: what these tests are about
// is a file a person committed, and building one through the struct would test
// the writer against itself.
func bindCmdFolder(t *testing.T, f *fakeSearchInstance, deps, bind string) string {
	t.Helper()
	root := t.TempDir()
	stack := fmt.Sprintf(`{"url": %q, "tenantID": %q, "featureID": "col-1"`, f.URL(), testTenantID)
	if bind != "" {
		stack += `, "bind": ` + bind
	}
	stack += "}"
	manifest := fmt.Sprintf(`{
  "formatVersion": 3,
  "kind": "workflow",
  "title": "Region Report",
  "entrypoint": "main.py",
  "dependencies": %s,
  "stacks": {"prod": %s}
}
`, deps, stack)
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	return root
}

// bindCmdStackBind reads the stack's recorded answers back off disk.
func bindCmdStackBind(t *testing.T, root, stack string) map[string]string {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m.Stacks[stack].Bind
}

// The ordinary case: one exact match, recorded under the stack.
func TestBindRecordsAnExactMatch(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-1", "orders")}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := bindCmdStackBind(t, root, "prod")["orders"]; got != "table-orders-1" {
		t.Errorf("bind[orders] = %q, want table-orders-1", got)
	}
}

// An alias that already has an answer is left alone, and — the half that is
// easy to lose — costs no request. A bind is a decision somebody made; going
// back to the server to second-guess it is how a rename in the app silently
// repoints a committed file.
func TestBindLeavesAnAlreadyBoundAliasAlone(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f,
		`{"orders": {"kind": "table"}}`,
		`{"orders": "table-chosen-by-hand"}`)

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if len(f.terms) != 0 {
		t.Errorf("bind searched for an already-bound alias: %v", f.terms)
	}
	payload := decodeJSON(t, out)
	if bound := payload["alreadyBound"].([]any); len(bound) != 1 || bound[0] != "orders" {
		t.Errorf("alreadyBound = %v, want [orders]", bound)
	}
	if got := bindCmdStackBind(t, root, "prod")["orders"]; got != "table-chosen-by-hand" {
		t.Errorf("bind[orders] = %q — an existing answer was rewritten", got)
	}
}

// Two rows of the right kind with the right name. Choosing between them is a
// person's job, and --yes does not make it the command's: --yes says "I trust
// the unambiguous ones", which is not an answer to this question.
func TestBindRefusesToPickBetweenTwoExactMatches(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{
		exactHit(api.SearchKindTable, "table-orders-1", "orders"),
		exactHit(api.SearchKindTable, "table-orders-2", "Orders"),
	}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	if len(bindCmdStackBind(t, root, "prod")) != 0 {
		t.Errorf("bind wrote something: %v", bindCmdStackBind(t, root, "prod"))
	}
	unresolved := decodeJSON(t, out)["unresolved"].([]any)
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %v, want one entry", unresolved)
	}
	entry := unresolved[0].(map[string]any)
	if entry["reason"] != bindReasonAmbiguous {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonAmbiguous)
	}
	// The candidates are the whole answer for whoever picks one by hand, so a
	// report that names the problem without naming them is not enough.
	if got := entry["candidates"].([]any); len(got) != 2 {
		t.Errorf("candidates = %v, want both ids", got)
	}
	if !strings.Contains(entry["detail"].(string), "are called") {
		t.Errorf("detail does not say it was ambiguous: %v", entry["detail"])
	}
}

// TestBindRefusesToProposeFromATruncatedSearch: the same invariant, defended
// STRUCTURALLY rather than by the candidate list happening to be complete.
//
// bind asks for exactly the server's hard cap and there is no paging, so a
// request that comes back full is a PREFIX of the answer, not the answer. The
// tag leg is what makes that reachable: it stamps a matching TAG's score onto
// every row it surfaces, so a tag used widely enough fills the page and the
// second table genuinely called `orders` never arrives. One survivor would then
// be proposed — and under --yes written, reported as a success — which is the
// "AMBIGUITY IS NEVER RESOLVED, --yes INCLUDED" rule broken by a result set
// nobody could see the end of.
func TestBindRefusesToProposeFromATruncatedSearch(t *testing.T) {
	f := newFakeSearchInstance(t)
	// A full page: one row really called `orders`, and the cap-1 rows a tag of
	// that name dragged in beside it. Every one of them scores exact.
	hits := []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-1", "orders")}
	for i := len(hits); i < api.MaxSearchLimit; i++ {
		hits = append(hits, exactHit(api.SearchKindTable, fmt.Sprintf("table-tagged-%d", i), "something else"))
	}
	f.hits["orders"] = hits
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	if got := bindCmdStackBind(t, root, "prod"); len(got) != 0 {
		t.Fatalf("--yes wrote a binding from a result set nobody could see the end of: %v", got)
	}
	payload := decodeJSON(t, out)
	if proposed := payload["proposed"].([]any); len(proposed) != 0 {
		t.Fatalf("proposed = %v, want nothing proposed from a truncated search", proposed)
	}
	entry := payload["unresolved"].([]any)[0].(map[string]any)
	if entry["reason"] != bindReasonAmbiguous {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonAmbiguous)
	}
	// The one candidate it DID see is named: it is very probably the right
	// answer, and the reader is the one allowed to say so.
	if got := entry["candidates"].([]any); len(got) != 1 || got[0] != "table-orders-1" {
		t.Errorf("candidates = %v, want the one exact match it saw", got)
	}
}

// Nothing is called that. Distinct from ambiguity in the report, because the two
// send the reader somewhere completely different.
func TestBindReportsNoMatchSeparatelyFromAmbiguity(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	entry := decodeJSON(t, out)["unresolved"].([]any)[0].(map[string]any)
	if entry["reason"] != bindReasonNoMatch {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonNoMatch)
	}
}

// A codex is not "no match" — the search endpoint does not fan out to codexes at
// all, so reporting an empty result would send the reader hunting the web app
// for a resource that is sitting right there.
func TestBindSaysCodexesAreNotSearchable(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"handbook": {"kind": "codex"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	if len(f.terms) != 0 {
		t.Errorf("bind searched for a codex, which the endpoint cannot answer: %v", f.terms)
	}
	entry := decodeJSON(t, out)["unresolved"].([]any)[0].(map[string]any)
	if entry["reason"] != bindReasonNotSearchable {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonNotSearchable)
	}
	if !strings.Contains(entry["detail"].(string), "does not cover codexes") {
		t.Errorf("detail does not say why: %v", entry["detail"])
	}
}

// The tag leg stamps a matching TAG's score onto whatever it is attached to, so
// an exact-scored hit is not necessarily a row with that name. Binding through
// one would resolve {{ ref('orders') }} to a table called something else.
func TestBindIgnoresAnExactScoreOnADifferentTitle(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{
		exactHit(api.SearchKindTable, "table-tagged", "Sales lines"),
	}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind bound a tag match")
	}
	if len(bindCmdStackBind(t, root, "prod")) != 0 {
		t.Errorf("bind wrote something: %v", bindCmdStackBind(t, root, "prod"))
	}
	entry := decodeJSON(t, out)["unresolved"].([]any)[0].(map[string]any)
	if entry["reason"] != bindReasonNoMatch {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonNoMatch)
	}
}

// A hit of the right name and the WRONG kind resolves nothing: substituting a
// workflow id into {{ ref(…) }} sends the server an id of the wrong primitive.
func TestBindIgnoresAHitOfAnotherKind(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{
		exactHit(api.SearchKindWorkflow, "workflow-orders", "orders"),
		exactHit(api.SearchKindTable, "table-orders", "orders"),
	}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := bindCmdStackBind(t, root, "prod")["orders"]; got != "table-orders" {
		t.Errorf("bind[orders] = %q, want the TABLE", got)
	}
}

// Only score 3 is acted on. A prefix or substring hit is a guess, and a wrong
// bind deploys the folder against the wrong row while reporting success.
func TestBindIgnoresANearMatch(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{
		{Kind: api.SearchKindTable, ID: "table-orders-eu", Title: "orders", Score: 2},
	}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json"); err == nil {
		t.Fatal("bind bound a prefix match")
	}
	if len(bindCmdStackBind(t, root, "prod")) != 0 {
		t.Errorf("bind wrote something: %v", bindCmdStackBind(t, root, "prod"))
	}
}

// No terminal and no --yes: refused rather than guessed. `go test` gives the
// command a stdin that is not a terminal, which is exactly the agent/CI shape.
func TestBindRefusesToWriteWithNothingToAskOn(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-1", "orders")}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	_, err := runCLI(t, root, "bind", "--stack", "prod")
	if err == nil {
		t.Fatal("bind wrote without confirmation and without a terminal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal does not name --yes: %v", err)
	}
	if len(bindCmdStackBind(t, root, "prod")) != 0 {
		t.Errorf("bind wrote something: %v", bindCmdStackBind(t, root, "prod"))
	}
}

// Nothing left unbound means exit zero — the property that lets this be a CI
// check rather than only an authoring aid.
func TestBindExitsZeroWhenEverythingIsBound(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f,
		`{"orders": {"kind": "table"}}`,
		`{"orders": "table-orders-1"}`)

	if _, err := runCLI(t, root, "bind", "--stack", "prod"); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

// The promotion case, and the one place `bind` has to say no: the folder is
// being pointed at an organization it has no stack for yet. Refused BEFORE
// anything is searched, because two thirds of a stack — which organization it
// means, which feature its resources live in — are things this command cannot
// discover, and writing the third would leave a half-answer in a committed file
// that reads as a decision somebody made.
func TestBindRefusesAStackTheFolderHasNotDeclaredYet(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	// The declared stack belongs to ANOTHER organization on the same instance, so
	// `--stack staging` is a name nothing here answers to rather than a near-miss
	// for `prod` (which wfdir.Select refuses on its own, and better).
	root := bindCmdFolderOn(t, f, "prod", "ten-other", `{"orders": {"kind": "table"}}`)

	_, err := runCLI(t, root, "bind", "--stack", "staging")
	if err == nil {
		t.Fatal("bind accepted a stack the folder does not declare")
	}
	if !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "featureID") {
		t.Errorf("refusal does not say what to add: %v", err)
	}
	if len(f.terms) != 0 {
		t.Errorf("bind searched before it knew where to write: %v", f.terms)
	}
}

// bindCmdFolderOn is bindCmdFolder for a folder whose only stack belongs to
// ANOTHER organization on the same instance — the promotion shape, where
// `--stack prod` names something the folder genuinely does not have.
func bindCmdFolderOn(t *testing.T, f *fakeSearchInstance, stack, tenantID, deps string) string {
	t.Helper()
	root := t.TempDir()
	manifest := fmt.Sprintf(`{
  "formatVersion": 3,
  "kind": "workflow",
  "title": "Region Report",
  "entrypoint": "main.py",
  "dependencies": %s,
  "stacks": {%q: {"url": %q, "tenantID": %q, "featureID": "col-dev"}}
}
`, deps, stack, f.URL(), tenantID)
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return root
}

// stackOf reads a whole stack back off disk.
func stackOf(t *testing.T, root, name string) wfdir.Stack {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m.Stacks[name]
}

// The promotion loop as one command: --feature supplies the half nothing can
// work out (which feature this organization's resources are created in), the url
// and organization come from the credential, and the names are bound in the same
// run.
func TestBindWithFeatureDeclaresTheStackAndBindsIt(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-prod", "orders")}
	signInTo(t, f.URL())
	root := bindCmdFolderOn(t, f, "dev", "ten-other", `{"orders": {"kind": "table"}}`)

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-prod", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	prod := stackOf(t, root, "prod")
	if prod.FeatureID != "col-prod" {
		t.Errorf("featureID = %q, want col-prod", prod.FeatureID)
	}
	if prod.TenantID != testTenantID {
		t.Errorf("tenantID = %q, want the credential's organization %s", prod.TenantID, testTenantID)
	}
	if prod.URL == "" {
		t.Error("the declared stack has no url")
	}
	if prod.Bind["orders"] != "table-orders-prod" {
		t.Errorf("bind = %v, want orders bound", prod.Bind)
	}
	// The stack it was promoted FROM is untouched.
	if dev := stackOf(t, root, "dev"); dev.FeatureID != "col-dev" || dev.TenantID != "ten-other" {
		t.Errorf("the other stack changed: %+v", dev)
	}
}

// A stack declared with --feature and nothing bindable is still declared. The
// person asked for an environment; reporting "nothing to bind" and quietly not
// declaring it would leave them exactly as deadlocked as before.
func TestBindWithFeatureDeclaresTheStackEvenWithNothingToBind(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolderOn(t, f, "dev", "ten-other", `{"orders": {"kind": "table"}}`)

	// Non-zero, because `orders` is still unbound — but the stack is there.
	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-prod", "--yes"); err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	if got := stackOf(t, root, "prod").FeatureID; got != "col-prod" {
		t.Errorf("featureID = %q — the stack was not declared", got)
	}
}

// Under a $RONJA_TOKEN credential the organization is not known locally, and a
// stack must name one. This is the credential the promotion loop runs under in
// CI, so it has to work rather than refuse.
func TestBindWithFeatureAsksWhichOrganizationTheTokenReaches(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	// A folder that names nothing on this instance at all, so nothing else would
	// have prompted the lookup.
	root := t.TempDir()
	manifest := `{
  "formatVersion": 3,
  "kind": "workflow",
  "title": "Region Report",
  "entrypoint": "main.py",
  "dependencies": {}
}
`
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-prod", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := stackOf(t, root, "prod").TenantID; got != testTenantID {
		t.Errorf("tenantID = %q, want the organization the token reaches", got)
	}
}

// --feature repointing an existing environment at a different feature is
// refused: that changes where every future resource is created, and it belongs
// in the file under review rather than on a command about aliases.
func TestBindRefusesFeatureAgainstAStackThatHasOne(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	_, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-elsewhere", "--yes")
	if err == nil {
		t.Fatal("bind repointed a declared stack at another feature")
	}
	if !strings.Contains(err.Error(), "col-1") || !strings.Contains(err.Error(), "col-elsewhere") {
		t.Errorf("refusal does not name both features: %v", err)
	}
	if got := stackOf(t, root, "prod").FeatureID; got != "col-1" {
		t.Errorf("featureID = %q — the stack was repointed anyway", got)
	}
	if len(f.terms) != 0 {
		t.Errorf("bind searched before checking where it would write: %v", f.terms)
	}
}

// The SAME feature is a no-op, not a refusal — otherwise a CI job could not run
// the same `bind --stack prod --feature col-…` line twice.
func TestBindAcceptsFeatureThatMatchesTheStack(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-1", "orders")}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-1", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got := bindCmdStackBind(t, root, "prod")["orders"]; got != "table-orders-1" {
		t.Errorf("bind[orders] = %q", got)
	}
}

// --feature with no --stack names nothing. Refused rather than ignored: whoever
// typed it meant to set an environment up.
func TestBindRefusesFeatureWithoutAStack(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	_, err := runCLI(t, root, "bind", "--feature", "col-prod", "--yes")
	if err == nil {
		t.Fatal("bind accepted --feature with no stack to name")
	}
	if !strings.Contains(err.Error(), "--stack") {
		t.Errorf("refusal does not name --stack: %v", err)
	}
}

// Without --feature the old refusal stands, and now points at the flag that
// resolves it — the near-deadlock this closes was that both halves said "edit
// the manifest".
func TestBindRefusalNamesTheFeatureFlag(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolderOn(t, f, "dev", "ten-other", `{"orders": {"kind": "table"}}`)

	_, err := runCLI(t, root, "bind", "--stack", "prod")
	if err == nil {
		t.Fatal("bind accepted a stack the folder does not declare")
	}
	if !strings.Contains(err.Error(), "--feature") {
		t.Errorf("refusal does not offer --feature: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, wfdir.LockName)); !os.IsNotExist(statErr) {
		t.Errorf("a refused bind wrote %s", wfdir.LockName)
	}
}

// A folder with no declarations has nothing to reconcile, and says so rather
// than reporting success at having done nothing.
func TestBindOnAFolderThatDeclaresNothing(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !strings.Contains(out, "declares no dependencies") {
		t.Errorf("output does not say the folder declares nothing:\n%s", out)
	}
}

// An instance that will not answer is fatal to the whole run. Recording "no
// exact match" against a question nobody got to ask is the one output that would
// send a reader to change the wrong thing.
func TestBindStopsWhenTheSearchFails(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.fail = http.StatusInternalServerError
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")

	_, err := runCLI(t, root, "bind", "--stack", "prod", "--yes")
	if err == nil {
		t.Fatal("bind reported a verdict after the search failed")
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Errorf("failure does not name what it was looking up: %v", err)
	}
}

// A bind naming a dependency nobody declared is what a typo looks like, and is
// refused before anything is added — appending the correctly-spelled entry
// beside it leaves two and no clue which is live.
func TestBindRefusesAManifestWhoseBindIsAlreadyBroken(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f,
		`{"orders": {"kind": "table"}}`,
		`{"order": "table-typo"}`)

	_, err := runCLI(t, root, "bind", "--stack", "prod", "--yes")
	if err == nil {
		t.Fatal("bind added to a manifest whose bind map is broken")
	}
	if !strings.Contains(err.Error(), `"order"`) {
		t.Errorf("refusal does not name the undeclared bind: %v", err)
	}
	if len(f.terms) != 0 {
		t.Errorf("bind searched before checking the manifest: %v", f.terms)
	}
}

// Every dependency kind resolves to a search kind, or is one of the two the
// search endpoint does not cover.
//
// This is the guard on ADDING one. A kind with no entry in searchKindOf and no
// case here would be reported as "not searchable", which is the codex sentence —
// and a reader told the search does not cover their kind stops looking for the
// row that is right there.
func TestEverySearchableDependencyKindIsMapped(t *testing.T) {
	// codex and module are the two kinds the endpoint has no leg for; both are
	// listed here so ADDING a third is a decision somebody makes on purpose.
	unsearchable := map[string]bool{markers.KindCodex: true, markers.KindModule: true}
	for _, kind := range markers.Kinds() {
		_, ok := searchKindOf(kind)
		if unsearchable[kind] {
			if ok {
				t.Errorf("%s has a search kind, but the endpoint does not fan out to it", kind)
			}
			continue
		}
		if !ok {
			t.Errorf("dependency kind %q has no search kind — `ronja bind` would report it as not searchable", kind)
		}
	}
}

// The reasons are the --json contract, so they have to stay distinct: two that
// collided would make a report unable to say which of two very different
// situations the reader is in.
func TestBindReasonsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range []string{
		bindReasonNoMatch, bindReasonAmbiguous, bindReasonNotSearchable,
		bindReasonTermTooShort, bindReasonUnknownKind,
	} {
		if seen[reason] {
			t.Errorf("two bind reasons share the string %q", reason)
		}
		seen[reason] = true
	}
}

// A term the endpoint will not search at all answers empty, exactly as "nothing
// is called that" does. Reported as what it is instead.
func TestBindSaysWhenAnAliasIsTooShortToSearch(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{"o": {"kind": "table"}}`, "")

	out, err := runCLI(t, root, "bind", "--stack", "prod", "--yes", "--json")
	if err == nil {
		t.Fatal("bind exited zero with an unbound dependency")
	}
	if len(f.terms) != 0 {
		t.Errorf("bind sent a term the endpoint answers empty: %v", f.terms)
	}
	entry := decodeJSON(t, out)["unresolved"].([]any)[0].(map[string]any)
	if entry["reason"] != bindReasonTermTooShort {
		t.Errorf("reason = %v, want %s", entry["reason"], bindReasonTermTooShort)
	}
}

// Writing is additive: the answers a person already committed survive, and so
// does everything else in the file.
func TestBindKeepsTheRestOfTheManifest(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.hits["customers"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-customers", "customers")}
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f,
		`{"orders": {"kind": "table"}, "customers": {"kind": "table"}}`,
		`{"orders": "table-orders-1"}`)

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got := bindCmdStackBind(t, root, "prod")
	if got["orders"] != "table-orders-1" || got["customers"] != "table-customers" {
		t.Errorf("bind = %v, want both answers", got)
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.Title != "Region Report" || m.Stacks["prod"].FeatureID != "col-1" {
		t.Errorf("manifest lost something: %+v", m)
	}
	// The lock file is not created: a bind is config a person decided, and no id
	// was discovered by a deploy.
	if _, err := os.Stat(filepath.Join(root, wfdir.LockName)); !os.IsNotExist(err) {
		t.Errorf("bind wrote %s (%v)", wfdir.LockName, err)
	}
}

// The mirrored search response decodes the fields bind reads. A tag that does
// not exist on the wire decodes to the zero value silently, which here would be
// an empty id — a bind recorded as "".
func TestSearchHitDecodesTheServerShape(t *testing.T) {
	body := `{"result":[{"kind":"table","subKind":"","id":"table-1","title":"orders",` +
		`"description":"the orders","featureID":"col-1","updatedAt":"2026-09-01T10:00:00Z","score":3}]}`
	var out struct {
		Result []api.SearchHit `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Result[0]
	want := api.SearchHit{Kind: "table", ID: "table-1", Title: "orders", Score: 3}
	if got != want {
		t.Errorf("hit = %+v, want %+v", got, want)
	}
}

// `ronja bind` is kind-agnostic on purpose — it reads `dependencies` and writes
// `bind`, which three of the four folder kinds carry. A MODULE folder is the
// one that must not be run to completion: module files are marker-free, so a
// declared alias has nothing to resolve INTO, and the bind loop's honest report
// on one is "0 names bound". That reads as "nothing was missing" rather than
// "this command does not apply here", which is why the refusal is explicit and
// arrives before the folder is even opened.
func TestBindRefusesAModuleFolderAndSaysWhy(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := t.TempDir()
	manifest := fmt.Sprintf(`{
  "formatVersion": 3,
  "kind": "module",
  "title": "Tracker",
  "name": "emab_tracker",
  "dependencies": {"orders": {"kind": "table"}},
  "stacks": {"prod": {"url": %q, "tenantID": %q, "featureID": "col-1"}}
}
`, f.URL(), testTenantID)
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "__init__.py"), []byte("\n"), 0o644); err != nil {
		t.Fatalf("write __init__.py: %v", err)
	}

	_, err := runCLI(t, root, "bind", "--stack", "prod", "--yes")
	if err == nil {
		t.Fatal("bind ran on a module folder instead of refusing")
	}
	// The refusal has to name the kind AND the reason. Naming only the kind
	// reads as "wrong verb, try another one", and there is no other verb.
	if !strings.Contains(err.Error(), "module") {
		t.Errorf("refusal does not name the folder kind: %v", err)
	}
	if !strings.Contains(err.Error(), "marker-free") {
		t.Errorf("refusal does not say WHY an alias cannot resolve here: %v", err)
	}
	// Nothing may be looked up: the folder was ruled out before any request.
	if len(f.terms) != 0 {
		t.Errorf("bind searched on a module folder: %v", f.terms)
	}
}
