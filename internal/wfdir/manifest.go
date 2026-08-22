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
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Entrypoint is the ONE file the server runs, and it is omitempty because a
	// pipeline folder has no such file at all: a folder of .sql tables has no
	// distinguished member, so `"entrypoint": ""` in a committed manifest would be
	// a key inviting somebody to fill it in with something meaningless. Safe for
	// the other two kinds because LoadManifest already falls back to
	// Kind.DefaultEntrypoint, so an absent key reads exactly as an empty one did.
	Entrypoint string `json:"entrypoint,omitempty"`
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
	// Runtime is the workflow's runtime version: 1 (the default) or 2, the
	// DURABLE runtime, whose @tools.step results are journaled so a failed run
	// can be resumed instead of re-run from the top.
	//
	// A plain int with omitempty rather than the three-state pointer Parameters
	// and Access carry, because the third state has nothing to describe. The
	// runtime moves ONE WAY (1 -> 2), so "unmanaged" and "declares the default"
	// both mean "send nothing, get v1" — a pointer would publish a distinction no
	// code could act on, and would make every ronja.json written before durable
	// workflows existed differ on disk from one that means exactly the same thing.
	//
	// It rides the create body of a first push, and after that a push RAISES a
	// row still on runtime 1 to match (the patch lands on the draft; publish
	// commits the flip). Declaring 1 against a row that is already Durable is a
	// REFUSAL, not a silent skip: the runtime cannot be lowered, and pushing v1
	// code at a v2 row is the outcome worth an error.
	//
	// 0 is therefore absent, and absent is 1. `wf init` writes the key only for
	// --runtime 2, so a v1 folder's manifest is byte-identical to what the CLI
	// wrote before this existed.
	Runtime int `json:"runtime,omitempty"`
	// ReportingTimezone is the IANA calendar the workflow's DuckDB session runs
	// at on every run — what date_trunc and date casts bucket against.
	//
	// It may live in a COMMITTED, shared file at all because a plain IANA name is
	// instance-independent: "Europe/Stockholm" means the same calendar on
	// localhost, staging and production, and on every profile. Nothing about it
	// is an id, a credential or an organization's, which is what keeps it above
	// Instances rather than inside a binding.
	//
	// A POINTER, with the same three states Parameters has and for the same
	// back-compat reason:
	//
	//	absent (nil)  — this folder does not manage the zone. Push never touches
	//	                it, which is what every folder created before this key
	//	                existed needs: reading absent as "declares UTC" would make
	//	                the first push after upgrading silently re-bucket the
	//	                workflow's calendar onto UTC.
	//	"" (non-nil)  — managed, declaring the RESET value. The API cannot express
	//	                "no declaration" (an explicit "" patches the row to the
	//	                literal "UTC"), so an empty string here means UTC, not
	//	                "leave it alone" — that is what absent is for.
	//	"Europe/…"    — managed, and this is the calendar. Push makes the row match.
	//
	// `wf clone` writes the key when the row DECLARES a zone; `wf init` leaves it
	// absent so a fresh folder inherits the organization's default.
	ReportingTimezone *string `json:"reportingTimezone,omitempty"`
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

// RuntimeVersion is the runtime this folder's workflow runs under, with the
// absent key resolved to the default. Read this rather than the field: an
// unstated runtime and a stated 1 are the same workflow, and only the create
// path cares which spelling the file used.
func (m *Manifest) RuntimeVersion() int {
	if m.Runtime == 0 {
		return RuntimeDefault
	}
	return m.Runtime
}

// IsDurable reports the runtime that journals EVERY step without the author
// writing a key — the one property anything outside the create path branches on.
// It is not the same question as "can this be resumed": a standard-runtime
// workflow whose steps carry explicit keys is resumable too, and the journal is
// what decides how much a resume skips.
func (m *Manifest) IsDurable() bool { return m.RuntimeVersion() >= RuntimeDurable }

// ManagesReportingTimezone reports whether the manifest is the source of truth
// for the workflow's declared execution calendar — i.e. whether push should sync
// it at all.
func (m *Manifest) ManagesReportingTimezone() bool { return m.ReportingTimezone != nil }

// DeclaredReportingTimezone is the declared zone, empty when the folder does not
// manage it. For reading; use ManagesReportingTimezone to decide whether to
// WRITE — an empty answer means "declares the reset value" for a managed folder
// and "no opinion" for an unmanaged one, and only the pointer tells them apart.
//
// Trimmed, because the server stores the trimmed form: a hand-edited
// " Europe/Stockholm " that compared unequal to what the row holds would patch
// on every push, forever.
func (m *Manifest) DeclaredReportingTimezone() string {
	if m.ReportingTimezone == nil {
		return ""
	}
	return strings.TrimSpace(*m.ReportingTimezone)
}

