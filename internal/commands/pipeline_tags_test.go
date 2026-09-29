package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The metric loop's TAG half: `"tags": [...]` in a metric file.
//
// What is under test is the one predicate push and status share, and the four
// promises it keeps: a tag the web app added is never touched, a tag the web app
// RENAMED is followed by id rather than reverted or re-minted, a tag the web app
// REMOVED is put back (the folder asserts its names), and a tag-only edit never
// becomes a draft.

// --- the fake -----------------------------------------------------------------

// fakeTags is the tag half of fakePipelineInstance, guarded by the same mutex.
//
// It models the server's MERGE rather than a set replace, because that is the
// route the CLI uses and the property the whole design leans on: a PATCH keeps
// every tag on the target it does not name.
type fakeTags struct {
	// catalog is the organization's tag catalog, by id.
	catalog map[string]*api.Tag
	// on is each target's tags, as catalog ids in assignment order.
	on map[string][]string
	// patches records every PATCH body, by target, for assertions.
	patches []recordedTagPatch
	// noTagRoute answers every tag route as an instance too old to serve it:
	// the fake's ordinary 404 for a path it does not know.
	noTagRoute bool
	// failGet and failPatch answer the route with a status, by target id. A
	// 403 is the handler's own refusal (no code) — on the GET its visibility
	// gate, on the PATCH its role gate; a 400 on failPatch is the merge's race
	// refusal, "one or more tags not found" (no code); anything else is a bare
	// failure. The CAP is not a knob: the fake computes it, coded tag_limit.
	failGet   map[string]int
	failPatch map[string]int
	// denyScope answers the given methods with the scoped-token middleware's
	// 403, code insufficient_scope — which the real server sends BEFORE the
	// handler runs, so it wins over every other knob here.
	denyScope map[string]bool
	// renameBeforePatch is a queue of catalog renames, one applied at the start
	// of each PATCH before the body is looked at — an admin renaming a tag
	// between the CLI's GET and its PATCH, which is the only way the real race
	// reaches the server's overlap refusal.
	renameBeforePatch [][2]string
	nextTagID         int
}

type recordedTagPatch struct {
	ID string
	api.PatchTagsInput
}

func (f *fakePipelineInstance) tags() *fakeTags {
	if f.tagState.catalog == nil {
		f.tagState.catalog = map[string]*api.Tag{}
		f.tagState.on = map[string][]string{}
		f.tagState.failGet = map[string]int{}
		f.tagState.failPatch = map[string]int{}
		f.tagState.denyScope = map[string]bool{}
	}
	return &f.tagState
}

// AddCatalogTag puts a tag in the organization's catalog, on no target.
func (f *fakePipelineInstance) AddCatalogTag(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags().catalog[id] = &api.Tag{ID: id, Name: name}
}

// TagInWebApp is a colleague adding a tag to a target in the web app: the name
// is reused case-insensitively from the catalog or minted.
func (f *fakePipelineInstance) TagInWebApp(targetID, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.ensureTag(name)
	if !slices.Contains(f.tags().on[targetID], id) {
		f.tags().on[targetID] = append(f.tags().on[targetID], id)
	}
	return id
}

// UntagInWebApp is a colleague removing a tag from a target in the web app.
func (f *fakePipelineInstance) UntagInWebApp(targetID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.tags()
	st.on[targetID] = slices.DeleteFunc(st.on[targetID], func(id string) bool {
		return strings.EqualFold(st.catalog[id].Name, name)
	})
}

// RenameTag is an admin renaming a catalog tag in the web app: the id stays.
func (f *fakePipelineInstance) RenameTag(oldName, newName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renameTag(oldName, newName)
}

// renameTag is RenameTag with f.mu already held.
func (f *fakePipelineInstance) renameTag(oldName, newName string) {
	for _, tag := range f.tags().catalog {
		if strings.EqualFold(tag.Name, oldName) {
			tag.Name = newName
			return
		}
	}
	f.t.Fatalf("RenameTag: no tag %q", oldName)
}

// RenameBeforeNextPatch queues an admin rename that lands between the CLI's
// read of a metric's tags and its next PATCH.
func (f *fakePipelineInstance) RenameBeforeNextPatch(oldName, newName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags().renameBeforePatch = append(f.tags().renameBeforePatch, [2]string{oldName, newName})
}

// TagNamesOn reads a target's tag names, sorted, for assertions.
func (f *fakePipelineInstance) TagNamesOn(targetID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range f.tags().on[targetID] {
		out = append(out, f.tags().catalog[id].Name)
	}
	sort.Strings(out)
	return out
}

// TagIDOf reads a catalog tag's id by name.
func (f *fakePipelineInstance) TagIDOf(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, tag := range f.tags().catalog {
		if strings.EqualFold(tag.Name, name) {
			return id
		}
	}
	return ""
}

// lastTagPatch is the most recent PATCH, failing the test when there is none.
func lastTagPatch(t *testing.T, f *fakePipelineInstance) recordedTagPatch {
	t.Helper()
	patches := f.tagPatches()
	if len(patches) == 0 {
		t.Fatal("no tag PATCH was sent")
	}
	return patches[len(patches)-1]
}

// tagPatches returns the recorded PATCH bodies.
func (f *fakePipelineInstance) tagPatches() []recordedTagPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedTagPatch{}, f.tags().patches...)
}

// ensureTag is EnsureByNames for one name: reuse case-insensitively, or mint.
// Caller holds f.mu.
func (f *fakePipelineInstance) ensureTag(name string) string {
	st := f.tags()
	for id, tag := range st.catalog {
		if strings.EqualFold(strings.TrimSpace(tag.Name), strings.TrimSpace(name)) {
			return id
		}
	}
	st.nextTagID++
	id := fmt.Sprintf("tag-%d", st.nextTagID)
	st.catalog[id] = &api.Tag{ID: id, Name: strings.TrimSpace(name)}
	return id
}

func (f *fakePipelineInstance) tagList(targetID string) []api.Tag {
	out := []api.Tag{}
	for _, id := range f.tags().on[targetID] {
		out = append(out, *f.tags().catalog[id])
	}
	return out
}

