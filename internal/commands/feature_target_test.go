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

// Which ORGANIZATION a folder is bound to, and whether the feature it names is
// one this credential can reach there.
//
// The two questions are one defect. `init` used to answer neither — it wrote a
// binding for whichever organization the credential happened to reach and a
// feature id nothing had ever looked at — so the first time either was wrong
// was several files later, at `push`, as "no rows (HTTP 400)". These tests pin
// the answers: init says which organization it bound, refuses a feature it
// cannot reach there, and push and validate name the organization instead of
// reporting a fact about a query.

// A feature this credential cannot reach is refused AT INIT, and the refusal
// leaves the directory exactly as it found it — the negative control for the
// whole design, since the alternative to a check here is a committed manifest
// naming a feature that was never going to work.
//
// Both server spellings, because the CLI ships on its own tag: an instance
// predating the backend half answers the raw table.ErrNoRows sentinel, a newer
// one answers "feature not found", and the 404 is the third door onto the same
// fact (a feature in THIS organization the caller may not read).
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

// The refusal names the CREDENTIAL as well as the organization, because "wrong
// feature" and "right feature, wrong organization" take different fixes and the
// reader cannot tell them apart from the id alone.
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

// `app init` is the same bootstrap with the same hazard, and `app` has no
// delete — so a folder bound to an unreachable feature is worse there, not
// better.
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
	// Nothing scaffolded either: the entrypoint is written after the check.
	if _, statErr := os.Stat(filepath.Join(dir, "App.tsx")); statErr == nil {
		t.Error("a refused app init scaffolded an entrypoint")
	}
}

// `ronja bind --stack <name> --feature <id>` is the documented way to promote a
// folder to a SECOND organization, which makes it the other door onto the same
// silent bind — the same id written into the same committed file, for an
// organization nobody checked it against.
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

// An instance that does not ANSWER is not a verdict on the feature. Init needs
// a credential; making it also need a live instance would turn an outage into
// "cannot start a folder", which is the class of message this change removes.
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

// The organization is reported at the moment the binding is written, not left
// to be discovered by `status` later. `url` alone was never the target — a
// folder is bound to (instance, organization).
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

// The default `wf push` validates BEFORE it writes, so an unreachable feature
// surfaces at the validate step — where the old message was "no rows (HTTP 400)
// (use --no-validate to skip this check)", advice that would have failed
// identically one step later at create.
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

// The generation of backend in between the two named codes: POST
// /workflow/validate answered gt.NewNotfoundError("not found") for a
// same-organization feature the caller cannot read, so the whole refusal reached
// the reader as "validate before pushing: not found (HTTP 404) (use
// --no-validate to skip this check)" — a sentence with no feature in it, ending
// in advice to skip the only check that named the problem.
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

// The status gate on that arm, which is the whole reason it is safe. "not found"
// is generic enough for any route to answer about anything, so only the 404 that
// a feature read really produces may be told as a story about the feature; the
// same words on a 400 keep the server's own message.
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

// The other thing that arrives at the same site: an instance that did not
// ANSWER. "(use --no-validate to skip this check)" on a 5xx advises pushing
// files nobody has looked at, on the strength of a check that fell over — and
// it reads as though the folder were the problem. Nothing has been written at
// that point, because validate runs before every write on this path, so the
// honest report is that the push did not start.
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

// The same outage on `wf validate`, which is the command most people meet it
// on: it is what the docs tell you to run first, and it is the whole of `ronja
// sync check`'s workflow leg.
//
// It used to return the bare api.Error — "Internal server error (HTTP 500)" —
// which is a sentence about a request rendered where a reader is looking for a
// verdict on their files. Nothing is saved by this command either way, so the
// only thing an outage costs here is the check; saying so is the difference
// between "try again" and "start debugging your folder".
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
	// The status is still carried — a reader debugging their own instance needs
	// it — but it is no longer the whole message.
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("validate dropped the status: %v", err)
	}
}

// `wf validate` reaches the same refusal by the same route and must say the
// same thing: it is the command people run first.
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

// With --no-validate nothing asks the server about the feature until the CREATE,
// so the mapping has to exist at that site too — and it is the only site an
// older CLI ever had.
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

// `push --profile <other>` on a folder bound elsewhere keeps REFUSING —
// repointing a folder on the strength of which credential was passed is exactly
// the silent bind this change is about. What it owes the reader is BOTH
// organizations by name, and the one command that adds the second one.
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

// testProfile is one stored profile for writeProfiles, including the
// organization NAME — which is what every cross-organization refusal prints,
// and which writeProfile's positional form has no room for.
// A CANCELLED request is not an outage, and the difference decides whether a
// folder gets scaffolded. root.go installs signal.NotifyContext, so Ctrl-C
// during the probe surfaces here as a *url.Error wrapping context.Canceled —
// the same shape a dead instance produces. Read as an outage it becomes a
// warning and init carries on writing files the reader just asked it to stop
// writing.
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

// A 401/403 is an answer about the CREDENTIAL, not about the feature. GET
// /api/v2/feature/:id is ScopeStructure while the folder loops are
// ScopeAutomation and ScopeData, so a PAT minted with only `automation:write`
// can push this folder perfectly well and still be refused the probe. Refusing
// init there stops a loop that was going to work, over a question the credential
// was never allowed to ask.
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

// A probe that ran out of TIME is the same non-answer as an instance that never
// replied, and init must survive it for the same reason: scaffolding a folder
// cannot be made to depend on a live, prompt instance. api.Unanswered
// deliberately excludes a deadline — it stopped at our end, and the callers that
// ask about a write have to go and look — but this one writes nothing and wanted
// an answer about the feature, which a request that timed out did not produce.
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

// featureFixAdvice may only name the manifest as the source of the id when the
// manifest really is. featureIDFor falls back to the WORKFLOW's own featureID
// for a hand-written manifest that binds a workflow and records no feature, and
// sending that reader to a `ronja.json` key that is not in their file is the
// same wild-goose chase the advice exists to prevent — worse, because they will
// open the file to check.
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

// `pipeline init` is the third door onto the same silent bind, and the one with
// the least around it: a pipeline folder has no entrypoint and nothing to push
// until a .sql file exists, so a wrong featureID sits in a committed manifest
// until somebody writes and pushes a table.
//
// It also creates a DIRECTORY when given a path, which the refusal must come
// before — the same property TestPipelineInitCreatesNoDirectoryWhenItRefuses
// pins for a bad-shape id, now for one the instance itself refuses.
func TestPipelineInitCreatesNoDirectoryWhenItRefusesAForeignFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	// Deliberately NOT registered: an id in another organization and one that
	// does not exist are one answer here, exactly as RLS makes them one answer
	// on the instance.
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

// The same outage rule bind gets: an instance that does not ANSWER is not a
// verdict on the feature, and refusing the promotion over it would turn a 503
// into "cannot declare an environment".
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

// The refusal comes BEFORE the folder is opened, and that ordering is the point.
// openFolder runs adoptStack, which REWRITES ronja.json from the legacy
// `instances[]` shape into `stacks` the moment a bound folder is named with
// --stack — so a check made after the open would rewrite the customer's
// committed file and only then refuse.
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
