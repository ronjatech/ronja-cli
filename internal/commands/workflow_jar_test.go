package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

const testJarPath = "drivers/x.jar"

// jarSize is the 1.2 MB the plan's example driver weighs.
const jarSize = 1_258_291

// jarWorkflow is wf-1, live, holding main.py and (when jar is non-nil) the jar
// at testJarPath.
func jarWorkflow(t *testing.T, jar []byte) *fakeInstance {
	t.Helper()
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	if jar != nil {
		f.AddJar("wf-1", testJarPath, jar)
	}
	signIn(t, f)
	return f
}

func writeLocalBytes(t *testing.T, root, path string, body []byte) {
	t.Helper()
	writeLocal(t, root, path, string(body))
}

// A jar is uploaded once, as its bytes, by the raw PUT; the next push finds
// its sha256 already on the server and uploads nothing.
func TestPushUploadsAJarOnceAndASecondPushNothing(t *testing.T) {
	f := jarWorkflow(t, nil)
	root := cloneFolder(t, f, "wf-1")
	jar := fakeJarBytes("v1", jarSize)
	writeLocalBytes(t, root, testJarPath, jar)

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	puts := f.jarPuts()
	if len(puts) != 1 || puts[0].Path != testJarPath || puts[0].Size != jarSize {
		t.Fatalf("raw PUTs = %+v, want one of %s with %d bytes", puts, testJarPath, jarSize)
	}
	if puts[0].BaseSha256 == nil || *puts[0].BaseSha256 != "" {
		t.Errorf("the first upload asserted %v, want \"\" (the jar must not exist yet)", puts[0].BaseSha256)
	}
	draftID := f.draftOf["wf-1"]
	if got, ok := f.JarOf(draftID, testJarPath); !ok || !bytes.Equal(got, jar) {
		t.Fatalf("the draft does not hold the jar's bytes (found %v)", ok)
	}

	f.Requests = nil
	out, err = runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	if !decodeJSON(t, out)["upToDate"].(bool) {
		t.Errorf("the second push was not up to date: %s", out)
	}
	if puts := f.jarPuts(); len(puts) != 1 {
		t.Errorf("the second push uploaded again: %+v", puts)
	}
	if writes := fileAndMetadataWrites(f); len(writes) != 0 {
		t.Errorf("the second push wrote %v", writes)
	}
}

// A changed jar is replaced with the jar's sha256 as its precondition, and a
// jar the folder dropped is deleted with that same digest: what the server
// checks a jar against, never a hash of its empty text.
func TestPushReplacesAndDeletesAJarByItsSha256(t *testing.T) {
	old := fakeJarBytes("v1", 4000)
	f := jarWorkflow(t, old)
	root := cloneFolder(t, f, "wf-1")
	jar := fakeJarBytes("v2", 5000)
	writeLocalBytes(t, root, testJarPath, jar)

	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	puts := f.jarPuts()
	if len(puts) != 1 || puts[0].BaseSha256 == nil || *puts[0].BaseSha256 != jarSHA(old) {
		t.Fatalf("raw PUTs = %+v, want one asserting the old jar's sha256 %s", puts, jarSHA(old))
	}

	if err := os.Remove(filepath.Join(root, filepath.FromSlash(testJarPath))); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push of the deletion: %v\n%s", err, out)
	}
	var deletes []recordedFileWrite
	for _, w := range f.writesFor(testJarPath) {
		if w.Method == "DELETE" {
			deletes = append(deletes, w)
		}
	}
	if len(deletes) != 1 || deletes[0].BaseSha256 == nil || *deletes[0].BaseSha256 != jarSHA(jar) {
		t.Fatalf("deletes = %+v, want one asserting the jar's sha256 %s", deletes, jarSHA(jar))
	}
}

