package commands

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The organization policy on `ronja context`. Inside the app it is injected
// into every agent turn; a coding agent working from outside was never told it
// existed. What matters here is that it is printed where it will be read —
// after identity, before the credential recipe — and that every way it can be
// missing is said in one line rather than left silent, because a silent
// omission reads as "no rules", the one wrong answer.

const testPolicy = "- Report every amount in SEK.\n- Every table name starts with the source system: `fortnox_…`, `hubspot_…`.\n"

// The two 403 bodies gt.EnforceScope sends (backend/lib/api/gt/scope.go), and
// the one platform/auth sends for a credential whose user is gone. `context`
// tells a scoped token from a dead credential by these words and nothing else.
const (
	scopeShortfall403 = `token scope "analytics" does not permit read`
	unscopedRoute403  = "this route is not accessible to scoped tokens"
	roleNotFound403   = "Forbidden: User role not found"
)

func TestContextInlinesTheOrganizationPolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.policy = &api.Policy{Content: testPolicy, Version: 7, UpdatedAt: "2026-09-01T10:00:00Z"}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "## Organization policy (version 7)") {
		t.Errorf("no policy section with the version:\n%s", out)
	}
	if !strings.Contains(out, "Report every amount in SEK.") {
		t.Errorf("the content is not rendered verbatim:\n%s", out)
	}
	// The heading names the organization /me reported, so the reader knows
	// whose rules these are.
	if !strings.Contains(out, "Standing rules Test Org's admins wrote") {
		t.Errorf("the framing does not name the organization:\n%s", out)
	}
	// The framing is the feature: a rule read without it becomes the task.
	if !strings.Contains(out, "not a description of your task") {
		t.Errorf("the framing sentence is missing:\n%s", out)
	}
	// Identity, then the rules, then how to call the API.
	signedAt := strings.Index(out, "Signed in:")
	policyAt := strings.Index(out, "## Organization policy")
	credsAt := strings.Index(out, "## Use your credentials")
	if signedAt < 0 || policyAt < 0 || credsAt < 0 || !(signedAt < policyAt && policyAt < credsAt) {
		t.Errorf("policy must sit between the identity line and the credential recipe (signed=%d policy=%d creds=%d):\n%s",
			signedAt, policyAt, credsAt, out)
	}
	// --no-docs skips the API index, not the policy — the flag's text says so.
	if strings.Contains(out, "# Ronja API\n") {
		t.Errorf("--no-docs did not skip the index:\n%s", out)
	}
}

// A written-then-blanked document has a version and no rules. The "none yet"
// verdict is keyed on the content, as the in-app prompt block keys it, not on
// the version — a version-7 document with nothing in it is still nothing to
// follow.
func TestContextSaysNoneWrittenForABlankPolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.policy = &api.Policy{Content: "  \n", Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: none written yet.") {
		t.Errorf("a blank document was not reported as none written:\n%s", out)
	}
	if strings.Contains(out, "## Organization policy") {
		t.Errorf("a blank document rendered as a section:\n%s", out)
	}
}

// A SCOPE 403 is unambiguous on this route — every member clears its role
// floor — so the line names the scope the token lacks rather than a generic
// failure. Keyed on the server's words: see the dead-credential test below.
func TestContextNamesTheScopeWhenThePolicyIsForbidden(t *testing.T) {
	f := newFakeInstance(t)
	f.failPolicy = http.StatusForbidden
	f.failPolicyMessage = scopeShortfall403
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: not readable with this token (needs analytics:read)") {
		t.Errorf("a scope 403 did not name the missing scope:\n%s", out)
	}
}

// ⚠️ 403 IS NOT ONLY THE SCOPE VERDICT. platform/auth answers 403 on the token
// path for a credential that is DEAD — a removed user ("Forbidden: User role
// not found"), a deleted organization, a PAT with no bound user — and in every
// one the policy read 403s too. Keyed on the status alone, `context` told each
// of those that the token "still works" and "needs analytics:read", both
// false, where "the stored token was rejected — run `ronja login`" was right.
func TestContextTreatsANonScope403AsADeadCredential(t *testing.T) {
	f := newFakeInstance(t)
	f.failMe = http.StatusForbidden
	f.failMeMessage = roleNotFound403
	f.failPolicy = http.StatusForbidden
	f.failPolicyMessage = roleNotFound403
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Signed in: NO — the stored token was rejected. Run `ronja login`.") {
		t.Errorf("a role-not-found 403 was not reported as a rejected credential:\n%s", out)
	}
	if strings.Contains(out, "as a scoped token") || strings.Contains(out, "still work") {
		t.Errorf("a dead credential was told it still works:\n%s", out)
	}
	if !strings.Contains(out, "Organization policy: could not read GET /api/v2/policy/org — HTTP 403.") {
		t.Errorf("a non-scope 403 on the policy did not get the generic line:\n%s", out)
	}
	if strings.Contains(out, "needs analytics:read") {
		t.Errorf("a dead credential was told which scope to add:\n%s", out)
	}

	// And --json says "error", not "forbidden": the scope verdict is the only
	// thing "forbidden" means to a consumer.
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	if got := decodeJSON(t, out)["organizationPolicyStatus"]; got != "error" {
		t.Errorf("organizationPolicyStatus = %v, want \"error\" for a non-scope 403", got)
	}
}

