package wfdir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
)

// ErrNoManifest is returned when a directory (or any of its parents) holds no
// ronja.json. Callers translate it into "run this inside a workflow folder".
var ErrNoManifest = errors.New("no ronja.json found")

// ManifestFormatVersion is the highest ronja.json format this build can read.
//
// It moves only for a change unknown-key preservation CANNOT absorb — a key
// that changed meaning, or an absence that stopped meaning what it used to.
// ADDING a key is not one of those: the whole point of the sidecar is that a
// CLI which has never heard of a key still carries it through untouched, so an
// added key must not cost every older CLI in the field its ability to read the
// file.
//
// 1 is also what an ABSENT formatVersion means. Every manifest committed before
// the key existed has none, and there is no version below 1, so "unstated" and
// "1" are the same claim — which is what lets this ship without rewriting a
// single file in a single customer's repo.
//
// **2 is stacks**, and it is worth writing down exactly why adding a key needed
// a version at all when the paragraph above says adding a key does not. What
// changed is not that "stacks" appeared: it is that the ABSENCE of an
// instances[] entry stopped meaning "this folder has never been pushed there".
// In a v2 manifest a stack says where the folder is bound and the ids live in
// ronja.lock.json, so an older CLI — which sees only the instances[] remainder —
// would read a fully-deployed folder as unbound and its next push would CREATE A
// SECOND SET OF RESOURCES in the customer's organization, reporting success. It
// is the failure InstanceKey's own doc names, arriving through a different door,
// and preservation cannot absorb it: the sidecar keeps the bytes, not the
// meaning. That is the bar for moving this number, and nothing smaller is.
//
// A manifest is written at 2 only when it CONTAINS a stack (see
// Manifest.MarshalJSON). A folder that has never named one round-trips as
// version 1, byte for byte, so nobody is forced onto a new CLI by a push that
// changed nothing meaningful.
//
// **3 is declared dependencies**, and it clears the same bar. Adding the
// `dependencies` key is, on its own, exactly the kind of change the sidecar
// absorbs — an older CLI preserves it perfectly, in place, and never notices.
// That is not what changed. What changed is WHAT THE CODE MEANS. In a v3 folder
// `{{ secret('acme_api', 'token') }}` is an ALIAS, and the id the server is
// meant to receive exists nowhere in the source: this CLI reads the stack's
// bind map and substitutes it in before the files are sent. An older CLI does
// none of that. It carries `dependencies` through untouched and sends the
// literal string `acme_api` as the secret id — and the secret marker family
// SOFT-FAILS an unresolvable id into a warning rather than refusing the save.
// So the push succeeds, the workflow deploys with no secret bound at all, and
// the failure surfaces at run time in an organization the pusher may not be
// watching. A wrong deploy reported as a good one is precisely the bar this
// number exists for, and preservation cannot absorb it: the sidecar keeps the
// bytes, not the meaning.
//
// The residual, stated once and not dressed up: a CLI OLDER THAN THE GATE
// ITSELF has no gate to trip. It reads a v3 manifest, ignores the version key
// it has no field for, and misreads the folder exactly as described above.
// Nothing shipped now can fix that — it is the same residual stacks already
// carries, and the protection is real only from the release that introduced the
// gate forwards.
//
// A manifest is written at 3 only when it CONTAINS a non-empty `dependencies`
// map, on the rule 2 already follows: a folder that declares none round-trips at
// whatever version its content requires, so nobody is pushed onto a newer CLI by
// a change that did not reach their file.
const ManifestFormatVersion = manifestVersionDependencies

// The versions the CONTENT of a manifest can require, named rather than spelled
// as bare integers at the two places that compare them. See
// Manifest.requiredFormatVersion, which is the ONE mapping from content to
// version — a second comparison written inline is how a folder ends up stamped
// with a version its content never earned.
const (
	manifestVersionStacks       = 2
	manifestVersionDependencies = 3
)

