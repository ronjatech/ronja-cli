package commands

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Which organization a folder binds to, and whether the credential can reach
// the feature it names there: init reports the first and refuses on the second.

// Both server spellings, because the CLI ships on its own tag: an older instance
// answers 400 "no rows", a newer one "feature not found", and a 404 means a
// feature in this organization the caller may not read.
func TestInitRefusesAFeatureItCannotReach(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeInstance(t)
			f.featureStatus["feat-foreign"] = status
			signIn(t, f)
			dir := t.TempDir()

			_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-foreign")
			if err == nil {
				t.Fatal("init accepted a feature this credential cannot reach")
			}
			if !strings.Contains(err.Error(), "Test Org") {
				t.Errorf("the refusal does not name the organization it would have bound: %v", err)
			}
			if !strings.Contains(err.Error(), "feat-foreign") {
				t.Errorf("the refusal does not name the feature: %v", err)
			}
			if strings.Contains(err.Error(), "no rows") {
				t.Errorf("the refusal passes the server's query-shaped sentinel through: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
				t.Error("a refused init wrote a manifest naming a feature it had just refused")
			}
			if _, statErr := os.Stat(filepath.Join(dir, ".ronja")); statErr == nil {
				t.Error("a refused init wrote a local baseline")
			}
		})
	}
}

