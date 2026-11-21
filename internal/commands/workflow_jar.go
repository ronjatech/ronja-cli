package commands

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A workflow's `.jar` (a JDBC driver) is the one binary file its source may
// hold. The server keeps it as bytes named by their sha256 and lists it with
// empty content and {sha256, size}, so the CLI cannot hold both sides as text
// the way it holds every other file.
//
// Instead, every content map the workflow loop builds (the folder's, the
// listing's, the baseline's `landed`) holds a jar as its STAND-IN: a string
// naming its sha256 and size. Two stand-ins are equal exactly when the bytes
// are, so every compare of local with remote content keeps working, and
// contentHash answers the jar's own sha256 for one, so a baseline records the
// digest the server checks a jar's baseSha256 against and the one a folder
// walk hashes the file on disk to. The bytes are read from disk only to upload
// them, and downloaded only to write them.
//
// A stand-in starts with a NUL byte, which no text file can hold (checkPushable
// refuses one, and the server stores text in a column that cannot), so no text
// content is ever mistaken for one.

// The caps rworkflow enforces on a workflow's jars (store_jars.go), refused
// here before any request.
const (
	maxJarBytes         = api.MaxJarBytes
	maxWorkflowJarBytes = 25 << 20
	maxWorkflowJars     = 64
)

const jarStandInPrefix = "\x00jar sha256="

var zipSignature = []byte("PK\x03\x04")

// isJarPath is rworkflow.IsJarPath: case-sensitive, as the server's is.
func isJarPath(path string) bool {
	return strings.HasSuffix(path, ".jar")
}

func jarStandIn(sha256Hex string, size int64) string {
	return jarStandInPrefix + sha256Hex + " size=" + strconv.FormatInt(size, 10)
}

