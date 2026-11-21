package commands

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// fakeJars is the fake instance's model of a workflow's jars, answering as
// phase 2's routes do (backend/api/v2/workflow/file_routes.go,
// resource/rworkflow/store_jars.go): a raw PUT stores the bytes by sha256, the
// listing shows a jar with empty content and {sha256, size}, ?raw=1 redirects
// to a signed link on ANOTHER origin, empty text over a jar is a no-op that
// checks no precondition, and any other text over a jar is refused.
type fakeJars struct {
	mu sync.Mutex
	// bytes maps a sha256 to the bytes stored under it.
	bytes map[string][]byte
	// puts records every raw PUT, accepted or not.
	puts []recordedJarPut
	// blob is the storage the signed links point at, started on first use.
	blob *httptest.Server
	// blobAuth records the Authorization header of every request the storage
	// link served, "" for none.
	blobAuth []string
}

type recordedJarPut struct {
	Path       string
	Size       int
	BaseSha256 *string
}

func jarSHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fakeJarBytes is a jar of about size bytes: the zip signature, then filler
// that differs by seed.
func fakeJarBytes(seed string, size int) []byte {
	body := append([]byte("PK\x03\x04"), seed...)
	for i := 0; len(body) < size; i++ {
		body = append(body, byte(i*7))
	}
	return body
}

// AddJar stores body as the jar at path of workflow id.
func (f *fakeInstance) AddJar(id, path string, body []byte) {
	f.jars.mu.Lock()
	if f.jars.bytes == nil {
		f.jars.bytes = map[string][]byte{}
	}
	f.jars.bytes[jarSHA(body)] = bytes.Clone(body)
	f.jars.mu.Unlock()
	f.storeJarRow(id, path, body)
}

func (f *fakeInstance) storeJarRow(id, path string, body []byte) {
	binary := &api.WorkflowFileBinary{SHA256: jarSHA(body), Size: int64(len(body))}
	for i := range f.files[id] {
		if f.files[id][i].Path == path {
			f.files[id][i].Content, f.files[id][i].Binary = "", binary
			return
		}
	}
	f.files[id] = append(f.files[id], api.WorkflowFile{
		WorkflowID: id, Path: path, Binary: binary,
		UpdatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	})
}

// JarOf is the bytes of the jar at path of workflow id, and whether one is there.
func (f *fakeInstance) JarOf(id, path string) ([]byte, bool) {
	for _, file := range f.files[id] {
		if file.Path == path && file.Binary != nil {
			f.jars.mu.Lock()
			defer f.jars.mu.Unlock()
			body, ok := f.jars.bytes[file.Binary.SHA256]
			return body, ok
		}
	}
	return nil, false
}

// preconditionContents is FileContents with each jar as its stand-in, whose
// digest is the jar's sha256: what the server checks a jar's baseSha256 against.
func (f *fakeInstance) preconditionContents(id string) map[string]string {
	out := f.FileContents(id)
	for _, file := range f.files[id] {
		if file.Binary != nil {
			out[file.Path] = jarStandIn(file.Binary.SHA256, file.Binary.Size)
		}
	}
	return out
}

func (f *fakeInstance) jarRow(id, path string) *api.WorkflowFile {
	for i := range f.files[id] {
		if f.files[id][i].Path == path && f.files[id][i].Binary != nil {
			return &f.files[id][i]
		}
	}
	return nil
}

