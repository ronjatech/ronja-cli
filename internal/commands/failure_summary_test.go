package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// A push or publish with failures ends with each failed file and its reason,
// after the count sentence, so the reason is in the last lines an agent reads.

func TestPipelinePushListsTheFailedFilesLast(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	editFile(t, root, "revenue.sql", "SELECT 3 FROM {{ ref('table-orders') }}")
	f.syncOutcome["table-orders"] = "build_failed"
	f.syncMessage["table-orders"] = "The transformation is impossible: raw has no column `broken`."

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	want := "1 of 2 file(s) did not land:\n    orders.sql: "
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "no column `broken`") {
		t.Errorf("error = %v\n  want it to start %q and carry the build's reason", err, want)
	}
	// Negative control: a file that landed is not listed.
	if err != nil && strings.Contains(err.Error(), "revenue.sql") {
		t.Errorf("a file that landed is listed as failed: %v", err)
	}
	if got, _ := decodeJSON(t, out)["error"].(string); err != nil && got != err.Error() {
		t.Errorf("--json error = %q, want the same text as the command's error %q", got, err.Error())
	}
}

func TestPipelinePushCapsTheFailedFileList(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	editFile(t, root, "revenue.sql", "SELECT broken FROM raw")
	f.syncOutcome["table-orders"] = "build_failed"
	f.syncOutcome["table-revenue"] = "build_failed"
	for i := 0; i < 21; i++ {
		editFile(t, root, fmt.Sprintf("sales_%02d.sql", i), "SELECT broken FROM raw")
	}
	for i := 1; i < 200; i++ {
		f.syncOutcome[fmt.Sprintf("table-new-%d", i)] = "build_failed"
		f.syncOutcome[fmt.Sprintf("table-draft-%d", i)] = "build_failed"
	}

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a push where every file failed exited zero")
	}
	lines := strings.Split(err.Error(), "\n")
	if !strings.HasPrefix(lines[0], "23 of 23 file(s) did not land") {
		t.Fatalf("first line = %q", lines[0])
	}
	if got := len(lines); got != 1+20+1 {
		t.Errorf("%d lines, want the count, 20 files and one 'and N more':\n%s", got, err)
	}
	if last := lines[len(lines)-1]; last != "    and 3 more" {
		t.Errorf("last line = %q, want %q", last, "    and 3 more")
	}
}

func TestPipelinePublishListsTheFailedFilesLast(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.tables["table-draft-5"].Status = api.TableStatusBuildFailed
	f.buildErr["table-draft-5"] = &api.TableBuildError{
		Message: "The transformation is impossible: raw has no column `staged`.", Type: "impossible"}

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	want := "1 of 1 draft(s) were not published:\n    "
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), ".sql: ") {
		t.Errorf("error = %v\n  want it to start %q and name the file", err, want)
	}
	if got, _ := decodeJSON(t, out)["error"].(string); err != nil && got != err.Error() {
		t.Errorf("--json error = %q, want %q", got, err.Error())
	}
}

func TestAutomationPushListsTheFailedFilesLast(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-2", Name: "weekly", CronExpr: "0 3 * * 1", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1", "weekly.json": "job-2"},
		map[string]string{"nightly.json": anchoredAt(baseTime), "weekly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "enabled": true}`,
		"weekly.json":  `{"cronExpr": "0 4 * * 1", "enabled": true}`,
	})
	f.Pause("job-1", "user", laterTime)

	out, _, err := runAutomationCLI(t, dir, "automation", "push", "--json")
	want := "1 of 2 file(s) did not land:\n    nightly.json: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %v\n  want it to start %q", err, want)
	}
	// Negative control: the file that landed is not listed.
	if err != nil && strings.Contains(err.Error(), "weekly.json") {
		t.Errorf("a file that landed is listed as failed: %v", err)
	}
	if got, _ := decodeJSON(t, out)["error"].(string); err != nil && got != err.Error() {
		t.Errorf("--json error = %q, want %q", got, err.Error())
	}
}

func TestListFailedFilesKeepsTheFirstLineOfEachReason(t *testing.T) {
	got := listFailedFiles([]failedFile{
		{"orders.sql", "the build failed\n  at line 3\n  near SELECT"},
		{"revenue.sql", ""},
		{"margin.sql", "  \n"},
	})
	want := ":\n    orders.sql: the build failed\n    revenue.sql: no reason was given\n    margin.sql: no reason was given"
	if got != want {
		t.Errorf("listFailedFiles = %q\n  want %q", got, want)
	}
	if got := listFailedFiles(nil); got != "" {
		t.Errorf("listFailedFiles(nil) = %q, want nothing", got)
	}
}

// The human report has printed each file's block already, so its summary line
// is the count sentence alone, while the command's error line carries the list.
func TestPipelinePushHumanReportKeepsTheCountSentence(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	f.syncOutcome["table-orders"] = "build_failed"

	out, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if want := "\n  1 of 1 file(s) did not land. The drafts above hold what did land; fix and push again.\n"; !strings.Contains(out, want) {
		t.Errorf("report does not end with the count sentence alone %q:\n%s", want, out)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "1 of 1 file(s) did not land:\n    orders.sql: ") {
		t.Errorf("error = %v, want the count sentence and the failed file", err)
	}
}

// sync apply's detail is one line for every reason: the folder's units carry
// the per-file outcomes, and a --json reader keys on the detail.
func TestSyncApplyDetailIsOneLineWhenFilesFail(t *testing.T) {
	t.Run("pipeline", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		seedFeature(f, "private")
		f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw"})
		tree, _ := applyPipelineTree(t, f, map[string]string{"orders.sql": "SELECT broken FROM raw"},
			map[string]string{"orders.sql": "table-orders"}, nil)
		f.syncOutcome["table-orders"] = "build_failed"

		out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
		line := applyFolderLine(t, applyReportOf(t, out), "sales")
		if line.Reason != syncReasonNotDeployed || line.Detail != "1 of 1 file(s) did not land" {
			t.Errorf("line = %+v, want not_deployed with the count sentence alone", line)
		}
	})
	t.Run("automation", func(t *testing.T) {
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		f.failCreateNamed = map[string]int{"b_nightly": 400}
		tree := t.TempDir()
		root := filepath.Join(tree, "orders")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, lock := automationStack(f, "collection-1", nil, nil)
		writeAutomationFolder(t, root, manifest, lock, map[string]string{
			"a_hook.json":    `{"triggerKind": "webhook", "prompt": "Handle the delivery."}`,
			"b_nightly.json": `{"cronExpr": "0 2 * * *", "prompt": "Run the nightly."}`,
		})

		out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		line := applyFolderLine(t, applyReportOf(t, out), "orders")
		if line.Reason != syncReasonNotDeployed || line.Detail != "1 of 2 file(s) did not land" {
			t.Errorf("line = %+v, want not_deployed with the count sentence alone", line)
		}
	})
}

// A metric whose tags failed is listed with the reason the tag half gave.
func TestPipelinePushListsAFailedTagReconcileWithItsReason(t *testing.T) {
	r := &pipelinePushResult{Metrics: []pipelineMetricFileResult{{
		Path: "metrics/revenue.json",
		Tags: &pipelineMetricTagResult{Outcome: tagOutcomeFailed, Reason: "tag \"finance\" does not exist"},
	}}}
	got := r.failedFiles()
	if len(got) != 1 || got[0] != (failedFile{"metrics/revenue.json", "tag \"finance\" does not exist"}) {
		t.Errorf("failedFiles() = %+v, want the tag half's reason", got)
	}
}
