package wfdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
)

// ErrNoManifest is returned when a directory (or any of its parents) holds no
// ronja.json. Callers translate it into "run this inside a workflow folder".
var ErrNoManifest = errors.New("no ronja.json found")

// Manifest is ronja.json — the committed half of a workflow folder.
//
// Everything above Instances describes the workflow independently of any
// instance, so it survives being cloned by someone signed in somewhere else.
// Instances holds the bindings, one per (instance, organization) this folder
// has been pushed to.
type Manifest struct {
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Entrypoint string `json:"entrypoint"`
	// Parameters are the workflow's declared parameters — the things `wf test
	// --param` supplies and the script reads with tools.getVariable(name).
	//
	// A POINTER so the manifest can say three things rather than two, and the
	// difference is load-bearing:
	//
	//	absent (nil)  — this folder does not manage parameters. Push never
	//	                touches them. Every folder created before parameters
	//	                were part of the manifest is this, which is why the
	//	                distinction has to exist: treating absent as "declares
	//	                none" would make the first push after upgrading CLEAR
	//	                whatever the workflow had.
	//	[] (non-nil)  — managed, and declares none. Push clears the row's.
	//	[...]         — managed, and this is the set.
	//
	// `wf init` and `wf clone` both write the key, so a folder created by
	// either manages its parameters from the start.
	//
	// The api type is reused rather than mirrored on purpose: a parameter is
	// passed through to create/validate/patch verbatim, and a second struct
	// would only add a conversion that silently drops any field added to one
	// side. Its json tags are therefore part of the ronja.json FILE FORMAT, not
	// just the wire — renaming one is a breaking change to committed manifests.
	Parameters *[]api.WorkflowParameter `json:"parameters,omitempty"`
	// Access is the DATA-APP analogue of Parameters: the capability allowlists
	// (tables, secrets, agents, workflows, codexes, metrics) and the capability
	// set the app's minted script-token is scoped from.
	//
	// It is in the manifest — and Parameters is not enough — because the two
	// kinds derive their bindings completely differently. A workflow's come from
	// `{{ ref }}` / `{{ write }}` / `{{ secret }}` markers IN THE CODE, so the
	// server can re-derive them from a file save. A data app's are explicit
	// columns that POST :id/validate deliberately does NOT scan source for, so
	// nothing on the server can recover them from the files: an app pushed
	// without them compiles and then can query nothing. A folder that could not
	// carry them would not be a complete description of its app.
	//
	// Pointer for exactly the reasons Parameters is one, and the three states
	// mean the same things:
	//
	//	absent (nil)  — this folder does not manage the allowlists. Push never
	//	                touches them, which is what a folder created before this
	//	                existed (or one deliberately leaving them to the web UI)
	//	                needs, since treating absent as "declares none" would
	//	                REVOKE the app's access on the first push after upgrading.
	//	zero value    — managed, and declares none.
	//	populated     — managed, and this is the set.
	//
	// Note what this makes ronja.json: a file that declares privileges, in the
	// customer's git. That is not a hole — governance.ValidateDataAppScope
	// re-checks every id against the pusher's own access at checkout,
	// ValidateAllowlistRefsLive re-checks at publish, and committing a shared
	// app's draft is admin-gated — but it is why `app push` prints the allowlist
	// change it is about to make rather than only noting that one exists.
	Access *api.DataAppAccess `json:"access,omitempty"`
	// Instances lists what this folder is bound to, one entry per instance and
	// organization. No entry is not an error — it means "never pushed there
	// yet", and the first push records the binding.
	//
	// A LIST rather than a map keyed by URL: one instance can carry several
	// organizations, and each needs its own workflow id. A list also puts the
	// organization on the page for whoever opens the committed file, instead of
	// hiding it inside a composite key.
	Instances []Instance `json:"instances"`
}

// Instance is one binding: where it points, and what it points at.
//
// TenantID is an organization identifier, not a secret. It is opaque, appears
// in ordinary API paths, and is useless without a credential — it is here for
// the same reason the URL is, so a committed folder says plainly which
// organization each binding belongs to.
type Instance struct {
	URL      string `json:"url"`
	TenantID string `json:"tenantID,omitempty"`
	Binding
}

// Key is the (instance, organization) this entry is bound to.
func (i Instance) Key() InstanceKey {
	return InstanceKey{URL: i.URL, TenantID: i.TenantID}
}

// ManagesParameters reports whether the manifest is the source of truth for the
// workflow's parameters — i.e. whether push should sync them at all.
func (m *Manifest) ManagesParameters() bool { return m.Parameters != nil }