// A current backend answers `404 {"error":"not found"}`, the bare sentinel;
// explainFeatureUnreachable accepts that code only on a 404, and this proves
// the gate lets it through.
func TestInitRefusesAFeatureItCannotReachOnACurrentBackend(t *testing.T) {
	f := newFakeInstance(t)
	f.featureNotFoundBody = "not found"
	f.featureStatus["feat-foreign"] = http.StatusNotFound
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-foreign")
	if err == nil {
		t.Fatal("init accepted a feature this credential cannot reach")
	}
	// Discriminating: an unmatched code still fails init, but only the matcher
	// produces this sentence.
	if !strings.Contains(err.Error(), "has no feature") || !strings.Contains(err.Error(), "you can reach") {
		t.Errorf("the current backend's bare sentinel reached the reader unexplained: %v", err)
	}
	if !strings.Contains(err.Error(), "Test Org") {
		t.Errorf("the refusal does not name the organization it would have bound: %v", err)
	}
	if !strings.Contains(err.Error(), "feat-foreign") {
		t.Errorf("the refusal does not name the feature: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
		t.Error("a refused init wrote a manifest naming a feature it had just refused")
	}
}

// "Wrong feature" and "right feature, wrong organization" take different fixes,
// so the refusal names the profile as well as the organization.
func TestInitRefusalNamesTheProfile(t *testing.T) {
	f := newFakeInstance(t)
	f.featureStatus["feat-foreign"] = http.StatusBadRequest
	signOut(t, f)
	writeProfiles(t, "altris", testProfile{
		Name: "altris", URL: f.URL(), TenantID: testTenantID, TenantName: "Test Org", Token: "test-token",
	})

	_, err := runCLI(t, t.TempDir(), "wf", "init", "--feature", "feat-foreign")
	if err == nil {
		t.Fatal("init accepted a feature this credential cannot reach")
	}
	if !strings.Contains(err.Error(), `profile "altris"`) {
		t.Errorf("the refusal does not name the profile in play: %v", err)
	}
	if !strings.Contains(err.Error(), "Test Org") {
		t.Errorf("the refusal does not name the organization: %v", err)
	}
}

func TestAppInitRefusesAFeatureItCannotReach(t *testing.T) {
	f := newFakeAppInstance(t)
	f.featureStatus["feat-foreign"] = http.StatusBadRequest
	signInApp(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "app", "init", "--feature", "feat-foreign")
	if err == nil {
		t.Fatal("app init accepted a feature this credential cannot reach")
	}
	if !strings.Contains(err.Error(), "has no feature feat-foreign you can reach") {
		t.Errorf("refusal = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
		t.Error("a refused app init wrote a manifest")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "App.tsx")); statErr == nil {
		t.Error("a refused app init scaffolded an entrypoint")
	}
}

func TestBindWithFeatureRefusesAFeatureItCannotReach(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.featureStatus["col-prod"] = http.StatusBadRequest
	signInTo(t, f.URL())
	root := bindCmdFolderOn(t, f, "dev", "ten-other", `{}`)

	_, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-prod", "--yes")
	if err == nil {
		t.Fatal("bind declared a stack for a feature this credential cannot reach")
	}
	if !strings.Contains(err.Error(), "has no feature col-prod you can reach") {
		t.Errorf("refusal = %v", err)
	}
	if got := stackOf(t, root, "prod"); got.FeatureID != "" {
		t.Errorf("a refused bind declared the stack anyway: %+v", got)
	}
}

// An instance that does not answer is not a verdict on the feature: init must
// not need a live instance.
func TestInitContinuesWhenTheInstanceDoesNotAnswerAboutTheFeature(t *testing.T) {
	f := newFakeInstance(t)
	f.featureStatus["feat-1"] = http.StatusServiceUnavailable
	signIn(t, f)
	dir := t.TempDir()

	var out string
	stderr := captureStderr(t, func() {
		var err error
		out, err = runCLI(t, dir, "wf", "init", "--feature", "feat-1")
		if err != nil {
			t.Fatalf("init refused a folder over an instance that did not answer: %v", err)
		}
	})
	if !strings.Contains(stderr, "could not confirm") {
		t.Errorf("init said nothing about the check it could not make: %q", stderr)
	}
	if !strings.Contains(out, "Target:") || !strings.Contains(out, "Test Org") {
		t.Errorf("init did not report the organization it bound:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr != nil {
		t.Errorf("init wrote no manifest: %v", statErr)
	}
}

// A folder is bound to (instance, organization), so init reports both.
func TestInitReportsTheOrganizationItBinds(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "Target:     Test Org on "+f.URL()+" (not pushed yet)") {
		t.Errorf("init report does not name the target:\n%s", out)
	}
	if strings.Contains(out, "Instance:") {
		t.Errorf("init still reports a bare instance, which names half the binding:\n%s", out)
	}

	jsonDir := t.TempDir()
	out, err = runCLI(t, jsonDir, "wf", "init", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init --json: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["organization"] != "Test Org" {
		t.Errorf("organization = %v, want Test Org", payload["organization"])
	}
	if payload["tenantID"] != testTenantID {
		t.Errorf("tenantID = %v, want %s", payload["tenantID"], testTenantID)
	}
}

// `--no-validate` advice would fail identically one step later at create.
func TestPushExplainsAnUnreachableFeatureAtValidate(t *testing.T) {
	for _, code := range []string{"no rows", "feature not found"} {
		t.Run(code, func(t *testing.T) {
			f, root := clonedFolder(t)
			f.failValidate = http.StatusBadRequest
			f.failValidateMessage = code
			writeLocal(t, root, "main.py", "print('changed')\n")

			_, err := runCLI(t, root, "wf", "push")
			if err == nil {
				t.Fatal("push accepted a feature the server refused")
			}
			if !strings.Contains(err.Error(), "has no feature") ||
				!strings.Contains(err.Error(), "you can reach") {
				t.Errorf("push did not explain the refusal: %v", err)
			}
			if strings.Contains(err.Error(), code) {
				t.Errorf("push passed the server's own wording through: %v", err)
			}
			if strings.Contains(err.Error(), "--no-validate") {
				t.Errorf("push advised skipping a check that would fail identically at create: %v", err)
			}
		})
	}
}

// An instance whose validate answers a bare 404 "not found" for a feature the
// caller cannot read.
func TestPushExplainsABareNotFoundAtValidate(t *testing.T) {
	f, root := clonedFolder(t)
	f.failValidate = http.StatusNotFound
	f.failValidateMessage = "not found"
	writeLocal(t, root, "main.py", "print('changed')\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push accepted a feature the server refused")
	}
	if !strings.Contains(err.Error(), "has no feature") ||
		!strings.Contains(err.Error(), "you can reach") {
		t.Errorf("push did not read the bare sentinel as an answer about the feature: %v", err)
	}
	if strings.Contains(err.Error(), "--no-validate") {
		t.Errorf("push advised skipping a check that would fail identically at create: %v", err)
	}
}

// "not found" is generic enough for any route, so only on a 404 is it read as
// an answer about the feature; on a 400 the server's message is kept.
func TestPushDoesNotReadABareNotFoundOnA400AsAFeatureAnswer(t *testing.T) {
	f, root := clonedFolder(t)
	f.failValidate = http.StatusBadRequest
	f.failValidateMessage = "not found"
	writeLocal(t, root, "main.py", "print('changed')\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push accepted a validate the server refused")
	}
	if strings.Contains(err.Error(), "has no feature") {
		t.Errorf("push told a feature story about a refusal that named no feature: %v", err)
	}
}