// A driver's version bump renames its jar. Two 13 MB jars are over the 25 MB
// total, so the old one has to go before the new one is uploaded.
func TestPushDeletesARenamedJarBeforeItUploadsTheNewOne(t *testing.T) {
	const oldPath, newPath = "drivers/x-42.7.3.jar", "drivers/x-42.7.4.jar"
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddJar("wf-1", oldPath, fakeJarBytes("42.7.3", 13<<20))
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(oldPath))); err != nil {
		t.Fatal(err)
	}
	writeLocalBytes(t, root, newPath, fakeJarBytes("42.7.4", 13<<20))

	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push of a renamed jar: %v\n%s", err, out)
	}
	var order []string
	for _, w := range f.fileWrites {
		if isJarPath(w.Path) {
			order = append(order, w.Method+" "+w.Path)
		}
	}
	if want := []string{"DELETE " + oldPath, "PUT " + newPath}; !slices.Equal(order, want) {
		t.Fatalf("jar writes = %v, want %v", order, want)
	}
}

// A jar rewritten on disk after push read the folder is refused by name and
// never uploaded, and the baseline records only what landed before it: the
// jar stays a local change, and a main.py pushed ahead of it does not.
func TestPushRefusesAJarEditedWhileItRuns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		editText bool
	}{{"only the jar", false}, {"the jar after main.py", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := jarWorkflow(t, nil)
			root := cloneFolder(t, f, "wf-1")
			writeLocalBytes(t, root, testJarPath, fakeJarBytes("v1", 4000))
			if tc.editText {
				writeLocal(t, root, "main.py", "edited\n")
			}
			f.beforeCheckout = func(string) { writeLocalBytes(t, root, testJarPath, fakeJarBytes("v2", 4000)) }

			out, err := runCLI(t, root, "wf", "push", "--json")
			if err == nil || !strings.Contains(err.Error(), testJarPath+" changed while this push was running") {
				t.Fatalf("push of a jar edited mid-push: %v, want a refusal naming %s\n%s", err, testJarPath, out)
			}
			if puts := f.jarPuts(); len(puts) != 0 {
				t.Fatalf("the edited jar was uploaded: %+v", puts)
			}
			if got := f.FileContents(f.draftOf["wf-1"])["main.py"]; tc.editText != (got == "edited\n") {
				t.Fatalf("the draft holds main.py %q", got)
			}
			out, err = runCLI(t, root, "wf", "status", "--json")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			var report statusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(report.Local.Added, []string{testJarPath}) || len(report.Local.Modified) != 0 {
				t.Errorf("local changes after the refusal = %+v, want only %s added", report.Local, testJarPath)
			}
		})
	}
}

// sync apply discards the checkout it made when the jar's refusal is the
// first write, as it does any refusal before the first write.
func TestSyncApplyDiscardsItsCheckoutWhenAJarIsEditedWhileItRuns(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	writeLocalBytes(t, fx.root, testJarPath, fakeJarBytes("v1", 4000))
	fx.wf.beforeCheckout = func(string) { writeLocalBytes(t, fx.root, testJarPath, fakeJarBytes("v2", 4000)) }

	if code, _, _, out := applyOnce(t, fx); code == syncExitClean || !strings.Contains(out, testJarPath+" changed while this push was running") {
		t.Fatalf("apply of a jar edited mid-apply: exit %d, want a refusal naming %s\n%s", code, testJarPath, out)
	}
	if puts := fx.wf.jarPuts(); len(puts) != 0 {
		t.Fatalf("the edited jar was uploaded: %+v", puts)
	}
	if len(fx.wf.discardedIDs) != 1 {
		t.Fatalf("discarded %v, want the checkout this apply made", fx.wf.discardedIDs)
	}
}