func (f *fakeInstance) serveJarPut(w http.ResponseWriter, r *http.Request, id, path string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, api.MaxJarBytes+1))
	if err != nil {
		f.t.Fatalf("read the jar PUT of %s: %v", path, err)
	}
	var base *string
	if v, ok := r.URL.Query()["baseSha256"]; ok {
		base = &v[0]
	}
	f.jars.mu.Lock()
	f.jars.puts = append(f.jars.puts, recordedJarPut{Path: path, Size: len(body), BaseSha256: base})
	f.jars.mu.Unlock()
	f.fileWrites = append(f.fileWrites, recordedFileWrite{Method: "PUT", Path: path, BaseSha256: base})
	switch {
	case !isJarPath(path):
		http.Error(w, fmt.Sprintf(`{"error":"%q is binary, and only a .jar may be binary"}`, path), http.StatusBadRequest)
		return
	case len(body) > api.MaxJarBytes:
		http.Error(w, `{"error":"too large"}`, http.StatusRequestEntityTooLarge)
		return
	case !bytes.HasPrefix(body, zipSignature):
		http.Error(w, fmt.Sprintf(`{"error":"%q is not a jar file"}`, path), http.StatusBadRequest)
		return
	case f.otherJarBytes(id, path)+int64(len(body)) > maxWorkflowJarBytes:
		http.Error(w, fmt.Sprintf(`{"error":"%q would bring this workflow's .jar files over 25 MB"}`, path), http.StatusBadRequest)
		return
	}
	if f.refusePrecondition(w, "workflow", f.preconditionContents(id), path, base) {
		return
	}
	f.jars.mu.Lock()
	if f.jars.bytes == nil {
		f.jars.bytes = map[string][]byte{}
	}
	f.jars.bytes[jarSHA(body)] = body
	f.jars.mu.Unlock()
	if wf := f.workflows[id]; wf != nil {
		wf.UpdatedAt = wf.UpdatedAt.Add(time.Minute)
	}
	f.storeJarRow(id, path, body)
	writeJSON(w, map[string]any{
		"id": "file-" + path, "workflowID": id, "path": path, "content": "",
		"binary": map[string]any{"sha256": jarSHA(body), "size": len(body)},
	})
}

// otherJarBytes is the size of every jar of workflow id but the one at path.
func (f *fakeInstance) otherJarBytes(id, path string) int64 {
	var n int64
	for _, file := range f.files[id] {
		if file.Path != path && file.Binary != nil {
			n += file.Binary.Size
		}
	}
	return n
}

// refuseTextOverJar answers a JSON text PUT to a path holding a jar as the
// server does: empty text (an older CLI pushing back the placeholder it pulled)
// is a 200 that changes nothing and checks no precondition; any other text is a
// 400. Reports whether it answered.
func (f *fakeInstance) refuseTextOverJar(w http.ResponseWriter, id, path, content string) bool {
	row := f.jarRow(id, path)
	if row == nil {
		return false
	}
	if content != "" {
		http.Error(w, fmt.Sprintf(`{"error":"%q: a .jar is saved as bytes, not text; update your ronja CLI"}`, path), http.StatusBadRequest)
		return true
	}
	writeJSON(w, row)
	return true
}

func (f *fakeInstance) serveJarDownload(w http.ResponseWriter, id, path string) {
	row := f.jarRow(id, path)
	if row == nil {
		http.Error(w, fmt.Sprintf(`{"error":"%q is a text file; read its content without ?raw=1"}`, path), http.StatusBadRequest)
		return
	}
	f.jars.mu.Lock()
	if f.jars.blob == nil {
		f.jars.blob = httptest.NewServer(http.HandlerFunc(f.serveBlob))
		f.t.Cleanup(f.jars.blob.Close)
	}
	link := f.jars.blob.URL + "/workflow-jars/" + row.Binary.SHA256 + ".jar?X-Amz-Signature=secret"
	f.jars.mu.Unlock()
	http.Redirect(w, &http.Request{Method: "GET"}, link, http.StatusFound)
}

func (f *fakeInstance) serveBlob(w http.ResponseWriter, r *http.Request) {
	f.jars.mu.Lock()
	defer f.jars.mu.Unlock()
	f.jars.blobAuth = append(f.jars.blobAuth, r.Header.Get("Authorization")+r.Header.Get(api.HeaderSpace))
	sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/workflow-jars/"), ".jar")
	body, ok := f.jars.bytes[sha]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", api.JarContentType)
	_, _ = w.Write(body)
}

// jarPuts is every raw PUT the fake was sent.
func (f *fakeInstance) jarPuts() []recordedJarPut {
	f.jars.mu.Lock()
	defer f.jars.mu.Unlock()
	return append([]recordedJarPut(nil), f.jars.puts...)
}