func TestContextReportsAPolicyReadThatFailed(t *testing.T) {
	f := newFakeInstance(t)
	f.failPolicy = http.StatusBadGateway
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: could not read GET /api/v2/policy/org — HTTP 502.") {
		t.Errorf("a failed read was not reported with its status:\n%s", out)
	}
}

// The route is served to every member of every organization, so a 404 is an
// instance that predates it — a CLI newer than the server — and never "no
// policy". Told "HTTP 404", a reader goes looking for a missing document; told
// this, it knows which side is old. --json says "error", the same as every
// failure: "none" is the one thing a 404 must not be read as.
func TestContextExplainsA404AsAnOlderInstance(t *testing.T) {
	f := newFakeInstance(t)
	f.failPolicy = http.StatusNotFound
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: this instance does not serve GET /api/v2/policy/org yet — the CLI is newer than the instance.") {
		t.Errorf("a 404 was not explained as an older instance:\n%s", out)
	}
	if strings.Contains(out, "HTTP 404") {
		t.Errorf("a 404 got the generic line:\n%s", out)
	}

	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["organizationPolicyStatus"] != "error" {
		t.Errorf("organizationPolicyStatus = %v, want \"error\" for a 404", payload["organizationPolicyStatus"])
	}
	if _, present := payload["organizationPolicy"]; present {
		t.Errorf("a 404 landed a document in --json: %v", payload["organizationPolicy"])
	}
}

// A working credential for a user in no organization: Me answers, with no
// tenant. Whatever the policy route said to that caller, the honest line is
// that there is no organization — Me is the authority on identity — and
// --json says "none" with NO document, because there is no version to write
// against either.
func TestContextSaysNoOrganizationWhenMeHasNoTenant(t *testing.T) {
	f := newFakeInstance(t)
	f.noTenant = true
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: you are in no organization, so there is no policy to read.") {
		t.Errorf("a user in no organization was not told so:\n%s", out)
	}
	if strings.Contains(out, "## Organization policy") || strings.Contains(out, "Report every amount") {
		t.Errorf("a document was rendered for a user in no organization:\n%s", out)
	}

	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["organizationPolicyStatus"] != "none" {
		t.Errorf("organizationPolicyStatus = %v, want \"none\" for no organization", payload["organizationPolicyStatus"])
	}
	if _, present := payload["organizationPolicy"]; present {
		t.Errorf("a document landed in --json for a user in no organization: %v", payload["organizationPolicy"])
	}
	if payload["identityStatus"] != "ok" {
		t.Errorf("identityStatus = %v, want \"ok\": Me answered", payload["identityStatus"])
	}
}

// A role-bound scoped API token cannot call /me at all (that group is
// admin-scoped) and is exactly the caller the policy was invisible to. The
// policy read goes out regardless, and the section is printed under a heading
// that does not name an organization Me could not report. The identity line
// says what a 403 there means — a scoped token, not a dead credential — because
// "run `ronja login`" would send an agent off to replace a token that has
// just, one line below, read the policy.
func TestContextPrintsThePolicyWhenMeIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	f.failMe = http.StatusForbidden
	f.failMeMessage = unscopedRoute403
	f.policy = &api.Policy{Content: testPolicy, Version: 3}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Signed in: as a scoped token — it cannot call /api/v2/authentication/me") {
		t.Errorf("a 403 from /me was not explained as a scoped token:\n%s", out)
	}
	if strings.Contains(out, "Signed in: NO") || strings.Contains(out, "Run `ronja login`") {
		t.Errorf("a working scoped token was told to sign in again:\n%s", out)
	}
	if !strings.Contains(out, "## Organization policy (version 3)") {
		t.Errorf("a refused /me suppressed the policy:\n%s", out)
	}
	if !strings.Contains(out, "Standing rules this organization's admins wrote") {
		t.Errorf("the tenant-less wording is missing:\n%s", out)
	}
	if strings.Contains(out, "Test Org") {
		t.Errorf("an organization name appeared that /me never reported:\n%s", out)
	}

	// --json used to say only `authenticated: false` beside a policy it had
	// just read, and a consumer keyed on the boolean signed in again to
	// replace a working token. `authenticated` keeps its type and meaning;
	// `identityStatus` is the finer verdict beside it.
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["authenticated"] != false {
		t.Errorf("authenticated = %v, want false: /me did not answer", payload["authenticated"])
	}
	if payload["identityStatus"] != "scoped" {
		t.Errorf("identityStatus = %v, want \"scoped\" for a scope-denied /me", payload["identityStatus"])
	}
	if payload["organizationPolicyStatus"] != "ok" {
		t.Errorf("organizationPolicyStatus = %v, want \"ok\": the scoped token read it", payload["organizationPolicyStatus"])
	}
}