// Clone writes a jar's bytes, fetched through the signed link with no
// credential on it, and the folder then reads as in sync.
func TestCloneRestoresAJarsBytesWithoutSendingTheToken(t *testing.T) {
	jar := fakeJarBytes("v1", jarSize)
	f := jarWorkflow(t, jar)
	root := cloneFolder(t, f, "wf-1")

	if got := readFile(t, root, testJarPath); got != string(jar) {
		t.Fatalf("the cloned jar holds %d bytes, want the %d it was saved with", len(got), len(jar))
	}
	if len(f.jars.blobAuth) != 1 || f.jars.blobAuth[0] != "" {
		t.Fatalf("the storage link saw credentials %q, want one request carrying none", f.jars.blobAuth)
	}
	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Local.Dirty() || report.Remote == nil || report.Remote.Drift == nil || report.Remote.Drift.Dirty() {
		t.Fatalf("a fresh clone with a jar is not in sync: local %+v remote %+v", report.Local, report.Remote)
	}
}

// A clone whose jar cannot be downloaded fails naming it, and leaves no
// folder behind for the next clone to refuse as not empty.
func TestCloneWritesNothingWhenAJarDownloadFails(t *testing.T) {
	jar := fakeJarBytes("v1", 4000)
	f := jarWorkflow(t, jar)
	f.jars.mu.Lock()
	delete(f.jars.bytes, jarSHA(jar))
	f.jars.mu.Unlock()
	target := filepath.Join(t.TempDir(), "clone")

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", target)
	if err == nil || !strings.Contains(err.Error(), testJarPath) {
		t.Fatalf("clone with an unreadable jar: %v, want a refusal naming %s\n%s", err, testJarPath, out)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("the failed clone left %s behind (%v)", target, statErr)
	}
}

func TestFormatJarSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 KB", 1023: "< 1 KB", 1_500: "1 KB", 1_048_063: "1023 KB", 1_048_064: "1.0 MB",
		1_258_291: "1.2 MB", 20 << 20: "20.0 MB", 25 << 20: "25.0 MB",
	} {
		if got := formatJarSize(n); got != want {
			t.Errorf("formatJarSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// status names a jar changed in the folder, and one changed on the server,
// by comparing the same sha256 on both sides.
func TestStatusShowsAChangedJar(t *testing.T) {
	f := jarWorkflow(t, fakeJarBytes("v1", 4000))
	root := cloneFolder(t, f, "wf-1")

	writeLocalBytes(t, root, testJarPath, fakeJarBytes("local", 4000))
	f.AddJar("wf-1", testJarPath, fakeJarBytes("remote", 4000))
	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Local.Modified, []string{testJarPath}) {
		t.Errorf("local modified = %v, want [%s]", report.Local.Modified, testJarPath)
	}
	if report.Remote == nil || report.Remote.Drift == nil || !slices.Equal(report.Remote.Drift.Modified, []string{testJarPath}) {
		t.Errorf("remote drift = %+v, want %s modified", report.Remote, testJarPath)
	}
}

// An older CLI wrote an empty placeholder for a jar and pushes it back as
// empty text: the server keeps the jar. This CLI, run in that folder, refuses
// the placeholder by name and writes nothing, so it can never replace or
// delete the jar from it.
func TestOlderCLIPlaceholderRoundTripLeavesTheJarIntact(t *testing.T) {
	jar := fakeJarBytes("v1", 4000)
	f := jarWorkflow(t, jar)
	f.AddDraft("wf-1", "draft-1", f.files["wf-1"]...)

	// The older CLI's requests: a forced push of its placeholder, then an edit.
	client := api.New(f.URL(), "test-token")
	if _, err := client.PutWorkflowFile(context.Background(), "draft-1", testJarPath, "", nil); err != nil {
		t.Fatalf("the placeholder push was refused: %v", err)
	}
	if got, ok := f.JarOf("draft-1", testJarPath); !ok || !bytes.Equal(got, jar) {
		t.Fatalf("the placeholder push changed the jar")
	}
	if _, err := client.PutWorkflowFile(context.Background(), "draft-1", testJarPath, "edited", nil); api.StatusOf(err) != 400 {
		t.Fatalf("text over a jar: %v, want a 400", err)
	}

	// This CLI over the older CLI's folder.
	root := bindFolder(t, f, "wf-1", map[string]string{"main.py": "M\n", testJarPath: ""})
	f.Requests = nil
	out, err := runCLI(t, root, "wf", "push", "--force", "--json")
	if err == nil || !strings.Contains(err.Error(), testJarPath) || !strings.Contains(err.Error(), "older ronja CLI") {
		t.Fatalf("push over a placeholder: %v, want a refusal naming %s\n%s", err, testJarPath, out)
	}
	if writes := fileAndMetadataWrites(f); len(writes) != 0 {
		t.Errorf("the refused push wrote %v", writes)
	}
	for _, id := range []string{"wf-1", "draft-1"} {
		if got, ok := f.JarOf(id, testJarPath); !ok || !bytes.Equal(got, jar) {
			t.Errorf("%s lost its jar", id)
		}
	}
}

// validate checks a folder with a 1.2 MB jar, posting it as its sha256 and
// size: no content, and none of its bytes.
func TestValidateWithAJarPostsNoContent(t *testing.T) {
	f := jarWorkflow(t, nil)
	root := cloneFolder(t, f, "wf-1")
	jar := fakeJarBytes("v1", jarSize)
	writeLocalBytes(t, root, testJarPath, jar)

	if out, err := runCLI(t, root, "wf", "validate", "--json"); err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if len(f.validated) != 1 {
		t.Fatalf("validate requests = %d, want 1", len(f.validated))
	}
	var posted *api.ValidateFile
	for i, file := range f.validated[0].Files {
		if file.Path == testJarPath {
			posted = &f.validated[0].Files[i]
		}
	}
	want := api.WorkflowFileBinary{SHA256: jarSHA(jar), Size: jarSize}
	if posted == nil || posted.Content != "" || posted.Binary == nil || *posted.Binary != want {
		t.Fatalf("the jar was posted as %+v, want binary %+v and no content", posted, want)
	}
	body, err := json.Marshal(f.validated[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 4096 {
		t.Errorf("the validate request is %d bytes: the jar's bytes went with it", len(body))
	}
}

// A folder's jars are held to the server's caps before anything is sent, and
// every other binary is still refused.
func TestCheckPushableHoldsJarsToTheirCaps(t *testing.T) {
	jar := func(size int64) string { return jarStandIn(strings.Repeat("a", 64), size) }
	jars := func(n int) map[string]string {
		files := map[string]string{}
		for i := range n {
			files[fmt.Sprintf("drivers/d%02d.jar", i)] = jar(10)
		}
		return files
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"a jar under the caps", map[string]string{"main.py": "x", "drivers/a.jar": jar(maxJarBytes)}, ""},
		{"a jar over 20 MB", map[string]string{"drivers/a.jar": jar(maxJarBytes + 1)}, "drivers/a.jar (20971521 bytes"},
		{"jars over 25 MB together", map[string]string{"drivers/a.jar": jar(13 << 20), "drivers/b.jar": jar(13 << 20)}, "the .jar files together"},
		{"64 jars", jars(64), ""},
		{"65 jars", jars(65), "the .jar files (65 of them; a workflow may hold at most 64)"},
		{"a .jar that is not a zip", map[string]string{"drivers/a.jar": "plain text"}, "not a jar file"},
		{"another binary", map[string]string{"data.bin": "a\x00b"}, "binary content: contains null bytes"},
		{"a jar's bytes under another name", map[string]string{"drivers/a.JAR": "PK\x03\x04\x00"}, "drivers/a.JAR (binary content"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkPushable(c.files, wfdir.WorkflowKind, maxFileBytes)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want a refusal containing %q", err, c.want)
			}
		})
	}
}

// The pull report names a replaced jar by its two sha256s and its size, and
// never prints its bytes as a diff.
func TestPullReportShowsAReplacedJarBySha256(t *testing.T) {
	was, now := fakeJarBytes("v1", jarSize), fakeJarBytes("v2", jarSize)
	before, after := string(was), jarStandIn(jarSHA(now), int64(len(now)))
	r := pullReport{Kind: wfdir.KindWorkflow}
	r.fill([]string{testJarPath}, map[string]*string{testJarPath: &before}, map[string]*string{testJarPath: &after})

	want := "replaced drivers/x.jar (sha256 " + jarSHA(was)[:12] + "… → " + jarSHA(now)[:12] + "…, 1.2 MB)\n"
	if r.Diff != want {
		t.Fatalf("diff = %q, want %q", r.Diff, want)
	}
	if len(r.Files) != 1 || r.Files[0] != (pullFileReport{Path: testJarPath, Change: "modified"}) {
		t.Fatalf("files = %+v", r.Files)
	}
}

// Another kind's file at a `.jar` path is text, and its pull report is the
// text diff.
func TestPullReportDiffsAnotherKindsJarPathAsText(t *testing.T) {
	before, after := "a\n", "b\n"
	r := pullReport{Kind: wfdir.KindDataApp}
	r.fill([]string{"assets/x.jar"}, map[string]*string{"assets/x.jar": &before}, map[string]*string{"assets/x.jar": &after})
	if !strings.Contains(r.Diff, "-a\n+b\n") || strings.Contains(r.Diff, "sha256") {
		t.Fatalf("diff = %q, want the text diff", r.Diff)
	}
}

// webPublishJar is a colleague saving body as the jar in the web app.
func webPublishJar(f *fakeInstance, body []byte) {
	webPublishWorkflow(f, func(files []api.WorkflowFile) []api.WorkflowFile {
		f.jars.mu.Lock()
		if f.jars.bytes == nil {
			f.jars.bytes = map[string][]byte{}
		}
		f.jars.bytes[jarSHA(body)] = body
		f.jars.mu.Unlock()
		binary := &api.WorkflowFileBinary{SHA256: jarSHA(body), Size: int64(len(body))}
		for i := range files {
			if files[i].Path == testJarPath {
				files[i].Binary = binary
				return files
			}
		}
		return append(files, api.WorkflowFile{WorkflowID: "wf-1", Path: testJarPath, Binary: binary})
	})
}

// pull takes a jar added and then replaced in the web app into the folder as
// its bytes, reporting the replacement by sha256, and the folder then applies
// with nothing to write.
func TestPullRestoresAJarsBytes(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}

	v1, v2 := fakeJarBytes("v1", jarSize), fakeJarBytes("v2", jarSize)
	webPublishJar(fx.wf, v1)
	if out, err := runCLI(t, fx.root, "wf", "pull", "--json"); err != nil {
		t.Fatalf("pull of an added jar: %v\n%s", err, out)
	}
	if got := readFile(t, fx.root, testJarPath); got != string(v1) {
		t.Fatalf("after pull the jar holds %d bytes, want the %d saved in the web app", len(got), len(v1))
	}

	webPublishJar(fx.wf, v2)
	out, err := runCLI(t, fx.root, "wf", "pull", "--json")
	if err != nil {
		t.Fatalf("pull of a replaced jar: %v\n%s", err, out)
	}
	report := decodePullReport(t, out)
	if want := "replaced drivers/x.jar (sha256 " + jarSHA(v1)[:12] + "… → " + jarSHA(v2)[:12] + "…, 1.2 MB)\n"; report.Diff != want {
		t.Fatalf("pull reported %q, want %q", report.Diff, want)
	}
	if got := readFile(t, fx.root, testJarPath); got != string(v2) {
		t.Fatalf("after the second pull the jar holds the wrong bytes")
	}
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("apply after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
	}
	if puts := fx.wf.jarPuts(); len(puts) != 0 {
		t.Fatalf("a pulled jar was uploaded back: %+v", puts)
	}
}

