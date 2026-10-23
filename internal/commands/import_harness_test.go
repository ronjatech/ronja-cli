package commands

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// fakeImportInstance is ONE instance serving every kind a feature holds — the
// shape `ronja import feature` needs, since an import reads tables, workflows,
// apps, modules and automations of one feature and claims them all in one
// ledger. It is the four kind fakes behind one router, sharing ONE managed
// ledger (fakeManaged, which models the server's api/v2/managed
// and probes whichever kind fake holds a row), plus a model of the reference
// inventory (fakeInventory, below).
type fakeImportInstance struct {
	t      *testing.T
	server *httptest.Server

	wf      *fakeInstance // workflows, modules, /me, secrets
	app     *fakeAppInstance
	pipe    *fakePipelineInstance
	auto    *fakeAutomationInstance
	managed *fakeManaged
	inv     *fakeInventory

	// searchHits answers GET /api/v2/search (feature lookup by name).
	searchHits []api.SearchHit
	// mcpServers answers GET /api/v2/mcpserver/:id, for `sync check`.
	mcpServers map[string]bool
	// codexes answers GET /api/v2/codex/:id and validate's codex write targets
	// (id → the codex). unreadable is every table or codex id this caller
	// cannot read: GET answers 404 and validate keeps only its position.
	codexes    map[string]api.Codex
	unreadable map[string]bool
	// fail answers "METHOD /path" with this status instead of routing it;
	// failOnce does so for the next such request only.
	fail     map[string]int
	failOnce map[string]int

	mu       sync.Mutex
	requests []string
	// noInventoryRoute answers the reference inventory as an instance older
	// than it does: the router's plain-text 404.
	noInventoryRoute bool
}

func newFakeImportInstance(t *testing.T) *fakeImportInstance {
	t.Helper()
	f := &fakeImportInstance{t: t, wf: newFakeInstance(t), app: newFakeAppInstance(t),
		pipe: newFakePipelineInstance(t), auto: newFakeAutomationInstance(t), inv: newFakeInventory(),
		mcpServers: map[string]bool{}, fail: map[string]int{}, failOnce: map[string]int{},
		codexes: map[string]api.Codex{}, unreadable: map[string]bool{}}
	// validate's writeTargets resolve against the rows this instance holds,
	// with the caller's reach (validate_write_targets.go :183-249).
	f.wf.writeRows = func(kind, id string) (fakeWriteRow, bool) {
		if f.unreadable[id] {
			return fakeWriteRow{}, false
		}
		if kind == api.WriteTargetCodex {
			c, ok := f.codexes[id]
			return fakeWriteRow{FeatureID: c.FeatureID, Name: c.Name}, ok
		}
		f.pipe.mu.Lock()
		defer f.pipe.mu.Unlock()
		row := f.pipe.tables[id]
		if row == nil {
			return fakeWriteRow{}, false
		}
		return fakeWriteRow{FeatureID: row.FeatureID, Name: row.Name, TableKind: row.Kind}, true
	}
	f.managed = newFakeManaged(t, func(kind, id string) fakeRowFacts {
		for _, probe := range []func(string, string) fakeRowFacts{f.wf.probeRow, f.app.probeRow, f.pipe.probeRow, f.auto.probeRow} {
			if facts := probe(kind, id); facts.Exists {
				return facts
			}
		}
		return fakeRowFacts{}
	})
	f.wf.managed, f.app.managed, f.pipe.managed, f.auto.managed = f.managed, f.managed, f.managed, f.managed
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeImportInstance) URL() string { return f.server.URL }

// writes answers every request this instance served that was not a GET.
// writes is every request that changes something. The import fixture's tree
// is ADOPTED, so its workflow folders' write gate validates on every deploy;
// that request persists nothing (writesBesidesGate).
func (f *fakeImportInstance) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return writesBesidesGate(f.requests)
}

