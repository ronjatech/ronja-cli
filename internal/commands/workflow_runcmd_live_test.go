package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `wf run` in a folder that holds exactly what live holds, whatever its sync
// record says, and the refusals when it does not.

// assertRanLive fails unless exactly one run of wf-1 was started.
func assertRanLive(t *testing.T, f *fakeInstance, err error, stderr string) {
	t.Helper()
	if err != nil {
		t.Fatalf("a folder holding what is live was refused: %v\n%s", err, stderr)
	}
	if len(f.runRequests) != 1 || f.runRequests[0].WorkflowID != "wf-1" {
		t.Fatalf("runs = %+v, want one of wf-1", f.runRequests)
	}
}

// assertLiveRunRefused fails unless err refuses naming every want and nothing ran.
func assertLiveRunRefused(t *testing.T, f *fakeInstance, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("ran live from a folder that does not hold what is live")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// assertNoFileLine fails if the refusal lists a file change: its evidence is
// the settings.
func assertNoFileLine(t *testing.T, err error) {
	t.Helper()
	for _, label := range []string{"new here", "changed here", "deleted here"} {
		if err != nil && strings.Contains(err.Error(), label) {
			t.Errorf("refusal lists a file (%q) where only the settings differ: %v", label, err)
		}
	}
}

// runnableLiveFolder is liveFolder with a run the fake will answer.
func runnableLiveFolder(t *testing.T, f *fakeInstance, shape func(*api.Workflow)) string {
	t.Helper()
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	return liveFolder(t, f, shape)
}

// folderRecord is the folder's machine-written files, nil for one absent.
func folderRecord(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, path := range []string{".ronja/state.json", wfdir.ManifestName, "ronja.lock.json"} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[path] = body
	}
	return out
}

// A change list deploys Ronja's copy of the folder, so the folder's record
// still describes the workflow before the deploy while live holds its files.
func TestRunRunsAFolderWhoseDeployedProposalIsLive(t *testing.T) {
	t.Run("files equal live", func(t *testing.T) {
		f := newFakeInstance(t)
		root := runnableLiveFolder(t, f, nil)
		writeLocal(t, root, "main.py", "print(2)\n")
		f.putFile("wf-1", "main.py", "print(2)\n")
		before := folderRecord(t, root)
		if before[".ronja/state.json"] == nil {
			t.Fatal("the clone left no sync record to hold still")
		}

		var err error
		stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
		assertRanLive(t, f, err, stderr)
		if strings.Contains(stderr, "did not come from this folder") {
			t.Errorf("drift note on a folder that holds what is live:\n%s", stderr)
		}
		// A run reads; re-anchoring the record is a sync verb's business.
		for path, body := range folderRecord(t, root) {
			if string(body) != string(before[path]) {
				t.Errorf("the run rewrote %s:\nbefore %q\nafter  %q", path, before[path], body)
			}
		}
	})
	// Push compares the title trimmed, so a title it would not write is one
	// live holds.
	t.Run("a title that differs only in spaces", func(t *testing.T) {
		f := newFakeInstance(t)
		root := runnableLiveFolder(t, f, func(w *api.Workflow) { w.Title = "Report" })
		writeLocal(t, root, "main.py", "print(2)\n")
		f.putFile("wf-1", "main.py", "print(2)\n")
		m := loadAnyManifest(t, root)
		m.Title = "Report "
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}

		var err error
		stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
		assertRanLive(t, f, err, stderr)
	})
}

// The record still lists the proposed file; the refusal names only the edit
// that is not live.
func TestRunRefusesAnEditMadeAfterTheProposal(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	f.putFile("wf-1", "main.py", "print(2)\n")
	f.putFile("wf-1", "proposed.py", "print(5)\n")
	writeLocal(t, root, "proposed.py", "print(5)\n")
	writeLocal(t, root, "main.py", "print(3)\n")

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "1 change(s) that are not in the live workflow", "changed here   main.py")
	if err != nil && strings.Contains(err.Error(), "proposed.py") {
		t.Errorf("refusal names a file live holds: %v", err)
	}
}

// The runtime is a setting push writes (a raise) or refuses (a lowering), so a
// folder declaring another runtime than live's is not live. A lowering is said
// in push's own words, since push and publish cannot take it live.
func TestRunRefusesWhenTheRuntimeDisagreesWithLive(t *testing.T) {
	for _, tc := range []struct {
		name            string
		declared, lives int
		want            string
	}{
		{"a raise push makes", 3, 2, "ronja.json sets runtime 3, live runs 2"},
		{"a lowering push refuses", 2, 3, "cannot be lowered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			root := runnableLiveFolder(t, f, func(w *api.Workflow) { w.RuntimeVersion = tc.lives })
			f.putFile("wf-1", "main.py", "print(2)\n")
			writeLocal(t, root, "main.py", "print(2)\n")
			m := loadAnyManifest(t, root)
			m.Runtime = tc.declared
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}

			_, err := runCLI(t, root, "wf", "run")
			assertLiveRunRefused(t, f, err, "1 change(s) that are not in the live workflow", tc.want)
			assertNoFileLine(t, err)
		})
	}
}