// serveTags models GET and PATCH /tag/of/table/:id.
//
// The gate is the server's: an unknown target is a 403 (visibility), never a
// 404 — which is what lets the CLI read a 404 as "no such route".
func (f *fakePipelineInstance) serveTags(w http.ResponseWriter, r *http.Request, targetID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.tags()
	if st.denyScope[r.Method] {
		// gt.requireScope's body: the action is the method default.
		action := "write"
		if r.Method == http.MethodGet {
			action = "read"
		}
		http.Error(w, `{"error":"token scope \"data\" does not permit `+action+`","code":"insufficient_scope"}`, http.StatusForbidden)
		return
	}
	if f.tables[targetID] == nil {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	failBody := func(status int) string {
		switch status {
		case http.StatusForbidden:
			return `{"error":"forbidden"}`
		case http.StatusBadRequest:
			return `{"error":"one or more tags not found"}`
		}
		return `{"error":"boom"}`
	}
	switch r.Method {
	case http.MethodGet:
		if status := st.failGet[targetID]; status != 0 {
			http.Error(w, failBody(status), status)
			return
		}
		writeJSON(w, f.tagList(targetID))
	case http.MethodPatch:
		if status := st.failPatch[targetID]; status != 0 {
			http.Error(w, failBody(status), status)
			return
		}
		if len(st.renameBeforePatch) > 0 {
			rename := st.renameBeforePatch[0]
			st.renameBeforePatch = st.renameBeforePatch[1:]
			f.renameTag(rename[0], rename[1])
		}
		var in api.PatchTagsInput
		decodeBody(f.t, r, &in)
		st.patches = append(st.patches, recordedTagPatch{ID: targetID, PatchTagsInput: in})
		if len(in.Add)+len(in.Remove)+len(in.RemoveIDs) == 0 {
			http.Error(w, `{"error":"at least one of add, remove or removeIDs must be non-empty"}`, http.StatusBadRequest)
			return
		}
		// The server's two pre-mint refusals, in its order: an add naming a
		// tag the same call removes (by name, or by id — which is what a
		// rename between the caller's read and this PATCH produces), then the
		// 20-tag cap on the FINAL set. Neither mints anything.
		removedNames := map[string]bool{}
		for _, name := range in.Remove {
			removedNames[strings.ToLower(strings.TrimSpace(name))] = true
		}
		for _, id := range in.RemoveIDs {
			if tag := st.catalog[id]; tag != nil {
				removedNames[strings.ToLower(tag.Name)] = true
			}
		}
		for _, name := range in.Add {
			if removedNames[strings.ToLower(strings.TrimSpace(name))] {
				body, _ := json.Marshal(map[string]string{
					"error": fmt.Sprintf("tag %q is both added and removed", strings.TrimSpace(name)),
					"code":  "tag_overlap",
				})
				http.Error(w, string(body), http.StatusBadRequest)
				return
			}
		}
		final := map[string]bool{}
		for _, id := range st.on[targetID] {
			if !slices.Contains(in.RemoveIDs, id) && !removedNames[strings.ToLower(st.catalog[id].Name)] {
				final[strings.ToLower(st.catalog[id].Name)] = true
			}
		}
		for _, name := range in.Add {
			final[strings.ToLower(strings.TrimSpace(name))] = true
		}
		if len(final) > 20 {
			http.Error(w, fmt.Sprintf(`{"error":"at most 20 tags per resource (this change would leave %d)","code":"tag_limit"}`, len(final)), http.StatusBadRequest)
			return
		}
		existed := map[string]bool{}
		for _, tag := range st.catalog {
			existed[strings.ToLower(tag.Name)] = true
		}
		var created, added, removed, unmatched []string
		current := st.on[targetID]
		for _, name := range in.Add {
			id := f.ensureTag(name)
			if !existed[strings.ToLower(strings.TrimSpace(name))] {
				created = append(created, strings.TrimSpace(name))
			}
			if !slices.Contains(current, id) {
				current = append(current, id)
				added = append(added, st.catalog[id].Name)
			}
		}
		for _, removeID := range in.RemoveIDs {
			if i := slices.Index(current, removeID); i >= 0 {
				removed = append(removed, st.catalog[removeID].Name)
				current = slices.Delete(current, i, i+1)
			} else {
				unmatched = append(unmatched, removeID)
			}
		}
		for _, name := range in.Remove {
			i := slices.IndexFunc(current, func(id string) bool { return strings.EqualFold(st.catalog[id].Name, name) })
			if i < 0 {
				unmatched = append(unmatched, name)
				continue
			}
			removed = append(removed, st.catalog[current[i]].Name)
			current = slices.Delete(current, i, i+1)
		}
		st.on[targetID] = current
		orEmpty := func(s []string) []string {
			if s == nil {
				return []string{}
			}
			return s
		}
		writeJSON(w, map[string]any{
			"tags": f.tagList(targetID), "added": orEmpty(added), "removed": orEmpty(removed),
			"unmatchedRemoves": orEmpty(unmatched), "created": orEmpty(created),
		})
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// --- fixtures -----------------------------------------------------------------

const taggedRecipe = `{"source":"table-orders","value":"revenue"}`

// taggedMetricBody is a metric file carrying `tags` (a JSON list literal), or
// no tags key at all when tags is "".
func taggedMetricBody(tags string) string {
	if tags == "" {
		return `{"recipe":` + taggedRecipe + `}`
	}
	return `{"recipe":` + taggedRecipe + `,"tags":` + tags + `}`
}

// seedTaggedStack lays out a NAMED-STACK folder defining one metric with the
// given tags, which is the shape a repository keeps: the tag record lands in
// the committed lock.
func seedTaggedStack(t *testing.T, f *fakePipelineInstance, tags string) string {
	t.Helper()
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	// One level down, so `sync apply` can walk the parent as a tree holding
	// exactly this folder.
	root := filepath.Join(t.TempDir(), "sales")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Sales pipeline",
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	writePipelineFolder(t, root, manifest, map[string]string{
		"metrics/average_order.json": taggedMetricBody(tags),
	})
	return root
}

const taggedPath = "metrics/average_order.json"

func setTags(t *testing.T, root, tags string) {
	t.Helper()
	if err := wfdir.WriteFile(root, taggedPath, taggedMetricBody(tags)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// pushTagged runs a --json push and returns the one metric entry.
func pushTagged(t *testing.T, root string, args ...string) (payload map[string]any, metric map[string]any) {
	t.Helper()
	out, stderr, err := runPipelineCLI(t, root, append([]string{"pipeline", "push", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	payload = decodeJSON(t, out)
	return payload, payload["metrics"].([]any)[0].(map[string]any)
}

func tagOutcomeOf(t *testing.T, metric map[string]any) map[string]any {
	t.Helper()
	tags, ok := metric["tags"].(map[string]any)
	if !ok {
		t.Fatalf("the metric entry carries no tag outcome: %v", metric)
	}
	return tags
}

// statusTagged runs a --json status and returns the one metric entry.
func statusTagged(t *testing.T, root string) map[string]any {
	t.Helper()
	out, _, _ := runPipelineCLI(t, root, "pipeline", "status", "--json")
	return decodeJSON(t, out)["remote"].(map[string]any)["metrics"].([]any)[0].(map[string]any)
}

func lockTagsOf(t *testing.T, root string) ([]wfdir.AppliedTag, *wfdir.TagsRefusal) {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	_, applied, refused := lock.MetricTags("dev", taggedPath)
	return applied, refused
}

func appliedNames(applied []wfdir.AppliedTag) string {
	var names []string
	for _, a := range applied {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func metricIDOf(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	id, _, _ := lock.MetricSeen("dev", taggedPath)
	if id == "" {
		t.Fatal("no metric recorded")
	}
	return id
}

// --- push ---------------------------------------------------------------------

// TestMetricTagsCreateWithTags: a new metric is created, and then tagged by ONE
// PATCH naming every name the file lists; the lock records each name against
// the id the server answered, and a name new to the catalog is said out loud.
func TestMetricTagsCreateWithTags(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance","Sales"]`)
	f.AddCatalogTag("tag-fin", "Finance")

	_, metric := pushTagged(t, root)
	id := metricIDOf(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Sales" {
		t.Fatalf("tags on the metric = %s", got)
	}
	patches := f.tagPatches()
	if len(patches) != 1 || patches[0].ID != id || len(patches[0].RemoveIDs) != 0 {
		t.Fatalf("patches = %+v", patches)
	}
	tags := tagOutcomeOf(t, metric)
	if tags["outcome"] != "applied" {
		t.Errorf("tag outcome = %v", tags)
	}
	if fmt.Sprint(tags["created"]) != "[Sales]" {
		t.Errorf("created = %v — a name new to the catalog must be said, so a typo is seen", tags["created"])
	}
	applied, _ := lockTagsOf(t, root)
	if appliedNames(applied) != "Finance,Sales" || applied[0].ID == "" {
		t.Errorf("tagsApplied = %+v", applied)
	}
	// And the human report says what happened.
	setTags(t, root, `["Finance","Sales","Finanace"]`)
	out, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, want := range []string{"+Finanace", `created tag "Finanace"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the push report does not say %q:\n%s", want, out)
		}
	}
}

// TestMetricTagsOnlyChangeNeedsNoDraft: tags are not drafted. A tag-only edit
// PATCHes the live metric and nothing else — no checkout, no recipe write, no
// build — and the push is NOT up to date, because it changed the organization.
func TestMetricTagsOnlyChangeNeedsNoDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	// Publish the created metric's draft away, so the next push starts from a
	// clean definition.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	checkouts, writes, syncs := len(f.checkouts), len(f.recipeWrites), len(f.syncs)

	setTags(t, root, `["Finance","Operations"]`)
	payload, metric := pushTagged(t, root)
	if metric["outcome"] != metricOutcomeUpToDate {
		t.Errorf("the recipe half = %v, want up to date — a tag edit is not a definition change", metric["outcome"])
	}
	if len(f.checkouts) != checkouts || len(f.recipeWrites) != writes || len(f.syncs) != syncs {
		t.Errorf("a tag-only change touched the draft lifecycle: checkouts %d→%d, writes %d→%d, syncs %d→%d",
			checkouts, len(f.checkouts), writes, len(f.recipeWrites), syncs, len(f.syncs))
	}
	if payload["upToDate"] == true {
		t.Error("a push that changed the metric's tags reported upToDate")
	}
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Operations" {
		t.Errorf("tags = %s", got)
	}
	// And a third push is up to date: nothing is pending any more.
	payload, _ = pushTagged(t, root)
	if payload["upToDate"] != true {
		t.Errorf("an unchanged folder was not up to date: %v", payload)
	}
}

// TestMetricTagsCaseFoldsLikeTheServer: the file says `finance`, the catalog
// holds `Finance`. The server reuses the existing tag, and the folder must then
// read as satisfied — a comparison that did not fold would leave it pending for
// ever and PATCH on every push.
func TestMetricTagsCaseFoldsLikeTheServer(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["finance"]`)
	f.AddCatalogTag("tag-fin", "Finance")

	pushTagged(t, root)
	patches := len(f.tagPatches())
	if entry := statusTagged(t, root); entry["tagsPending"] == true {
		t.Errorf("status reports a satisfied case-folded name as pending: %v", entry)
	}
	pushTagged(t, root)
	if len(f.tagPatches()) != patches {
		t.Errorf("a second push PATCHed a satisfied name: %+v", f.tagPatches())
	}
	applied, _ := lockTagsOf(t, root)
	if len(applied) != 1 || applied[0].ID != "tag-fin" || applied[0].Name != "Finance" {
		t.Errorf("tagsApplied = %+v, want the server's spelling against its id", applied)
	}
}

// TestMetricTagsNeverTouchAWebAppTag: a colleague tagged the metric in the web
// app. Removing a DIFFERENT name from the file removes only that one, by id.
func TestMetricTagsNeverTouchAWebAppTag(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance","Sales"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	f.TagInWebApp(id, "Board KPI")

	if entry := statusTagged(t, root); entry["tagsPending"] == true {
		t.Errorf("a web-app-only tag made the folder pending: %v", entry)
	}
	setTags(t, root, `["Finance"]`)
	_, metric := pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Board KPI,Finance" {
		t.Errorf("tags = %s", got)
	}
	last := lastTagPatch(t, f)
	if len(last.Add) != 0 || len(last.Remove) != 0 || fmt.Sprint(last.RemoveIDs) != "["+f.TagIDOf("Sales")+"]" {
		t.Errorf("the removal was not one id: %+v", last)
	}
	if tags := tagOutcomeOf(t, metric); fmt.Sprint(tags["removed"]) != "[Sales]" {
		t.Errorf("removed = %v", tags["removed"])
	}
}

// TestMetricTagsFolderWinsOverAWebAppRemoval: the folder ASSERTS its names, so
// a colleague removing one in the web app makes the folder pending, and the
// next push puts it back (Adam's decision, 2026-09-28).
func TestMetricTagsFolderWinsOverAWebAppRemoval(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance","Sales"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	f.UntagInWebApp(id, "Sales")

	entry := statusTagged(t, root)
	if entry["tagsPending"] != true {
		t.Fatalf("a removed folder tag did not make the folder pending: %v", entry)
	}
	pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Sales" {
		t.Errorf("tags = %s — the folder's name was not re-added", got)
	}
	last := lastTagPatch(t, f)
	if fmt.Sprint(last.Add) != "[Sales]" || len(last.RemoveIDs) != 0 {
		t.Errorf("patch = %+v", last)
	}
}

// TestMetricTagsFollowARename: an admin renamed the folder's tag in the web app.
// The rename is KEPT — the recorded id still on the metric satisfies the file's
// old name, so nothing is re-minted and nothing reverted — and status says so.
// Then the file drops the name, and the push removes the tag BY ID, under the
// name the metric shows now.
func TestMetricTagsFollowARename(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Fin"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	finID := f.TagIDOf("Fin")
	f.RenameTag("Fin", "Finance")
	patches := len(f.tagPatches())

	entry := statusTagged(t, root)
	if entry["tagsPending"] == true {
		t.Errorf("a rename made the folder pending: %v", entry)
	}
	tags, _ := entry["tags"].(map[string]any)
	if !strings.Contains(fmt.Sprint(tags["renamed"]), "Fin → Finance") {
		t.Errorf("status does not report the rename: %v", entry)
	}
	out, _, _ := runPipelineCLI(t, root, "pipeline", "status")
	if !strings.Contains(out, "renamed in the web app: Fin → Finance — update the file") {
		t.Errorf("the human status does not say it:\n%s", out)
	}
	pushOut, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(pushOut, "renamed in the web app: Fin → Finance — update the file") {
		t.Errorf("an up-to-date push dropped the rename note:\n%s", pushOut)
	}
	if len(f.tagPatches()) != patches {
		t.Errorf("a rename was PATCHed: %+v", f.tagPatches()[patches:])
	}
	if f.TagIDOf("Fin") != "" || strings.Join(f.TagNamesOn(id), ",") != "Finance" {
		t.Errorf("the old name was re-minted: catalog Fin=%q, tags=%v", f.TagIDOf("Fin"), f.TagNamesOn(id))
	}

	setTags(t, root, `[]`)
	_, metric := pushTagged(t, root)
	last := lastTagPatch(t, f)
	if fmt.Sprint(last.RemoveIDs) != "["+finID+"]" {
		t.Errorf("the renamed tag was not removed by id: %+v", last)
	}
	if len(f.TagNamesOn(id)) != 0 {
		t.Errorf("tags = %v", f.TagNamesOn(id))
	}
	if outcome := tagOutcomeOf(t, metric); fmt.Sprint(outcome["removed"]) != "[Finance]" {
		t.Errorf("removed = %v", outcome["removed"])
	}
}

// TestMetricTagsWithoutARecordAreAdditiveOnly: an existing metric that already
// carries tags, and a folder with no record of which are its own — a legacy
// folder, a rebind, the first push of a `tags` key. Nothing is removed, because
// nothing here can tell the folder's tags from anybody else's.
func TestMetricTagsWithoutARecordAreAdditiveOnly(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, "")
	pushTagged(t, root)
	id := metricIDOf(t, root)
	f.TagInWebApp(id, "Legacy")

	setTags(t, root, `["Finance"]`)
	pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Legacy" {
		t.Errorf("tags = %s", got)
	}
	for _, p := range f.tagPatches() {
		if len(p.RemoveIDs) != 0 || len(p.Remove) != 0 {
			t.Errorf("a push with no record removed something: %+v", p)
		}
	}
	// `[]` with no record asserts none, and still removes nothing it did not put.
	setTags(t, root, `[]`)
	pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Legacy" {
		t.Errorf("tags = %s, want only the web app's", got)
	}
}

// TestMetricTagsAbsentKeyIsUnmanaged: a file with no `tags` key never reads or
// writes tags, whatever the lock records — deleting the key is how a folder
// stops managing them, and it must not strip what it once put there.
func TestMetricTagsAbsentKeyIsUnmanaged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	setTags(t, root, "")
	before := f.requestsMatching("/api/v2/tag/")
	_, metric := pushTagged(t, root)
	if f.requestsMatching("/api/v2/tag/") != before {
		t.Errorf("a file with no tags key reached the tag routes")
	}
	if _, has := metric["tags"]; has {
		t.Errorf("a file with no tags key reported a tag outcome: %v", metric)
	}
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance" {
		t.Errorf("tags = %s", got)
	}
}

// TestMetricTagsSkipAnArchivedMetric: an archived or hidden metric is not tag-
// reconciled, is not pending, and the skip is said.
func TestMetricTagsSkipAnArchivedMetric(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	f.mu.Lock()
	f.tables[id].Archived = true
	f.mu.Unlock()
	setTags(t, root, `["Finance","Sales"]`)

	entry := statusTagged(t, root)
	if entry["tagsPending"] == true {
		t.Errorf("an archived metric was reported tag-pending: %v", entry)
	}
	if tags, _ := entry["tags"].(map[string]any); tags["state"] != "skipped_archived" {
		t.Errorf("status tags = %v", entry["tags"])
	}
	patches := len(f.tagPatches())
	_, metric := pushTagged(t, root)
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "skipped_archived" {
		t.Errorf("tag outcome = %v", tags)
	}
	if len(f.tagPatches()) != patches {
		t.Errorf("an archived metric was PATCHed")
	}
}

// TestMetricTagsNeverPatchABoundPlainTable: `ronja bind` cannot tell a metric
// from a table of the same name, so a metric file can be bound to a plain
// table. That is refused by the recipe half — and the tag half must not have
// run before it.
func TestMetricTagsNeverPatchABoundPlainTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.SetMetricSeen("dev", taggedPath, "table-orders", "", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	_, _, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a metric file bound to a plain table must be refused")
	}
	if f.requestsMatching("PATCH /api/v2/tag/") != 0 {
		t.Errorf("a plain table was PATCHed: %v", f.tagPatches())
	}
}

// TestMetricTagsOlderServerIsRefusedOnceNotLooped: an instance with no tag
// route. The tag half is refused with a sentence naming the cause, the recipe
// half is unaffected, the refusal is recorded — and the metric is then NOT
// pending, so `sync apply` does not re-send the doomed request on every run,
// TestMetricTagsOlderServerIsFailedAndRetried: an instance with no tag route (a
// 404) is a fact about the DEPLOY, not about the file — recording it in the
// committed lock would stop every colleague's push, and this one's after the
// organization updates, from ever trying again. `failed`, never recorded, the
// push exits non-zero, the recipe half still lands, and the next push tries on
// its own.
func TestMetricTagsOlderServerIsFailedAndRetried(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.tagState.noTagRoute = true
	root := seedTaggedStack(t, f, `["Finance"]`)

	_, metric, _ := pushTaggedFailing(t, root)
	if metric["outcome"] != metricOutcomePushed {
		t.Errorf("the recipe half = %v — a tag failure must not fail it", metric["outcome"])
	}
	tags := tagOutcomeOf(t, metric)
	if want := "this organization's Ronja doesn't have tag support yet — push again after it's updated"; tags["outcome"] != "failed" || !strings.Contains(fmt.Sprint(tags["reason"]), want) {
		t.Errorf("tag outcome = %v, want failed saying %q", tags, want)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Fatalf("an older server was recorded as a refusal: %+v", refused)
	}
	if st, _ := statusTagged(t, root)["tags"].(map[string]any); st["state"] != "unread" {
		t.Errorf("status tags = %v, want unread for an instance with no tag route", st)
	}
	before := f.requestsMatching("/api/v2/tag/")
	pushTaggedFailing(t, root)
	if f.requestsMatching("/api/v2/tag/") == before {
		t.Error("the next push did not try again without --retry-tags")
	}
	// Once the server can answer, a plain push lands.
	f.tagState.noTagRoute = false
	pushTagged(t, root)
	id := metricIDOf(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance" {
		t.Errorf("tags = %s", got)
	}
}

// TestMetricTagsSurviveAFailedPublishAndADiscard: publish and discard never
// touch tags, and neither resets the tag record — so the next push removes no
// web-app tag and re-adds nothing it already put there.
func TestMetricTagsSurviveAFailedPublishAndADiscard(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	f.TagInWebApp(id, "Board KPI")

	// A failed publish.
	f.mu.Lock()
	f.failCommit[f.draftOf[id]] = http.StatusInternalServerError
	f.mu.Unlock()
	runPipelineCLI(t, root, "pipeline", "publish", "--json")
	// A discard.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes", "--json"); err != nil {
		t.Fatalf("discard: %v\n%s", err, stderr)
	}
	applied, _ := lockTagsOf(t, root)
	if appliedNames(applied) != "Finance" {
		t.Fatalf("the tag record did not survive publish/discard: %+v", applied)
	}
	patches := len(f.tagPatches())
	pushTagged(t, root)
	if len(f.tagPatches()) != patches {
		t.Errorf("the next push PATCHed: %+v", f.tagPatches()[patches:])
	}
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Board KPI,Finance" {
		t.Errorf("tags = %s", got)
	}
}

