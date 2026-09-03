package wfdir

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// v3Manifest is a folder that declares dependencies and binds them per stack —
// the shape this whole slice exists to make committable.
const v3Manifest = `{"formatVersion":3,"kind":"workflow","title":"Monthly report","entrypoint":"main.py","dependencies":{"acme_api":{"kind":"secret"},"orders":{"kind":"table"}},"stacks":{"dev":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-dev","bind":{"acme_api":"secret-dev","orders":"table-dev"}},"prod":{"url":"https://app.ronja.tech","tenantID":"ten-acme","featureID":"collection-prod","bind":{"acme_api":"secret-prod","orders":"table-prod"}}}}`

// TestV3ManifestRoundTripsByteForByte: `dependencies` and `bind` are committed
// config that no deploy writes, so a load and a save must leave them exactly as
// the author typed them — including their position, which is what keeps a
// colleague's pull from showing a diff nobody made.
func TestV3ManifestRoundTripsByteForByte(t *testing.T) {
	body := canonicalManifest(t, v3Manifest)
	if got := loadSave(t, body); got != body {
		t.Fatalf("a v3 manifest must round-trip unchanged\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestDependenciesStampVersionThree is the format gate this slice adds, and the
// two halves of it that are easy to get wrong.
//
// The version has to be stamped by the CONTENT — a folder that declares a
// dependency is one an older CLI must refuse, because that CLI would send the
// alias to the server as a literal id. And it must NOT be stamped by a folder
// that declares none: bumping the constant is not licence to push every
// stacks-only folder in the field onto a newer CLI.
func TestDependenciesStampVersionThree(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"declares a dependency", `{"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table"}},"instances":[]}`, 3},
		{"stacks only", `{"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"prod":{"url":"http://a","tenantID":"t1"}}}`, 2},
		{"legacy folder", `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t, tc.body))
			m, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := SaveManifest(root, m); err != nil {
				t.Fatalf("save: %v", err)
			}
			raw, err := os.ReadFile(ManifestPath(root))
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				FormatVersion int `json:"formatVersion"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if probe.FormatVersion != tc.want {
				t.Errorf("formatVersion = %d, want %d:\n%s", probe.FormatVersion, tc.want, raw)
			}
			// Stamped FIRST, not appended: it is the first thing anybody opening
			// the file needs to know, and it is what an older CLI's gate reads.
			if tc.want > 0 && !strings.HasPrefix(string(raw), "{\n  \"formatVersion\"") {
				t.Errorf("formatVersion must lead the file:\n%s", raw)
			}
		})
	}
}