// The other three identityStatus values, so the set is pinned end to end:
// "ok" when /me answered, "rejected" for any other failure (a dead
// credential's 403 here), "none" with no token at all.
func TestContextJSONIdentityStatus(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	if got := decodeJSON(t, out)["identityStatus"]; got != "ok" {
		t.Errorf("identityStatus = %v, want \"ok\"", got)
	}

	f.failMe = http.StatusForbidden
	f.failMeMessage = roleNotFound403
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	if got := decodeJSON(t, out)["identityStatus"]; got != "rejected" {
		t.Errorf("identityStatus = %v, want \"rejected\" for a dead credential's 403", got)
	}

	// Signed out: the URL is known (RONJA_URL still is), the token is not,
	// and the config dir signIn pointed at holds no profile to fall back to.
	t.Setenv("RONJA_TOKEN", "")
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json (signed out): %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["identityStatus"] != "none" {
		t.Errorf("identityStatus = %v, want \"none\" with no token", payload["identityStatus"])
	}
	if _, present := payload["organizationPolicyStatus"]; present {
		t.Errorf("no token, yet a policy verdict: %v", payload["organizationPolicyStatus"])
	}
}

// One dead credential is two refusals with the same status. The `Signed in:
// NO` line already says it; a second line about the policy would report the
// same thing twice and read as a second problem.
func TestContextSaysADeadCredentialOnce(t *testing.T) {
	f := newFakeInstance(t)
	f.failMe = http.StatusUnauthorized
	f.failPolicy = http.StatusUnauthorized
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Signed in: NO — the stored token was rejected") {
		t.Errorf("the rejected credential was not reported:\n%s", out)
	}
	if strings.Contains(out, "Organization policy") {
		t.Errorf("the dead credential was reported twice:\n%s", out)
	}
}

// The drop above is for 401 and 401 only. A shared 500 is the instance
// failing, not the credential; dropping the policy line there would leave
// "the stored token was rejected" as the only, and wrong, report.
func TestContextReportsAPolicyReadThatFailedAlongsideMe(t *testing.T) {
	f := newFakeInstance(t)
	f.failMe = http.StatusInternalServerError
	f.failPolicy = http.StatusInternalServerError
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: could not read GET /api/v2/policy/org — HTTP 500.") {
		t.Errorf("a 500 shared with /me was swallowed as a dead credential:\n%s", out)
	}
}