// TestMetricTagsOnALegacyFolder: a stack-less folder keeps the tag record in
// .ronja/state.json, reads it back on the next push, and naming the folder's
// stack carries it into the committed lock — without it the named folder would
// fall back to additive-only and never remove a name the file drops.
func TestMetricTagsOnALegacyFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedMetricFolder(t, f)
	seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
	bindPipelineTable(t, root, f.Key(), "orders.sql", "table-orders")
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		`{"recipe":`+aovRecipe+`,"tags":["Finance","Sales"]}`); err != nil {
		t.Fatal(err)
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	recorded := metricStateOf(t, root, f.Key(), "metrics/average_order.json")
	if appliedNames(recorded.TagsApplied) != "Finance,Sales" {
		t.Fatalf("the legacy baseline recorded %+v", recorded)
	}
	patches := len(f.tagPatches())
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatal(err)
	}
	if len(f.tagPatches()) != patches {
		t.Error("the legacy record was not read back: a satisfied folder PATCHed again")
	}

	// Name the stack: the record moves into the committed lock.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push --stack: %v\n%s", err, stderr)
	}
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	id, applied, _ := lock.MetricTags("dev", "metrics/average_order.json")
	if id != "table-aov" || appliedNames(applied) != "Finance,Sales" {
		t.Fatalf("naming the stack did not carry the tag record: %q %+v", id, applied)
	}
	// And the carried record is what makes a removal work there.
	if err := wfdir.WriteFile(root, "metrics/average_order.json",
		`{"recipe":`+aovRecipe+`,"tags":["Finance"]}`); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := strings.Join(f.TagNamesOn("table-aov"), ","); got != "Finance" {
		t.Errorf("tags = %s", got)
	}
}