// TestUnknownKeyInsideADependencySurvivesRewrite: a dependency is an object in a
// committed file, so it needs its own sidecar. Without one, a colleague's newer
// CLI writes a key here — a description, a default — and this build deletes it
// on the next push with no message anywhere.
func TestUnknownKeyInsideADependencySurvivesRewrite(t *testing.T) {
	body := canonicalManifest(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table","description":"the order lines","optional":true}},"instances":[]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("an unknown key inside a dependency must survive a rewrite\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestDependencyKeysCoverEveryField is the sidecar's silent failure, guarded the
// way the other three levels are: a field whose key is missing from the known
// set is captured as "unknown" and then re-emitted from the STALE captured copy,
// so every write of it is reverted by the very layer meant to protect it.
func TestDependencyKeysCoverEveryField(t *testing.T) {
	if !dependencyKeys["kind"] {
		t.Errorf("dependencyKeys is missing %q", "kind")
	}
	if len(dependencyKeys) != reflect.TypeOf(Dependency{}).NumField()-1 {
		t.Errorf("dependencyKeys has %d keys for %d exported fields — a new field needs a case above", len(dependencyKeys), reflect.TypeOf(Dependency{}).NumField()-1)
	}
}

// TestSelectionCarriesTheBindMap: a command reads one shape, whichever of the
// two the folder is written in.
func TestSelectionCarriesTheBindMap(t *testing.T) {
	t.Run("named stack", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t, v3Manifest))
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		sel, err := m.Select(&Lock{}, InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}, "")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if got := sel.Bind["orders"]; got != "table-prod" {
			t.Errorf("prod bind for orders = %q, want table-prod", got)
		}
	})
	t.Run("legacy entry", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table"}},"instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"collection-1","bind":{"orders":"table-legacy"}}]}`))
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		sel, err := m.Select(&Lock{}, InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}, "")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if got := sel.Bind["orders"]; got != "table-legacy" {
			t.Errorf("legacy bind for orders = %q, want table-legacy", got)
		}
	})
}

// TestBindSurvivesADiscard is the hazard that decided where Bind lives.
//
// SetBinding replaces an entry from a caller-supplied Binding, and `wf discard`
// hands in a freshly built one carrying nothing but a featureID. A bind map
// reachable through that value would be erased — a human's committed alias
// bindings deleted because a machine's struct happened not to carry them.
func TestBindSurvivesADiscard(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	write(t, root, ManifestName, canonicalManifest(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table"}},"instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"collection-1","bind":{"orders":"table-legacy"}}]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.SetBinding(key, Binding{FeatureID: "collection-1"})
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry, ok, err := reloaded.BindingEntry(key)
	if err != nil || !ok {
		t.Fatalf("expected the entry to survive: ok=%v err=%v", ok, err)
	}
	if entry.WorkflowID != "" {
		t.Errorf("discard must clear the workflow id, got %q", entry.WorkflowID)
	}
	if got := entry.Bind["orders"]; got != "table-legacy" {
		t.Errorf("discard must not touch the bind map, got %q", got)
	}
}

// TestNamingAStackMigratesTheBind: naming a legacy entry moves it into stacks,
// and its alias bindings are config about that same (instance, organization) —
// so they move with it rather than being dropped by the one rewrite that
// migrates the folder.
func TestNamingAStackMigratesTheBind(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	write(t, root, ManifestName, canonicalManifest(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table"}},"instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"collection-1","bind":{"orders":"table-legacy"}}]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lock := &Lock{}
	sel, err := m.Select(lock, key, "prod")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	m.Record(lock, sel, sel.Binding)
	if got := m.Stacks["prod"].Bind["orders"]; got != "table-legacy" {
		t.Errorf("the migrated stack's bind for orders = %q, want table-legacy", got)
	}
	if len(m.Instances) != 0 {
		t.Errorf("the legacy entry must be dropped, got %+v", m.Instances)
	}
}

// TestValidAliasName covers the two rules that exist only for aliases, because
// each is a silently different resource rather than a cosmetic complaint.
func TestValidAliasName(t *testing.T) {
	for _, ok := range []string{"orders", "acme_api", "v2.orders", "order-lines", "a", "x9"} {
		if err := ValidAliasName(ok); err != nil {
			t.Errorf("ValidAliasName(%q) = %v, want it accepted", ok, err)
		}
	}
	cases := []struct{ name, wantErr string }{
		{"", "cannot be empty"},
		{strings.Repeat("a", 65), "characters"},
		{"orders table", "letters, digits"},
		{"orders/lines", "letters, digits"},
		{"_orders", "must start with a letter or a digit"},
		{"-orders", "must start with a letter or a digit"},
		// The positional-ref grammar. `{{ ref('0') }}` is an INDEX, and it is
		// what the web app writes, so an all-digit alias is a name that reads as
		// an index to everything downstream.
		{"0", "already means something else"},
		{"42", "already means something else"},
		// Id-shaped, for any kind — it would resolve to itself.
		{"table-orders", "shaped like a table id"},
		{"secret-acme", "shaped like a secret id"},
		{"cdx-policies", "shaped like a codex id"},
		{"modelv2-old", "shaped like a table id"},
	}
	for _, tc := range cases {
		err := ValidAliasName(tc.name)
		if err == nil {
			t.Errorf("ValidAliasName(%q) must be refused", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidAliasName(%q) = %v, want it to say %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestCheckDependenciesRefusals: the per-dependency rules that are enforced at
// LOAD, because this build writes both, neither can be produced by merging two
// individually-valid halves, and both make the declaration unresolvable.
func TestCheckDependenciesRefusals(t *testing.T) {
	cases := []struct{ name, deps, wantErr string }{
		{"an untypeable alias", `{"order lines":{"kind":"table"}}`, "letters, digits"},
		{"an all-digit alias", `{"0":{"kind":"table"}}`, "already means something else"},
		{"an id-shaped alias", `{"table-orders":{"kind":"table"}}`, "shaped like a table id"},
		{"no kind at all", `{"orders":{}}`, `no "kind"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t,
				`{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":`+tc.deps+`,"instances":[]}`))
			_, err := LoadManifest(root, WorkflowKind)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal = %v, want it to say %q", err, tc.wantErr)
			}
		})
	}
}