// Files alone are not the workflow: an entrypoint the folder names and live
// does not would run other code than the folder's. The refusal names the
// setting, which `wf status` does not show.
func TestRunRefusesWhenTheSettingsDisagreeWithLive(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, func(w *api.Workflow) { w.Entrypoint = "main.py" })
	f.putFile("wf-1", "main.py", "print(2)\n")
	f.putFile("wf-1", "other.py", "print(3)\n")
	writeLocal(t, root, "main.py", "print(2)\n")
	writeLocal(t, root, "other.py", "print(3)\n")
	m := loadAnyManifest(t, root)
	m.Entrypoint = "other.py"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "1 change(s) that are not in the live workflow", "ronja.json sets entrypoint other.py, live has main.py")
	assertNoFileLine(t, err)
	if err != nil && strings.Contains(err.Error(), "wf status") {
		t.Errorf("refusal points at wf status, which does not show the entrypoint: %v", err)
	}
}

// The language is fixed at create, so push refuses the difference, and the
// refusal says so in push's words.
func TestRunRefusesWhenTheLanguageDisagreesWithLive(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")
	f.putFile("wf-1", "main.py", "print(2)\n")
	f.workflows["wf-1"].Language = wfdir.LanguageJS

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "1 change(s) that are not in the live workflow", "language is fixed when it is created")
	assertNoFileLine(t, err)
}

// A file and a setting both differing are both listed, and both counted.
func TestRunListsFilesAndSettingsThatDisagreeWithLive(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, func(w *api.Workflow) { w.Title = "Report" })
	f.putFile("wf-1", "main.py", "print(9)\n")
	writeLocal(t, root, "main.py", "print(2)\n")
	m := loadAnyManifest(t, root)
	m.Title = "Digest"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "2 change(s) that are not in the live workflow",
		"changed here   main.py", `ronja.json sets title "Digest", live has "Report"`)
}

func TestRunRefusesAProposalAColleaguePublishedOver(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")
	f.putFile("wf-1", "main.py", "print(9)\n")

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "not in the live workflow", "main.py")
}

// The same paths both ways, not just the folder's paths agreeing.
func TestRunRefusesWhenLiveAndTheFolderHoldDifferentFiles(t *testing.T) {
	t.Run("live holds a file the folder lacks", func(t *testing.T) {
		f := newFakeInstance(t)
		root := runnableLiveFolder(t, f, nil)
		writeLocal(t, root, "main.py", "print(2)\n")
		f.putFile("wf-1", "main.py", "print(2)\n")
		f.putFile("wf-1", "extra.py", "print(3)\n")

		_, err := runCLI(t, root, "wf", "run")
		assertLiveRunRefused(t, f, err, "not in the live workflow")
	})
	t.Run("the folder holds a file live lacks", func(t *testing.T) {
		f := newFakeInstance(t)
		root := runnableLiveFolder(t, f, nil)
		writeLocal(t, root, "main.py", "print(2)\n")
		writeLocal(t, root, "extra.py", "print(3)\n")
		f.putFile("wf-1", "main.py", "print(2)\n")

		_, err := runCLI(t, root, "wf", "run")
		assertLiveRunRefused(t, f, err, "not in the live workflow", "extra.py")
	})
}

// A listing that fails is never a pass: the answer is the record's, as before
// the folder was compared with live.
func TestRunFallsBackToItsRecordWhenLiveCannotBeRead(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")
	f.putFile("wf-1", "main.py", "print(2)\n")
	f.failFiles = http.StatusInternalServerError

	_, err := runCLI(t, root, "wf", "run")
	assertLiveRunRefused(t, f, err, "not in the live workflow", "main.py")
}

func TestRunRunsACloneFromGitThatHoldsWhatIsLive(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
		t.Fatalf("remove baseline: %v", err)
	}

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
	assertRanLive(t, f, err, stderr)
}

// The review path: the folder's record names the draft an admin committed.
func TestRunRunsAFolderWhoseReviewWasCommitted(t *testing.T) {
	f := newFakeInstance(t)
	root := runnableLiveFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := f.draftOf["wf-1"]
	if draftID == "" {
		t.Fatal("push opened no draft")
	}
	delete(f.draftOf, "wf-1")
	delete(f.workflows, draftID)
	f.putFile("wf-1", "main.py", "print(2)\n")

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
	assertRanLive(t, f, err, stderr)
}