// --- failures that are NOT refusals ---------------------------------------------

// pushTaggedFailing runs a --json push that must exit non-zero, and returns the
// one metric entry.
func pushTaggedFailing(t *testing.T, root string, args ...string) (payload map[string]any, metric map[string]any, stdout string) {
	t.Helper()
	out, stderr, err := runPipelineCLI(t, root, append([]string{"pipeline", "push", "--json"}, args...)...)
	if err == nil {
		t.Fatalf("the push exited zero:\n%s\n%s", out, stderr)
	}
	payload = decodeJSON(t, out)
	return payload, payload["metrics"].([]any)[0].(map[string]any), out
}

// TestMetricTagsTransientFailureIsFailedNotRefused: a 5xx on the PATCH says
// nothing lasting about the metric. It is `failed`, never recorded (so the next
// push retries without --retry-tags), the push exits non-zero and is not up to
// date — and `sync apply` reads the folder as a failure, never "nothing to
// push".
func TestMetricTagsTransientFailureIsFailedNotRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	id := metricIDOf(t, root)
	setTags(t, root, `["Finance","Sales"]`)
	f.tags().failPatch[id] = http.StatusServiceUnavailable

	payload, metric, _ := pushTaggedFailing(t, root)
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "failed" {
		t.Errorf("tag outcome = %v, want failed — a 503 is not the server refusing the tags", tags)
	}
	if payload["upToDate"] == true {
		t.Error("a push whose tag half failed reported upToDate")
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Errorf("a transient failure was recorded as a refusal: %+v", refused)
	}
	if entry := statusTagged(t, root); entry["tagsPending"] != true {
		t.Errorf("an unapplied tag change is no longer pending: %v", entry)
	}

	tree := filepath.Dir(root)
	out, err := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--json")
	if exitCodeOf(err) == syncExitClean {
		t.Fatalf("sync apply exited clean over a failed tag reconcile:\n%s", out)
	}
	if folder := applyFolderLine(t, applyReportOf(t, out), filepath.Base(root)); folder.Verdict != verdictUnknown || folder.Reason != syncReasonTagsNotApplied {
		t.Errorf("folder = %+v, want unknown/tags_not_applied — a 503 on a write may have landed", folder)
	}

	// The next push, with the server back, retries on its own.
	delete(f.tags().failPatch, id)
	pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Sales" {
		t.Errorf("tags = %s", got)
	}
}

