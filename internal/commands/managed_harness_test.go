package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// fakeManaged is the server-side state behind an adopted folder — the
// `managed_resources` ledger and its five routes (api/v2/managed), plus the
// claim a create route inserts in its own transaction.
//
// It MODELS THE SERVER, not the CLI's intent, and every behaviour below cites
// the server file it mirrors (backend/resource/rmanaged,
// backend/api/v2/managed). Shared by the four
// kind fakes: each serves its own rows and hands this fake two things — the
// requests under /api/v2/managed, and a probe of its rows (fakeRowFacts), which
// is what the real route's probe.go reads off each kind's table.
//
// ⚠️ The probe is called WITHOUT the host fake's lock: a create holds that lock
// while it inserts its claim, and a 409's holder view probes the host's rows
// (sync.Mutex is not reentrant). The CLI makes its requests one at a time on
// every path these tests drive, so the maps are never written concurrently.
type fakeManaged struct {
	t  *testing.T
	mu sync.Mutex

	claims map[string]*fakeClaim
	nextID int
	// probe answers one row's facts from the host fake (probe.go loadFacts).
	probe func(kind, id string) fakeRowFacts

	// setRowFeature moves one of the host fake's rows to another feature — the
	// row half of a cross-feature move (each kind's MoveToFeatureInTx). Set by
	// every host; moveRow is the whole move, ledger half included.
	setRowFeature func(kind, id, feature string)

	// Overrides of what the probe answers, for the states a host fake has no
	// field for (probe.go reachable / rowState):
	//   unreadable — the caller may not read the row (another person's draft);
	//   trashed    — the row is in the trash;
	//   gone       — the row no longer exists (purged).
	unreadable map[string]bool
	trashed    map[string]bool
	gone       map[string]bool

	// The CALLER, as the server sees it (probe.go):
	//   principal — the credential the draft rule matches a drafter against;
	//   admin     — USR_ADMIN: the role floor of a table or metric claim,
	//               anchor or create (probe.go), and an admin reads a
	//               proposal (probe.go);
	//   denied    — token scopes this credential lacks, "<scope>:<read|write>":
	//               structure on the routes (handler.go, claims.go) and
	//               each row kind's own (probe.go);
	//   readOnlyFeatures — features the caller may read but not write
	//               (rmanaged.FeatureWrite, claim_tx.go, store.go).
	principal        string
	admin            bool
	denied           map[string]bool
	readOnlyFeatures map[string]bool
	// itemCode answers a claim item for this resource id with a refusal of
	// this code — "internal_error" is a server fault on that one item
	// (claim_tx.go: an unmapped error), which the docs say to retry.
	itemCode map[string]string
	// itemCodeOnce answers only the FIRST claim of each such resource with its
	// itemCode — a transient fault a retry would get past.
	itemCodeOnce bool
	// moveConflictOnce makes the next POST /managed/move lose to a concurrent
	// move of the same claim: the uncoded 409 rjerr.Conflictf answers
	// (store.go).
	moveConflictOnce bool

	// noRoute makes every managed path answer the way an instance predating the
	// routes does: the router's plain-text breadcrumb 404, which is not JSON.
	noRoute bool
	// staleOnce makes the next PUT :id/applied find the claim's revision moved
	// on (another apply relabelled it): the revision is bumped, applied kept.
	staleOnce bool
	// raceOnce makes the next PUT :id/applied find another apply's anchor
	// recorded first.
	raceOnce map[string]any

	// race, when set, runs once at the top of the next create's claim — a
	// concurrent apply that committed the same address first.
	race func(m *fakeManaged)
	// afterList, when set, runs once after the next GET /managed has answered —
	// a concurrent apply committing between this one's read and its create.
	afterList func(m *fakeManaged)
	// dropOnApplied makes the next PUT :id/applied find its claim unmanaged by
	// somebody else meanwhile.
	dropOnApplied bool
	// failUnmanageOnce makes the next DELETE :id answer this status and release
	// nothing — a release that dies part way, as a crash would leave it.
	failUnmanageOnce int
	// beforeApplied, when set, runs once (m.mu held) at the top of the next PUT
	// :id/applied — a move of the row committing while this request ran.
	beforeApplied func(m *fakeManaged, c *fakeClaim)

	// requests records every managed request as "METHOD /path", for
	// assertions that a read-only path sent no write.
	requests []string
	// appliedWrites counts the PUT :id/applied that landed.
	appliedWrites int

	// folders is `managed_folders` (rmanaged/folder.go): one FOLDER ANCHOR per
	// (feature, folder id), keyed by fakeFolderKey. unreadableFeatures are
	// features the caller may not read at all — FeatureRead and FeatureWrite
	// answer them 404 (access.go).
	folders            map[string]*fakeFolderAnchor
	unreadableFeatures map[string]bool
	// folderCap is rmanaged.MaxFolderAppliedBytes (folder.go); a test may
	// lower it to reach the 413 without writing thousands of sidecars.
	folderCap int
	// folderStaleOnce makes the next PUT folder/applied find the anchor's
	// revision moved on with its state unchanged; folderRaceOnce makes it find
	// another apply's different state recorded first.
	folderStaleOnce bool
	folderRaceOnce  map[string]any
	// folderWrites counts the PUT folder/applied that landed.
	folderWrites int
	// noManagedBy makes every single-row GET answer without `managedBy`, the
	// way an instance older than the `managedBy` field does.
	noManagedBy bool
}

