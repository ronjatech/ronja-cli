package commands

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The organization policy on `ronja context`: every way it can be missing must
// be said in one line, since silence reads as "no rules".

const testPolicy = "- Report every amount in SEK.\n- Every table name starts with the source system: `fortnox_…`, `hubspot_…`.\n"

// The two 403 bodies gt's requireScope sends (backend/lib/api/gt/scope.go), and
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
	if !strings.Contains(out, "\n## Organization policy\n\n") {
		t.Errorf("no policy section:\n%s", out)
	}
	if !strings.Contains(out, "Report every amount in SEK.") {
		t.Errorf("the content is not rendered verbatim:\n%s", out)
	}
	if !strings.Contains(out, "Standing rules Test Org's admins wrote") {
		t.Errorf("the framing does not name the organization:\n%s", out)
	}
	// A rule read without the framing becomes the task.
	if !strings.Contains(out, "not a description of your task") {
		t.Errorf("the framing sentence is missing:\n%s", out)
	}
	signedAt := strings.Index(out, "Signed in:")
	policyAt := strings.Index(out, "## Organization policy")
	credsAt := strings.Index(out, "## Use your credentials")
	if signedAt < 0 || policyAt < 0 || credsAt < 0 || !(signedAt < policyAt && policyAt < credsAt) {
		t.Errorf("policy must sit between the identity line and the credential recipe (signed=%d policy=%d creds=%d):\n%s",
			signedAt, policyAt, credsAt, out)
	}
	if strings.Contains(out, "# Ronja API\n") {
		t.Errorf("--no-docs did not skip the index:\n%s", out)
	}
}

// "None written" is keyed on content, not version: a blanked version-7
// document is still nothing to follow.
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

// Every member clears this route's role floor, so a scope 403 can name the
// missing scope.
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

// ⚠️ platform/auth also answers 403 for a dead credential (removed user,
// deleted organization), which must be told to sign in again, not that it
// still works or needs a scope.
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

	// "forbidden" means only the scope verdict to a consumer.
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

// The route serves every member, so a 404 means an older instance and must
// never read as "none".
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

// Me is the authority on identity: whatever the policy route served, a user in
// no organization gets "none" and no document.
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

// A scoped token cannot call the admin-scoped /me but can read the policy, and
// must not be told to sign in again.
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
	if !strings.Contains(out, "\n## Organization policy\n\n") {
		t.Errorf("a refused /me suppressed the policy:\n%s", out)
	}
	if !strings.Contains(out, "Standing rules this organization's admins wrote") {
		t.Errorf("the tenant-less wording is missing:\n%s", out)
	}
	if strings.Contains(out, "Test Org") {
		t.Errorf("an organization name appeared that /me never reported:\n%s", out)
	}

	// `authenticated` keeps meaning "/me answered"; identityStatus is the finer
	// verdict.
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

	// Signed out: RONJA_URL is still set, and signIn's config dir holds no
	// profile to fall back to.
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

// Two 401s are one dead credential, already reported by `Signed in: NO`.
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

// The drop is for 401 only: a shared 500 is the instance failing, not the
// credential.
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

// C0, the one-byte C1 CSI (U+009B) and bidi/zero-width characters are dropped
// from the terminal; --json carries the document byte for byte.
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

// The admin-authored organization name is set unquoted into our framing line.
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

	// A newline in the name would start a line of admin text reading as ours.
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
	// The applied view's general part: the whole document's version is not
	// its CAS token.
	if _, has := doc["version"]; has || doc["isGeneralPart"] != true {
		t.Errorf("organizationPolicy = %v, want the general part with no version", doc)
	}
	// No error key: --json carries a closed-set status, never error text.
	if _, present := payload["organizationPolicyError"]; present {
		t.Errorf("--json carries an organizationPolicyError key: %v", payload)
	}
	if payload["organizationPolicyStatus"] != "ok" {
		t.Errorf("organizationPolicyStatus = %v, want \"ok\"", payload["organizationPolicyStatus"])
	}
}

// A refused read leaves organizationPolicy absent, so only the status tells it
// from "no rules".
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

// An unwritten document is still carried at the served version: it is the
// expectedVersion for the first write, 1 for genesis but any value once
// blanked. Only the whole document carries one: an instance older than
// sections.
func TestContextJSONCarriesAnUnwrittenPolicyAtItsVersion(t *testing.T) {
	f := newFakeInstance(t)
	f.failApplied = http.StatusNotFound
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

// The whole document (an instance older than sections) carries its version
// into the recipe; the general part's recipe is
// TestContextAdminRecipeWritesTheGeneralPartThroughThePartRoute.
func TestContextOffersTheWriteRecipeToAnAdmin(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = adminPrivilegeLevel
	f.policy = &api.Policy{Content: testPolicy, Version: 7}
	f.failApplied = http.StatusNotFound
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
	// /me reports the role, not a scope grant, so the offer names the scope.
	if !strings.Contains(out, "As an admin you can change it (a scoped token also needs `analytics:write`):") {
		t.Errorf("the recipe's offer does not name the scope a scoped token needs:\n%s", out)
	}
	if !strings.Contains(out, "-d @body.json for a long document — the file holds the JSON body, not the bare text") {
		t.Errorf("the recipe's -d @file hint does not say the file holds the JSON body:\n%s", out)
	}
	// Above the document, not after admin-authored content.
	recipeAt := strings.Index(out, "expectedVersion")
	bodyAt := strings.Index(out, "Report every amount in SEK.")
	if recipeAt < 0 || bodyAt < 0 || recipeAt > bodyAt {
		t.Errorf("the recipe must precede the document body (recipe=%d body=%d):\n%s", recipeAt, bodyAt, out)
	}
}

// Privilege counts down: a super-admin's level is below an admin's.
func TestContextOffersTheWriteRecipeToASuperAdmin(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 0
	f.policy = &api.Policy{Content: testPolicy, Version: 2}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "ronja api -X PUT /api/v2/policy/org") {
		t.Errorf("a super-admin was not given the write recipe:\n%s", out)
	}
}

// The recipe carries the served version, never a literal 1 — on the whole
// document, which an instance older than sections serves.
func TestContextOffersAnAdminTheRecipeForAnUnwrittenPolicy(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = adminPrivilegeLevel
	f.policy = &api.Policy{Content: "", Version: 4}
	f.failApplied = http.StatusNotFound
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

// A member is not handed a PUT the gate will refuse.
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
	f.policy = &api.Policy{Content: "", Version: 1}
	out, err = runCLI(t, t.TempDir(), "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "Organization policy: none written yet.\n") || strings.Contains(out, "expectedVersion") {
		t.Errorf("a member with no policy was offered to write one:\n%s", out)
	}
}

// The recipe is human output only; --json is unchanged by the role.
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
	if len(doc) != 2 || doc["content"] != testPolicy || doc["isGeneralPart"] != true {
		t.Errorf("organizationPolicy = %v, want exactly {content, isGeneralPart}", doc)
	}
	if strings.Contains(out, "expectedVersion") {
		t.Errorf("the write recipe leaked into --json:\n%s", out)
	}
}