// parseJarStandIn answers a stand-in's sha256 and size.
func parseJarStandIn(content string) (sha256Hex string, size int64, ok bool) {
	rest, ok := strings.CutPrefix(content, jarStandInPrefix)
	if !ok {
		return "", 0, false
	}
	sha256Hex, sizeText, ok := strings.Cut(rest, " size=")
	if !ok {
		return "", 0, false
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return sha256Hex, size, true
}

func isJarStandIn(content string) bool {
	return strings.HasPrefix(content, jarStandInPrefix)
}

// contentHash is the digest a baseline records for content from a content
// map: a jar's sha256 for its stand-in, the sha256 of the text otherwise.
func contentHash(content string) string {
	if sha, _, ok := parseJarStandIn(content); ok {
		return sha
	}
	return wfdir.HashString(content)
}

// localContent is a file's entry in a folder's content map. Only a workflow
// folder holds jars, and only a file that is a zip is one: anything else at a
// `.jar` path (an older CLI's empty placeholder) stays as its bytes, for
// checkPushable to refuse by name.
func localContent(kind wfdir.Kind, path string, body []byte) string {
	if kind.Name == wfdir.KindWorkflow && isJarPath(path) && bytes.HasPrefix(body, zipSignature) {
		return jarStandIn(wfdir.Hash(body), int64(len(body)))
	}
	return string(body)
}

// remoteContent is a listed file's entry in a content map, in disk form.
func remoteContent(codec aliasCodec, f api.WorkflowFile) string {
	if f.Binary != nil {
		return jarStandIn(f.Binary.SHA256, f.Binary.Size)
	}
	return codec.toDisk(f.Content)
}

// storedContent is remoteContent as the server stores it, with no alias
// substituted: what a precondition is computed over.
func storedContent(f api.WorkflowFile) string {
	if f.Binary != nil {
		return jarStandIn(f.Binary.SHA256, f.Binary.Size)
	}
	return f.Content
}

// validateFileOf is one entry of a validate request: a jar by its sha256 and
// size, never its bytes and never through the alias codec; text in wire form.
func validateFileOf(codec aliasCodec, path, content string) api.ValidateFile {
	if sha, size, ok := parseJarStandIn(content); ok {
		return api.ValidateFile{Path: path, Binary: &api.WorkflowFileBinary{SHA256: sha, Size: size}}
	}
	return api.ValidateFile{Path: path, Content: codec.toWire(content)}
}

// validateFileOfRow is validateFileOf for a stored row, sent as the row holds it.
func validateFileOfRow(f api.WorkflowFile) api.ValidateFile {
	if f.Binary != nil {
		return api.ValidateFile{Path: f.Path, Binary: &api.WorkflowFileBinary{SHA256: f.Binary.SHA256, Size: f.Binary.Size}}
	}
	return api.ValidateFile{Path: f.Path, Content: f.Content}
}

// readJarForUpload reads a jar's bytes from the folder and refuses them when
// they are no longer the bytes standIn was taken from: the push validated and
// compared those, not these.
func readJarForUpload(root, path, standIn string) ([]byte, error) {
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if localContent(wfdir.WorkflowKind, path, body) != standIn {
		return nil, fmt.Errorf("%s changed while this push was running; run it again", path)
	}
	return body, nil
}

// jarDiskBytes is what a listed file writes into a folder: a jar's downloaded
// bytes, or the text in disk form.
func jarDiskBytes(ctx context.Context, client *api.Client, codec aliasCodec, workflowID string, f api.WorkflowFile) (string, error) {
	if f.Binary == nil {
		return codec.toDisk(f.Content), nil
	}
	body, err := client.DownloadWorkflowJar(ctx, workflowID, f.Path, *f.Binary)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// checkJars is checkPushable's rule for a workflow's jars: each must be a jar,
// at most maxJarBytes, together at most maxWorkflowJarBytes, and at most
// maxWorkflowJars of them. It answers the refusals, one line each.
func checkJars(files map[string]string) []string {
	var rejected []string
	var total, count int64
	for _, path := range sortedPaths(files) {
		if !isJarPath(path) {
			continue
		}
		_, size, ok := parseJarStandIn(files[path])
		switch {
		case !ok && files[path] == "":
			rejected = append(rejected, path+" (empty: the placeholder an older ronja CLI wrote for a jar; put the jar itself there, which a fresh `ronja wf clone` of the workflow fetches)")
		case !ok:
			rejected = append(rejected, path+" (not a jar file: its bytes do not start with the zip signature)")
		case size > maxJarBytes:
			rejected = append(rejected, fmt.Sprintf("%s (%d bytes, limit %d for a .jar)", path, size, maxJarBytes))
		default:
			total += size
			count++
		}
	}
	if total > maxWorkflowJarBytes {
		rejected = append(rejected, fmt.Sprintf("the .jar files together (%d bytes, limit %d for all of a workflow's jars)", total, maxWorkflowJarBytes))
	}
	if count > maxWorkflowJars {
		rejected = append(rejected, fmt.Sprintf("the .jar files (%d of them; a workflow may hold at most %d)", count, maxWorkflowJars))
	}
	return rejected
}

// formatJarSize reads a size as the UI's formatFileSize does, counting in
// 1024s like the caps, so one jar reads the same everywhere.
func formatJarSize(n int64) string {
	kb := math.Round(float64(n) / 1024)
	switch {
	case n == 0:
		return "0 KB"
	case n < 1024:
		return "< 1 KB"
	case kb < 1024:
		return strconv.FormatFloat(kb, 'f', 0, 64) + " KB"
	}
	return strconv.FormatFloat(math.Round(float64(n)/(1<<20)*10)/10, 'f', 1, 64) + " MB"
}

// shortSHA is the first twelve hex digits of a sha256, for a report line.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12] + "…"
	}
	return sha
}

// jarIdentity is a jar's sha256 and size, from its stand-in or its bytes.
func jarIdentity(content string) (string, int64) {
	if sha, size, ok := parseJarStandIn(content); ok {
		return sha, size
	}
	return wfdir.HashString(content), int64(len(content))
}

// jarDiff is unifiedDiff for a jar: one line naming it by its sha256s and
// size, never its bytes. "" for no change.
func jarDiff(path string, before, after *string) string {
	switch {
	case before == nil && after == nil:
		return ""
	case before == nil:
		sha, size := jarIdentity(*after)
		return fmt.Sprintf("added %s (sha256 %s, %s)\n", path, shortSHA(sha), formatJarSize(size))
	case after == nil:
		sha, size := jarIdentity(*before)
		return fmt.Sprintf("deleted %s (sha256 %s, %s)\n", path, shortSHA(sha), formatJarSize(size))
	}
	was, _ := jarIdentity(*before)
	now, size := jarIdentity(*after)
	if was == now {
		return ""
	}
	return fmt.Sprintf("replaced %s (sha256 %s → %s, %s)\n", path, shortSHA(was), shortSHA(now), formatJarSize(size))
}