// Manifest is ronja.json — the committed half of a workflow folder.
//
// Everything above Instances describes the workflow independently of any
// instance, so it survives being cloned by someone signed in somewhere else.
// Instances holds the bindings, one per (instance, organization) this folder
// has been pushed to.
//
// Keys this build of the CLI does not know are PRESERVED across a rewrite, and
// FormatVersion is the escape hatch for the day preservation is not enough —
// see unknownKeys in forwardcompat.go and ManifestFormatVersion above.
type Manifest struct {
	// FormatVersion is the manifest FORMAT, not the workflow's. It exists so a
	// change that preservation cannot survive — a key whose meaning moved, a
	// value whose absence stopped meaning what it used to — can be REFUSED by an
	// older CLI instead of quietly half-read.
	//
	// A plain int with omitempty, and the CLI never writes 1, for the reason
	// Runtime is not a pointer either: absent already means 1, so stamping it
	// would publish a distinction no code can act on and would make every
	// manifest ever committed differ on disk from one that means exactly the
	// same thing. Every folder written before this key existed is version 1 and
	// says so by saying nothing. The key first appears on the file that first
	// needs version 2.
	//
	// Reading a version ABOVE what this CLI understands is a refusal, not a
	// best-effort read — see LoadManifest.
	FormatVersion int    `json:"formatVersion,omitempty"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`
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
	// Runtime is the workflow's runtime version: 1 — the ORIGINAL runtime, which
	// journals nothing and cannot resume — 2, the DURABLE runtime, whose
	// @tools.step results are journaled so a failed run can be resumed instead
	// of re-run from the top, or 3, which is Durable plus a container that holds
	// no table credential and reads tables only through tools.query. 3 is what a
	// workflow is CREATED on; see the warning below on what an absent key means.
	//
	// A plain int with omitempty rather than the three-state pointer Parameters
	// and Access carry, because the third state has nothing to describe: the
	// runtime only ever moves UP (1 -> 2, 1 -> 3, 2 -> 3; every downgrade is
	// refused), so a pointer would publish a distinction no code could act on.
	//
	// It rides the create body of a first push, and after that a push RAISES a
	// row still on a lower runtime to match (the patch lands on the draft;
	// publish commits the flip). Declaring a LOWER runtime than the row already
	// has is a REFUSAL, not a silent skip: the runtime cannot be lowered, and
	// pushing code written for one runtime at a row running another is the
	// outcome worth an error.
	//
	// ⚠️ ABSENT is NOT "the default runtime" any more — it is "let the SERVER
	// choose", and the server's create default is 3. A folder that wants 1 or 2
	// has to say so. RuntimeVersion() still answers RuntimeDefault for an absent
	// key, because that is what an absent key means to a workflow this CLI has
	// already created (every such row predates the flip, or had the key written
	// back onto it by the push that created it — see resolvePushTarget).
	//
	// So it is written down wherever an answer is actually known: `wf init` writes
	// it whenever --runtime names one (1 included), `wf clone` writes the runtime
	// the cloned row reports (1 included), and the first push of a folder that
	// declared nothing writes back whatever the server stamped.
	//
	// ⚠️ A value this build does not know is refused at LOAD (wfdir.ValidRuntime,
	// applied in LoadManifest), not here. Nothing used to validate it on push, so
	// a hand-edited `"runtime": 3` pushed and was then modelled as v2 by
	// IsDurable's `>= RuntimeDurable` — which happens to be the right answer for
	// 3 and would have been the wrong one for 4.
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
	// Dependencies are the names this folder's CODE uses for rows it does not
	// own — alias → what kind of thing it is. See Dependency in dependencies.go
	// for what an alias is for; each stack's Bind says which of ITS
	// organization's rows answers to each name.
	//
	// Above Stacks because it is instance-independent: a declaration survives
	// being cloned by somebody signed in somewhere else, which is the whole
	// point of it. Presence is what makes the manifest format version 3, so an
	// older CLI refuses the file rather than sending the alias to the server as
	// a literal id — see ManifestFormatVersion.
	Dependencies map[string]Dependency `json:"dependencies,omitempty"`
	// Stacks are the named environments this folder deploys to — "dev", "prod" —
	// each saying where it points and which feature its resources live in. See
	// Stack for what belongs here and what belongs in ronja.lock.json.
	//
	// A MAP keyed by name, where Instances is a list keyed by (url, tenantID),
	// and the difference is the entire feature: a name is a thing a person can
	// say, on a command line and in a message. `--stack prod` is unambiguous
	// where "the entry for https://…, organization ten-…" needed a stored
	// profile to express.
	//
	// Presence is what makes the manifest format version 2, so an older CLI
	// refuses the file rather than reading only the legacy remainder and
	// concluding the folder is unbound — see ManifestFormatVersion.
	Stacks map[string]Stack `json:"stacks,omitempty"`
	// Instances lists what this folder is bound to, one entry per instance and
	// organization. No entry is not an error — it means "never pushed there
	// yet", and the first push records the binding.
	//
	// A LIST rather than a map keyed by URL: one instance can carry several
	// organizations, and each needs its own workflow id. A list also puts the
	// organization on the page for whoever opens the committed file, instead of
	// hiding it inside a composite key.
	//
	// LEGACY, and left alone rather than migrated. It is the pre-stack shape and
	// it still works exactly as it did, ids and all — a folder nobody has named a
	// stack in is never rewritten by a push, which is the promise that keeps a
	// customer off a forced CLI upgrade. Once a stack is named for one
	// (instance, organization), Manifest.Record moves that entry across and drops
	// it from here, so the two shapes never both answer for one place.
	//
	// The tag has no omitempty, so a v1 manifest keeps writing the key it always
	// wrote. MarshalJSON drops it once the folder has stacks, where an empty
	// legacy list would only be an invitation to fill it in.
	Instances []Instance `json:"instances"`

	// unknown is the forward-compatibility sidecar: the root-level keys this
	// build has no field for, plus the order the file's keys appeared in. Set by
	// UnmarshalJSON, replayed by MarshalJSON, and unexported because it is not
	// part of the format — it is what stops the format being truncated.
	unknown unknownKeys
}