// On a 5xx, --no-validate advice would push unchecked files; validate runs
// before every write, so the push did not start.
func TestPushSaysTheInstanceDidNotAnswerTheValidate(t *testing.T) {
	f, root := clonedFolder(t)
	f.failValidate = http.StatusInternalServerError
	f.failValidateMessage = "Internal server error"
	writeLocal(t, root, "main.py", "print('changed')\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push proceeded on a validate that never answered")
	}
	if !strings.Contains(err.Error(), "the instance did not answer") {
		t.Errorf("push did not name the outage: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was pushed") {
		t.Errorf("push did not say the write never started: %v", err)
	}
	if !strings.Contains(err.Error(), "not a verdict on your files") {
		t.Errorf("push did not disclaim a verdict: %v", err)
	}
	if strings.Contains(err.Error(), "--no-validate") {
		t.Errorf("push advised skipping the check on an instance that did not answer: %v", err)
	}
}

func TestValidateSaysTheInstanceDidNotAnswer(t *testing.T) {
	f, root := clonedFolder(t)
	f.failValidate = http.StatusInternalServerError
	f.failValidateMessage = "Internal server error"

	_, err := runCLI(t, root, "wf", "validate")
	if err == nil {
		t.Fatal("validate reported a verdict from an instance that gave none")
	}
	if !strings.Contains(err.Error(), "the instance did not answer") {
		t.Errorf("validate did not name the outage: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was checked") {
		t.Errorf("validate did not say the check never happened: %v", err)
	}
	if !strings.Contains(err.Error(), "not a verdict on your files") {
		t.Errorf("validate did not disclaim a verdict: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("validate dropped the status: %v", err)
	}
}

func TestValidateExplainsAnUnreachableFeature(t *testing.T) {
	f, root := clonedFolder(t)
	f.failValidate = http.StatusBadRequest
	f.failValidateMessage = "feature not found"

	_, err := runCLI(t, root, "wf", "validate")
	if err == nil {
		t.Fatal("validate accepted a feature the server refused")
	}
	if !strings.Contains(err.Error(), "has no feature") {
		t.Errorf("validate did not explain the refusal: %v", err)
	}
}

// With --no-validate the create is the first request about the feature, so the
// mapping must exist there too.
func TestPushExplainsAnUnreachableFeatureAtCreate(t *testing.T) {
	f := newFakeInstance(t)
	f.failCreate = http.StatusBadRequest
	f.failCreateMessage = "no rows"
	signIn(t, f)
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, dir, "main.py", "print('hi')\n")

	_, err := runCLI(t, dir, "wf", "push", "--no-validate")
	if err == nil {
		t.Fatal("push created a workflow in a feature the server refused")
	}
	if !strings.Contains(err.Error(), "has no feature feat-1 you can reach") {
		t.Errorf("push did not explain the create refusal: %v", err)
	}
	if strings.Contains(err.Error(), "no rows") {
		t.Errorf("push passed the server's query-shaped sentinel through: %v", err)
	}
}

// Repointing a folder because of which credential was passed would be a silent
// bind, so push refuses and names both organizations and the bind command.
func TestPushUnderAnotherProfileNamesBothOrganizations(t *testing.T) {
	f, root := clonedFolder(t)
	rebindFolderTo(t, root, f.Key(), wfdir.Binding{WorkflowID: "wf-1", FeatureID: "feat-1"})
	signOut(t, f)
	writeProfiles(t, "other",
		testProfile{Name: "home", URL: f.URL(), TenantID: testTenantID, TenantName: "Test Org", Token: "test-token"},
		testProfile{Name: "other", URL: f.URL(), TenantID: "ten-other", TenantName: "Other Org", Token: "test-token"},
	)
	writeLocal(t, root, "main.py", "print('changed')\n")

	_, err := runCLI(t, root, "wf", "push", "--profile", "other")
	if err == nil {
		t.Fatal("push adopted another organization's binding because --profile named this one")
	}
	if !strings.Contains(err.Error(), "Test Org") {
		t.Errorf("the refusal does not name the organization the folder is bound to: %v", err)
	}
	if !strings.Contains(err.Error(), "Other Org") {
		t.Errorf("the refusal does not name the organization the credential reaches: %v", err)
	}
	if !strings.Contains(err.Error(), "ronja bind --stack") {
		t.Errorf("the refusal does not name the one command that adds this organization: %v", err)
	}
}

// Ctrl-C during the probe surfaces as a *url.Error wrapping context.Canceled,
// the same shape as a dead instance; read as an outage, init would carry on
// writing files the reader asked it to stop.
func TestConfirmFeatureInStopsOnACancelledContext(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	resolved := &config.Resolved{URL: f.URL(), Token: "test-token", TenantID: testTenantID, TenantName: "Test Org"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := confirmFeatureIn(ctx, "feat-1", resolved)
	if err == nil {
		t.Fatal("a cancelled probe was treated as an instance that did not answer, so init would have carried on")
	}
	if !strings.Contains(err.Error(), "feat-1") {
		t.Errorf("the refusal does not name the feature it was checking: %v", err)
	}
}

// A 401/403 is about the credential: GET /api/v2/feature/:id is ScopeStructure
// while the folder loops are ScopeAutomation and ScopeData, so a PAT that can
// push may still be refused the probe.
func TestInitContinuesWhenTheCredentialMayNotReadFeatures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeInstance(t)
			f.featureStatus["feat-1"] = status
			signIn(t, f)
			dir := t.TempDir()

			stderr := captureStderr(t, func() {
				if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
					t.Fatalf("init refused a folder over a probe this credential may not make: %v", err)
				}
			})
			if !strings.Contains(stderr, "may not read features") {
				t.Errorf("init did not say why it could not check: %q", stderr)
			}
			if strings.Contains(stderr, "did not answer") {
				t.Errorf("init blamed the instance for an answer about the credential: %q", stderr)
			}
			if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr != nil {
				t.Errorf("init wrote no manifest: %v", statErr)
			}
		})
	}
}

