package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A workflow's `.jar` (a JDBC driver) is the one file the file API carries as
// bytes rather than JSON text: saved with a raw PUT, read back through a
// redirect to a short-lived signed link (backend/api/v2/workflow/file_routes.go).

// JarContentType is the Content-Type that makes PUT :id/files/*path store the
// body as a jar's bytes.
const JarContentType = "application/java-archive"

// MaxJarBytes is rworkflow.MaxJarBytes: the server reads at most one byte more
// than this from a raw PUT, and a download is never read past it.
const MaxJarBytes = 20 << 20

// jarTransferTimeout bounds one jar's upload or download: the slow-request
// bound, plus a second per 32 KiB, so a 20 MiB jar gets about 760 s, enough
// over a connection of about 32 KiB/s.
func jarTransferTimeout(size int64) time.Duration {
	const perSecond = 32 << 10
	return slowRequestTimeout + time.Duration((size+perSecond-1)/perSecond)*time.Second
}

// PutWorkflowJar saves one `.jar` from its bytes. baseSha256 is the same
// three-state precondition PutWorkflowFile takes, compared with the jar's
// sha256; it rides in the query string because the body is the jar.
func (c *Client) PutWorkflowJar(ctx context.Context, id, path string, body []byte, baseSha256 *string) (*FileSaveResponse, error) {
	route := "api/v2/" + workflowFilePath(id, path)
	if baseSha256 != nil {
		route += "?baseSha256=" + url.QueryEscape(*baseSha256)
	}
	header := http.Header{}
	header.Set("Content-Type", JarContentType)
	header.Set("Accept", "application/json")
	resp, err := c.DoRaw(ctx, "PUT", route, header, BytesBody(body), jarTransferTimeout(int64(len(body))))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Status < 200 || resp.Status > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, errorFrom(resp.Status, raw)
	}
	var out FileSaveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode the response to PUT %s: %w", route, err)
	}
	return &out, nil
}

// DownloadWorkflowJar reads one `.jar`'s bytes and checks them against want,
// the sha256 and size the file listing reported.
//
// GET …?raw=1 answers a redirect to a signed link, which is followed by hand
// with a request that carries no credential and no Space: the link is its own
// authorization, and it points at the bucket, not at the instance the token
// belongs to. The client's redirect guard would refuse it as another origin,
// and following it with the token would hand the token to the bucket.
func (c *Client) DownloadWorkflowJar(ctx context.Context, id, path string, want WorkflowFileBinary) ([]byte, error) {
	route := "api/v2/" + workflowFilePath(id, path) + "?raw=1"
	stopAtRedirect := *c
	httpClient := *c.httpFor(slowRequestTimeout)
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	stopAtRedirect.HTTP = &httpClient
	resp, err := stopAtRedirect.DoRaw(ctx, "GET", route, nil, nil, slowRequestTimeout)
	if err != nil {
		// net/http parses Location before CheckRedirect runs, and a malformed
		// one comes back quoted in this error: the signed link, never printed.
		// Any other transport error names only the instance's own URL, so it
		// passes through: "connection refused" says what to fix.
		var transport *url.Error
		if errors.As(err, &transport) && strings.Contains(transport.Err.Error(), "Location header") {
			return nil, fmt.Errorf("download %s: the instance's download link is not a URL", path)
		}
		return nil, err
	}
	location := resp.Header.Get("Location")
	status := resp.Status
	if status < 300 || status > 399 || location == "" {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		if status >= 200 && status <= 299 {
			return nil, fmt.Errorf("download %s: the instance answered %d instead of a link to its bytes", path, status)
		}
		return nil, errorFrom(status, raw)
	}
	resp.Body.Close()
	base, err := url.Parse(c.BaseURL + "/" + route)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path, err)
	}
	link, err := base.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("download %s: the instance's download link is not a URL", path)
	}

	ctx, cancel := context.WithTimeout(ctx, jarTransferTimeout(want.Size))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", link.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path, err)
	}
	req.Header.Set("User-Agent", UserAgent)
	got, err := (&http.Client{}).Do(req)
	if err != nil {
		// The link's query is its signature: never printed.
		return nil, fmt.Errorf("download %s from %s://%s: %w", path, link.Scheme, link.Host, unwrapURLError(err))
	}
	defer got.Body.Close()
	if got.StatusCode < 200 || got.StatusCode > 299 {
		return nil, fmt.Errorf("download %s: the storage link answered %d", path, got.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(got.Body, MaxJarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
		return nil, fmt.Errorf("download %s: got %d bytes with sha256 %s, but the workflow lists %d bytes with sha256 %s — the jar may have been replaced since it was listed; run the command again",
			path, len(body), hex.EncodeToString(sum[:]), want.Size, want.SHA256)
	}
	return body, nil
}

// unwrapURLError drops the *url.Error wrapper, whose message repeats the URL
// and with it a signed link's signature.
func unwrapURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}