// manifestKeys / instanceKeys are the object keys each type marshals to, so the
// sidecar can tell "a key this CLI owns" from "a key from the future".
//
// instanceKeys covers Binding's fields too: Instance embeds Binding, and both
// flatten into ONE object per entry, so workflowID and url are keys of the same
// thing.
var (
	manifestKeys = jsonFieldNames(reflect.TypeOf(Manifest{}))
	instanceKeys = jsonFieldNames(reflect.TypeOf(Instance{}))
)

// UnmarshalJSON decodes the manifest and keeps whatever it did not recognise.
//
// plainManifest is what stops this recursing: a defined type does not carry its
// source type's methods, so json.Unmarshal on it does the ordinary struct decode
// this method is wrapping. It leaks into encoding/json's error text, which is
// why LoadManifest renames it back — see rewriteInternalTypeNames.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var decoded plainManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Manifest(decoded)
	m.unknown.capture(data, manifestKeys)
	return nil
}

// MarshalJSON writes the struct's own keys and puts the unrecognised ones back
// in the places they were read from.
//
// A VALUE receiver, because SaveManifest hands MarshalIndent a *Manifest while
// Instances is a slice of values — a pointer receiver would cover the first and
// silently skip the second.
func (m Manifest) MarshalJSON() ([]byte, error) {
	if required := m.requiredFormatVersion(); required > 1 {
		// The version the CONTENT requires, stamped here rather than by whoever
		// added the stack or the dependency, so no writer can forget it. Raised
		// only — never lowered — because a file that once declared a version this
		// build does not fully own is not a file to quietly downgrade, and
		// because deleting the last dependency from a v3 folder must not
		// re-publish it as a v2 file that an older CLI would then open and
		// rewrite.
		if m.FormatVersion < required {
			m.FormatVersion = required
		}
		// Read as the file's own declaration, not as a line appended after the
		// migration that needed it. See unknownKeys.ensureLeading.
		m.unknown = m.unknown.ensureLeading("formatVersion")
	}
	body, err := json.Marshal(plainManifest(m))
	if err != nil {
		return nil, err
	}
	if len(m.Stacks) > 0 && len(m.Instances) == 0 {
		// The legacy key has no omitempty, so a stack folder with no legacy
		// remainder would carry `"instances": null` for ever. Dropped HERE rather
		// than by tagging the field, because the tag is what keeps a v1 manifest
		// writing the key it has always written — and "byte-identical when
		// nothing changed" is the property that lets a customer stay on their CLI.
		if body, err = dropObjectKey(body, "instances"); err != nil {
			return nil, err
		}
	}
	return m.unknown.merge(body)
}

// StampedFormatVersion is the version a save of this manifest writes into the
// file — the higher of what it already declares and what its content requires.
//
// The one thing a message about "what your colleagues need" may say. The
// ManifestFormatVersion constant is the highest version this BUILD can read,
// which is a different number and a much larger claim: a folder that names a
// stack and declares no dependencies is stamped 2, and telling its author that
// everyone needs a version-3 CLI asks for an upgrade the change never required.
func (m Manifest) StampedFormatVersion() int {
	if required := m.requiredFormatVersion(); required > m.FormatVersion {
		return required
	}
	return m.FormatVersion
}