// A pull whose jar cannot be downloaded fails naming it, and writes nothing:
// not the text file changed beside it, nor the baseline, nor ronja.json. The
// jar's bytes are never stored, so its link answers 404, and it sorts after
// main.py, so a download inside the write loop would land main.py first.
func TestPullWritesNothingWhenAJarDownloadFails(t *testing.T) {
	const jarPath = "vendor/x.jar"
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	jar := fakeJarBytes("v1", 4000)
	webPublishWorkflow(fx.wf, func(files []api.WorkflowFile) []api.WorkflowFile {
		for i := range files {
			if files[i].Path == "main.py" {
				files[i].Content = "fixed in the web app\n"
			}
		}
		return append(files, api.WorkflowFile{WorkflowID: "wf-1", Path: jarPath,
			Binary: &api.WorkflowFileBinary{SHA256: jarSHA(jar), Size: int64(len(jar))}})
	})
	before := treeBytes(t, fx.tree)

	out, err := runCLI(t, fx.root, "wf", "pull", "--json")
	if err == nil || !strings.Contains(err.Error(), jarPath) {
		t.Fatalf("pull with an unreadable jar: %v, want a refusal naming %s\n%s", err, jarPath, out)
	}
	after := treeBytes(t, fx.tree)
	for path, was := range before {
		if now, ok := after[path]; !ok || now != was {
			t.Errorf("the failed pull changed or removed %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("the failed pull wrote %s", path)
		}
	}
}

// treeBytes is every file beneath root, by its slash path.
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// propose packs a jar's bytes into the change list's copy, and the approved
// copy's deploy uploads them with the raw PUT.
func TestProposeThenApplyCarriesAJar(t *testing.T) {
	f, dir := proposeFixture(t)
	jar := fakeJarBytes("v1", jarSize)
	writeLocalBytes(t, filepath.Join(dir, "workflows/sales-report"), testJarPath, jar)

	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	archive := f.changeLists.proposals[0].archive
	if got := archiveFiles(t, archive)["workflows/sales-report/"+testJarPath]; got != string(jar) {
		t.Fatalf("the copy holds %d bytes for the jar, want %d", len(got), len(jar))
	}
	if puts := f.wf.jarPuts(); len(puts) != 0 {
		t.Fatalf("propose uploaded the jar: %+v", puts)
	}

	deployCopy(t, archive)
	puts := f.wf.jarPuts()
	if len(puts) != 1 || puts[0].Path != testJarPath || puts[0].Size != jarSize {
		t.Fatalf("the deploy's raw PUTs = %+v, want the jar once", puts)
	}
	held := 0
	for id := range f.wf.files {
		if got, ok := f.wf.JarOf(id, testJarPath); ok && bytes.Equal(got, jar) {
			held++
		}
	}
	if held == 0 {
		t.Fatalf("no workflow holds the jar after the deploy")
	}
}

// publish records the now-live jar in the baseline by its sha256, so the
// folder it published from reads as in sync and the next push writes nothing.
func TestPublishRecordsAJarBySha256(t *testing.T) {
	f := jarWorkflow(t, fakeJarBytes("v1", 4000))
	f.workflows["wf-1"].FeatureScope = "private"
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	root := cloneFolder(t, f, "wf-1")
	writeLocalBytes(t, root, testJarPath, fakeJarBytes("v2", 4000))
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "wf", "publish"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Local.Dirty() || report.Remote == nil || report.Remote.Drift == nil || report.Remote.Drift.Dirty() {
		t.Fatalf("after publish the folder is not in sync: local %+v remote drift %+v", report.Local, report.Remote.Drift)
	}
	puts := len(f.jarPuts())
	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil || !decodeJSON(t, out)["upToDate"].(bool) {
		t.Fatalf("push after publish: %v\n%s", err, out)
	}
	if len(f.jarPuts()) != puts {
		t.Fatalf("the push after publish uploaded the jar again")
	}
}