// fakeFolderAnchor is one managed_folders row (rdb.ManagedFolder).
type fakeFolderAnchor struct {
	FeatureID, FolderID string
	Applied             map[string]any
	Revision            int64
	UpdatedBy           string
	UpdatedAt           time.Time
}

func fakeFolderKey(feature, folder string) string { return feature + "\x00" + folder }

// fakeClaim is one managed_resources row (rdb.ManagedResource).
type fakeClaim struct {
	ID, FeatureID, FolderID, Address, Kind, ResourceID string
	StackLabel, SourceLabel, CreatedBy, UpdatedBy      string
	Revision                                           int64
	Applied                                            map[string]any
	CreatedAt, UpdatedAt                               time.Time
	// DepartedAt / DepartedTo make the claim a TOMBSTONE (migration 000669's
	// departed_at, departed_to_feature_id): its row moved to another feature.
	DepartedAt *time.Time
	DepartedTo string
}

// departed is rdb.ManagedResource.Departed.
func (c *fakeClaim) departed() bool { return c.DepartedAt != nil }

// fakeRowFacts is one row as probe.go's loadFacts reads it.
type fakeRowFacts struct {
	Exists    bool
	FeatureID string
	Root      bool
	Name      string
	Lifecycle string
	// Drafter is a draft's or a proposal's drafter (probe.go).
	Drafter string
	// ModelKind is the models_v2 kind of a table or metric row.
	ModelKind string
}

// fakePrincipal is the attribution id every claim the fake inserts carries —
// the signed-in user the kind fakes answer /me with.
const fakePrincipal = "usr-1"

func newFakeManaged(t *testing.T, probe func(kind, id string) fakeRowFacts) *fakeManaged {
	return &fakeManaged{
		t: t, claims: map[string]*fakeClaim{}, probe: probe,
		unreadable: map[string]bool{}, trashed: map[string]bool{}, gone: map[string]bool{},
		principal: fakePrincipal, admin: true, denied: map[string]bool{}, readOnlyFeatures: map[string]bool{},
		folders: map[string]*fakeFolderAnchor{}, unreadableFeatures: map[string]bool{}, folderCap: 256 << 10,
	}
}

// folderIDPattern is rmanaged.folderIDRE.
var folderIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// normalizeClaim is rmanaged.ClaimInput.Normalize: the folder id lowercased
// and validated, the address checked against the kind's shape and never
// rewritten, the labels trimmed.
func normalizeClaim(kind string, in api.ManagedClaimInput) (api.ManagedClaimInput, string) {
	folder := strings.ToLower(strings.TrimSpace(in.FolderID))
	if !folderIDPattern.MatchString(folder) {
		return in, fmt.Sprintf("folderID %q is not a UUID (8-4-4-4-12 hex digits)", in.FolderID)
	}
	if msg := validateFakeAddress(kind, in.Address); msg != "" {
		return in, msg
	}
	return api.ManagedClaimInput{FolderID: folder, Address: in.Address,
		StackLabel: strings.TrimSpace(in.StackLabel), SourceLabel: strings.TrimSpace(in.SourceLabel)}, ""
}