// requiredFormatVersion is the lowest format version that can express what this
// manifest CONTAINS — the one mapping from content to version, so the stamp and
// any future reader cannot disagree about which files are v3.
//
// Ordered by version descending, since the conditions are not exclusive: a
// folder can perfectly well have both stacks and dependencies, and the answer is
// the higher of the two.
func (m Manifest) requiredFormatVersion() int {
	switch {
	case len(m.Dependencies) > 0:
		return manifestVersionDependencies
	case len(m.Stacks) > 0:
		return manifestVersionStacks
	default:
		// Nothing here needs a version at all, and 1 is what an ABSENT
		// formatVersion means — so this is the answer that keeps a legacy
		// folder's file byte-identical after a rewrite.
		return 1
	}
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
	// Bind is this entry's alias → resource id map, the legacy shape's half of
	// what Stack.Bind carries. See Stack.Bind for why it sits on the ENTRY and
	// not on the embedded Binding — the reason is the same one, and it is the
	// reason that matters most here: SetBinding replaces an entry wholesale from
	// a caller-supplied Binding, so a Bind that travelled inside Binding would
	// be erased by `wf discard`.
	Bind map[string]string `json:"bind,omitempty"`

	// unknown is this ENTRY's forward-compatibility sidecar, covering the keys
	// Instance and its embedded Binding flatten into together.
	//
	// It sits on the entry rather than on Binding because the entry is what the
	// unknown keys belong to — they were read from the object identified by
	// (url, tenantID). SetBinding carries it across when it replaces the entry,
	// so a caller that hands in a freshly built Binding (as `wf discard` does,
	// deliberately resetting to just the featureID) cannot drop it.
	unknown unknownKeys
}

// Key is the (instance, organization) this entry is bound to.
func (i Instance) Key() InstanceKey {
	return InstanceKey{URL: i.URL, TenantID: i.TenantID}
}

// UnmarshalJSON decodes one entry and keeps whatever it did not recognise, for
// the reasons on Manifest.UnmarshalJSON.
//
// ⚠️ These two methods must stay on Instance and must NEVER be added to
// Binding, and the vector is plainInstance rather than Instance itself. On
// Instance these methods sit at depth 0 and SHADOW anything promoted from the
// embedded Binding, so a Binding.MarshalJSON looks harmless — it is not.
// plainInstance has no methods of its own but still embeds Binding, so a
// Binding.MarshalJSON WOULD be promoted onto it and would win inside
// Instance.MarshalJSON: json.Marshal(plainInstance(i)) would emit the binding
// alone, and the entry would lose the url and tenantID that say which instance
// and organization it belongs to — from a file the customer has committed. The
// unmarshal direction fails the same way: a promoted Binding.UnmarshalJSON
// would swallow the whole entry object and url/tenantID would never be decoded.
// TestBindingHasNoJSONMethods is the guard, and it is load-bearing: nothing
// about adding a method to Binding fails to compile.
func (i *Instance) UnmarshalJSON(data []byte) error {
	var decoded plainInstance
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = Instance(decoded)
	i.unknown.capture(data, instanceKeys)
	return nil
}

// MarshalJSON writes the entry's own keys and puts the unrecognised ones back.
func (i Instance) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainInstance(i))
	if err != nil {
		return nil, err
	}
	return i.unknown.merge(body)
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

// RuntimeForValidate answers which runtime a CANDIDATE should be checked
// against, which is not always the one the folder declares.
//
// A folder that declares one is checked against it, on a push to an existing row
// included: that is both what the metadata patch may raise the row to and, either
// way, the runtime this code's author wrote for.
//
// An absent key means "let the instance choose", and validate is a PRE-CREATION
// endpoint with no row to read it from — it reads 0 as "check nothing
// runtime-scoped". So the answer depends on what the push is about to do. A
// CREATE (live == nil) is rehearsed against RuntimeCreateDefault, because that is
// the runtime the workflow will exist on and a 0 would rehearse a different save
// from the one about to happen. A push to an existing row is checked against THAT
// row — and against nothing runtime-scoped when the instance did not report one,
// which is exactly what every caller written before runtimes existed asks for.
func (m *Manifest) RuntimeForValidate(live *api.Workflow) int {
	if m.Runtime != 0 {
		return m.Runtime
	}
	if live == nil {
		return RuntimeCreateDefault
	}
	return live.RuntimeVersion
}