func (f *fakeImportInstance) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeImportInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	once := f.failOnce[r.Method+" "+r.URL.Path]
	delete(f.failOnce, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	p := r.URL.Path
	if once != 0 {
		http.Error(w, `{"error":"boom"}`, once)
		return
	}
	if status := f.fail[r.Method+" "+p]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	if id, ok := strings.CutPrefix(p, "/api/v2/feature/model/"); ok && f.unreadable[id] {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	switch {
	case strings.HasPrefix(p, "/api/v2/codex/"):
		id := strings.TrimPrefix(p, "/api/v2/codex/")
		c, ok := f.codexes[id]
		if !ok || f.unreadable[id] {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, c)
	case strings.HasPrefix(p, "/api/v2/managed"):
		f.managed.serve(w, r)
	case strings.HasPrefix(p, "/api/v2/feature/") && strings.HasSuffix(p, "/references") && !strings.HasPrefix(p, "/api/v2/feature/model"):
		if f.noInventoryRoute {
			// An instance older than the route: the router's plain-text 404.
			http.NotFound(w, r)
			return
		}
		f.inv.serve(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/api/v2/feature/"), "/references"))
	case p == "/api/v2/search":
		writeJSON(w, map[string]any{"result": f.searchHits})
	case strings.HasPrefix(p, "/api/v2/mcpserver/"):
		id := strings.TrimPrefix(p, "/api/v2/mcpserver/")
		if !f.mcpServers[id] {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": id, "name": id})
	case strings.HasPrefix(p, "/api/v2/dataapp"):
		f.app.serve(w, r)
	case strings.HasPrefix(p, "/api/v2/automations"):
		f.auto.serve(w, r)
	case strings.HasPrefix(p, "/api/v2/feature/"), strings.HasPrefix(p, "/api/v2/table/"),
		strings.HasPrefix(p, "/api/v2/tag/"), strings.HasPrefix(p, "/api/v2/duckdb"):
		f.pipe.serve(w, r)
	default:
		// /me, /policy, workflows, modules, secrets, agents.
		f.wf.serve(w, r)
	}
}

// fakeInventory is GET /api/v2/feature/:id/references as the server
// (backend/api/v2/feature/api_references.go) serves it — the paging, the cursor and the shape, each citing the
// line it mirrors in backend/engine/erefs:
//
//   - resources ordered by kind (ResourceKinds order) then id (inventory.go
//     sortResources :712-723, and fillKind's `ORDER BY id` :361-362);
//   - ?limit 1..500, default 500, out of range a 400 (inventory.go :244-250);
//   - ?after is an opaque cursor "v1|<kind>|<last id>", base64url without
//     padding (encodeCursor :106-108), refused 400 when it does not decode or
//     names a kind the call's ?kinds= does not list (decodeCursor :113-124);
//   - a page ends BEFORE the resource that would cross a bound and carries
//     `next` (fillKind :350-417); a page may be short, even empty, and still
//     carry `next` (view.go Next doc) — modelled by shortPages, a page size the
//     fake uses in place of the server's byte and reference bounds;
//   - omittedKinds lists every kind the token may not read, on every page
//     (inventory.go :289-292).
type fakeInventory struct {
	mu        sync.Mutex
	features  map[string]string // id → name
	resources map[string][]api.ReferenceResource
	omitted   []api.OmittedRefKind
	// shortPages, when > 0, caps a page below ?limit, as the server's bounds
	// do; emptyPageFirst answers the first page with no resources and a next.
	shortPages     int
	emptyPageFirst bool
	pagesServed    int
}

func newFakeInventory() *fakeInventory {
	return &fakeInventory{features: map[string]string{}, resources: map[string][]api.ReferenceResource{}}
}

// erefsKindOrder is erefs.ResourceKinds (vocab.go :55-58).
var erefsKindOrder = []string{"table", "metric", "workflow", "dataapp", "module", "automation", "agent", "note", "secret", "codex", "mcpserver"}

func (inv *fakeInventory) add(featureID string, res api.ReferenceResource) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if res.References == nil {
		res.References = []api.ReferencePosition{}
	}
	inv.resources[featureID] = append(inv.resources[featureID], res)
}

func encodeFakeCursor(kind, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1|" + kind + "|" + id))
}

func (inv *fakeInventory) serve(w http.ResponseWriter, r *http.Request, featureID string) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.pagesServed++
	name, ok := inv.features[featureID]
	if !ok {
		http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	limit := 500
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, `{"error":"limit: must be between 1 and 500"}`, http.StatusBadRequest)
			return
		}
		limit = n
	}
	order := map[string]int{}
	for i, k := range erefsKindOrder {
		order[k] = i
	}
	rows := slices.Clone(inv.resources[featureID])
	sort.SliceStable(rows, func(i, j int) bool {
		if order[rows[i].Kind] != order[rows[j].Kind] {
			return order[rows[i].Kind] < order[rows[j].Kind]
		}
		return rows[i].ID < rows[j].ID
	})
	start := 0
	if after := q.Get("after"); after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		kind, id, found := strings.Cut(strings.TrimPrefix(string(raw), "v1|"), "|")
		if err != nil || !strings.HasPrefix(string(raw), "v1|") || !found {
			http.Error(w, `{"error":"after: not a cursor this endpoint returned"}`, http.StatusBadRequest)
			return
		}
		start = len(rows)
		for i, row := range rows {
			if order[row.Kind] > order[kind] || (row.Kind == kind && row.ID > id) {
				start = i
				break
			}
		}
	}
	size := limit
	if inv.shortPages > 0 && inv.shortPages < size {
		size = inv.shortPages
	}
	view := api.FeatureReferences{FeatureID: featureID, FeatureName: name,
		Resources: []api.ReferenceResource{}, OmittedKinds: append([]api.OmittedRefKind{}, inv.omitted...)}
	if inv.emptyPageFirst && q.Get("after") == "" {
		// A page that read rows it could not list (inventory.go maxCandidateRows)
		// ends with a cursor and no resources.
		view.Next = encodeFakeCursor(erefsKindOrder[0], "")
		writeJSON(w, view)
		return
	}
	end := min(start+size, len(rows))
	view.Resources = append(view.Resources, rows[start:end]...)
	if end < len(rows) {
		last := rows[end-1]
		view.Next = encodeFakeCursor(last.Kind, last.ID)
	}
	writeJSON(w, view)
}