// SetReportingTimezone marks the folder as managing the declared zone and
// records it. An empty zone still marks it managed, as a declaration of the
// reset value — callers that mean "stop managing" clear the field directly.
func (m *Manifest) SetReportingTimezone(zone string) {
	zone = strings.TrimSpace(zone)
	m.ReportingTimezone = &zone
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
	// Tables is a PIPELINE folder's binding: local relative path → the live
	// `table-…` id that file builds. One folder, many resources — which is the
	// one real divergence from the other two kinds and the reason the single-id
	// helpers below refuse KindPipeline instead of quietly answering with
	// WorkflowID.
	//
	// Keyed by the SLASH-separated relative path, exactly as Enumerate reports
	// it, so the manifest and the baseline agree about what a file is called on
	// every platform.
	//
	// omitempty: a workflow or data-app manifest must not grow an empty "tables"
	// key, and a pipeline folder that has never pushed has nothing to record.
	Tables map[string]string `json:"tables,omitempty"`
}

// ErrMultiResourceBinding is returned by the single-id binding accessors for a
// kind whose folder binds many resources.
//
// A sentinel rather than a formatted string because the refusal is a PROGRAMMING
// error, not a user-facing one: reaching here means a command asked a pipeline
// folder for "the" bound row, and the answer is that it has to use Tables.
var ErrMultiResourceBinding = errors.New("this folder kind binds many resources, not one — use Binding.Tables")

// ResourceID is the bound row for a kind, so callers that do not care which kind
// they are working with do not have to branch.
//
// It REFUSES a multi-resource kind rather than falling through to WorkflowID.
// The fall-through was safe while there were exactly two kinds and the branch
// was `if dataapp { … } else { workflow }`; with a third it becomes a silent
// wrong answer — an empty string that reads as "never pushed here" for a folder
// that is bound to a dozen tables, which a push would then act on by creating
// all of them again.
func (b Binding) ResourceID(kind Kind) (string, error) {
	switch kind.Name {
	case KindWorkflow:
		return b.WorkflowID, nil
	case KindDataApp:
		return b.DataAppID, nil
	case KindPipeline:
		return "", fmt.Errorf("%s: %w", kind.Label, ErrMultiResourceBinding)
	default:
		return "", fmt.Errorf("unknown folder kind %q", kind.Name)
	}
}

// WithResourceID returns a copy naming the bound row for a kind, refusing a
// multi-resource kind for the reason ResourceID does — and here the fall-through
// would have been worse than a wrong read: it would have WRITTEN a table id into
// a pipeline manifest's workflowID field, where nothing would ever look for it.
func (b Binding) WithResourceID(kind Kind, id string) (Binding, error) {
	switch kind.Name {
	case KindWorkflow:
		b.WorkflowID = id
		return b, nil
	case KindDataApp:
		b.DataAppID = id
		return b, nil
	case KindPipeline:
		return b, fmt.Errorf("%s: %w", kind.Label, ErrMultiResourceBinding)
	default:
		return b, fmt.Errorf("unknown folder kind %q", kind.Name)
	}
}

// WithTable returns a copy binding one relative path to one live table id.
//
// It COPIES the map rather than writing through it. Binding is a value type but
// a map field is a reference, so mutating the caller's copy in place would reach
// into whatever Manifest.Instances entry it came from — a binding "recorded"
// before SaveManifest ran, and silently kept after a failure that should have
// left it unwritten.
func (b Binding) WithTable(path, tableID string) Binding {
	tables := make(map[string]string, len(b.Tables)+1)
	for k, v := range b.Tables {
		tables[k] = v
	}
	tables[path] = tableID
	b.Tables = tables
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
			// The command named is the CALLER'S OWN, not a hardcoded `ronja wf
			// init`: this refusal fires before `kind` has been looked at, so the
			// only thing known about the folder is which command opened it — and
			// telling a pipeline author to recreate their folder with `wf init`
			// sends them to a command that would refuse it.
			return nil, fmt.Errorf("%s has an old-style \"instances\" object — it is now a list, one entry per instance and organization. Recreate the folder with `%s init`, or rewrite the entry as [{\"url\": …, \"tenantID\": …, \"workflowID\": …, \"featureID\": …}]",
				path, kind.Command)
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Kind == "" {
		// Distinguished from a wrong kind because the causes differ: an absent
		// kind is a hand-written or truncated manifest, and `describes a ""
		// folder` reads like a bug in the CLI rather than a missing field.
		return nil, fmt.Errorf("%s has no kind — add %q to it, or recreate the folder with `%s init`",
			path, `"kind": "`+kind.Name+`"`, kind.Command)
	}
	if m.Kind != kind.Name {
		// Names the OTHER command rather than only refusing, because a wrong kind
		// is almost always the right folder and the wrong verb — someone typing
		// `wf push` out of habit inside a data-app folder.
		if other, ok := kindByName[m.Kind]; ok {
			return nil, fmt.Errorf("%s describes a %s, not a %s — use `%s` instead",
				path, other.Label, kind.Label, other.Command)
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
//
// It is also the registry the kind-safety test walks: every entry must carry a
// Command, and no two may share one.
var kindByName = map[string]Kind{
	KindWorkflow: WorkflowKind,
	KindDataApp:  DataAppKind,
	KindPipeline: PipelineKind,
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