// IsDurable reports the runtime that journals EVERY step without the author
// writing a key — the one property anything outside the create path branches on.
// It is not the same question as "can this be resumed": a standard-runtime
// workflow whose steps carry explicit keys is resumable too, and the journal is
// what decides how much a resume skips.
//
// `>=`, and it is correct rather than accidental: RuntimeQuery is a SUPERSET of
// Durable — same journal, plus the withheld table credential — so every runtime
// above RuntimeDurable journals too. What made `>=` unsafe was not the operator
// but the absence of a load-time check, which let an UNKNOWN runtime through and
// be modelled as Durable on nothing but its number. ValidRuntime is that check.
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
	// real divergence from the two single-resource kinds and the reason the
	// single-id helpers below refuse KindPipeline (and KindAutomation) instead
	// of quietly answering with WorkflowID.
	//
	// Keyed by the SLASH-separated relative path, exactly as Enumerate reports
	// it, so the manifest and the baseline agree about what a file is called on
	// every platform.
	//
	// omitempty: a workflow or data-app manifest must not grow an empty "tables"
	// key, and a pipeline folder that has never pushed has nothing to record.
	Tables map[string]string `json:"tables,omitempty"`
	// Automations is an AUTOMATION folder's binding: local relative path → the
	// `scheduled_jobs` id that file describes. Keyed exactly as Tables is, and
	// separate from it because the two kinds cannot share a folder — one map per
	// kind keeps "which resource does this path name" answerable without also
	// asking what kind of folder we are in.
	//
	// ⚠️ The id is the ONLY tie between a file and its row, and what a lost
	// entry costs is stated once, on LockAutomation.AutomationID. Read it before
	// treating this map as recoverable bookkeeping.
	Automations map[string]string `json:"automations,omitempty"`
}

// ErrMultiResourceBinding is returned by the single-id binding accessors for a
// kind whose folder binds many resources.
//
// A sentinel rather than a formatted string because the refusal is a PROGRAMMING
// error, not a user-facing one: reaching here means a command asked a pipeline
// folder for "the" bound row, and the answer is that it has to use Tables.
var ErrMultiResourceBinding = errors.New("this folder kind binds many resources, not one — use the per-path map its kind keeps (Binding.Tables, Binding.Automations)")

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
	case KindPipeline, KindAutomation:
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
	case KindPipeline, KindAutomation:
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

// WithAutomation returns a copy binding one relative path to one automation id.
//
// It COPIES the map for WithTable's reason, which bites harder here: a binding
// "recorded" by writing through the caller's map and then lost to a failed save
// is a path whose row the next push cannot find, and for this kind that is not a
// miss — see LockAutomation.AutomationID for what the next push does instead.
func (b Binding) WithAutomation(path, automationID string) Binding {
	automations := make(map[string]string, len(b.Automations)+1)
	for k, v := range b.Automations {
		automations[k] = v
	}
	automations[path] = automationID
	b.Automations = automations
	return b
}

