package commands

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A change list's DRAFT of each data app it creates or updates: the app as
// this folder's push would write it, which `ronja propose` uploads as the
// `previews` part and the person opens before approving. These types are the
// backend's echangelist.Previews, which the CLI module cannot import.
//
// The deploy is held to it: the server fingerprints each preview
// (echangelist.PreviewDigest) and the job's own `ronja`, re-planning the copy,
// reports previewDigest over what its push would write, so a copy swapped
// after propose, or a `ronja` that resolves aliases differently, is refused
// before anything is written.

type appPreviews struct {
	CLIVersion string       `json:"cliVersion"`
	Apps       []appPreview `json:"apps"`
}

// appPreview is one app. A nil Description, ServerDataOnly or Access is one
// the push leaves as the live app has it.
type appPreview struct {
	FolderID       string             `json:"folderID"`
	Title          string             `json:"title"`
	Description    *string            `json:"description,omitempty"`
	ServerDataOnly *bool              `json:"serverDataOnly,omitempty"`
	Access         *api.DataAppAccess `json:"access,omitempty"`
	Files          []appPreviewFile   `json:"files"`
}

type appPreviewFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// buildAppPreview is f as its push writes it: every local file in wire form
// (the push PUTs each that differs and deletes the rest), the access block
// and settings a create sends in its body or an update patches.
func buildAppPreview(f *folder, folderID string, create bool) (*appPreview, error) {
	local, _, err := readLocalFiles(f.Root, wfdir.DataAppKind)
	if err != nil {
		return nil, err
	}
	// What the deploy's push would refuse, the propose refuses first.
	if err := checkAppPushable(local); err != nil {
		return nil, err
	}
	p := &appPreview{FolderID: folderID, Title: f.Manifest.Title, Files: []appPreviewFile{}}
	for _, path := range sortedPaths(local) {
		p.Files = append(p.Files, appPreviewFile{Path: path, Content: f.Codec.toWire(local[path])})
	}
	switch {
	case create:
		description, serverDataOnly, access := derefOr(f.Manifest.Description, ""), derefOr(f.Manifest.ServerDataOnly, false), f.declaredAccess()
		p.Description, p.ServerDataOnly, p.Access = &description, &serverDataOnly, &access
	default:
		p.Description, p.ServerDataOnly = f.Manifest.Description, f.Manifest.ServerDataOnly
		if f.Manifest.ManagesAccess() {
			access := f.declaredAccess()
			p.Access = &access
		}
	}
	return p, nil
}

// previewDigest is echangelist.PreviewDigest, pinned to it by the fixture
// testdata/preview_digest.json, which both modules' tests read
// (TestPreviewDigestMatchesItsFixture here, TestCLIPreviewDigestMirrorsTheServer
// there). Every field is length-prefixed, so no two previews spell one digest.
func previewDigest(p appPreview) string {
	h := sha256.New()
	field := func(parts ...string) {
		for _, s := range parts {
			fmt.Fprintf(h, "%d:%s", len(s), s)
		}
	}
	files := slices.Clone(p.Files)
	slices.SortFunc(files, func(a, b appPreviewFile) int {
		switch {
		case a.Path < b.Path:
			return -1
		case a.Path > b.Path:
			return 1
		}
		return 0
	})
	field("files", strconv.Itoa(len(files)))
	for _, f := range files {
		sum := sha256.Sum256([]byte(f.Content))
		field(f.Path, hex.EncodeToString(sum[:]))
	}
	if p.Access == nil {
		field("access", "unmanaged")
	} else {
		a := p.Access
		for _, slot := range []struct {
			name string
			ids  []string
		}{
			{"tables", a.AllowedTableIDs}, {"secrets", a.AllowedSecretIDs}, {"agents", a.AllowedAgentIDs},
			{"workflows", a.AllowedWorkflowIDs}, {"codexes", a.AllowedCodexIDs}, {"metrics", a.AllowedMetricIDs},
			{"capabilities", a.Capabilities},
		} {
			ids := slices.Clone(slot.ids)
			slices.Sort(ids)
			field(slot.name, strconv.Itoa(len(ids)))
			field(ids...)
		}
	}
	field("title", p.Title)
	if p.Description == nil {
		field("description", "unmanaged")
	} else {
		field("description", "set", *p.Description)
	}
	if p.ServerDataOnly == nil {
		field("serverDataOnly", "unmanaged")
	} else {
		field("serverDataOnly", strconv.FormatBool(*p.ServerDataOnly))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// previewable is echangelist.Folder.Previewable: a data-app folder whose unit
// creates or updates, which the `previews` part must cover.
func previewable(f syncApplyFolderReport) bool {
	if f.Kind != wfdir.KindDataApp {
		return false
	}
	for _, u := range f.Units {
		switch applyAction(u.Action) {
		case applyActionCreate, applyActionUpdate, applyActionAdopt:
			return true
		}
	}
	return false
}

// noteAppPreview records, in a dry run, the app as this folder's push would
// write it, and its digest on the folder's line.
func noteAppPreview(plan *plannedFolder) {
	line := &plan.line
	if plan.f == nil || plan.f.Kind.Name != wfdir.KindDataApp {
		return
	}
	create := false
	for _, u := range line.Units {
		create = create || applyAction(u.Action) == applyActionCreate
	}
	p, err := buildAppPreview(plan.f, line.FolderID, create)
	if err != nil {
		line.previewErr = err
		return
	}
	line.preview = p
	line.PreviewDigest = previewDigest(*p)
}

// proposedPreviews is the `previews` part, gzipped: one entry for every
// folder the plan creates or updates an app in, and none for any other.
func proposedPreviews(folders []syncApplyFolderReport) ([]byte, error) {
	out := appPreviews{CLIVersion: resolveVersion(), Apps: []appPreview{}}
	for _, f := range folders {
		if !previewable(f) {
			continue
		}
		if f.preview == nil {
			return nil, fmt.Errorf("nothing was proposed: the app in %s could not be read for its draft: %v", f.Path, f.previewErr)
		}
		out.Apps = append(out.Apps, *f.preview)
	}
	if len(out.Apps) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	// Unescaped, so an app's markup does not grow toward the server's cap on
	// the part (six bytes for each <, > and &).
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
