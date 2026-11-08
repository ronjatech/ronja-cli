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
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// writeLegacyRecord writes a .ronja/state.json the way a CLI before per-stack
// records did: one entry keyed by (instance, organization) alone.
func writeLegacyRecord(t *testing.T, root string, key wfdir.InstanceKey, sourceID string) {
	t.Helper()
	state := &wfdir.State{}
	stateSetKey(state, key, &wfdir.InstanceState{SourceID: sourceID})
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}
}

func recordsByStack(t *testing.T, root string) map[string]string {
	t.Helper()
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range state.Instances {
		if e.State != nil {
			out[e.Stack] = e.State.SourceID
		}
	}
	return out
}

// `bind --feature` is a declaring command: it may add a second stack on an
// organization the folder already names, and the legacy local record is given
// to the stack already there before the second one exists.
func TestBindDeclaresASecondStackOnOneOrganization(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{}`, "")
	writeLegacyRecord(t, root, wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}, "wf-prod")

	if _, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	dev, prod := stackOf(t, root, "dev"), stackOf(t, root, "prod")
	if dev.FeatureID != "col-dev" || dev.TenantID != testTenantID || prod.FeatureID != "col-1" {
		t.Fatalf("stacks after bind: dev %+v, prod %+v", dev, prod)
	}
	if got := recordsByStack(t, root); len(got) != 1 || got["prod"] != "wf-prod" {
		t.Fatalf("the legacy record was not given to prod before dev was declared: %v", got)
	}
}

// Two stacks naming one feature would deploy the same rows twice.
func TestBindRefusesASecondStackOnTheSameFeature(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{}`, "")

	_, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), `stack "prod" already deploys into feature col-1`) {
		t.Fatalf("bind onto prod's feature: %v", err)
	}
	if _, declared := loadStacks(t, root)["dev"]; declared {
		t.Fatal("dev was declared anyway")
	}
}

// A command that does not declare a stack is still refused a name the folder
// does not declare on this organization: `--stack mian` is a typo, and the
// refusal names every stack that is there.
func TestPushRefusesAMistypedStackOnAFolderWithTwoStacks(t *testing.T) {
	f := newFakeSearchInstance(t)
	signInTo(t, f.URL())
	root := bindCmdFolder(t, f, `{}`, "")
	if _, err := runCLI(t, root, "bind", "--stack", "main", "--feature", "col-main", "--yes"); err != nil {
		t.Fatalf("declare main: %v", err)
	}

	_, err := runCLI(t, root, "wf", "push", "--stack", "mian")
	if err == nil {
		t.Fatal("--stack mian was accepted")
	}
	for _, want := range []string{`"main" and "prod"`, `there is no stack "mian"`, "--stack main or --stack prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to say %q", err, want)
		}
	}
	if _, declared := loadStacks(t, root)["mian"]; declared {
		t.Fatal("mian was declared")
	}
}

// A legacy record belongs to the one stack declared on its organization, and
// is renamed to it on the next save.
func TestALegacyRecordIsUsedByTheOneStackAndRenamedToIt(t *testing.T) {
	m := &wfdir.Manifest{Stacks: map[string]wfdir.Stack{"main": {URL: "http://x.test", TenantID: "ten-a"}}}
	key := wfdir.InstanceKey{URL: "http://x.test", TenantID: "ten-a"}
	state := &wfdir.State{}
	stateSetKey(state, key, &wfdir.InstanceState{SourceID: "wf-main"})
	f := &folder{Root: t.TempDir(), Manifest: m, State: state, Stack: "main", Key: key}
	f.claimLegacyBaseline()
	if got := f.State.For(f.Baseline()); got == nil || got.SourceID != "wf-main" {
		t.Fatalf("main does not read its legacy record: %+v", got)
	}
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		t.Fatal(err)
	}
	if got := recordsByStack(t, f.Root); got["main"] != "wf-main" || len(got) != 1 {
		t.Fatalf("records after save = %v, want the one record renamed to main", got)
	}
}