// A timed-out probe is a non-answer too. Unlike a write's caller, this probe
// writes nothing, so a deadline needs no follow-up look.
func TestInitContinuesWhenTheFeatureProbeTimesOut(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	failOnceWithATimeout(t, http.MethodGet, "/api/v2/feature/feat-1")
	dir := t.TempDir()

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
			t.Fatalf("init refused a folder over a probe that merely ran out of time: %v", err)
		}
	})
	if !strings.Contains(stderr, "did not answer in time") {
		t.Errorf("init did not say the probe timed out: %q", stderr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr != nil {
		t.Errorf("init wrote no manifest: %v", statErr)
	}
}

// featureIDFor falls back to the workflow's own featureID when the manifest
// records none, so featureFixAdvice must not send the reader to a manifest key
// that is not in their file.
func TestUnreachableFeatureAdviceNamesTheWorkflowWhenTheManifestRecordsNone(t *testing.T) {
	f, root := clonedFolder(t)
	// The row knows its feature; the committed file does not.
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report", FeatureID: "feat-1"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"})
	rebindFolderTo(t, root, f.Key(), wfdir.Binding{WorkflowID: "wf-1"})
	f.failValidate = http.StatusBadRequest
	f.failValidateMessage = "feature not found"

	_, err := runCLI(t, root, "wf", "validate")
	if err == nil {
		t.Fatal("validate accepted a feature the server refused")
	}
	if !strings.Contains(err.Error(), "records no feature") {
		t.Errorf("the advice claims an origin the file does not have: %v", err)
	}
	if strings.Contains(err.Error(), "That id comes from this folder's binding") {
		t.Errorf("the advice sends the reader to a manifest key that is not in their file: %v", err)
	}
}