// TestMetricTagsScopeDeniedIsFailedNotRefused: a token whose scope does not
// cover tags is a fact about the TOKEN, not the metric — recording it in the
// COMMITTED lock would stop every colleague's push from trying. `failed`, never
// recorded, with a reason that names the fix; and status reads a scope-denied
// GET as unread, on the one predicate push uses.
func TestMetricTagsScopeDeniedIsFailedNotRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	setTags(t, root, `["Finance","Sales"]`)
	f.tags().denyScope[http.MethodPatch] = true

	_, metric, _ := pushTaggedFailing(t, root)
	tags := tagOutcomeOf(t, metric)
	if tags["outcome"] != "failed" {
		t.Errorf("tag outcome = %v, want failed", tags)
	}
	if want := "this token's scope doesn't allow write on tags — use a token with data:write"; !strings.Contains(fmt.Sprint(tags["reason"]), want) {
		t.Errorf("reason = %v, want it to say %q", tags["reason"], want)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Errorf("a scope denial was recorded as a refusal: %+v", refused)
	}

	f.tags().denyScope[http.MethodGet] = true
	entry := statusTagged(t, root)
	if st, _ := entry["tags"].(map[string]any); st["state"] != "unread" {
		t.Errorf("status tags = %v, want unread for a scope-denied read", entry["tags"])
	}
	if entry["tagsPending"] == true {
		t.Errorf("an unread tag half was reported pending: %v", entry)
	}
}