// WithoutAutomation returns a copy of the binding with one path dropped — what
// `automation push --prune` records once the row behind a deleted file is gone.
//
// It COPIES for WithAutomation's reason, and the asymmetry matters here too: a
// forget written through the caller's map and then LOST to a failed save leaves
// a folder still bound to a row that no longer exists, whose next push refuses
// on a binding nobody can repair from the message. Copying means the forget is
// recorded exactly when the save succeeds.
//
// A nil map answers a nil map: dropping a path from a binding that has none is
// not a state worth allocating for.
func (b Binding) WithoutAutomation(path string) Binding {
	if len(b.Automations) == 0 {
		return b
	}
	automations := make(map[string]string, len(b.Automations))
	for k, v := range b.Automations {
		if k == path {
			continue
		}
		automations[k] = v
	}
	b.Automations = automations
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
	// The format gate runs BEFORE the full decode, off a probe that reads one
	// key, because a manifest from the future is exactly the file whose decode
	// cannot be trusted to fail in a legible way: a reshaped "instances" would
	// hit the legacy-object migration message below and send its author to
	// rewrite a file that is not old but new. A probe that cannot parse at all
	// is left to the decode, which produces the better error for that case.
	if err := checkFormatVersion(path, raw, ManifestFormatVersion); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		// "instances" used to be an object keyed by URL, before one instance
		// could carry several organizations. A folder written by that shape
		// fails here with `cannot unmarshal object into []Instance`, which says
		// nothing about what to do — so name the fix instead.
		//
		// Matched on the TYPED error rather than on the message. The message
		// names the Go type the decode was working on, and Manifest.UnmarshalJSON
		// decodes through the plainManifest twin, so the struct name in it is an
		// implementation detail of this package that a substring match would
		// quietly stop recognising.
		//
		// The nested case does NOT collide with this one: a bad field inside an
		// entry arrives as Field "instances.url", never as bare "instances".
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field == "instances" && typeErr.Value == "object" {
			// The command named is the CALLER'S OWN, not a hardcoded `ronja wf
			// init`: this refusal fires before `kind` has been looked at, so the
			// only thing known about the folder is which command opened it — and
			// telling a pipeline author to recreate their folder with `wf init`
			// sends them to a command that would refuse it.
			return nil, fmt.Errorf("%s has an old-style \"instances\" object — it is now a list, one entry per instance and organization. Recreate the folder with `%s init`, or rewrite the entry as [{\"url\": …, \"tenantID\": …, \"workflowID\": …, \"featureID\": …}]",
				path, kind.Command)
		}
		// Every other decode failure goes to a person hand-editing a committed
		// file, so it must not name a type that exists only inside this package.
		rewriteInternalTypeNames(err)
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
	// A runtime this build does not know is refused HERE, so it fails at the push
	// (and at `wf test`) that would have created a workflow nobody can name,
	// rather than at the first unattended run. The manifest is committed, so the
	// value can arrive from a hand edit or from a colleague on a newer CLI —
	// naming the versions this build understands is the whole message.
	if !ValidRuntime(m.Runtime) {
		return nil, fmt.Errorf("%s declares \"runtime\": %d, which this CLI does not know — the runtimes it understands are %s. Upgrade the CLI — run: ronja update — or set a runtime from that list",
			path, m.Runtime, joinInts(Runtimes))
	}
	if err := m.checkStacks(path); err != nil {
		return nil, err
	}
	if err := m.checkDependencies(path); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.Entrypoint) == "" {
		m.Entrypoint = kind.DefaultEntrypoint
	}
	return &m, nil
}