// A pipeline folder has nothing to push until a .sql file exists, so a wrong
// featureID would sit in the manifest unnoticed; the refusal must also precede
// creating the directory.
func TestPipelineInitCreatesNoDirectoryWhenItRefusesAForeignFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	// Deliberately not registered: under RLS a foreign id and a missing one are
	// the same answer.
	signInPipeline(t, f)
	dir := t.TempDir()

	target := filepath.Join(dir, "sales")
	_, _, err := runPipelineCLI(t, dir, "pipeline", "init", target, "--feature", "collection-foreign")
	if err == nil {
		t.Fatal("pipeline init accepted a feature this credential cannot reach")
	}
	if !strings.Contains(err.Error(), "has no feature collection-foreign you can reach") {
		t.Errorf("refusal = %v", err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("a refused init created %s", target)
	}
	if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
		t.Error("a refused init wrote a manifest")
	}
}

func TestBindWithFeatureContinuesWhenTheInstanceDoesNotAnswer(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.featureStatus["col-prod"] = http.StatusServiceUnavailable
	signInTo(t, f.URL())
	root := bindCmdFolderOn(t, f, "dev", "ten-other", `{}`)

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-prod", "--yes"); err != nil {
			t.Fatalf("bind refused a promotion over an instance that did not answer: %v", err)
		}
	})
	if !strings.Contains(stderr, "could not confirm") {
		t.Errorf("bind said nothing about the check it could not make: %q", stderr)
	}
	if got := stackOf(t, root, "prod"); got.FeatureID != "col-prod" {
		t.Errorf("bind did not declare the stack it was told to: %+v", got)
	}
}

// The refusal must come before openFolder: its adoptStack rewrites a legacy
// `instances[]` ronja.json into `stacks` as soon as --stack names it.
func TestBindWithFeatureRefusesBeforeItRewritesTheManifest(t *testing.T) {
	f := newFakeSearchInstance(t)
	f.featureStatus["col-bad"] = http.StatusBadRequest
	signInTo(t, f.URL())

	root := t.TempDir()
	legacy := fmt.Sprintf(`{
  "kind": "workflow",
  "title": "Region Report",
  "entrypoint": "main.py",
  "instances": [{"url": %q, "tenantID": %q, "featureID": "col-dev", "workflowID": "wf-1"}]
}
`, f.URL(), testTenantID)
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-bad", "--yes"); err == nil {
		t.Fatal("bind declared a stack for a feature this credential cannot reach")
	}
	after, err := os.ReadFile(filepath.Join(root, wfdir.ManifestName))
	if err != nil {
		t.Fatalf("read manifest back: %v", err)
	}
	if string(after) != legacy {
		t.Errorf("a refused bind rewrote the committed manifest:\n%s", after)
	}
}