// TestMetricTagsCallerRefusalsAreFailed: the handler's own 403s are facts
// about the CALLER — a role that may not change tags, a metric this caller
// cannot see — and a colleague's push may well be allowed. `failed`, never
// recorded, each with the reason that names it: a 403 on the PATCH after the
// GET was answered is the role gate (the GET and the PATCH share the
// visibility gate), a 403 on the GET is visibility.
func TestMetricTagsCallerRefusalsAreFailed(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	setTags(t, root, `["Finance","Sales"]`)

	f.tags().failPatch[id] = http.StatusForbidden
	_, metric, _ := pushTaggedFailing(t, root)
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "failed" || !strings.Contains(fmt.Sprint(tags["reason"]), "your role can't change tags") {
		t.Errorf("tag outcome = %v, want failed: your role can't change tags", tags)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Errorf("a role 403 was recorded: %+v", refused)
	}

	delete(f.tags().failPatch, id)
	f.tags().failGet[id] = http.StatusForbidden
	_, metric, _ = pushTaggedFailing(t, root)
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "failed" || !strings.Contains(fmt.Sprint(tags["reason"]), "you can't see this metric") {
		t.Errorf("tag outcome = %v, want failed: you can't see this metric", tags)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Errorf("a visibility 403 was recorded: %+v", refused)
	}
}

// TestMetricTagsRaceNotFoundIsFailed: the merge's "one or more tags not found"
// is a tag deleted between the add's resolve and the merge — a race, which the
// next push's fresh read clears. `failed`, never recorded.
func TestMetricTagsRaceNotFoundIsFailed(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	setTags(t, root, `["Finance","Sales"]`)
	f.tags().failPatch[id] = http.StatusBadRequest

	_, metric, _ := pushTaggedFailing(t, root)
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "failed" {
		t.Errorf("tag outcome = %v, want failed", tags)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Fatalf("a race 400 was recorded: %+v", refused)
	}
	delete(f.tags().failPatch, id)
	pushTagged(t, root)
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Sales" {
		t.Errorf("the next push did not retry: %s", got)
	}
}

// TestMetricTagsCapRefusalIsRecordedAndSaidOnce: the web app filled the metric
// to 19 tags, and the file adds two more. The server refuses the final set of
// 21 — a refusal that answers the same way until something changes, so it is
// recorded, reported, and not re-sent on the next push.
func TestMetricTagsCapRefusalIsRecordedAndSaidOnce(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, "")
	pushTagged(t, root)
	id := metricIDOf(t, root)
	for i := range 19 {
		f.TagInWebApp(id, fmt.Sprintf("web-%02d", i))
	}
	setTags(t, root, `["Finance","Sales"]`)

	_, metric := pushTagged(t, root)
	tags := tagOutcomeOf(t, metric)
	if tags["outcome"] != "refused" || !strings.Contains(fmt.Sprint(tags["reason"]), "at most 20 tags") {
		t.Errorf("tag outcome = %v", tags)
	}
	if _, refused := lockTagsOf(t, root); refused == nil {
		t.Fatal("the cap refusal was not recorded")
	}
	if n := len(f.TagNamesOn(id)); n != 19 {
		t.Errorf("a refused PATCH moved the metric: %d tags", n)
	}
	before := len(f.tagPatches())
	_, metric = pushTagged(t, root)
	if len(f.tagPatches()) != before {
		t.Error("a recorded cap refusal was re-sent with the file unchanged")
	}
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "refused" {
		t.Errorf("the recorded refusal was not said: %v", tags)
	}
}

// TestMetricTagsOverlapRaceReplansOnce: an admin renames the tag this folder
// is removing TO a name the file adds, between the push's read and its PATCH.
// The server refuses the body (tag_overlap); the push re-reads, re-plans —
// the renamed tag now satisfies the file's name — and lands the rest.
func TestMetricTagsOverlapRaceReplansOnce(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Fin"]`)
	pushTagged(t, root)
	id := metricIDOf(t, root)
	finID := f.TagIDOf("Fin")
	setTags(t, root, `["Board","Ops"]`)
	f.RenameBeforeNextPatch("Fin", "Board")
	before := len(f.tagPatches())

	_, metric := pushTagged(t, root)
	patches := f.tagPatches()[before:]
	if len(patches) != 2 {
		t.Fatalf("want the refused PATCH and one re-planned PATCH, got %+v", patches)
	}
	if len(patches[1].RemoveIDs) != 0 || fmt.Sprint(patches[1].Add) != "[Ops]" {
		t.Errorf("re-planned PATCH = %+v, want add [Ops] only — the renamed tag satisfies Board", patches[1])
	}
	tags := tagOutcomeOf(t, metric)
	if tags["outcome"] != "applied" || fmt.Sprint(tags["added"]) != "[Ops]" {
		t.Errorf("tag outcome = %v", tags)
	}
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Board,Ops" {
		t.Errorf("tags = %s", got)
	}
	applied, refused := lockTagsOf(t, root)
	if refused != nil {
		t.Errorf("a race that re-planned was recorded as a refusal: %+v", refused)
	}
	if appliedNames(applied) != "Board,Ops" || !slices.ContainsFunc(applied, func(a wfdir.AppliedTag) bool { return a.ID == finID }) {
		t.Errorf("tagsApplied = %+v, want Board recorded against the renamed id %s", applied, finID)
	}
}

// TestMetricTagsSecondOverlapIsFailed: the race again on the re-planned PATCH.
// One re-plan only: the second overlap is `failed`, not recorded, and the push
// exits non-zero — the next push starts over from a fresh read.
func TestMetricTagsSecondOverlapIsFailed(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Fin","Old"]`)
	pushTagged(t, root)
	setTags(t, root, `["Board","Ops"]`)
	f.RenameBeforeNextPatch("Fin", "Board")
	f.RenameBeforeNextPatch("Old", "Ops")
	before := len(f.tagPatches())

	_, metric, _ := pushTaggedFailing(t, root)
	if n := len(f.tagPatches()) - before; n != 2 {
		t.Errorf("want exactly one re-plan (2 PATCHes), got %d", n)
	}
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "failed" {
		t.Errorf("tag outcome = %v, want failed", tags)
	}
	if _, refused := lockTagsOf(t, root); refused != nil {
		t.Errorf("a race was recorded as a refusal: %+v", refused)
	}
}

// TestMetricTagsAdoptedArchivedMetricIsSkipped: a create refused as
// name_taken adopts the metric already holding the name — and when that row is
// archived or hidden its tags are left alone, as on any other push.
func TestMetricTagsAdoptedArchivedMetricIsSkipped(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	adopted := f.AddMetric("table-aov", "average_order", `{"source":"table-orders","value":"revenue","version":1}`)
	adopted.FeatureID = "collection-1"
	adopted.Archived = true
	f.nameTakenOnMetricCreate = `a table or metric called "average_order" already exists in this feature`

	out, stderr, _ := runPipelineCLI(t, root, "pipeline", "push", "--json")
	metric := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
	if metric["metricID"] != "table-aov" {
		t.Fatalf("the metric was not adopted: %v\n%s", metric, stderr)
	}
	if tags := tagOutcomeOf(t, metric); tags["outcome"] != "skipped_archived" {
		t.Errorf("tag outcome = %v, want skipped_archived", tags)
	}
	if len(f.tagPatches()) != 0 {
		t.Errorf("an archived adopted metric was PATCHed: %+v", f.tagPatches())
	}
}