// The document is admin-authored free text printed to a terminal that is
// usually an agent's whole view. A C0 control in it — an ESC sequence that
// repaints the screen, a carriage return that overwrites the framing — is
// dropped from the human output. --json is data, not a terminal, and carries
// the document byte for byte.
//
// C0 is not the whole of it: a terminal honours the one-byte C1 CSI (U+009B)
// exactly as ESC-[, and a bidi override (U+202E) makes what the reader sees
// differ from what the bytes say. Both are dropped, along with the rest of the
// set backend/lib/untrusted strips.
func TestContextStripsControlCharactersFromThePolicyBody(t *testing.T) {
	f := newFakeInstance(t)
	f.policy = &api.Policy{Content: "- Report in SEK.\x1b[2J\r\n- Keep\ttabs.\x07\n- Also\u009b2J in \u202eSEK\u200b.\n", Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if strings.ContainsAny(out, "\x1b\r\x07\u009b\u202e\u200b") {
		t.Errorf("a control or invisible character reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, "- Report in SEK.[2J\n- Keep\ttabs.\n- Also2J in SEK.\n") {
		t.Errorf("newline and tab were not kept, or the text around the controls was lost:\n%q", out)
	}

	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	doc, ok := decodeJSON(t, out)["organizationPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("organizationPolicy missing from --json payload")
	}
	if doc["content"] != f.policy.Content {
		t.Errorf("--json content = %q, want the document verbatim %q", doc["content"], f.policy.Content)
	}
}

// The organization name is admin-authored too, and it is set into OUR framing
// line unquoted — so it gets the same filter as the body.
func TestContextStripsControlCharactersFromTheOrganizationName(t *testing.T) {
	f := newFakeInstance(t)
	f.tenantName = "Test\x1b\u202e Org"
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Standing rules Test Org's admins wrote") {
		t.Errorf("the organization name was not filtered in the framing line:\n%q", out)
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
		t.Errorf("a control character in the organization name reached the terminal:\n%q", out)
	}

	// terminalSafe keeps newline and tab for the body's sake; a name is one
	// line, and a newline in it would end the framing sentence early with
	// admin-chosen text on the next line reading as ours. Every run of
	// whitespace collapses to one space.
	f.tenantName = "Test\n\t  Org\nLtd"
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Standing rules Test Org Ltd's admins wrote for how work here is done.") {
		t.Errorf("whitespace in the organization name was not collapsed to one line:\n%q", out)
	}
}

func TestContextJSONCarriesTheOrganizationPolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload := decodeJSON(t, out)
	doc, ok := payload["organizationPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("organizationPolicy missing from --json payload: %v", payload)
	}
	if doc["content"] != testPolicy {
		t.Errorf("organizationPolicy.content = %q, want the document verbatim", doc["content"])
	}
	if doc["version"] != float64(7) {
		t.Errorf("organizationPolicy.version = %v, want 7", doc["version"])
	}
	// A failed read has no error key — the human output is where the reason
	// lives — but it does have a STATUS, from a closed set, so a consumer can
	// tell "no rules" from "refused" without parsing prose.
	if _, present := payload["organizationPolicyError"]; present {
		t.Errorf("--json carries an organizationPolicyError key: %v", payload)
	}
	if payload["organizationPolicyStatus"] != "ok" {
		t.Errorf("organizationPolicyStatus = %v, want \"ok\"", payload["organizationPolicyStatus"])
	}
}

// The status is what separates "the organization has no rules" (build freely)
// from "the read was refused" (ask the admin for the rules first): both leave
// `organizationPolicy` absent, and a consumer with only that key would build
// on an unread policy.
func TestContextJSONStatusTellsNoneFromForbidden(t *testing.T) {
	f := newFakeInstance(t)
	f.failPolicy = http.StatusForbidden
	f.failPolicyMessage = scopeShortfall403
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload := decodeJSON(t, out)
	if _, present := payload["organizationPolicy"]; present {
		t.Errorf("a refused read landed a document in --json: %v", payload["organizationPolicy"])
	}
	if payload["organizationPolicyStatus"] != "forbidden" {
		t.Errorf("organizationPolicyStatus = %v, want \"forbidden\"", payload["organizationPolicyStatus"])
	}

	f.failPolicy = http.StatusServiceUnavailable
	f.failPolicyMessage = ""
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if got := decodeJSON(t, out)["organizationPolicyStatus"]; got != "error" {
		t.Errorf("organizationPolicyStatus = %v, want \"error\" for a 503", got)
	}
}

// An unwritten document is "none" on the STATUS key — that is what a consumer
// branches on — and the document is still carried, empty, at the version the
// server reported: an admin's agent asked to write the first rules needs that
// version as expectedVersion, and there is nowhere else to get it without a
// second read. The genesis document sits at 1; a blanked one wherever the
// blanking left it, so the version is the server's, never a literal.
func TestContextJSONCarriesAnUnwrittenPolicyAtItsVersion(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["organizationPolicyStatus"] != "none" {
		t.Errorf("organizationPolicyStatus = %v, want \"none\"", payload["organizationPolicyStatus"])
	}
	doc, ok := payload["organizationPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("the genesis document is not carried in --json: %v", payload)
	}
	if doc["content"] != "" || doc["version"] != float64(1) {
		t.Errorf("organizationPolicy = %v, want {content: \"\", version: 1}", doc)
	}

	f.policy = &api.Policy{Content: "  \n", Version: 4}
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload = decodeJSON(t, out)
	if payload["organizationPolicyStatus"] != "none" {
		t.Errorf("a blanked document: organizationPolicyStatus = %v, want \"none\"", payload["organizationPolicyStatus"])
	}
	if doc, _ := payload["organizationPolicy"].(map[string]any); doc == nil || doc["version"] != float64(4) {
		t.Errorf("a blanked document must be carried at the served version 4, got %v", payload["organizationPolicy"])
	}
}

