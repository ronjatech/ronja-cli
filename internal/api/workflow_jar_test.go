package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// jarInstance answers a jar's ?raw=1 with a redirect to storage on another
// origin, which serves stored, and records what each side was sent.
func jarInstance(t *testing.T, stored []byte) (*httptest.Server, *recordingServer, *[]*http.Request) {
	t.Helper()
	storage := &recordingServer{}
	storage.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		storage.mu.Lock()
		storage.hits++
		storage.auth = append(storage.auth, req.Header.Get("Authorization"))
		storage.mu.Unlock()
		_, _ = w.Write(stored)
	}))
	t.Cleanup(storage.server.Close)
	var seen []*http.Request
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		seen = append(seen, req)
		switch {
		case req.Method == "GET" && req.URL.Query().Get("raw") == "1":
			http.Redirect(w, req, storage.server.URL+"/workflow-jars/x.jar?X-Amz-Signature=secret", http.StatusFound)
		case req.Method == "PUT":
			sum := sha256.Sum256(body)
			writeTestJSON(w, `{"path":"drivers/x.jar","content":"","binary":{"sha256":"`+hex.EncodeToString(sum[:])+`","size":`+strconv.Itoa(len(body))+`}}`)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(instance.Close)
	return instance, storage, &seen
}

func digestOf(body []byte) WorkflowFileBinary {
	sum := sha256.Sum256(body)
	return WorkflowFileBinary{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

// The signed link is followed without the token, and its bytes are checked
// against the listing before they are handed back.
func TestDownloadWorkflowJarFollowsTheLinkWithoutTheToken(t *testing.T) {
	jar := []byte("PK\x03\x04driver")
	instance, storage, _ := jarInstance(t, jar)
	client := New(instance.URL, "secret-token")

	got, err := client.DownloadWorkflowJar(context.Background(), "wf-1", "drivers/x.jar", digestOf(jar))
	if err != nil || string(got) != string(jar) {
		t.Fatalf("download = %q, %v", got, err)
	}
	if hits, auth := storage.sawAuthorization(); hits != 1 || auth[0] != "" {
		t.Fatalf("storage saw %d requests with Authorization %q, want one with none", hits, auth)
	}

	_, err = client.DownloadWorkflowJar(context.Background(), "wf-1", "drivers/x.jar", digestOf([]byte("PK\x03\x04other")))
	if err == nil || !strings.Contains(err.Error(), "drivers/x.jar") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a download whose bytes are not the listed ones: %v, want a refusal naming the path and not the signature", err)
	}
}

// net/http quotes a Location it cannot parse in its error, and that Location
// is the signed link.
func TestDownloadWorkflowJarNeverPrintsAMalformedLink(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Location", "http://%zz/workflow-jars/x.jar?X-Amz-Signature=SECRET")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(instance.Close)
	client := New(instance.URL, "secret-token")

	_, err := client.DownloadWorkflowJar(context.Background(), "wf-1", "drivers/x.jar", digestOf([]byte("PK")))
	if err == nil || !strings.Contains(err.Error(), "drivers/x.jar") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("a malformed link: %v, want a refusal naming the path and not the signature", err)
	}
}

// Only the Location parse error is replaced; a plain network failure keeps its cause.
func TestDownloadWorkflowJarKeepsANetworkErrorsCause(t *testing.T) {
	instance := httptest.NewServer(http.NotFoundHandler())
	addr := instance.URL
	instance.Close()
	client := New(addr, "secret-token")

	_, err := client.DownloadWorkflowJar(context.Background(), "wf-1", "drivers/x.jar", digestOf([]byte("PK")))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("an unreachable instance: %v, want its cause", err)
	}
}

// A jar is PUT as its bytes, with the precondition in the query string.
func TestPutWorkflowJarSendsTheBytes(t *testing.T) {
	jar := []byte("PK\x03\x04driver")
	instance, _, seen := jarInstance(t, jar)
	client := New(instance.URL, "secret-token")
	base := ""

	saved, err := client.PutWorkflowJar(context.Background(), "wf-1", "drivers/x.jar", jar, &base)
	if err != nil || saved.Binary == nil || *saved.Binary != digestOf(jar) {
		t.Fatalf("put = %+v, %v", saved, err)
	}
	req := (*seen)[0]
	body, _ := io.ReadAll(req.Body)
	if req.Header.Get("Content-Type") != JarContentType || string(body) != string(jar) {
		t.Fatalf("sent Content-Type %q and %q", req.Header.Get("Content-Type"), body)
	}
	if v, ok := req.URL.Query()["baseSha256"]; !ok || v[0] != "" || req.URL.Path != "/api/v2/workflow/wf-1/files/drivers/x.jar" {
		t.Fatalf("sent %s", req.URL)
	}
}

// A jar's transfer gets the slow-request bound plus a second per 32 KiB, for
// the upload and the download alike.
func TestJarTransferTimeoutGrowsWithTheSize(t *testing.T) {
	for size, want := range map[int64]time.Duration{
		0:           120 * time.Second,
		1:           121 * time.Second,
		32 << 10:    121 * time.Second,
		32<<10 + 1:  122 * time.Second,
		MaxJarBytes: 760 * time.Second,
	} {
		if got := jarTransferTimeout(size); got != want {
			t.Errorf("jarTransferTimeout(%d) = %s, want %s", size, got, want)
		}
	}
}