// --- "Next: publish" ---------------------------------------------------------------

// TestPipelinePushSaysPublishOnlyAfterADraft: "Next: ronja pipeline publish" is
// the step after a push that STAGED something. A tag-only push staged nothing,
// and neither did one whose only outcome is a tag failure.
func TestPipelinePushSaysPublishOnlyAfterADraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	out, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(out, "Next: ronja pipeline publish") {
		t.Errorf("a push that staged a draft did not point at publish:\n%s", out)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}

	setTags(t, root, `["Finance","Sales"]`)
	out, _, err = runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(out, "Next: ronja pipeline publish") {
		t.Errorf("a tag-only push pointed at publish:\n%s", out)
	}

	id := metricIDOf(t, root)
	for i := range 19 {
		f.TagInWebApp(id, fmt.Sprintf("web-%02d", i))
	}
	setTags(t, root, `["Finance","Sales","Ops"]`)
	out, _, _ = runPipelineCLI(t, root, "pipeline", "push")
	if strings.Contains(out, "Next: ronja pipeline publish") {
		t.Errorf("a push whose only outcome is a tag refusal pointed at publish:\n%s", out)
	}
}

// --- naming a legacy folder carries the RECIPE record too ---------------------------

// TestNamingALegacyFolderCarriesTheMetricRecipeRecord: adoptLiveHashes carries a
// legacy folder's metric entries into the lock whole — so the first push after
// naming the stack neither re-stages an unchanged metric (the declared
// fingerprint came across) nor overwrites a colleague's live change unasked
// (the live fingerprint did).
func TestNamingALegacyFolderCarriesTheMetricRecipeRecord(t *testing.T) {
	t.Run("unchanged stages nothing", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		root := seedMetricFolder(t, f)
		seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
		bindPipelineTable(t, root, f.Key(), "orders.sql", "table-orders")

		out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "dev", "--json")
		if err != nil {
			t.Fatalf("push --stack: %v\n%s", err, stderr)
		}
		entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
		if entry["outcome"] != metricOutcomeUpToDate {
			t.Errorf("outcome = %v, want up to date", entry["outcome"])
		}
		if len(f.checkouts) != 0 || len(f.recipeWrites) != 0 {
			t.Errorf("naming the stack re-staged an unchanged metric: checkouts %d, writes %+v", len(f.checkouts), f.recipeWrites)
		}
	})
	t.Run("a live change is refused", func(t *testing.T) {
		f := newFakePipelineInstance(t)
		signInPipeline(t, f)
		root := seedMetricFolder(t, f)
		live := seedStagedMetric(t, f, root, api.MetricStatusUnvetted)
		bindPipelineTable(t, root, f.Key(), "orders.sql", "table-orders")
		live.MetricRecipe = json.RawMessage(`{"source":"table-orders","value":"orders / revenue","version":1}`)
		if err := wfdir.WriteFile(root, "metrics/average_order.json",
			metricFileBody(`{"source":"orders","value":"revenue * 2"}`)); err != nil {
			t.Fatalf("write: %v", err)
		}

		out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "dev", "--json")
		if err == nil {
			t.Fatal("a live definition that moved must refuse after naming the stack")
		}
		entry := decodeJSON(t, out)["metrics"].([]any)[0].(map[string]any)
		if entry["outcome"] != metricOutcomeRefused || !strings.Contains(fmt.Sprint(entry["error"]), "changed on the server") {
			t.Errorf("entry = %v", entry)
		}
		if len(f.recipeWrites) != 0 {
			t.Errorf("the live change was overwritten: %+v", f.recipeWrites)
		}
	})
}

// --- clone --------------------------------------------------------------------

// TestPipelineCloneWritesMetricTags: a clone writes the metric's tags into the
// file and records them as applied, so the folder reads as clean and a later
// removal from the file works. A metric with no tags gets no key; a failed read
// gets no key and a note — never `[]`, which would ASSERT no tags and strip
// every tag on the next push.
func TestPipelineCloneWritesMetricTags(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1",
		Code: "SELECT * FROM raw"})
	f.AddMetric("table-aov", "average_order", `{"value":"revenue","source":"table-orders","version":1}`)
	f.AddMetric("table-plain", "plain_metric", `{"value":"revenue","source":"table-orders","version":1}`)
	f.AddMetric("table-locked", "locked_metric", `{"value":"revenue","source":"table-orders","version":1}`)
	f.TagInWebApp("table-aov", "Finance")
	f.TagInWebApp("table-locked", "Secret")
	f.tags().failGet["table-locked"] = http.StatusForbidden

	dir := t.TempDir()
	out, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v\n%s\n%s", err, out, stderr)
	}
	root := filepath.Join(dir, "out")
	var file struct {
		Tags *[]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(readFile(t, root, "metrics/average_order.json")), &file); err != nil {
		t.Fatal(err)
	}
	if file.Tags == nil || strings.Join(*file.Tags, ",") != "Finance" {
		t.Errorf("tags = %v", file.Tags)
	}
	for _, path := range []string{"metrics/plain_metric.json", "metrics/locked_metric.json"} {
		if body := readFile(t, root, path); strings.Contains(body, `"tags"`) {
			t.Errorf("%s got a tags key:\n%s", path, body)
		}
	}
	if !strings.Contains(stderr, "locked_metric") || !strings.Contains(stderr, "tags") {
		t.Errorf("the failed tag read was not explained:\n%s", stderr)
	}
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, _ := lock.MetricTags("dev", "metrics/average_order.json"); appliedNames(applied) != "Finance" {
		t.Errorf("tagsApplied = %+v", applied)
	}
	// Clean on arrival: a push changes nothing.
	payload, _, err := func() (map[string]any, string, error) {
		o, e, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
		if err != nil {
			return nil, e, err
		}
		return decodeJSON(t, o), e, nil
	}()
	if err != nil {
		t.Fatalf("push after clone: %v", err)
	}
	if payload["upToDate"] != true || len(f.tagPatches()) != 0 {
		t.Errorf("a push straight after a clone changed tags: %v %+v", payload["upToDate"], f.tagPatches())
	}
}

// --- sync apply -----------------------------------------------------------------

// TestSyncApplyDeploysATagOnlyChange: a tag-only edit is a change `sync apply`
// deploys — without staging, building or publishing a draft.
func TestSyncApplyDeploysATagOnlyChange(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedTaggedStack(t, f, `["Finance"]`)
	pushTagged(t, root)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	id := metricIDOf(t, root)
	setTags(t, root, `["Finance","Sales"]`)

	// The tree verdict sees it as a local change first.
	tree := filepath.Dir(root)
	checkouts, syncs := len(f.checkouts), len(f.syncs)
	out, err := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v)\n%s", code, err, out)
	}
	if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance,Sales" {
		t.Errorf("sync apply did not deploy the tag change: %s\n%s", got, out)
	}
	if len(f.checkouts) != checkouts || len(f.syncs) != syncs {
		t.Errorf("a tag-only apply touched the draft lifecycle")
	}
	folder := applyFolderLine(t, applyReportOf(t, out), filepath.Base(root))
	if folder.Verdict != verdictApplied || !strings.Contains(folder.Detail, "tag") {
		t.Errorf("folder = %+v", folder)
	}
}