// An admin's personal access token can write the organization policy (that is
// the CLI's whole write path), so the section ends with the one-line CAS
// recipe carrying the version JUST SERVED as expectedVersion — the reader
// never has to fetch the CAS token separately — and the rule that keeps a
// coding agent from editing on its own initiative.
func TestContextOffersTheWriteRecipeToAnAdmin(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = adminPrivilegeLevel
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, `ronja api -X PUT /api/v2/policy/org -d '{"content":"…","expectedVersion":7}'`) {
		t.Errorf("an admin was not given the write recipe with the served version:\n%s", out)
	}
	if !strings.Contains(out, "only when the admin you are working for asks you to") {
		t.Errorf("the recipe lost the only-when-asked rule:\n%s", out)
	}
	if !strings.Contains(out, "show them the\nnew text before the PUT") {
		t.Errorf("the recipe lost the show-the-text-first rule:\n%s", out)
	}
	if strings.Contains(out, "Only an admin can change it.") {
		t.Errorf("an admin was told only an admin can change it:\n%s", out)
	}
	// /me reports the role and nothing about a scope grant, so the offer is
	// "as an admin you can", with the scope a scoped token would also need.
	if !strings.Contains(out, "As an admin you can change it (a scoped token also needs `analytics:write`):") {
		t.Errorf("the recipe's offer does not name the scope a scoped token needs:\n%s", out)
	}
	if !strings.Contains(out, "-d @body.json for a long document — the file holds the JSON body, not the bare text") {
		t.Errorf("the recipe's -d @file hint does not say the file holds the JSON body:\n%s", out)
	}
	// The recipe is part of the framing, above the document, not appended
	// after content an admin authored.
	recipeAt := strings.Index(out, "expectedVersion")
	bodyAt := strings.Index(out, "Report every amount in SEK.")
	if recipeAt < 0 || bodyAt < 0 || recipeAt > bodyAt {
		t.Errorf("the recipe must precede the document body (recipe=%d body=%d):\n%s", recipeAt, bodyAt, out)
	}
}

// A super-admin's level is BELOW an admin's (privilege counts down), and must
// not be read as "not an admin" by a check written the wrong way round.
func TestContextOffersTheWriteRecipeToASuperAdmin(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 0
	f.policy = &api.Policy{Content: testPolicy, Version: 2}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, `"expectedVersion":2}`) {
		t.Errorf("a super-admin was not given the write recipe:\n%s", out)
	}
}

// The genesis document has no rules and sits at version 1; a blanked one sits
// wherever the blanking left it. An admin is offered the recipe with the
// version the SERVER reported, whichever it is — never a literal.
func TestContextOffersAnAdminTheRecipeForAnUnwrittenPolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = adminPrivilegeLevel
	f.policy = &api.Policy{Content: "", Version: 4}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: none written yet. As an admin you can write one (a scoped token also needs `analytics:write`):\n    ronja api -X PUT") {
		t.Errorf("an admin with no policy was not offered to write one:\n%s", out)
	}
	if !strings.Contains(out, `"expectedVersion":4}`) {
		t.Errorf("the recipe does not carry the served version:\n%s", out)
	}
}

// A member reads the rules and is told who can change them — no recipe, so a
// non-admin's coding agent is not handed a PUT the gate will refuse.
func TestContextTellsAMemberOnlyAnAdminCanChangeThePolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Only an admin can change it.") {
		t.Errorf("a member was not told only an admin can change it:\n%s", out)
	}
	if strings.Contains(out, "expectedVersion") {
		t.Errorf("a member was handed the write recipe:\n%s", out)
	}
	// And the same for a blank document: "none written yet" and nothing more.
	f.policy = &api.Policy{Content: "", Version: 1}
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: none written yet.\n") || strings.Contains(out, "expectedVersion") {
		t.Errorf("a member with no policy was offered to write one:\n%s", out)
	}
}

// The --json contract is unchanged by the role: `organizationPolicy` is
// exactly content + version, and the recipe is prose for the human output
// only.
func TestContextJSONIsUnchangedForAnAdmin(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = adminPrivilegeLevel
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload := decodeJSON(t, out)
	doc, ok := payload["organizationPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("organizationPolicy missing from --json payload: %v", payload)
	}
	if len(doc) != 2 || doc["content"] != testPolicy || doc["version"] != float64(7) {
		t.Errorf("organizationPolicy = %v, want exactly {content, version}", doc)
	}
	if strings.Contains(out, "expectedVersion") {
		t.Errorf("the write recipe leaked into --json:\n%s", out)
	}
}