// TestAnUnknownKindLoadsAndIsRefusedAtAcceptance is the trade, both halves.
//
// A kind is the one declaration a NEWER CLI can legitimately write without
// moving the format version — adding a kind adds no key, so the version gate
// never fires on it. Refused at load, that folder is unopenable by `status`,
// which is the command its owner needs most; refused nowhere, its alias reaches
// the server as a literal id and a secret marker soft-fails it into a warning.
// So it loads, and the push refuses.
func TestAnUnknownKindLoadsAndIsRefusedAtAcceptance(t *testing.T) {
	for _, kind := range []string{
		"dashboard",
		// `file` is deliberately not a dependency kind: the marker's first
		// argument is a store-relative folder path, not a `file-` id.
		"file",
	} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t,
				`{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"`+kind+`"}},"instances":[]}`))
			m, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("a folder written by a newer CLI must still open: %v", err)
			}
			err = m.CheckDeclarations()
			if err == nil {
				t.Fatal("expected an acceptance refusal")
			}
			if !strings.Contains(err.Error(), "resolves only") {
				t.Errorf("refusal = %v, want it to list the kinds this build resolves", err)
			}
		})
	}
}

// TestAnAliasShapedLikeAMarkerlessIDLoadsAndIsRefusedAtAcceptance is the
// regression for the thing adding a dependency kind can break BACKWARDS.
//
// ValidAliasName refuses an id-shaped alias, and it runs at LOAD. The set of
// kinds it scans GROWS: `note` joined it carrying the `note-` and `skill-`
// prefixes, and had that scan been markers.Kinds(), an alias called
// `skill-cache` — legal in every build before it, sitting in a committed
// manifest — would have stopped loading. Not stopped pushing: stopped LOADING,
// so `status` would refuse too and hand-editing a committed file would be the
// only way out. That is verbatim
// the invariant checkStacks states: THE CLI MUST NEVER WRITE A MANIFEST IT WILL
// THEN REFUSE TO LOAD.
//
// So both halves are pinned here: the folder opens, and the push refuses.
func TestAnAliasShapedLikeAMarkerlessIDLoadsAndIsRefusedAtAcceptance(t *testing.T) {
	for _, tc := range []struct{ alias, kind, wantErr string }{
		{"note-x", "note", "shaped like a note id"},
		// The legacy prefix is the sharper case: `skill-` names nothing in this
		// folder's vocabulary and reads as an ordinary word with a dash.
		{"skill-cache", "note", "shaped like a note id"},
	} {
		t.Run(tc.alias, func(t *testing.T) {
			// ValidAliasName itself must still accept it — it is the load-time
			// half, and widening it is the mistake.
			if err := ValidAliasName(tc.alias); err != nil {
				t.Errorf("ValidAliasName(%q) = %v; the load-time check must not scan a marker-less kind's prefixes", tc.alias, err)
			}
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t,
				`{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"`+tc.alias+`":{"kind":"`+tc.kind+`"}},"instances":[]}`))
			m, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("a manifest committed before `note` was a kind must still open: %v", err)
			}
			err = m.CheckDeclarations()
			if err == nil {
				t.Fatal("expected an acceptance refusal — the name still resolves to itself")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal = %v, want it to say %q", err, tc.wantErr)
			}
		})
	}

	// ⚠️ AND THE CROSS-KIND CASE IS NOT REFUSED AT ALL. A workflow folder
	// committed months ago whose `table` alias is called `note-taking` predates
	// `note` being a kind, and nothing about it stopped working when `note` was
	// added: resolution is keyed by (kind, name), so `{{ ref('note-taking') }}`
	// looks the name up under `table` and finds it. Refusing it would break a
	// folder on an edit nobody made, which is the failure this acceptance/load
	// split exists to avoid.
	t.Run("a marker kind's alias named like a marker-less id still pushes", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t,
			`{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"note-taking":{"kind":"table"}},"instances":[]}`))
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if err := m.CheckDeclarations(); err != nil {
			t.Errorf("CheckDeclarations() = %v; a `table` alias named `note-taking` resolves fine and must not be refused", err)
		}
	})

	// And a MARKER kind's id shape stays exactly where it was: at load. Moving
	// it would be the opposite mistake — the folder opens, `status` reports, and
	// nothing refuses until a push that is not always the next thing to run.
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"table-orders":{"kind":"table"}},"instances":[]}`))
	if _, err := LoadManifest(root, WorkflowKind); err == nil {
		t.Error("an alias shaped like a TABLE id must still be refused at load")
	}
}

// TestDependenciesBelowVersionThreeAreRefusedAtAcceptance.
//
// The pair is one this build cannot write — MarshalJSON stamps 3 on any manifest
// carrying a dependency — so it can only be hand-authored, which is exactly what
// a reader copying the JSON out of the docs produces. It has to be refused
// somewhere: the file MEANS v3 while DECLARING that a v1 or v2 CLI may read it,
// so the version gate that exists to stop an older CLI misreading an alias waves
// that CLI straight through, and it sends the alias to the server as a literal
// secret id, which soft-fails into a warning. Green push, nothing bound.
//
// At ACCEPTANCE, not at load, on checkStacks's discipline: refusing at load
// would make the folder unopenable by `status` rather than un-pushable.
func TestDependenciesBelowVersionThreeAreRefusedAtAcceptance(t *testing.T) {
	for _, declared := range []string{"", `"formatVersion":1,`, `"formatVersion":2,`} {
		t.Run("declared "+declared, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName,
				`{`+declared+`"kind":"workflow","title":"x","entrypoint":"main.py","dependencies":{"orders":{"kind":"table"}},"instances":[]}`)
			m, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("the folder must still open: %v", err)
			}
			err = m.CheckDeclarations()
			if err == nil {
				t.Fatal("expected an acceptance refusal")
			}
			// The fix is one line, and the message has to quote it.
			if !strings.Contains(err.Error(), `"formatVersion": 3`) {
				t.Errorf("refusal = %v, want it to name the line to add", err)
			}
		})
	}
}

// TestAVersionThreeManifestPassesTheDeclarationCheck is the other half: nothing
// this build writes is refused by the rule above.
func TestAVersionThreeManifestPassesTheDeclarationCheck(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Kind: KindWorkflow, Title: "x", Entrypoint: "main.py",
		Dependencies: map[string]Dependency{"orders": {Kind: "table"}}}
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := loaded.CheckDeclarations(); err != nil {
		t.Errorf("a manifest this build just wrote was refused: %v", err)
	}
}

// TestIncoherentBindStillLoads is the boundary checkDependencies stops at, and
// it is the invariant rather than a leniency: THE CLI MUST NEVER WRITE A
// MANIFEST IT WILL THEN REFUSE TO LOAD. Removing a dependency leaves every
// stack's bind for it behind, and a git merge can pair one branch's
// declarations with another's binds — so refusing at load would make a folder
// unopenable by `status` and by an explicit --stack that resolves perfectly
// well. The refusal is CheckBind's, at the point a command acts.
func TestIncoherentBindStillLoads(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"prod":{"url":"https://app.ronja.tech","tenantID":"ten-acme","bind":{"orders":"table-1"}}}}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("a bind with no declaration must still LOAD: %v", err)
	}
	sel, err := m.Select(&Lock{}, InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}, "prod")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if err := m.CheckBind(sel); err == nil {
		t.Error("CheckBind must refuse a bind with no declaration")
	}
}

// TestCheckBindRefusals: the three cross-object rules, each named for what it
// prevents rather than for the shape it matches.
func TestCheckBindRefusals(t *testing.T) {
	deps := map[string]Dependency{
		"orders":   {Kind: "table"},
		"lines":    {Kind: "table"},
		"acme_api": {Kind: "secret"},
	}
	cases := []struct {
		name    string
		bind    map[string]string
		wantErr string
	}{
		{"declared and bound", map[string]string{"orders": "table-1", "acme_api": "secret-1"}, ""},
		{"nothing bound", nil, ""},
		// A typo. Inert on its own, which is exactly why it has to be said out
		// loud: unreported, the reader sees only "orders has no binding here"
		// and adds a second entry beside the one they meant to fix.
		{"a bind with no declaration", map[string]string{"order": "table-1"}, "declares no dependency by that name"},
		// The value is substituted into the source and sent as a real id.
		{"a bind that is not an id", map[string]string{"orders": "the orders table"}, "not a table id"},
		{"a bind of the wrong kind", map[string]string{"acme_api": "table-1"}, "not a secret id"},
		// Ambiguous in the id → alias direction, which is what any reverse
		// mapping needs.
		{"two aliases on one id", map[string]string{"orders": "table-1", "lines": "table-1"}, "binds both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Dependencies: deps}
			err := m.CheckBind(Selection{Name: "prod", Bind: tc.bind})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("expected no refusal, got %v", err)
			case tc.wantErr == "":
			case err == nil:
				t.Errorf("expected a refusal saying %q", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("refusal = %v, want it to say %q", err, tc.wantErr)
			case !strings.Contains(err.Error(), `stack "prod"`):
				t.Errorf("refusal = %v, want it to name the stack", err)
			}
		})
	}
	// "two aliases on one id" must not depend on map iteration order: the same
	// broken folder has to name the same pair every run, or a CI log and a
	// developer's terminal disagree about what is wrong.
	m := &Manifest{Dependencies: deps}
	first := m.CheckBind(Selection{Bind: map[string]string{"orders": "table-1", "lines": "table-1"}})
	for range 20 {
		if got := m.CheckBind(Selection{Bind: map[string]string{"orders": "table-1", "lines": "table-1"}}); got.Error() != first.Error() {
			t.Fatalf("refusal is not deterministic:\n%v\n%v", first, got)
		}
	}
}

// TestCheckAliasCollisions: a pipeline folder resolves a ref against a sibling
// file first, so a declaration of the same name is dead text that reads as if it
// were in force — somebody binds it in every stack and the code goes on reading
// the local file.
func TestCheckAliasCollisions(t *testing.T) {
	deps := map[string]Dependency{"orders": {Kind: "table"}, "acme_api": {Kind: "secret"}}
	if err := CheckAliasCollisions(deps, []string{"totals", "regions"}); err != nil {
		t.Errorf("no collision, got %v", err)
	}
	if err := CheckAliasCollisions(nil, []string{"orders"}); err != nil {
		t.Errorf("no declarations, got %v", err)
	}
	err := CheckAliasCollisions(deps, []string{"totals", "orders"})
	if err == nil {
		t.Fatal("a declaration colliding with a sibling file must be refused")
	}
	if !strings.Contains(err.Error(), `"orders"`) {
		t.Errorf("refusal = %v, want it to name the collision", err)
	}
}

// TestCheckAliasCollisionsFoldsCase: `Orders.sql` and a dependency called
// `orders` are the SAME collision, arrived at through a capital letter.
//
// `ronja bind` answers a declaration with strings.EqualFold against a hit's
// title — it has to, because the search endpoint's own exact-match test is ILIKE
// — so the declaration binds to the very row `Orders.sql` created, and one word
// then names one row twice while ronja.json asserts the two are different. A
// byte-for-byte comparison waves exactly that through.
func TestCheckAliasCollisionsFoldsCase(t *testing.T) {
	deps := map[string]Dependency{"orders": {Kind: "table"}}
	err := CheckAliasCollisions(deps, []string{"Orders"})
	if err == nil {
		t.Fatal("a declaration colliding with a sibling file's stem in another case must be refused")
	}
	// Both spellings, because the reader has to find a file called `Orders` and a
	// declaration called `orders`, and a message naming one of them twice sends
	// them looking for the wrong thing.
	if !strings.Contains(err.Error(), `"orders"`) || !strings.Contains(err.Error(), `"Orders"`) {
		t.Errorf("refusal = %v, want it to name both spellings", err)
	}
}