// TestSyncApplyPublishesTheSQLPastAFailedTagHalf: tags are not drafted and the
// SQL is, so a tag half that failed must not hold the folder's publish
// hostage — the health-checks precedent. The staged SQL is published; the
// folder's verdict is tags_not_applied, and the run still exits non-zero.
func TestSyncApplyPublishesTheSQLPastAFailedTagHalf(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped", taggedPath: taggedMetricBody(`["Finance"]`)},
		map[string]string{"orders.sql": "table-orders"}, nil)
	f.tags().denyScope[http.MethodPatch] = true

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" {
		t.Fatalf("a failed tag half held the SQL publish back: %q", f.CodeOf("table-orders"))
	}
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Verdict != verdictRefused || line.Reason != syncReasonTagsNotApplied ||
		!strings.Contains(line.Detail, "orders.sql") || !strings.Contains(line.Detail, taggedPath) {
		t.Fatalf("line = %+v", line)
	}
	if strings.Contains(err.Error(), "was not deployed") || !strings.Contains(err.Error(), "not fully deployed") {
		t.Errorf("summary = %v", err)
	}
}

// TestMetricTagsTwoEntriesForOneIDNeverRemoveAnAssertedName: the file listed a
// renamed tag under BOTH its old and its new name, so the record holds two
// entries for one id — in the file's order, either way round. Dropping either
// name must leave the tag on the metric, because the other name still asserts
// it; only dropping both removes it.
func TestMetricTagsTwoEntriesForOneIDNeverRemoveAnAssertedName(t *testing.T) {
	for _, order := range []string{`["Fin","Finance"]`, `["Finance","Fin"]`} {
		for _, keep := range []string{"Fin", "Finance"} {
			t.Run(order+" keep "+keep, func(t *testing.T) {
				f := newFakePipelineInstance(t)
				signInPipeline(t, f)
				root := seedTaggedStack(t, f, `["Fin"]`)
				pushTagged(t, root)
				id := metricIDOf(t, root)
				finID := f.TagIDOf("Fin")
				f.RenameTag("Fin", "Finance")
				setTags(t, root, order)
				pushTagged(t, root)
				if applied, _ := lockTagsOf(t, root); len(applied) != 2 {
					t.Fatalf("tagsApplied = %+v, want two entries for %s", applied, finID)
				}
				before := len(f.tagPatches())

				setTags(t, root, `["`+keep+`"]`)
				pushTagged(t, root)
				if got := strings.Join(f.TagNamesOn(id), ","); got != "Finance" {
					t.Errorf("tags = %q, want the renamed tag kept", got)
				}
				for _, p := range f.tagPatches()[before:] {
					if slices.Contains(p.RemoveIDs, finID) {
						t.Errorf("a name the file still lists was removed: %+v", p)
					}
				}
				setTags(t, root, `[]`)
				pushTagged(t, root)
				if got := f.TagNamesOn(id); len(got) != 0 {
					t.Errorf("dropping both names left %v", got)
				}
			})
		}
	}
}

// --- the predicate, directly ------------------------------------------------------

// TestPlanMetricTags pins the rule table the harness tests above walk through
// one server at a time.
func TestPlanMetricTags(t *testing.T) {
	tag := func(id, name string) api.Tag { return api.Tag{ID: id, Name: name} }
	rec := func(id, name string) wfdir.AppliedTag { return wfdir.AppliedTag{ID: id, Name: name} }
	tests := []struct {
		name        string
		file        []string
		live        []api.Tag
		applied     []wfdir.AppliedTag
		adds        string
		removes     string
		renamed     string
		wantPending bool
	}{
		{name: "satisfied across case", file: []string{"finance"}, live: []api.Tag{tag("t1", "Finance")}},
		{name: "missing is an add", file: []string{"Sales"}, adds: "Sales", wantPending: true},
		{name: "no record removes nothing", file: []string{}, live: []api.Tag{tag("t1", "Legacy")}},
		{name: "web-app tag is never removed", file: []string{"Finance"},
			live: []api.Tag{tag("t1", "Finance"), tag("t2", "Board KPI")}, applied: []wfdir.AppliedTag{rec("t1", "Finance")}},
		{name: "a dropped recorded name is removed by id", file: []string{},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Finance")},
			removes: "Finance", wantPending: true},
		{name: "a recorded id already gone needs nothing", file: []string{},
			applied: []wfdir.AppliedTag{rec("t1", "Finance")}},
		{name: "a rename is kept and reported", file: []string{"Fin"},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin")}, renamed: "Fin → Finance"},
		{name: "a renamed tag the file dropped is removed under its new name", file: []string{},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin")},
			removes: "Finance", wantPending: true},
		{name: "the file updated after a rename keeps it", file: []string{"Finance"},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin")}},
		{name: "a web-app removal of a folder name is re-added", file: []string{"Sales"},
			applied: []wfdir.AppliedTag{rec("t1", "Sales")}, adds: "Sales", wantPending: true},
		// Two entries for ONE id — the file listed a tag's old and new name. An
		// id is removed only when NO entry for it is still named by the file, in
		// whatever order the entries were recorded.
		{name: "two entries for one id, the file keeps the old name", file: []string{"Fin"},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin"), rec("t1", "Finance")},
			renamed: "Fin → Finance"},
		{name: "two entries for one id, reversed, the file keeps the old name", file: []string{"Fin"},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Finance"), rec("t1", "Fin")},
			renamed: "Fin → Finance"},
		{name: "two entries for one id, the file keeps the new name", file: []string{"Finance"},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin"), rec("t1", "Finance")}},
		{name: "two entries for one id, both dropped, removed once", file: []string{},
			live: []api.Tag{tag("t1", "Finance")}, applied: []wfdir.AppliedTag{rec("t1", "Fin"), rec("t1", "Finance")},
			removes: "Finance", wantPending: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := planMetricTags(tc.file, tc.live, tc.applied)
			if got := strings.Join(plan.Adds, ","); got != tc.adds {
				t.Errorf("adds = %q, want %q", got, tc.adds)
			}
			if got := strings.Join(plan.removeNamesList(), ","); got != tc.removes {
				t.Errorf("removes = %q, want %q", got, tc.removes)
			}
			if got := strings.Join(plan.Renamed, ","); got != tc.renamed {
				t.Errorf("renamed = %q, want %q", got, tc.renamed)
			}
			if plan.pending() != tc.wantPending {
				t.Errorf("pending = %v", plan.pending())
			}
		})
	}
}