// checkFormatVersion refuses a file whose declared format this build does not
// understand, before anything has looked at its shape.
//
// Shared by the manifest and the lock file rather than written twice, with the
// version each understands passed in. They are two files with one hazard — both
// committed, both rewritten whole by whoever opens them — so a gate that existed
// on one and not the other would be a gap nobody would notice until a lock file
// from a newer CLI came back stripped.
//
// The declared version is read as a NUMBER rather than into an int field,
// because the gate has to fire on every spelling of a future version. `2.0`,
// `2e1` and `2.5` are all rejected by an int decode, and a probe that decodes
// into an int therefore treats each of them as "no version stated", skips the
// gate, and hands the author a type error about a Go field instead of the one
// sentence that helps: upgrade the CLI.
//
// A value that is not a number at all — `"2"`, `true`, an object — is REFUSED
// rather than ignored. The alternative is to fall through to the typed decode,
// which does say something ("cannot unmarshal string into … formatVersion of
// type int") but says it about the wrong thing: the reader is told they typed
// the wrong Go type when what they need to know is that this file's format
// could not be established and nothing was read from it. `null` is the one
// exception, and it is absence rather than a spelling: null into a typed field
// is a documented no-op everywhere else in this decode, so a manifest carrying
// it means exactly what one omitting the key means — version 1.
//
// A file that does not parse at all is left to the decode, which produces the
// better error for that case.
func checkFormatVersion(path string, raw []byte, understood int) error {
	var probe struct {
		FormatVersion json.RawMessage `json:"formatVersion"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.FormatVersion) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(probe.FormatVersion))
	decoder.UseNumber()
	var declared any
	if err := decoder.Decode(&declared); err != nil {
		return nil
	}
	switch value := declared.(type) {
	case nil:
		return nil
	case json.Number:
		// A number too large to represent is certainly not a version this build
		// understands, so an unparseable one refuses with the rest.
		if declaredVersion, err := value.Float64(); err == nil && declaredVersion <= float64(understood) {
			// At or below what this build reads. A spelling the FormatVersion
			// int cannot take even so (`1.0`) falls through to the typed decode,
			// which names both the file and the key.
			return nil
		}
		// A refusal rather than a best-effort read, because the alternative is
		// the silent downgrade this whole change exists to stop: read what we
		// recognise, write the file back, and whatever the newer format carries
		// that preservation could not is gone from the customer's repo.
		return fmt.Errorf("%s is format version %s, and this ronja understands %d — upgrade the CLI, run: ronja update (`ronja --version` reports what you are running). Reading it with this build would be a guess, and rewriting it could drop what the newer format carries",
			path, value.String(), understood)
	default:
		return fmt.Errorf("%s declares \"formatVersion\": %s, which is not a number. It is the FILE FORMAT version and has to be a bare number this build can compare against the %d it understands; leaving the key out means version 1. Nothing was read from the file",
			path, probe.FormatVersion, understood)
	}
}

// kindByName resolves a manifest's `kind` back to the Kind that owns it, so a
// wrong-kind refusal can name the command that WOULD work.
//
// It is also the registry the kind-safety test walks: every entry must carry a
// Command, and no two may share one.
var kindByName = map[string]Kind{
	KindWorkflow:   WorkflowKind,
	KindDataApp:    DataAppKind,
	KindPipeline:   PipelineKind,
	KindAutomation: AutomationKind,
}

// KindByName resolves a manifest's `kind` for a caller OUTSIDE this package —
// one that has to read a folder's kind without already knowing it.
//
// Exported because there are exactly two such readers and BOTH were hand-kept
// lists that the fourth kind was added without: `ronja bind`'s kind-agnostic
// probe (folderKindAt) and `ronja context`'s local-work report. Neither failure
// is loud. The probe reports an unrecognised kind as an unreadable folder, which
// makes `ronja sync status` exit 2 for the whole tree; the report falls back to
// "this is a workflow folder" and recommends `ronja wf` — which is precisely the
// misrouting Kind.Command was introduced to stop. Derived from the registry,
// a fifth kind cannot repeat either.
func KindByName(name string) (Kind, bool) {
	kind, ok := kindByName[name]
	return kind, ok
}

// AllKinds is every folder kind this build understands, ordered by name so a
// listing built from it reads the same way twice.
func AllKinds() []Kind {
	names := make([]string, 0, len(kindByName))
	for name := range kindByName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Kind, 0, len(names))
	for _, name := range names {
		out = append(out, kindByName[name])
	}
	return out
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
// ⚠️ This reads the LEGACY instances[] shape ONLY, and a folder that has named
// stacks will answer "not bound here" to it — which every push path treats as
// "create it". Nothing in the commands should call this; go through
// Manifest.Select, which joins the stack config with the lock file's state and
// falls back to instances[] for a folder that has not migrated. It stays
// exported because the tests that pin the legacy shape need to read it directly,
// and because SetBinding below is still how Record writes that half.
//
// The error is ErrAmbiguousInstance, and only a caller with an UNKNOWN
// organization can get it — see Find.
//
// Who that is: the three `status` commands, which deliberately neither resolve
// nor refuse — signed out, or holding a credential whose organization lookup
// failed. Commands that ACT resolve the organization before reading a binding
// (commands.folder.resolveBinding) and refuse when they cannot.
//
// It used to say here that resolving a credential resolved the organization
// with it, so this reached only the signed-out half of `wf status`. That was
// wrong, and expensively: picking a profile and finding a token in it says
// nothing about which organization that token reaches, and an environment token
// carries none at all. Every command reached this with an unknown organization
// on that path, read the ambiguity as "not bound here", and reported a missing
// featureID — advice to add a key the file already had.
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
			continue
		}
		// The entry being replaced may carry keys this build does not know, and
		// they belong to the (instance, organization) rather than to the Binding
		// the caller passed — which may be a freshly built one, as `wf discard`
		// deliberately hands in. Carry them across so replacing an entry updates
		// it rather than truncating it.
		entry.unknown = existing.unknown
		// The alias bind map is carried for the same reason and one stronger
		// one: it is not merely unrecognised, it is CONFIG this build fully
		// understands and that no deploy ever writes. Dropping it here would
		// mean a `wf discard` — or any push through the legacy path — silently
		// deleted a human's committed alias bindings, which is the exact
		// direction the config/state split must never go.
		entry.Bind = existing.Bind
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