// DeclaredParameters is the declared set, empty when the folder does not manage
// them. For reading; use ManagesParameters to decide whether to WRITE.
func (m *Manifest) DeclaredParameters() []api.WorkflowParameter {
	if m.Parameters == nil {
		return nil
	}
	return *m.Parameters
}

// SetParameters marks the folder as managing parameters and records the set. A
// nil params still marks it managed, as an empty declaration — callers that mean
// "stop managing" clear the field directly.
func (m *Manifest) SetParameters(params []api.WorkflowParameter) {
	if params == nil {
		params = []api.WorkflowParameter{}
	}
	m.Parameters = &params
}

// ManagesAccess reports whether the manifest is the source of truth for a data
// app's allowlists — i.e. whether push should sync them at all.
func (m *Manifest) ManagesAccess() bool { return m.Access != nil }

// DeclaredAccess is the declared allowlist set, zero-valued when the folder does
// not manage it. For reading; use ManagesAccess to decide whether to WRITE.
func (m *Manifest) DeclaredAccess() api.DataAppAccess {
	if m.Access == nil {
		return api.DataAppAccess{}
	}
	return m.Access.Normalized()
}

// SetAccess marks the folder as managing the allowlists and records them,
// normalized so a manifest never round-trips `null` where the app declares an
// empty list.
func (m *Manifest) SetAccess(access api.DataAppAccess) {
	normalized := access.Normalized()
	m.Access = &normalized
}

// Binding is what one instance knows about this folder.
//
// WorkflowID is ABSENT until the first push: `wf init` binds a feature (the
// container the workflow will be created in) but creates nothing server-side,
// so there is no id to record yet. FeatureID is what makes that first push
// possible, which is why it is recorded from the start.
//
// WorkflowID is always the STABLE identity — the live row, or a parentless
// draft's own id — never a per-user edit draft's id. Draft ids come and go with
// every checkout/commit cycle; a manifest that recorded one would be a
// committed file pointing at a row that stops existing the moment its author
// publishes.
type Binding struct {
	WorkflowID string `json:"workflowID,omitempty"`
	// DataAppID is the WorkflowID of a data-app folder. Two fields rather than
	// one polymorphic "resourceID" because the manifest is a file people read: a
	// key naming what it points at is worth more than one saved line, and the
	// kind gate means only one of the two can ever be populated.
	//
	// Like WorkflowID, this may name a row that is still a parentless DRAFT:
	// since migration 000495 POST /dataapp mints a draft, and publish promotes
	// that same row in place, so the id is stable from first push onwards but
	// only names a LIVE app once it has been published.
	DataAppID string `json:"dataAppID,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
}

// ResourceID is the bound row for a kind, so callers that do not care which kind
// they are working with do not have to branch.
func (b Binding) ResourceID(kind Kind) string {
	if kind.Name == KindDataApp {
		return b.DataAppID
	}
	return b.WorkflowID
}

// WithResourceID returns a copy naming the bound row for a kind.
func (b Binding) WithResourceID(kind Kind, id string) Binding {
	if kind.Name == KindDataApp {
		b.DataAppID = id
	} else {
		b.WorkflowID = id
	}
	return b
}

// FindRoot walks up from start looking for a ronja.json, git-style, so the
// commands work from a subdirectory of the folder rather than only from its
// root. Returns ErrNoManifest when the filesystem root is reached.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", start, err)
	}
	for {
		if _, err := os.Stat(ManifestPath(dir)); err == nil {
			return dir, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read %s: %w", ManifestPath(dir), err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoManifest
		}
		dir = parent
	}
}

// LoadManifest reads and validates the manifest at a folder root, refusing one
// that describes a different KIND than the caller works with.
//
// That refusal is load-bearing rather than tidy. A data-app folder and a
// workflow folder are indistinguishable by shape — both are source files plus a
// ronja.json — so without the check `ronja wf push` inside a data-app folder
// would create a workflow from TSX files, and `ronja app push` inside a workflow
// folder would overwrite an app's source with Python. Both destroy work while
// reporting success.
func LoadManifest(root string, kind Kind) (*Manifest, error) {
	path := ManifestPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoManifest
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		// "instances" used to be an object keyed by URL, before one instance
		// could carry several organizations. A folder written by that shape
		// fails here with `cannot unmarshal object into []Instance`, which says
		// nothing about what to do — so name the fix instead.
		if strings.Contains(err.Error(), "cannot unmarshal object into Go struct field Manifest.instances") {
			return nil, fmt.Errorf("%s has an old-style \"instances\" object — it is now a list, one entry per instance and organization. Recreate the folder with `ronja wf init`, or rewrite the entry as [{\"url\": …, \"tenantID\": …, \"workflowID\": …, \"featureID\": …}]", path)
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Kind == "" {
		// Distinguished from a wrong kind because the causes differ: an absent
		// kind is a hand-written or truncated manifest, and `describes a ""
		// folder` reads like a bug in the CLI rather than a missing field.
		return nil, fmt.Errorf("%s has no kind — add %q to it, or recreate the folder with `%s init`",
			path, `"kind": "`+kind.Name+`"`, commandFor(kind))
	}
	if m.Kind != kind.Name {
		// Names the OTHER command rather than only refusing, because a wrong kind
		// is almost always the right folder and the wrong verb — someone typing
		// `wf push` out of habit inside a data-app folder.
		if other, ok := kindByName[m.Kind]; ok {
			return nil, fmt.Errorf("%s describes a %s, not a %s — use `%s` instead",
				path, other.Label, kind.Label, commandFor(other))
		}
		return nil, fmt.Errorf("%s describes a %q folder — this command only understands kind %q",
			path, m.Kind, kind.Name)
	}
	if strings.TrimSpace(m.Entrypoint) == "" {
		m.Entrypoint = kind.DefaultEntrypoint
	}
	return &m, nil
}