// Two stacks on one organization (a git merge brought them together) and a
// legacy record: neither uses it, and one line says so.
func TestTwoStacksLeaveALegacyRecordUnclaimedAndSaySo(t *testing.T) {
	key := wfdir.InstanceKey{URL: "http://x.test", TenantID: "ten-a"}
	m := &wfdir.Manifest{Stacks: map[string]wfdir.Stack{
		"dev":  {URL: key.URL, TenantID: key.TenantID, FeatureID: "col-dev"},
		"main": {URL: key.URL, TenantID: key.TenantID, FeatureID: "col-main"},
	}}
	state := &wfdir.State{}
	stateSetKey(state, key, &wfdir.InstanceState{SourceID: "wf-?"})
	var stderr string
	for _, stack := range []string{"dev", "main"} {
		f := &folder{Root: t.TempDir(), Manifest: m, State: state, Stack: stack, Key: key}
		stderr += captureStderr(t, func() {
			f.claimLegacyBaseline()
			f.claimLegacyBaseline()
		})
		if got := f.State.For(f.Baseline()); got != nil {
			t.Fatalf("stack %s used the unclaimed record: %+v", stack, got)
		}
	}
	if n := strings.Count(stderr, "neither uses it"); n != 2 {
		t.Fatalf("want one note per stack, got %d:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "dev, main") {
		t.Fatalf("the note does not name the stacks:\n%s", stderr)
	}
}

func loadStacks(t *testing.T, root string) map[string]wfdir.Stack {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	return m.Stacks
}

// spacedSearch is a bind fake of one organization with two Spaces: Main holds
// col-1 (the folder's prod stack) and col-main, Dev holds col-dev and
// col-dev-other.
func spacedSearch(t *testing.T) *fakeSearchInstance {
	t.Helper()
	f := newFakeSearchInstance(t)
	f.featureSpace = map[string]string{"col-1": "space-main", "col-main": "space-main", "col-dev": "space-dev", "col-dev-other": "space-dev"}
	f.spaceNames = map[string]string{"space-main": "Main", "space-dev": "Dev"}
	f.defaultSpace = "space-main"
	f.rows = map[string]fakeBindRow{}
	signInTo(t, f.URL())
	return f
}

// Declaring a stack writes the Space its feature is in: the id it is checked
// by, and the name as a label.
func TestBindDeclaringAStackWritesItsSpace(t *testing.T) {
	f := spacedSearch(t)
	root := bindCmdFolder(t, f, `{}`, "")

	if _, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if dev := stackOf(t, root, "dev"); dev.SpaceID != "space-dev" || dev.Space != "Dev" {
		t.Fatalf("dev = %+v, want spaceID space-dev labelled Dev", dev)
	}
	if prod := stackOf(t, root, "prod"); prod.SpaceID != "" {
		t.Fatalf("the stack already there gained a Space: %+v", prod)
	}
}

// `bind --feature` with no --stack names the stack after the feature's Space.
func TestBindNamesANewStackAfterItsSpace(t *testing.T) {
	f := spacedSearch(t)
	root := bindCmdFolder(t, f, `{}`, "")

	if _, err := runCLI(t, root, "bind", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("bind --feature: %v", err)
	}
	if dev := stackOf(t, root, "dev"); dev.FeatureID != "col-dev" || dev.SpaceID != "space-dev" {
		t.Fatalf("stacks = %+v, want a dev stack on col-dev", loadStacks(t, root))
	}
}

// A default name a stack already has is refused, and the person names one.
func TestBindRefusesADefaultStackNameThatIsTaken(t *testing.T) {
	f := spacedSearch(t)
	root := bindCmdFolderOn(t, f, "main", "ten-other", `{}`)

	_, err := runCLI(t, root, "bind", "--feature", "col-main", "--yes")
	if err == nil || !strings.Contains(err.Error(), `already has a stack "main"`) || !strings.Contains(err.Error(), "--stack <name>") {
		t.Fatalf("bind --feature onto a taken default: %v", err)
	}
	if got := stackOf(t, root, "main"); got.FeatureID != "col-dev" || got.TenantID != "ten-other" {
		t.Fatalf("the existing main changed: %+v", got)
	}
}

// The picker on a Space stack: a read binds on the one same-named row in the
// Space, a write only on the one in the stack's own feature, a row in no
// feature never, and everything else is left for a hand bind.
func TestBindOnASpaceStackPicksOnlyInsideTheSpace(t *testing.T) {
	f := spacedSearch(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-main", "orders"), exactHit(api.SearchKindTable, "table-orders-dev", "orders")}
	f.hits["customers"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-customers-main", "customers")}
	f.hits["ledger"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-ledger-main", "ledger"), exactHit(api.SearchKindTable, "table-ledger-dev", "ledger")}
	f.hits["journal"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-journal-other", "journal")}
	f.hits["hubspot"] = []api.SearchHit{exactHit(api.SearchKindSecret, "secret-loose", "hubspot")}
	for id, feature := range map[string]string{"table-orders-main": "col-main", "table-orders-dev": "col-dev-other",
		"table-customers-main": "col-main", "table-ledger-main": "col-main", "table-ledger-dev": "col-dev",
		"table-journal-other": "col-dev-other", "secret-loose": ""} {
		f.rows[id] = fakeBindRow{featureID: feature}
	}
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}, "customers": {"kind": "table"},
	  "ledger": {"kind": "table", "write": true}, "journal": {"kind": "table", "write": true},
	  "hubspot": {"kind": "secret"}}`, "")

	out, _ := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes", "--json")
	var result bindResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got := stackOf(t, root, "dev").Bind; got["orders"] != "table-orders-dev" || got["ledger"] != "table-ledger-dev" || len(got) != 2 {
		t.Fatalf("dev bind = %v, want orders and ledger bound to Dev's rows and nothing else", got)
	}
	reasons := map[string]string{}
	for _, u := range result.Unresolved {
		reasons[u.Alias] = u.Reason
	}
	if reasons["customers"] != bindReasonNoMatch || reasons["journal"] != bindReasonWriteTarget || reasons["hubspot"] != bindReasonNoMatch || result.Space != "Dev" {
		t.Fatalf("unresolved = %+v (space %q)", result.Unresolved, result.Space)
	}
}

// A table another stack binds to a snapshot copy is bound to the copy's
// source, by id, when the source is in this stack's Space.
func TestBindProposesTheSourceOfACopyAnotherStackBinds(t *testing.T) {
	f := spacedSearch(t)
	f.rows["table-copy"] = fakeBindRow{featureID: "col-dev", name: "erp_orders", copiedFrom: "table-erp-main"}
	f.rows["table-erp-main"] = fakeBindRow{featureID: "col-main", name: "ERP orders"}
	root := bindCmdFolderOn(t, f, "dev", testTenantID, `{"erp_orders": {"kind": "table"}}`)
	m := loadAnyManifest(t, root)
	dev := m.Stacks["dev"]
	dev.Bind = map[string]string{"erp_orders": "table-copy"}
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "bind", "--stack", "main", "--feature", "col-main", "--yes"); err != nil {
		t.Fatalf("bind main: %v", err)
	}
	if got := stackOf(t, root, "main").Bind["erp_orders"]; got != "table-erp-main" {
		t.Fatalf("main binds erp_orders to %q, want the copy's source table-erp-main", got)
	}
	if len(f.terms) != 0 {
		t.Fatalf("a copy's source was searched for by name: %v", f.terms)
	}
}

// spacedInstance is a workflow fake of one organization with two Spaces:
// feat-1 is in Main, feat-dev in Dev.
func spacedInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := newFakeInstance(t)
	f.featureSpace = map[string]string{"feat-1": "space-main", "feat-dev": "space-dev", "feat-dev-2": "space-dev"}
	f.spaceNames = map[string]string{"space-main": "Main", "space-dev": "Dev"}
	f.defaultSpace = "space-main"
	f.tableFeature = map[string]string{}
	signIn(t, f)
	return f
}

// devStackFolder is a workflow folder initialised on the Dev feature as stack
// "dev", with deps declared and bound and main.py holding code.
func devStackFolder(t *testing.T, f *fakeInstance, deps string, bind map[string]string, code string) string {
	t.Helper()
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-dev", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	m := loadAnyManifest(t, root)
	if deps != "" {
		if err := json.Unmarshal([]byte(deps), &m.Dependencies); err != nil {
			t.Fatal(err)
		}
	}
	dev := m.Stacks["dev"]
	dev.Bind = bind
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", code)
	return root
}

// init --stack writes the Space; a feature id later pasted in from another
// Space is refused space_mismatch before anything is written.
func TestPushRefusesAStackWhoseFeatureIsInAnotherSpace(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	if dev := stackOf(t, root, "dev"); dev.SpaceID != "space-dev" || dev.Space != "Dev" {
		t.Fatalf("init --stack dev wrote %+v, want Space Dev", dev)
	}
	m := loadAnyManifest(t, root)
	dev := m.Stacks["dev"]
	dev.FeatureID = "feat-1"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonSpaceMismatch ||
		!strings.Contains(r.Detail, `stack "dev" is declared for Space Dev, and its feature Feature (feat-1) is in Space Main`) {
		t.Fatalf("push with Main's feature on the dev stack: %v", err)
	}
	if len(f.created) != 0 || len(requestsMatching(f, "PUT ")) != 0 {
		t.Fatalf("wrote something anyway: %v", f.Requests)
	}
}

// A write or run bind into another Space, or to a row in no feature, is
// refused bind_space_mismatch; a read bind into another Space is not.
func TestPushRefusesAWriteOrRunBindOutsideTheStacksSpace(t *testing.T) {
	f := spacedInstance(t)
	f.tableNames = map[string]string{"table-ledger-main": "ledger", "table-orders-main": "orders", "table-loose": "loose", "table-ledger-dev": "ledger"}
	f.tableFeature = map[string]string{"table-ledger-main": "feat-1", "table-orders-main": "feat-1", "table-ledger-dev": "feat-dev"}
	f.AddWorkflow(&api.Workflow{ID: "workflow-close-main", FeatureID: "feat-1", Title: "Close"})
	deps := `{"orders": {"kind": "table"}, "ledger": {"kind": "table", "write": true}, "close": {"kind": "workflow", "run": true}}`
	code := "{{ ref('orders') }}\n{{ write('ledger') }}\n{{ workflow('close') }}\n"

	for _, tc := range []struct {
		name string
		bind map[string]string
		want string
	}{
		{"write into Main", map[string]string{"orders": "table-orders-main", "ledger": "table-ledger-main", "close": "workflow-close-main"},
			`"ledger" is written by this folder and names table-ledger-main in Space Main`},
		{"write to a row in no feature", map[string]string{"orders": "table-orders-main", "ledger": "table-loose", "close": "workflow-close-main"},
			`"ledger" is written by this folder and names table-loose, which is in no feature and so in no Space`},
		{"run in Main", map[string]string{"orders": "table-orders-main", "ledger": "table-ledger-dev", "close": "workflow-close-main"},
			`"close" is run by this folder and names workflow-close-main in Space Main`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := devStackFolder(t, f, deps, tc.bind, code)
			_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
			r, ok := asManagedRefusal(err)
			if !ok || r.Reason != syncReasonBindSpaceMismatch || !strings.Contains(r.Detail, tc.want) {
				t.Fatalf("push: %v, want bind_space_mismatch saying %q", err, tc.want)
			}
			if strings.Contains(r.Detail, `"orders"`) {
				t.Fatalf("the read bind into Main was refused too: %s", r.Detail)
			}
			if len(f.created) != 0 {
				t.Fatalf("created %v anyway", f.created)
			}
		})
	}
}

// The same refusal in a dry run of `sync apply`, on the folder's line.
func TestSyncApplyDryRunRefusesABindOutsideTheStacksSpace(t *testing.T) {
	f := spacedInstance(t)
	f.tableNames = map[string]string{"table-ledger-main": "ledger"}
	f.tableFeature = map[string]string{"table-ledger-main": "feat-1"}
	tree := t.TempDir()
	root := devStackFolder(t, f, `{"ledger": {"kind": "table", "write": true}}`, map[string]string{"ledger": "table-ledger-main"}, "{{ write('ledger') }}\n")
	if err := os.Rename(root, filepath.Join(tree, "report")); err != nil {
		t.Fatal(err)
	}
	report, _ := applyStack(t, tree, "dev", "--dry-run", "--allow-create")
	if len(report.Folders) != 1 || report.Folders[0].Verdict != verdictRefused || report.Folders[0].Reason != syncReasonBindSpaceMismatch {
		t.Fatalf("dry run:\n%s", describeApply(report))
	}
}

// A raw id the code only reads in another Space is a `sync check` warning,
// not a refusal.
func TestARawIDReadFromAnotherSpaceIsASyncCheckWarning(t *testing.T) {
	f := spacedInstance(t)
	f.tableNames = map[string]string{"table-in-main": "in"}
	f.tableFeature = map[string]string{"table-in-main": "feat-1"}

	tree := t.TempDir()
	reader := devStackFolder(t, f, "", nil, "rows = {{ ref('table-in-main') }}\n")
	if err := os.Rename(reader, filepath.Join(tree, "report")); err != nil {
		t.Fatal(err)
	}
	out, _ := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	var report syncCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(report.Folders) != 1 || !strings.Contains(strings.Join(report.Folders[0].Warnings, "\n"), "main.py reads table-in-main in Space Main") ||
		report.Folders[0].Verdict == verdictBroken {
		t.Fatalf("sync check on a read from Main: %+v", report.Folders)
	}
}

// Two folders' same-named stacks that declare different Spaces cannot resolve
// an address between them, before any row is read. A stack that declares no
// Space is placed by the row its address resolves to
// (TestAnAddressIntoAnotherSpaceIsRefusedEvenForARead).
func TestAnAddressIsRefusedAcrossDeclaredSpaces(t *testing.T) {
	here := wfdir.Stack{SpaceID: "space-dev", Space: "Dev"}
	if got := addressSpaceMismatch(here, wfdir.Stack{SpaceID: "space-main", Space: "Main"}, "../report", "dev"); got == nil ||
		got.Reason != syncReasonAddressSpaceMismatch || !strings.Contains(got.Detail, `../report's stack "dev" is declared for Space Main and this folder's for Space Dev`) {
		t.Fatalf("across Spaces: %+v", got)
	}
	if got := addressSpaceMismatch(here, wfdir.Stack{SpaceID: "space-dev"}, "../report", "dev"); got != nil {
		t.Fatalf("the same Space refused: %+v", got)
	}
}

// A stack with no "spaceID" — every folder today, the two-folder Dev recipe's
// included — is not checked: no Space is read, and the push goes ahead.
func TestAStackWithNoSpaceIsNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	f.featureSpace = map[string]string{"feat-1": "space-main"}
	f.spaceNames = map[string]string{"space-main": "Main"}
	f.defaultSpace = "space-main"
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatal(err)
	}
	m := loadAnyManifest(t, root)
	dev := m.Stacks["dev"]
	dev.SpaceID, dev.Space = "", ""
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", "result = 1\n")
	f.Requests = nil
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "GET /api/v2/space/") || req == "GET /api/v2/feature/feat-1" {
			t.Fatalf("a stack with no Space was checked against one: %v", f.Requests)
		}
	}
}

// `sync apply --allow-create` into a feature that already holds a workflow of
// the folder's name is refused name_taken instead of creating a twin, and the
// adopt path the refusal names — the row's id recorded for the stack in the
// lock — makes the next apply update that row.
func TestAllowCreateRefusesToTwinASameNamedWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	f.managed.inventory = func(feature string) []api.ReferenceResource {
		var out []api.ReferenceResource
		for _, wf := range f.workflows {
			if wf.FeatureID == feature && wf.Lifecycle != api.LifecycleDraft {
				out = append(out, api.ReferenceResource{Kind: api.RefKindWorkflow, ID: wf.ID, Name: wf.Title})
			}
		}
		return out
	}
	f.AddWorkflow(&api.Workflow{ID: "workflow-board", Title: "Region Board", FeatureID: "feat-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "result = 1\n"})
	tree := t.TempDir()
	root := filepath.Join(tree, "board")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "main", "--title", "Region Board"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, root, "main.py", "result = 1\n")

	report, _ := applyStack(t, tree, "main", "--allow-create")
	line := report.Folders[0]
	if line.Verdict != verdictRefused || line.Reason != syncReasonNameTaken ||
		!strings.Contains(line.Detail, "already has a workflow called Region Board (workflow-board)") || !strings.Contains(line.Detail, `"workflowID": "workflow-board"`) {
		t.Fatalf("apply --allow-create beside a same-named workflow:\n%s", describeApply(report))
	}
	if len(f.created) != 0 {
		t.Fatalf("created a twin: %+v", f.created)
	}

	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	b := lock.Binding("main")
	b.WorkflowID = "workflow-board"
	lock.SetBinding("main", b)
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	again, err := applyStack(t, tree, "main", "--allow-create")
	if err != nil || again.Folders[0].Verdict != verdictApplied || len(f.created) != 0 {
		t.Fatalf("apply after adopting the row: %v (created %d)\n%s", err, len(f.created), describeApply(again))
	}
}

// `db migrate push --stack` remembers a database per stack, so the databases
// of two Spaces of one organization are told apart, and a push with no --stack
// does not pick up either.
func TestMigratePushRemembersADatabasePerStack(t *testing.T) {
	var hit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/authentication/me" {
			writeJSON(w, map[string]any{"user": map[string]any{"id": "usr-1"}, "tenant": map[string]any{"id": testTenantID, "name": "Test Org"}})
			return
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/api/v2/database/")
		if id, ok = strings.CutSuffix(id, "/migrations"); !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		hit = append(hit, id)
		writeJSON(w, map[string]any{"databaseID": id, "results": []map[string]any{{"name": "0001_leads", "outcome": "skipped", "seq": 1}}})
	}))
	t.Cleanup(srv.Close)
	signInTo(t, srv.URL)
	dir := migrationsDir(t)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"--stack", "dev", "--database", "mdb-dev"},
		{"--stack", "main", "--database", "mdb-main"},
		{"--stack", "dev"},
		{"--stack", "main"},
	} {
		if _, err := runCLI(t, dir, append([]string{"db", "migrate", "push"}, args...)...); err != nil {
			t.Fatalf("push %v: %v", args, err)
		}
	}
	if strings.Join(hit, ",") != "mdb-dev,mdb-main,mdb-dev,mdb-main" {
		t.Fatalf("pushed to %v", hit)
	}
	if _, err := runCLI(t, dir, "db", "migrate", "push"); err == nil || !strings.Contains(err.Error(), "not bound to a database") {
		t.Fatalf("push with no --stack used a stack's database: %v", err)
	}
	if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "qa"); err == nil || !strings.Contains(err.Error(), `no database for stack "qa"`) {
		t.Fatalf("push --stack qa: %v", err)
	}
}

// With no credential named, --stack picks the stored profile for the
// organization that stack names: a read with a note, a deploy only on a yes at
// a terminal. With no terminal a deploy refuses, naming the --profile to pass;
// a chosen profile is never overridden.
func TestStackPicksTheProfileStoredForItsOrganization(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", "result = 1\n")
	writeProfiles(t, "other",
		testProfile{Name: "other", URL: f.URL(), TenantID: "ten-other", Token: "tok-other"},
		testProfile{Name: "test", URL: f.URL(), TenantID: testTenantID, Token: "tok-test"})
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, root, "wf", "status", "--stack", "dev"); err != nil {
			t.Fatalf("status --stack dev under another organization's current profile: %v", err)
		}
	})
	if !strings.Contains(stderr, `Using profile "test", not "other"`) {
		t.Fatalf("status switched without saying so:\n%s", stderr)
	}
	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if err == nil || !strings.Contains(err.Error(), "pass --profile test") || len(f.created) != 0 {
		t.Fatalf("push with no terminal: %v (created %d)", err, len(f.created))
	}
	withTerminalStdin(t)
	withStdin(t, "n\n")
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err == nil || len(f.created) != 0 {
		t.Fatalf("push answered no: %v (created %d)", err, len(f.created))
	}
	withStdin(t, "y\n")
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil || len(f.created) != 1 {
		t.Fatalf("push answered yes: %v (created %d)", err, len(f.created))
	}
	if _, err := runCLI(t, root, "wf", "status", "--stack", "dev", "--profile", "other"); err == nil {
		t.Fatal("status with an explicit --profile for another organization was not refused")
	}
	if out, err := runCLI(t, root, "wf", "push", "--stack", "dev", "--profile", "other"); err == nil {
		t.Fatalf("an explicit --profile for another organization was overridden: %s", out)
	}
}

// Declaring a second stack on a folder still on the unnamed instances[] shape
// is refused before anything is written: taking the migration path would turn
// the existing deployment into the new stack.
func TestBindRefusesToDeclareOverAnUnnamedEntry(t *testing.T) {
	f := spacedSearch(t)
	root := t.TempDir()
	manifest := `{"formatVersion": 1, "kind": "workflow", "title": "Region Report", "entrypoint": "main.py",
	  "instances": [{"url": "` + f.URL() + `", "tenantID": "` + testTenantID + `", "featureID": "col-main", "workflowID": "workflow-1"}]}`
	writeLocal(t, root, wfdir.ManifestName, manifest)

	_, err := runCLI(t, root, "bind", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), `unnamed "instances" entry already deploys`) {
		t.Fatalf("bind --feature over an unnamed entry: %v", err)
	}
	if got := readFile(t, root, wfdir.ManifestName); got != manifest {
		t.Fatalf("ronja.json was rewritten:\n%s", got)
	}
}

// Declaring a stack reads the Space its feature is in; when that read fails,
// the stack is not declared at all, rather than declared unchecked, and init
// has written nothing, not even its starter.
func TestDeclaringAStackRefusesWhenItsSpaceCannotBeRead(t *testing.T) {
	t.Run("bind", func(t *testing.T) {
		f := spacedSearch(t)
		root := bindCmdFolder(t, f, `{}`, "")
		f.featureFailFrom = map[string]int{"col-dev": 2}
		_, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes")
		if err == nil || !strings.Contains(err.Error(), "could not read which Space feature col-dev is in") || !strings.Contains(err.Error(), "try again") {
			t.Fatalf("bind with the Space read failing: %v", err)
		}
		if _, declared := loadStacks(t, root)["dev"]; declared {
			t.Fatal("dev was declared anyway")
		}
	})
	t.Run("init", func(t *testing.T) {
		f := spacedInstance(t)
		reads := 0
		f.beforeServe = func(r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/v2/feature/feat-dev" {
				if reads++; reads >= 2 {
					f.featureStatus = map[string]int{"feat-dev": http.StatusServiceUnavailable}
				}
			}
		}
		root := t.TempDir()
		_, err := runCLI(t, root, "wf", "init", "--feature", "feat-dev", "--stack", "dev")
		if err == nil || !strings.Contains(err.Error(), "could not read which Space feature feat-dev is in") {
			t.Fatalf("init with the Space read failing: %v", err)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("init wrote %v anyway: a refusal must leave the directory as it found it", entries)
		}
	})
}

// `bind --feature` run again on the feature its stack already names goes on
// with that stack, rather than refusing the name it would default to.
func TestBindAgainOnAStacksOwnFeatureReusesTheStack(t *testing.T) {
	f := spacedSearch(t)
	root := bindCmdFolder(t, f, `{}`, "")
	if _, err := runCLI(t, root, "bind", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	before := loadStacks(t, root)
	if _, err := runCLI(t, root, "bind", "--feature", "col-dev", "--yes"); err != nil {
		t.Fatalf("bind again on dev's feature: %v", err)
	}
	after := loadStacks(t, root)
	if len(after) != len(before) || after["dev"].FeatureID != "col-dev" || after["dev"].SpaceID != "space-dev" {
		t.Fatalf("stacks before %+v, after %+v", before, after)
	}
}

// A tree command given --stack, with no credential chosen, uses the stored
// profile for the one organization that stack names across the tree (an
// apply only on a yes), and refuses, naming them, when it names several.
func TestATreeCommandPicksTheProfileForItsStacksOrganization(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	for _, name := range []string{"report", "export"} {
		root := filepath.Join(tree, name)
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev", "--title", name); err != nil {
			t.Fatal(err)
		}
		writeLocal(t, root, "main.py", "result = 1\n")
	}
	writeProfiles(t, "other",
		testProfile{Name: "other", URL: f.URL(), TenantID: "ten-other", Token: "tok-other"},
		testProfile{Name: "test", URL: f.URL(), TenantID: testTenantID, Token: "tok-test"})
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")

	if _, err := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--allow-create"); err == nil || !strings.Contains(err.Error(), "pass --profile test") || len(f.created) != 0 {
		t.Fatalf("apply with no terminal: %v (created %d)", err, len(f.created))
	}
	withTerminalStdin(t)
	withStdin(t, "y\n")
	stderr := captureStderr(t, func() {
		if out, err := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--allow-create"); err != nil {
			t.Fatalf("apply --stack dev under another organization's current profile: %v\n%s", err, out)
		}
	})
	if !strings.Contains(stderr, `Using profile "test"`) || len(f.created) != 2 {
		t.Fatalf("created %d workflows; stderr:\n%s", len(f.created), stderr)
	}

	m := loadAnyManifest(t, filepath.Join(tree, "export"))
	dev := m.Stacks["dev"]
	dev.TenantID = "ten-third"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(filepath.Join(tree, "export"), m); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range [][]string{{"sync", "apply", "--stack", "dev"}, {"sync", "check", "--stack", "dev"}} {
		_, err := runCLI(t, tree, cmd...)
		if err == nil || !strings.Contains(err.Error(), `stack "dev" names 2 organizations`) || !strings.Contains(err.Error(), "ten-third on ") ||
			!strings.Contains(err.Error(), testTenantID+" on ") || !strings.Contains(err.Error(), "use --profile or RONJA_TOKEN") {
			t.Fatalf("%v across two organizations: %v", cmd, err)
		}
		if code := exitCodeOf(err); code != syncExitUnknown {
			t.Fatalf("%v: exit %d", cmd, code)
		}
	}
}

// A single-folder push report names the stack and its Space on its first
// line, as a tree apply's does.
func TestAPushReportNamesItsStackAndSpaceFirst(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	out, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if first, _, _ := strings.Cut(out, "\n"); first != "  Stack:      dev · Space Dev" {
		t.Fatalf("first line %q, want the stack and Space:\n%s", first, out)
	}
}

// `init` and `clone` given only a feature outside the organization's default
// Space declare a stack named after that Space, so the folder is checked
// against it; a feature in the default Space is written exactly as before.
func TestInitAndCloneOutsideTheDefaultSpaceDeclareAStack(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		f := spacedInstance(t)
		f.defaultSpace = "space-main"
		root := t.TempDir()
		if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-dev"); err != nil {
			t.Fatalf("init: %v", err)
		}
		m := loadAnyManifest(t, root)
		if dev, ok := m.Stacks["dev"]; !ok || dev.FeatureID != "feat-dev" || dev.SpaceID != "space-dev" || dev.Space != "Dev" || m.ID == "" {
			t.Fatalf("init on a Dev feature wrote %s", readFile(t, root, wfdir.ManifestName))
		}
	})
	t.Run("clone", func(t *testing.T) {
		f := spacedInstance(t)
		f.defaultSpace = "space-main"
		f.AddWorkflow(&api.Workflow{ID: "workflow-dev", FeatureID: "feat-dev", Title: "Dev report", Lifecycle: api.LifecycleLive},
			api.WorkflowFile{Path: "main.py", Content: "result = 1\n"})
		root := filepath.Join(t.TempDir(), "report")
		if _, err := runCLI(t, t.TempDir(), "wf", "clone", "workflow-dev", root); err != nil {
			t.Fatalf("clone: %v", err)
		}
		if dev, ok := loadAnyManifest(t, root).Stacks["dev"]; !ok || dev.SpaceID != "space-dev" {
			t.Fatalf("clone of a Dev workflow wrote %s", readFile(t, root, wfdir.ManifestName))
		}
	})
	t.Run("default Space unchanged", func(t *testing.T) {
		written := func(spaces bool) string {
			f := newFakeInstance(t)
			if spaces {
				f.featureSpace = map[string]string{"feat-1": "space-main"}
				f.spaceNames = map[string]string{"space-main": "Main"}
				f.defaultSpace = "space-main"
			}
			signIn(t, f)
			root := filepath.Join(t.TempDir(), "report")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1"); err != nil {
				t.Fatalf("init: %v", err)
			}
			var out []string
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(root, path)
					out = append(out, rel+"\n"+strings.ReplaceAll(readFile(t, root, rel), f.URL(), "URL"))
				}
				return nil
			})
			return strings.Join(out, "\n---\n")
		}
		if before, after := written(false), written(true); before != after {
			t.Fatalf("init on a default-Space feature changed:\nwithout Spaces:\n%s\nwith:\n%s", before, after)
		}
	})
}

// classicDevFolder is devStackFolder made a classic folder (no folder id)
// whose lock records workflowID for its dev stack.
func classicDevFolder(t *testing.T, f *fakeInstance, workflowID string) string {
	t.Helper()
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	m := loadAnyManifest(t, root)
	m.ID = ""
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Stacks == nil {
		lock.Stacks = map[string]wfdir.LockStack{}
	}
	st := lock.Stacks["dev"]
	st.WorkflowID = workflowID
	lock.Stacks["dev"] = st
	if err := wfdir.SaveFolder(root, m, lock); err != nil {
		t.Fatal(err)
	}
	return root
}

// A classic Dev stack whose own workflow, as its lock records it, is Main's
// row is refused bind_space_mismatch by the push before anything is written,
// and by `sync check`; the stack's feature being in Dev is not enough.
func TestAStacksOwnRowInAnotherSpaceIsRefused(t *testing.T) {
	f := spacedInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "workflow-main", FeatureID: "feat-1", Title: "Report", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "result = 0\n"})
	root := classicDevFolder(t, f, "workflow-main")

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev", "--force")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonBindSpaceMismatch || !strings.Contains(r.Detail, "workflow workflow-main") || !strings.Contains(r.Detail, "Space Main") {
		t.Fatalf("push onto Main's workflow from the dev stack: %v", err)
	}
	if len(f.created) != 0 || len(requestsMatching(f, "PUT ")) != 0 || len(requestsMatching(f, "POST /api/v2/workflow/workflow-main")) != 0 {
		t.Fatalf("wrote something anyway: %v", f.Requests)
	}

	out, _ := runCLI(t, root, "sync", "check", "--stack", "dev", "--json")
	if !strings.Contains(out, `"reason": "`+syncReasonBindSpaceMismatch+`"`) || !strings.Contains(out, "workflow-main") {
		t.Fatalf("sync check did not place the stack's own row:\n%s", out)
	}
}

// Declaring a stack over a bound entry that names no feature reads the row it
// deploys first, and refuses when that row is in another feature.
func TestBindRefusesToDeclareAStackOverAnotherFeaturesRow(t *testing.T) {
	f := spacedSearch(t)
	f.rows["workflow-main"] = fakeBindRow{featureID: "col-main", name: "Report"}
	root := t.TempDir()
	manifest := `{"formatVersion": 1, "kind": "workflow", "title": "Region Report", "entrypoint": "main.py",
	  "instances": [{"url": "` + f.URL() + `", "tenantID": "` + testTenantID + `", "workflowID": "workflow-main"}]}`
	writeLocal(t, root, wfdir.ManifestName, manifest)

	_, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "workflow workflow-main, which is in feature col-main") {
		t.Fatalf("bind declaring dev over Main's workflow: %v", err)
	}
	if st, ok := loadStacks(t, root)["dev"]; ok && (st.FeatureID != "" || st.SpaceID != "") {
		t.Fatalf("dev was declared on the feature anyway: %+v", st)
	}
}

// A candidate whose Space cannot be read for any reason but being out of
// reach refuses the bind, rather than dropping the candidate and binding the
// one left.
func TestBindRefusesWhenACandidatesSpaceCannotBeRead(t *testing.T) {
	f := spacedSearch(t)
	f.hits["orders"] = []api.SearchHit{exactHit(api.SearchKindTable, "table-orders-dev", "orders"), exactHit(api.SearchKindTable, "table-orders-flaky", "orders")}
	f.rows["table-orders-dev"] = fakeBindRow{featureID: "col-dev"}
	f.rows["table-orders-flaky"] = fakeBindRow{featureID: "col-flaky"}
	f.featureFailFrom = map[string]int{"col-flaky": 1}
	root := bindCmdFolder(t, f, `{"orders": {"kind": "table"}}`, "")
	before := readFile(t, root, wfdir.ManifestName)

	_, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes")
	if err == nil || !strings.Contains(err.Error(), "check which Space table table-orders-flaky is in") {
		t.Fatalf("bind with a candidate's Space unreadable: %v", err)
	}
	if after := readFile(t, root, wfdir.ManifestName); after != before {
		t.Fatalf("ronja.json was rewritten:\n%s", after)
	}
}

// On a stack that records its Space the search is asked in that Space, so a
// same-Space row the organization-wide page would have cut off is proposed,
// and the Space filter still runs on every hit.
func TestBindOnASpaceStackSearchesInsideTheSpace(t *testing.T) {
	f := spacedSearch(t)
	var hits []api.SearchHit
	for i := range api.MaxSearchLimit {
		id := fmt.Sprintf("table-orders-main-%d", i)
		hits = append(hits, exactHit(api.SearchKindTable, id, "orders"))
		f.rows[id] = fakeBindRow{featureID: "col-main"}
	}
	f.hits["orders"] = append(hits, exactHit(api.SearchKindTable, "table-orders-dev", "orders"))
	f.rows["table-orders-dev"] = fakeBindRow{featureID: "col-dev-other"}
	root := bindCmdFolderOn(t, f, "dev", testTenantID, `{"orders": {"kind": "table"}}`)
	m := loadAnyManifest(t, root)
	dev := m.Stacks["dev"]
	dev.SpaceID, dev.Space = "space-dev", "Dev"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	if out, err := runCLI(t, root, "bind", "--stack", "dev", "--yes"); err != nil {
		t.Fatalf("bind: %v\n%s", err, out)
	}
	if got := stackOf(t, root, "dev").Bind["orders"]; got != "table-orders-dev" {
		t.Fatalf("dev binds orders to %q, want Dev's table-orders-dev", got)
	}
}