// validateFakeAddress is rmanaged.ValidateAddress.
func validateFakeAddress(kind, addr string) string {
	switch kind {
	case "workflow", "dataapp", "module":
		if addr != "" {
			return fmt.Sprintf("address must be empty for a %s", kind)
		}
		return ""
	case "table", "metric", "automation":
	default:
		return fmt.Sprintf("unknown resourceKind %q", kind)
	}
	if addr == "" {
		return fmt.Sprintf("address is required for a %s", kind)
	}
	if strings.HasPrefix(addr, "/") || strings.Contains(addr, `\`) {
		return fmt.Sprintf("address %q must be a folder-relative, slash-separated path", addr)
	}
	for _, seg := range strings.Split(addr, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.TrimSpace(seg) == "" {
			return fmt.Sprintf("address %q has an empty, '.' or '..' segment", addr)
		}
	}
	switch kind {
	case "table":
		if !strings.HasSuffix(addr, ".sql") {
			return fmt.Sprintf("a table's address is its .sql file's path; %q does not end in .sql", addr)
		}
	case "metric":
		if !strings.HasPrefix(addr, "metrics/") || !strings.HasSuffix(addr, ".json") {
			return fmt.Sprintf("a metric's address is metrics/<name>.json; got %q", addr)
		}
	case "automation":
		if !strings.HasSuffix(addr, ".json") {
			return fmt.Sprintf("an automation's address is its .json file's path; %q does not end in .json", addr)
		}
	}
	return ""
}

// fakeKindScope is probe.go kindScope (probe.go).
func fakeKindScope(kind string) string {
	switch kind {
	case "dataapp":
		return "analytics"
	case "table", "metric":
		return "data"
	}
	return "automation"
}

// rowState is probe.go's probeRows for one claim's row, overrides applied:
// reach needs the kind's scope at read (probe.go), and on top of reach a
// DRAFT is readable by its drafter only — an admin included — and a PROPOSAL by
// its drafter or an admin (probe.go).
func (m *fakeManaged) rowState(kind, id string) (facts fakeRowFacts, readable, trashed bool) {
	if m.gone[id] {
		return fakeRowFacts{}, false, false
	}
	facts = m.probe(kind, id)
	if !facts.Exists {
		return facts, false, false
	}
	readable = !m.unreadable[id] && !m.denied[fakeKindScope(kind)+":read"]
	switch facts.Lifecycle {
	case "draft":
		readable = readable && facts.Drafter != "" && facts.Drafter == m.principal
	case "proposed":
		readable = readable && (m.admin || (facts.Drafter != "" && facts.Drafter == m.principal))
	}
	return facts, readable, m.trashed[id]
}

// writeInsufficientScope is gt's scoped-token 403.
func writeInsufficientScope(w http.ResponseWriter, scope, action string) {
	writeCoded(w, http.StatusForbidden, api.InsufficientScopeCode,
		fmt.Sprintf("token scope %q does not permit %s", scope, action), nil)
}

// view is views.go toView (views.go): liveView, then a TOMBSTONE always
// reads moved_out with departedAt, and currentFeatureID — where the row lives
// NOW, from the row — only when readable.
func (m *fakeManaged) view(c *fakeClaim) api.ManagedClaim {
	v := m.liveView(c)
	if !c.departed() {
		return v
	}
	at := *c.DepartedAt
	v.DepartedAt = &at
	v.State = api.ManagedStateMovedOut
	v.CurrentFeatureID = ""
	if v.Readable {
		facts, _, _ := m.rowState(c.Kind, c.ResourceID)
		v.CurrentFeatureID = facts.FeatureID
	}
	return v
}

// liveView is views.go liveView (views.go): the ONE redaction rule —
// nothing about the row leaves unless it is readable, except a lost claim's
// resourceID.
func (m *fakeManaged) liveView(c *fakeClaim) api.ManagedClaim {
	v := api.ManagedClaim{
		ID: c.ID, FeatureID: c.FeatureID, FolderID: c.FolderID, Address: c.Address,
		ResourceKind: c.Kind, StackLabel: c.StackLabel, SourceLabel: c.SourceLabel,
		Claimant: c.CreatedBy, UpdatedBy: c.UpdatedBy, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		Revision: c.Revision,
	}
	facts, readable, trashed := m.rowState(c.Kind, c.ResourceID)
	switch {
	case !facts.Exists:
		v.State = api.ManagedStateLost
		v.ResourceID = c.ResourceID
		return v
	case !readable:
		v.State = api.ManagedStateNotReadable
		return v
	}
	v.Readable = true
	v.ResourceID = c.ResourceID
	v.ResourceName = facts.Name
	v.Lifecycle = facts.Lifecycle
	v.Applied = copyAnchor(c.Applied)
	switch {
	case facts.FeatureID != c.FeatureID:
		v.State = api.ManagedStateMovedOut
		v.CurrentFeatureID = facts.FeatureID
	case trashed:
		v.State = api.ManagedStateTrashed
	default:
		v.State = api.ManagedStateOK
	}
	return v
}

func copyAnchor(in map[string]any) map[string]any {
	raw, _ := json.Marshal(in)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// writeCoded is gt.ErrorHandler's body for a coded refusal: `error`, `code`,
// and every details key at the TOP LEVEL beside them.
func writeCoded(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	body := map[string]any{"error": msg}
	if code != "" {
		body["code"] = code
	}
	for k, v := range details {
		if k == "error" || k == "code" {
			continue
		}
		body[k] = v
	}
	writeJSONStatus(w, status, body)
}

// byAddress / byResource are the two unique indexes (migration 000687):
// the address index is FULL — a tombstone keeps its address — and the resource
// index is partial on live claims, so a row's tombstones never block its claim
// where it lives now (store.go GetByResource reads the live one only).
func (m *fakeManaged) byAddress(feature, folder, address string) *fakeClaim {
	for _, c := range m.claims {
		if c.FeatureID == feature && c.FolderID == folder && c.Address == address {
			return c
		}
	}
	return nil
}

func (m *fakeManaged) byResource(kind, id string) *fakeClaim {
	for _, c := range m.claims {
		if c.Kind == kind && c.ResourceID == id && !c.departed() {
			return c
		}
	}
	return nil
}

func (m *fakeManaged) insert(feature, kind, resourceID string, in api.ManagedClaimInput) *fakeClaim {
	m.nextID++
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(m.nextID) * time.Minute)
	c := &fakeClaim{
		ID: fmt.Sprintf("managed-%d", m.nextID), FeatureID: feature, FolderID: in.FolderID, Address: in.Address,
		Kind: kind, ResourceID: resourceID, StackLabel: in.StackLabel, SourceLabel: in.SourceLabel,
		CreatedBy: fakePrincipal, UpdatedBy: fakePrincipal, Applied: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	m.claims[c.ID] = c
	return c
}

// addressClaimedDetails is claims.go mapAddressClaimed's details: the address,
// the holder redacted as GET redacts it, and a resourceID that mirrors the
// holder's.
func (m *fakeManaged) addressClaimedDetails(holder *fakeClaim) map[string]any {
	details := map[string]any{"featureID": holder.FeatureID, "folderID": holder.FolderID, "address": holder.Address}
	view := m.view(holder)
	details["claim"] = view
	if view.ResourceID != "" {
		details["resourceID"] = view.ResourceID
	}
	return details
}

// createClaim is a create route's half of create-with-claim (claims.go Prepare
// + rmanaged.ClaimCreatedTx): validate the `managed` object, then insert the
// claim in the create's transaction. A non-zero status means the create must
// be ROLLED BACK by the caller — the host fake deletes the row it inserted —
// and the body is the answer to send.
//
// The feature is the created row's own (featureID), never a client value.
func (m *fakeManaged) createClaim(kind, featureID, resourceID string, in *api.ManagedClaimInput) (int, map[string]any) {
	if in == nil {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if race := m.race; race != nil {
		m.race = nil
		race(m)
	}
	// claims.go Prepare: a managed create needs structure at write.
	if m.denied["structure:write"] {
		return http.StatusForbidden, map[string]any{"error": `a managed create records repository state, which needs the token scope "structure" at write`,
			"code": api.InsufficientScopeCode}
	}
	norm, msg := normalizeClaim(kind, *in)
	if msg != "" {
		return http.StatusBadRequest, map[string]any{"error": msg}
	}
	// The create routes of a table or a metric are admin-only (probe.go
	// names the same floor), and ClaimCreatedTx runs FeatureWrite on the
	// created row's feature (claim_tx.go) — which is what refuses a mere
	// reader whose module create would otherwise file a proposal.
	if (kind == "table" || kind == "metric") && !m.admin {
		return http.StatusForbidden, map[string]any{"error": "forbidden"}
	}
	if m.readOnlyFeatures[featureID] {
		return http.StatusForbidden, map[string]any{"error": "forbidden"}
	}
	if kind == "table" || kind == "metric" {
		facts := m.probe(kind, resourceID)
		want := "derived"
		if kind == "metric" {
			want = "metric"
		}
		if facts.ModelKind != want {
			return http.StatusBadRequest, map[string]any{"error": "only a derived table can be managed from a repository; nothing was created", "code": api.ManagedCodeNotClaimable}
		}
	}
	if holder := m.byAddress(featureID, norm.FolderID, norm.Address); holder != nil {
		body := map[string]any{"error": "this folder address already holds a row in this feature; nothing was created. Re-read GET /api/v2/managed and continue with that row",
			"code": api.ManagedCodeAddressClaimed}
		for k, v := range m.addressClaimedDetails(holder) {
			body[k] = v
		}
		return http.StatusConflict, body
	}
	m.insert(featureID, kind, resourceID, norm)
	return 0, nil
}

// moveRow is a cross-feature move of one row: the row's feature_id update
// (the host's setRowFeature) and, in the same transaction,
// rmanaged.DepartOnMoveTx (claim_tx.go):
//
//  1. DEPART — every live claim on the row keyed to another feature becomes a
//     tombstone at its address, revision bumped; a live claim already keyed to
//     the destination is left alone, and then nothing is revived.
//  2. REVIVE — with no live claim left, the destination's newest tombstone of
//     the row becomes live again, address and anchor kept, revision bumped.
func (m *fakeManaged) moveRow(kind, id, toFeature string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moveRowLocked(kind, id, toFeature)
}

// moveRowLocked is moveRow for a hook that already holds m.mu (afterList,
// beforeApplied).
func (m *fakeManaged) moveRowLocked(kind, id, toFeature string) {
	m.setRowFeature(kind, id, toFeature)
	m.nextID++
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC).Add(time.Duration(m.nextID) * time.Minute)
	liveInDestination := false
	for _, c := range m.claims {
		if c.Kind != kind || c.ResourceID != id || c.departed() {
			continue
		}
		if c.FeatureID == toFeature {
			liveInDestination = true
			continue
		}
		at := now
		c.DepartedAt, c.DepartedTo = &at, toFeature
		c.Revision++
		c.UpdatedBy, c.UpdatedAt = fakePrincipal, now
	}
	if liveInDestination {
		return
	}
	var tomb *fakeClaim
	for _, c := range m.claims {
		if c.Kind == kind && c.ResourceID == id && c.FeatureID == toFeature && c.departed() {
			if tomb == nil || c.DepartedAt.After(*tomb.DepartedAt) || (c.DepartedAt.Equal(*tomb.DepartedAt) && c.ID > tomb.ID) {
				tomb = c
			}
		}
	}
	if tomb != nil {
		tomb.DepartedAt, tomb.DepartedTo = nil, ""
		tomb.Revision++
		tomb.UpdatedBy, tomb.UpdatedAt = fakePrincipal, now
	}
}

// Claims answers every claim of a feature in the server's order (folder, then
// address), for assertions.
func (m *fakeManaged) Claims(feature string) []*fakeClaim {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*fakeClaim
	for _, c := range m.claims {
		if feature == "" || c.FeatureID == feature {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FolderID != out[j].FolderID {
			return out[i].FolderID < out[j].FolderID
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// ClaimAt answers the claim at one address of one folder, nil when none.
func (m *fakeManaged) ClaimAt(feature, folder, address string) *fakeClaim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byAddress(feature, folder, address)
}

// writesSeen answers the managed requests that were not reads.
func (m *fakeManaged) writesSeen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

// serve answers the seven routes (handler.go Register, handler.go). It reports whether the
// path was a managed one.
func (m *fakeManaged) serve(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/managed")
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return false
	}
	if m.noRoute {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r.Method+" "+r.URL.Path)
	rest = strings.TrimPrefix(rest, "/")
	// The group is structure-scoped (handler.go): read for GET, write for
	// the rest.
	if action := map[bool]string{true: "read", false: "write"}[r.Method == http.MethodGet]; m.denied["structure:"+action] {
		writeInsufficientScope(w, "structure", action)
		return true
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		m.serveList(w, r)
	// The two folder routes are static segments, which gin matches before the
	// :id ones (handler.go).
	case rest == "folder/applied" && r.Method == http.MethodPut:
		m.serveFolderApplied(w, r)
	case rest == "folder" && r.Method == http.MethodDelete:
		m.serveFolderRelease(w, r)
	case rest == "claim" && r.Method == http.MethodPost:
		m.serveClaim(w, r)
	case rest == "move" && r.Method == http.MethodPost:
		m.serveMove(w, r)
	case strings.HasSuffix(rest, "/applied") && r.Method == http.MethodPut:
		m.serveApplied(w, r, strings.TrimSuffix(rest, "/applied"))
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		m.serveUnmanage(w, rest)
	default:
		http.Error(w, "404 page not found", http.StatusNotFound)
	}
	return true
}

// serveList is handler.go list.
func (m *fakeManaged) serveList(w http.ResponseWriter, r *http.Request) {
	feature := r.URL.Query().Get("feature")
	folder := r.URL.Query().Get("folder")
	if folder != "" {
		norm := strings.ToLower(strings.TrimSpace(folder))
		if !folderIDPattern.MatchString(norm) {
			writeCoded(w, http.StatusBadRequest, "", fmt.Sprintf("folderID %q is not a UUID (8-4-4-4-12 hex digits)", folder), nil)
			return
		}
		folder = norm
	}
	if feature == "" {
		writeCoded(w, http.StatusBadRequest, "", "feature is required", nil)
		return
	}
	// FeatureRead (handler.go): a feature the caller cannot read is a 404.
	if m.unreadableFeatures[feature] {
		writeCoded(w, http.StatusNotFound, "", "feature not found", nil)
		return
	}
	var rows []*fakeClaim
	for _, c := range m.claims {
		if c.FeatureID == feature && (folder == "" || c.FolderID == folder) {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].FolderID != rows[j].FolderID {
			return rows[i].FolderID < rows[j].FolderID
		}
		return rows[i].Address < rows[j].Address
	})
	views := make([]api.ManagedClaim, 0, len(rows))
	for _, c := range rows {
		views = append(views, m.view(c))
	}
	body := map[string]any{"claims": views}
	// With a folder, its anchor rides on the same answer (handler.go).
	if folder != "" {
		body["folder"] = m.folderView(feature, folder, m.folders[fakeFolderKey(feature, folder)])
	}
	writeJSON(w, body)
	if after := m.afterList; after != nil {
		m.afterList = nil
		after(m)
	}
}

// stage inserts a claim directly — one another apply, or another folder, made.
func (m *fakeManaged) stage(feature, folder, address, kind, id, source string) *fakeClaim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.insert(feature, kind, id, api.ManagedClaimInput{FolderID: folder, Address: address, StackLabel: "prod", SourceLabel: source})
}

// stageTombstone inserts a claim another folder's apply made and then departs
// it, as a cross-feature move of its row does — the row itself is not moved.
// m.mu must be held (an afterList hook).
func (m *fakeManaged) stageTombstoneLocked(feature, folder, address, kind, id, to string) *fakeClaim {
	c := m.insert(feature, kind, id, api.ManagedClaimInput{FolderID: folder, Address: address, StackLabel: "prod"})
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	c.DepartedAt, c.DepartedTo = &at, to
	c.Revision++
	return c
}

// requireRowWrite is probe.go requireRowWrite, as an item's refusal (code,
// message). Empty code is "go ahead".
func (m *fakeManaged) requireRowWrite(kind, id, feature string) (code, msg string, facts fakeRowFacts) {
	if m.denied[fakeKindScope(kind)+":write"] {
		return api.InsufficientScopeCode, fmt.Sprintf("token scope %q does not permit write", fakeKindScope(kind)), facts
	}
	if (kind == "table" || kind == "metric") && !m.admin {
		// gt.NewForbiddenError: no code; a claim item reports "forbidden".
		return "forbidden", "forbidden", facts
	}
	facts, readable, trashed := m.rowState(kind, id)
	switch {
	case !facts.Exists || !readable:
		return "not_found", "not found", facts
	case !facts.Root:
		return api.ManagedCodeNotRoot, "this id is a draft or version of another row; claim the row the folder's lock records, not a child of it", facts
	case facts.FeatureID != feature:
		return api.ManagedCodeMovedOut, "the row does not live in this feature: an address is keyed by the row's own feature", facts
	case trashed:
		return api.ManagedCodeTrashed, "the row is in the trash; restore it first", facts
	}
	return "", "", facts
}

// serveClaim is handler.go claim + claimOne + rmanaged ClaimExisting.
func (m *fakeManaged) serveClaim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FeatureID string                 `json:"featureID"`
		Claims    []api.ManagedClaimItem `json:"claims"`
	}
	decodeBody(m.t, r, &in)
	if len(in.Claims) == 0 {
		writeCoded(w, http.StatusBadRequest, "", "claims is required", nil)
		return
	}
	if len(in.Claims) > api.MaxManagedClaimItems {
		writeCoded(w, http.StatusBadRequest, "", "at most 200 claims per request", nil)
		return
	}
	// One refusal for a feature the caller may not write (handler.go).
	if m.readOnlyFeatures[in.FeatureID] {
		writeCoded(w, http.StatusForbidden, "", "forbidden", nil)
		return
	}
	results := make([]api.ManagedClaimResult, 0, len(in.Claims))
	for i, item := range in.Claims {
		res := api.ManagedClaimResult{Index: i}
		refuse := func(code, msg string) {
			res.Outcome, res.Code, res.Error = api.ManagedOutcomeRefused, code, msg
		}
		norm, msg := normalizeClaim(item.ResourceKind, item.ManagedClaimInput)
		switch {
		case msg != "":
			refuse("invalid", msg)
		case item.ResourceID == "":
			refuse("invalid", "resourceID is required")
		case m.itemCode[item.ResourceID] != "":
			refuse(m.itemCode[item.ResourceID], "the server could not claim this row")
			if m.itemCodeOnce {
				delete(m.itemCode, item.ResourceID)
			}
		default:
			code, why, facts := m.requireRowWrite(item.ResourceKind, item.ResourceID, in.FeatureID)
			if code == "" && (item.ResourceKind == "table" || item.ResourceKind == "metric") {
				want := map[string]string{"table": "derived", "metric": "metric"}[item.ResourceKind]
				if facts.ModelKind != want {
					code, why = api.ManagedCodeNotClaimable, "only a derived table can be managed from a repository"
				}
			}
			if code != "" {
				refuse(code, why)
				break
			}
			holder := m.byAddress(in.FeatureID, norm.FolderID, norm.Address)
			owner := m.byResource(item.ResourceKind, item.ResourceID)
			// The insert's unique indexes, address first: Postgres checks a
			// table's unique indexes in creation (OID) order, and 000669
			// creates the address index first (up.sql), so a row that
			// would trip both is refused address_claimed (claim_tx.go
			// mapUniqueViolation names whichever constraint fired).
			switch {
			case holder != nil && !holder.departed() && holder.Kind == item.ResourceKind && holder.ResourceID == item.ResourceID:
				// The retry of a claim that landed: the exact pairing exists. A
				// TOMBSTONE of the same pairing is not one (store.go).
				res.Outcome = api.ManagedOutcomeAlreadyClaimed
				v := m.view(holder)
				res.Claim = &v
			case holder != nil:
				refuse(api.ManagedCodeAddressClaimed, fmt.Sprintf("address %q of folder %s is already claimed in this feature", norm.Address, norm.FolderID))
				v := m.view(holder)
				res.Claim = &v
			case owner != nil:
				refuse(api.ManagedCodeResourceClaimed, fmt.Sprintf("this %s is already managed at another address", item.ResourceKind))
				if owner.FeatureID == in.FeatureID {
					v := m.view(owner)
					res.Claim = &v
				}
			default:
				c := m.insert(in.FeatureID, item.ResourceKind, item.ResourceID, norm)
				res.Outcome = api.ManagedOutcomeClaimed
				v := m.view(c)
				res.Claim = &v
			}
		}
		results = append(results, res)
	}
	writeJSON(w, map[string]any{"results": results})
}

// serveMove is handler.go move + rmanaged Move.
func (m *fakeManaged) serveMove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FeatureID, FolderID, From, To string
	}
	decodeBody(m.t, r, &in)
	folder := strings.ToLower(strings.TrimSpace(in.FolderID))
	if !folderIDPattern.MatchString(folder) {
		writeCoded(w, http.StatusBadRequest, "", "folderID is not a UUID", nil)
		return
	}
	if in.From == in.To {
		writeCoded(w, http.StatusBadRequest, "", "from and to are the same address", nil)
		return
	}
	if m.readOnlyFeatures[in.FeatureID] {
		writeCoded(w, http.StatusForbidden, "", "forbidden", nil)
		return
	}
	c := m.byAddress(in.FeatureID, folder, in.From)
	if c == nil {
		writeCoded(w, http.StatusNotFound, api.ManagedCodeNotClaimed, "no claim at that address in this folder", nil)
		return
	}
	// A tombstone cannot be moved (store.go, handler.go).
	if c.departed() {
		writeCoded(w, http.StatusConflict, api.ManagedCodeMovedOut, departedMessage("the claim was not moved"), nil)
		return
	}
	// requireKindWriteScope (handler.go).
	if m.denied[fakeKindScope(c.Kind)+":write"] {
		writeInsufficientScope(w, fakeKindScope(c.Kind), "write")
		return
	}
	if msg := validateFakeAddress(c.Kind, in.To); msg != "" {
		writeCoded(w, http.StatusBadRequest, "", msg, nil)
		return
	}
	if m.moveConflictOnce {
		m.moveConflictOnce = false
		writeCoded(w, http.StatusConflict, "", fmt.Sprintf("the claim at %q moved while this request ran; re-read the folder's claims", in.From), nil)
		return
	}
	if holder := m.byAddress(in.FeatureID, folder, in.To); holder != nil {
		writeCoded(w, http.StatusConflict, api.ManagedCodeAddressClaimed,
			"the destination address already holds a row in this folder; the claim was not moved. Re-read GET /api/v2/managed, and unmanage or move the holder first",
			m.addressClaimedDetails(holder))
		return
	}
	c.Address, c.UpdatedBy, c.UpdatedAt = in.To, fakePrincipal, c.UpdatedAt.Add(time.Second)
	writeJSON(w, m.view(c))
}

// serveUnmanage is handler.go unmanage: the claim as it stood, the row
// untouched.
func (m *fakeManaged) serveUnmanage(w http.ResponseWriter, id string) {
	if status := m.failUnmanageOnce; status != 0 {
		m.failUnmanageOnce = 0
		writeCoded(w, status, "", "the instance is restarting", nil)
		return
	}
	c := m.claims[id]
	if c == nil {
		writeCoded(w, http.StatusNotFound, "", "claim not found", nil)
		return
	}
	// The CLAIM's feature, never the row's (store.go), then the
	// kind's write scope.
	if m.readOnlyFeatures[c.FeatureID] {
		writeCoded(w, http.StatusForbidden, "", "forbidden", nil)
		return
	}
	if m.denied[fakeKindScope(c.Kind)+":write"] {
		writeInsufficientScope(w, fakeKindScope(c.Kind), "write")
		return
	}
	delete(m.claims, id)
	writeJSON(w, m.view(c))
}

// serveApplied is handler.go recordApplied + rmanaged RecordApplied: a CAS on
// revision, the row checks in the transaction, a 404 for a claim gone.
func (m *fakeManaged) serveApplied(w http.ResponseWriter, r *http.Request, id string) {
	var in api.ManagedAppliedInput
	decodeBody(m.t, r, &in)
	raw, _ := json.Marshal(in.Applied)
	if len(raw) > 16<<10 {
		writeCoded(w, http.StatusBadRequest, "", "applied is too large", nil)
		return
	}
	if in.ExpectedRevision < 0 {
		writeCoded(w, http.StatusBadRequest, "", "expectedRevision must be zero or more", nil)
		return
	}
	if m.dropOnApplied {
		m.dropOnApplied = false
		delete(m.claims, id)
	}
	if hook := m.beforeApplied; hook != nil && m.claims[id] != nil {
		m.beforeApplied = nil
		hook(m, m.claims[id])
	}
	c := m.claims[id]
	if c == nil {
		writeCoded(w, http.StatusNotFound, "", "claim not found", nil)
		return
	}
	if m.readOnlyFeatures[c.FeatureID] {
		writeCoded(w, http.StatusForbidden, "", "forbidden", nil)
		return
	}
	// A tombstone takes no anchor — checked after the feature, before the row
	// (store.go, handler.go).
	if c.departed() {
		writeCoded(w, http.StatusConflict, api.ManagedCodeMovedOut, departedMessage("no anchor was recorded"), nil)
		return
	}
	if code, why, _ := m.requireRowWrite(c.Kind, c.ResourceID, c.FeatureID); code != "" {
		switch code {
		case "not_found":
			// gt.NewNotfoundError: no code, so a refusal is no oracle.
			writeCoded(w, http.StatusNotFound, "", why, nil)
		case "forbidden":
			writeCoded(w, http.StatusForbidden, "", why, nil)
		case api.InsufficientScopeCode:
			writeCoded(w, http.StatusForbidden, code, why, nil)
		case api.ManagedCodeNotRoot:
			writeCoded(w, http.StatusBadRequest, code, why, nil)
		default:
			writeCoded(w, http.StatusConflict, code, why, nil)
		}
		return
	}
	if m.staleOnce {
		m.staleOnce = false
		c.Revision++
	}
	if m.raceOnce != nil {
		c.Applied, m.raceOnce = m.raceOnce, nil
		c.Revision++
	}
	if c.Revision != in.ExpectedRevision {
		writeCoded(w, http.StatusConflict, api.ManagedCodeStaleRevision,
			"the claim's applied anchor was recorded by someone else since you read it; re-read the claim", nil)
		return
	}
	c.Applied = in.Applied
	if c.Applied == nil {
		c.Applied = map[string]any{}
	}
	c.Revision = in.ExpectedRevision + 1
	if in.StackLabel != nil {
		c.StackLabel = strings.TrimSpace(*in.StackLabel)
	}
	if in.SourceLabel != nil {
		c.SourceLabel = strings.TrimSpace(*in.SourceLabel)
	}
	c.UpdatedBy = fakePrincipal
	m.appliedWrites++
	writeJSON(w, m.view(c))
}

// folderView is views.go folderView (views.go): a folder with no anchor
// reads revision 0, an empty applied, and no updatedBy/updatedAt.
func (m *fakeManaged) folderView(feature, folder string, a *fakeFolderAnchor) map[string]any {
	if a == nil {
		return map[string]any{"featureID": feature, "folderID": folder, "applied": map[string]any{}, "revision": 0}
	}
	return map[string]any{"featureID": a.FeatureID, "folderID": a.FolderID, "applied": copyAnchor(a.Applied),
		"revision": a.Revision, "updatedBy": a.UpdatedBy, "updatedAt": a.UpdatedAt}
}

// requireFolderFeatureWrite is rmanaged.FeatureWrite as the folder routes meet
// it (access.go): unreadable is a 404, readable but not writable a 403.
// It reports whether the caller may go ahead.
func (m *fakeManaged) requireFolderFeatureWrite(w http.ResponseWriter, feature string) bool {
	switch {
	case m.unreadableFeatures[feature]:
		writeCoded(w, http.StatusNotFound, "", "feature not found", nil)
		return false
	case m.readOnlyFeatures[feature]:
		writeCoded(w, http.StatusForbidden, "", "forbidden", nil)
		return false
	}
	return true
}

// FolderAnchor answers one folder's anchor, nil when none, for assertions.
func (m *fakeManaged) FolderAnchor(feature, folder string) *fakeFolderAnchor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.folders[fakeFolderKey(feature, folder)]
}

// serveFolderApplied is handler.go recordFolderApplied (handler.go) +
// rmanaged RecordFolderApplied (folder.go). In the server's order:
// the input checks before any database work (validateFolderApplied,
// folder.go — a 400 each, and the 413 applied_too_large over the cap,
// nothing written), then FeatureWrite in the transaction (folder.go), then
// revision 0 INSERTs, refused 409 stale_revision when an anchor exists (the
// unique index, folder.go), and any other revision is the CAS UPDATE,
// refused 409 stale_revision when it no longer matches — moved on, or the
// anchor released meanwhile (folder.go).
func (m *fakeManaged) serveFolderApplied(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FeatureID string         `json:"featureID"`
		FolderID  string         `json:"folderID"`
		Applied   map[string]any `json:"applied"`
		Revision  int64          `json:"revision"`
	}
	decodeBody(m.t, r, &in)
	folder := strings.ToLower(strings.TrimSpace(in.FolderID))
	switch {
	case in.FeatureID == "":
		writeCoded(w, http.StatusBadRequest, "", "featureID is required", nil)
		return
	case !folderIDPattern.MatchString(folder):
		writeCoded(w, http.StatusBadRequest, "", fmt.Sprintf("folderID %q is not a UUID (8-4-4-4-12 hex digits)", in.FolderID), nil)
		return
	case in.Revision < 0:
		writeCoded(w, http.StatusBadRequest, "", "revision must be zero or more", nil)
		return
	case in.Applied == nil:
		// A missing or null applied is refused, never read as "clear".
		writeCoded(w, http.StatusBadRequest, "", "applied is required: send the folder's whole applied state, or {} to clear it", nil)
		return
	}
	if raw, _ := json.Marshal(in.Applied); len(raw) > m.folderCap {
		writeCoded(w, http.StatusRequestEntityTooLarge, api.ManagedCodeAppliedTooLarge,
			fmt.Sprintf("applied is %d bytes; the maximum is %d — nothing was written", len(raw), m.folderCap), nil)
		return
	}
	if !m.requireFolderFeatureWrite(w, in.FeatureID) {
		return
	}
	key := fakeFolderKey(in.FeatureID, folder)
	a := m.folders[key]
	if a != nil && m.folderStaleOnce {
		m.folderStaleOnce = false
		a.Revision++
	}
	if a != nil && m.folderRaceOnce != nil {
		a.Applied, m.folderRaceOnce = m.folderRaceOnce, nil
		a.Revision++
	}
	stale := func() {
		writeCoded(w, http.StatusConflict, api.ManagedCodeStaleRevision,
			"the folder's applied anchor was written by someone else since you read it; re-read the folder (GET /api/v2/managed?feature=&folder=) and decide again", nil)
	}
	m.nextID++
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC).Add(time.Duration(m.nextID) * time.Minute)
	if in.Revision == 0 {
		if a != nil {
			stale()
			return
		}
		a = &fakeFolderAnchor{FeatureID: in.FeatureID, FolderID: folder}
		m.folders[key] = a
	} else if a == nil || a.Revision != in.Revision {
		stale()
		return
	}
	a.Applied = copyAnchor(in.Applied)
	a.Revision = in.Revision + 1
	a.UpdatedBy, a.UpdatedAt = fakePrincipal, now
	m.folderWrites++
	writeJSON(w, m.folderView(a.FeatureID, a.FolderID, a))
}

// serveFolderRelease is handler.go releaseFolder (handler.go) +
// rmanaged ReleaseFolder (folder.go): FeatureWrite, then the anchor as
// it was — or a 404 no_folder_anchor for a folder with none.
func (m *fakeManaged) serveFolderRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FeatureID string `json:"featureID"`
		FolderID  string `json:"folderID"`
	}
	decodeBody(m.t, r, &in)
	folder := strings.ToLower(strings.TrimSpace(in.FolderID))
	switch {
	case in.FeatureID == "":
		writeCoded(w, http.StatusBadRequest, "", "featureID is required", nil)
		return
	case !folderIDPattern.MatchString(folder):
		writeCoded(w, http.StatusBadRequest, "", fmt.Sprintf("folderID %q is not a UUID (8-4-4-4-12 hex digits)", in.FolderID), nil)
		return
	}
	if !m.requireFolderFeatureWrite(w, in.FeatureID) {
		return
	}
	key := fakeFolderKey(in.FeatureID, folder)
	a := m.folders[key]
	if a == nil {
		writeCoded(w, http.StatusNotFound, api.ManagedCodeNoFolderAnchor, "this folder has no applied anchor in this feature", nil)
		return
	}
	delete(m.folders, key)
	writeJSON(w, m.folderView(a.FeatureID, a.FolderID, a))
}

// departedMessage is handler.go departedError's sentence (handler.go).
func departedMessage(outcome string) string {
	return "this claim's row was moved to another feature, so the claim only holds its address; " + outcome +
		". Unmanage it (DELETE /api/v2/managed/:id) to free the address"
}

// writeCreateRefusal sends createClaim's refusal body.
func writeCreateRefusal(w http.ResponseWriter, status int, body map[string]any) {
	writeJSONStatus(w, status, body)
}

// managedBy is the `managedBy` object a single-row GET carries — what each
// detail handler sets through Claims.ManagedBy
// (backend/api/v2/managed/claims.go): the LIVE claim on the row, or on
// the root a draft or version child names (`parentID`), never a tombstone
// (rmanaged/managed_by.go, `departed_at IS NULL`), a table and a
// metric matching each other's kind (lookupKinds, :90-95), projected to its
// address half (ManagedByOf, :55-66). Nil when no claim names the row.
//
// noManagedBy models an instance older than the field: no GET carries it.
func (m *fakeManaged) managedBy(kind, id, parentID string) *api.ManagedBy {
	if m == nil || id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.noManagedBy {
		return nil
	}
	kinds := map[string]bool{kind: true}
	if kind == "table" || kind == "metric" {
		kinds = map[string]bool{"table": true, "metric": true}
	}
	for _, c := range m.claims {
		if !kinds[c.Kind] || c.departed() {
			continue
		}
		if c.ResourceID == id || (parentID != "" && c.ResourceID == parentID) {
			return &api.ManagedBy{
				SourceLabel: c.SourceLabel, StackLabel: c.StackLabel, FolderID: c.FolderID,
				Address: c.Address, FeatureID: c.FeatureID, UpdatedAt: c.UpdatedAt,
			}
		}
	}
	return nil
}