// kindByName resolves a manifest's `kind` back to the Kind that owns it, so a
// wrong-kind refusal can name the command that WOULD work.
var kindByName = map[string]Kind{
	KindWorkflow: WorkflowKind,
	KindDataApp:  DataAppKind,
}

// commandFor names the CLI command a kind's folders belong to.
func commandFor(kind Kind) string {
	if kind.Name == KindDataApp {
		return "ronja app"
	}
	return "ronja wf"
}

// SaveManifest writes the manifest back. It is committed to the customer's
// repo, so it is indented and newline-terminated — a file people read and
// diff, not an opaque blob.
func SaveManifest(root string, m *Manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	return writeAtomic(ManifestPath(root), append(body, '\n'), 0o644)
}

// Binding returns this folder's binding for one (instance, organization), and
// whether one exists. The bool matters: an absent entry is the ordinary "not
// pushed here yet" state, not a failure.
//
// The error is ErrAmbiguousInstance, and only a caller with an UNKNOWN
// organization can get it — see Find. Every command that resolves a credential
// knows its organization, so in practice this reaches only the signed-out half
// of `wf status`.
func (m *Manifest) Binding(key InstanceKey) (Binding, bool, error) {
	entry, ok, err := m.BindingEntry(key)
	return entry.Binding, ok, err
}

// BindingEntry is Binding plus the entry's own key.
//
// The key matters when the caller's organization was UNKNOWN: a matched entry
// names the organization it belongs to, which is a free answer to a question
// that would otherwise cost a round trip to /me. It is also the right answer —
// this folder is bound there, whatever the ambient credential thinks.
func (m *Manifest) BindingEntry(key InstanceKey) (Instance, bool, error) {
	i, err := Find(m.Instances, Instance.Key, key)
	if err != nil || i < 0 {
		return Instance{}, false, err
	}
	return m.Instances[i], true, nil
}

// SetBinding records (or updates) the binding for one (instance, organization),
// writing the canonical URL spelling and replacing any equivalent entry.
//
// Without the replace, a push against a hand-edited "https://app.ronja.tech/"
// entry would leave the manifest carrying that one AND a canonical one, and
// which of the two answered a later Binding() would depend on nothing legible.
func (m *Manifest) SetBinding(key InstanceKey, b Binding) {
	url := key.URL
	if norm, err := config.NormalizeURL(url); err == nil {
		url = norm
	}
	entry := Instance{URL: url, TenantID: key.TenantID, Binding: b}

	kept := m.Instances[:0]
	for _, existing := range m.Instances {
		if !Matches(existing.Key(), key) {
			kept = append(kept, existing)
		}
	}
	m.Instances = append(kept, entry)
	// Deterministic on disk, so a push does not reorder the committed file for
	// reasons unrelated to what changed.
	sort.Slice(m.Instances, func(a, b int) bool {
		if m.Instances[a].URL != m.Instances[b].URL {
			return m.Instances[a].URL < m.Instances[b].URL
		}
		return m.Instances[a].TenantID < m.Instances[b].TenantID
	})
}
