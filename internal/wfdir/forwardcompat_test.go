package wfdir

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// withoutSidecars is a manifest with the forward-compatibility bookkeeping
// stripped, for tests comparing DECLARED content. A manifest built in code has
// read no file and therefore remembers no key order, so comparing the sidecar
// would only ever assert which of the two came off disk.
func withoutSidecars(m *Manifest) *Manifest {
	if m == nil {
		return nil
	}
	clean := *m
	clean.unknown = unknownKeys{}
	clean.Instances = make([]Instance, len(m.Instances))
	for i, entry := range m.Instances {
		entry.unknown = unknownKeys{}
		clean.Instances[i] = entry
	}
	return &clean
}

// canonicalManifest renders compact JSON exactly as SaveManifest would write
// it, so a byte comparison in these tests is about keys and their order rather
// than about whitespace nobody typed.
func canonicalManifest(t *testing.T, compact string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(compact), "", "  "); err != nil {
		t.Fatalf("indent fixture: %v", err)
	}
	buf.WriteByte('\n')
	return buf.String()
}

// loadSave is the whole hazard in one line: read a committed manifest with this
// build, write it back, and report what the file now says.
func loadSave(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, ManifestName, body)
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(raw)
}

// TestUnknownRootKeySurvivesRewrite is the bug this file exists for. A manifest
// carrying a key the running CLI has never heard of must come back out of a
// rewrite with the same data in the same place — push, discard and clone all
// rewrite it, so anything else means a stale CLI deletes a colleague's
// declaration with no message.
//
// The fixtures below all round-trip byte for byte, which is the stronger claim
// and worth asserting, but it is not the contract: values are re-encoded, so a
// character the JSON encoder escapes comes back in its escaped spelling. See
// TestHTMLCharactersInAPreservedValueAreEscaped.
func TestUnknownRootKeySurvivesRewrite(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"Monthly report","entrypoint":"main.py","description":"the one that mails finance","instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"feat-1"}]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("an unknown root key must survive a rewrite\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestUnknownInstanceKeySurvivesRewrite: an entry is one flattened object —
// Instance's own keys and the embedded Binding's — so the sidecar has to sit at
// that level too. This is the case a root-only fix would silently miss.
func TestUnknownInstanceKeySurvivesRewrite(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"Monthly report","entrypoint":"main.py","instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","bind":{"orders":"table-1"},"featureID":"feat-1","headVersionID":"wfv-9"}]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("an unknown instance key must survive a rewrite\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestUnknownKeysAtBothLevelsSurviveRewrite: the two sidecars are independent,
// and a manifest from a later slice will carry keys at both levels at once.
func TestUnknownKeysAtBothLevelsSurviveRewrite(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"Monthly report","entrypoint":"main.py","description":"the one that mails finance","instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"feat-1","headVersionID":"wfv-9"},{"url":"http://localhost:8098","tenantID":"development","featureID":"feat-local","note":"the laptop one"}]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("unknown keys at both levels must survive a rewrite\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestUnknownKeyValuesAreOpaque: an unknown key's value is carried verbatim,
// whatever shape it has. The sidecar must not model what it does not know.
func TestUnknownKeyValuesAreOpaque(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","futureNull":null,"futureList":[1,"two",{"three":true}],"futureNumber":1.50,"instances":[]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("unknown values must be carried verbatim\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestUnknownKeySurvivesSetBinding: the rewrite that actually happens. `push`
// records a binding and `discard` resets one to just its feature — the second
// hands SetBinding a FRESHLY BUILT Binding, so preservation cannot live on the
// value the caller passes and has to be carried by the entry it replaces.
func TestUnknownKeySurvivesSetBinding(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1","featureID":"feat-1","headVersionID":"wfv-9"}]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Exactly what `wf discard` does: forget the workflow, keep the feature.
	m.SetBinding(key, Binding{FeatureID: "feat-1"})
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry, ok, err := reloaded.BindingEntry(key)
	if err != nil || !ok {
		t.Fatalf("expected the binding to survive: ok=%v err=%v", ok, err)
	}
	if entry.WorkflowID != "" {
		t.Errorf("discard must clear the workflow id, got %q", entry.WorkflowID)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"headVersionID"`) {
		t.Errorf("replacing an entry must keep its unknown keys:\n%s", raw)
	}
}

// TestClearedKnownKeyIsNotResurrected is the sidecar's own failure mode, and it
// is the dangerous direction: if a captured value were replayed for a key the
// struct DOES own, clearing an omitempty field would be silently undone and the
// folder would go on declaring something the author removed.
func TestClearedKnownKeyIsNotResurrected(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","reportingTimezone":"Europe/Stockholm","runtime":2,"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.ReportingTimezone = nil
	m.Runtime = 0
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reportingTimezone") || strings.Contains(string(raw), "runtime") {
		t.Errorf("a cleared known key must be dropped, not restored from the sidecar:\n%s", raw)
	}
}

// TestNewlyWrittenKeyIsAppended: a key the file did not have goes on the end
// rather than being dropped for want of a remembered position.
func TestNewlyWrittenKeyIsAppended(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","future":1,"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.SetReportingTimezone("Europe/Stockholm")
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.DeclaredReportingTimezone(); got != "Europe/Stockholm" {
		t.Errorf("declared zone = %q, want Europe/Stockholm", got)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"future"`) {
		t.Errorf("the unknown key must survive a write that adds a known one:\n%s", raw)
	}
	// And the file must be STABLE: a second load/save changes nothing, so a
	// folder does not churn a line of its committed manifest per command.
	if got := loadSave(t, string(raw)); got != string(raw) {
		t.Errorf("a second rewrite must be a no-op\n got:\n%s\nwant:\n%s", got, raw)
	}
}

// TestAbsentFormatVersionMeansOne: every manifest committed before the key
// existed has none, so absent must load cleanly, read as version 1, and — the
// part that keeps this change free of churn in customers' repos — must not be
// stamped by the rewrite.
func TestAbsentFormatVersionMeansOne(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`)
	root := t.TempDir()
	write(t, root, ManifestName, body)
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("a manifest with no formatVersion must load: %v", err)
	}
	if m.FormatVersion != 0 {
		t.Errorf("absent formatVersion decoded as %d, want the zero value that means 1", m.FormatVersion)
	}
	if got := loadSave(t, body); got != body {
		t.Fatalf("the rewrite must not stamp formatVersion\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestDeclaredFormatVersionIsPreserved: a file that DOES state version 1 keeps
// saying so. The CLI never writes the key, but it must never delete one either.
func TestDeclaredFormatVersionIsPreserved(t *testing.T) {
	body := canonicalManifest(t, `{"formatVersion":1,"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`)
	if got := loadSave(t, body); got != body {
		t.Fatalf("a stated formatVersion must survive\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestFutureFormatVersionIsRefused: the escape hatch. A version above what this
// build understands is refused with something actionable, and — the point of
// refusing rather than reading — the file on disk is left exactly as it was.
func TestFutureFormatVersionIsRefused(t *testing.T) {
	root := t.TempDir()
	body := canonicalManifest(t, `{"formatVersion":99,"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`)
	write(t, root, ManifestName, body)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected a manifest from the future to be refused")
	}
	if !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("refusal = %v, want one naming the remedy", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Errorf("a refused manifest must be left untouched:\n%s", raw)
	}
}

// TestFutureFormatVersionRefusalBeatsTheLegacyMigrationMessage: the gate runs
// before the typed decode on purpose. A future format that reshapes "instances"
// would otherwise hit the legacy-object message and send its author to rewrite a
// file that is not old but NEW.
func TestFutureFormatVersionRefusalBeatsTheLegacyMigrationMessage(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"formatVersion":99,"kind":"workflow","title":"x","instances":{"https://app.ronja.tech":{"workflowID":"wf-1"}}}`)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "format version 99") {
		t.Errorf("refusal = %v, want the format-version one rather than the legacy-instances migration", err)
	}
}

// TestFutureFormatVersionRefusalIsNotShadowedByKind: the version gate outranks
// the kind gate, because a file this build cannot read is not a file it can
// classify. Both refuse, so nothing is destroyed either way — this is about
// which sentence the author gets.
func TestFutureFormatVersionRefusalIsNotShadowedByKind(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"formatVersion":99,"kind":"dataapp","title":"x","instances":[]}`)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "format version 99") {
		t.Errorf("refusal = %v, want the format-version one", err)
	}
}

// TestLegacyInstancesObjectStillNamesItsFix: the migration message for the
// pre-list "instances" object survives the decode being routed through
// Manifest.UnmarshalJSON. It used to be recognised by a substring naming the Go
// struct, which the plainManifest twin silently changed — leaving a bare
// `cannot unmarshal object into []Instance` where a rewrite instruction used to
// be.
func TestLegacyInstancesObjectStillNamesItsFix(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"x","instances":{"https://app.ronja.tech":{"workflowID":"wf-1"}}}`)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected an old-style instances object to be refused")
	}
	if !strings.Contains(err.Error(), "old-style") || !strings.Contains(err.Error(), "wf init") {
		t.Errorf("refusal = %v, want the migration message naming the caller's own init command", err)
	}
}

// TestThreeStatePointersSurvivePreservation: parameters, reportingTimezone and
// access each say three things, and the sidecar sits between the file and the
// struct on every write. Absent, empty and populated must still be three
// distinguishable states after a round trip through it.
func TestThreeStatePointersSurvivePreservation(t *testing.T) {
	cases := []struct {
		name string
		// body is the committed manifest, always carrying an unknown key so the
		// merge path is the one under test.
		body               string
		wantManagesParams  bool
		wantParams         int
		wantManagesZone    bool
		wantZone           string
		wantManagesAccess  bool
		wantAccessTableIDs int
	}{
		{
			name: "absent",
			body: `{"kind":"workflow","title":"x","entrypoint":"main.py","future":true,"instances":[]}`,
		},
		{
			name:              "empty declarations",
			body:              `{"kind":"workflow","title":"x","entrypoint":"main.py","future":true,"parameters":[],"reportingTimezone":"","access":{"allowedTableIDs":[],"allowedSecretIDs":[],"allowedAgentIDs":[],"allowedWorkflowIDs":[],"allowedCodexIDs":[],"allowedMetricIDs":[],"capabilities":[]},"instances":[]}`,
			wantManagesParams: true,
			wantManagesZone:   true,
			wantManagesAccess: true,
		},
		{
			name:               "populated",
			body:               `{"kind":"workflow","title":"x","entrypoint":"main.py","future":true,"parameters":[{"name":"region","label":"Region","type":"string","required":false}],"reportingTimezone":"Europe/Stockholm","access":{"allowedTableIDs":["table-1"],"allowedSecretIDs":[],"allowedAgentIDs":[],"allowedWorkflowIDs":[],"allowedCodexIDs":[],"allowedMetricIDs":[],"capabilities":[]},"instances":[]}`,
			wantManagesParams:  true,
			wantParams:         1,
			wantManagesZone:    true,
			wantZone:           "Europe/Stockholm",
			wantManagesAccess:  true,
			wantAccessTableIDs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := canonicalManifest(t, tc.body)
			root := t.TempDir()
			write(t, root, ManifestName, body)
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
			if string(raw) != body {
				t.Fatalf("rewrite changed the file\n got:\n%s\nwant:\n%s", raw, body)
			}
			out, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if out.ManagesParameters() != tc.wantManagesParams {
				t.Errorf("ManagesParameters = %v, want %v", out.ManagesParameters(), tc.wantManagesParams)
			}
			if got := len(out.DeclaredParameters()); got != tc.wantParams {
				t.Errorf("declared parameters = %d, want %d", got, tc.wantParams)
			}
			if out.ManagesReportingTimezone() != tc.wantManagesZone {
				t.Errorf("ManagesReportingTimezone = %v, want %v", out.ManagesReportingTimezone(), tc.wantManagesZone)
			}
			if got := out.DeclaredReportingTimezone(); got != tc.wantZone {
				t.Errorf("declared zone = %q, want %q", got, tc.wantZone)
			}
			if out.ManagesAccess() != tc.wantManagesAccess {
				t.Errorf("ManagesAccess = %v, want %v", out.ManagesAccess(), tc.wantManagesAccess)
			}
			if got := len(out.DeclaredAccess().AllowedTableIDs); got != tc.wantAccessTableIDs {
				t.Errorf("declared access tableIDs = %d, want %d", got, tc.wantAccessTableIDs)
			}
		})
	}
}

// TestSetterThreeStatesAreUnaffected: the setters are the other half of the
// three-state contract, and they run on manifests that were loaded (sidecar
// populated) as often as on ones built in code.
func TestSetterThreeStatesAreUnaffected(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","future":true,"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.SetParameters(nil)
	m.SetReportingTimezone("")
	m.SetAccess(api.DataAppAccess{})
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !out.ManagesParameters() || !out.ManagesReportingTimezone() || !out.ManagesAccess() {
		t.Fatalf("an empty declaration must read back as MANAGED, not absent: %+v", out)
	}
	if len(out.DeclaredParameters()) != 0 || out.DeclaredReportingTimezone() != "" {
		t.Errorf("empty declarations must stay empty: %+v", out)
	}
}

// TestKnownKeySetsCoverEveryField is the maintenance guard on the sidecar. A
// field whose key is missing from the set is captured as "unknown" and then
// replayed from the stale copy on every write — so the field would appear to be
// unwritable, silently. Reflection derives both sets, and this asserts the
// derivation actually sees the embedded Binding.
func TestKnownKeySetsCoverEveryField(t *testing.T) {
	for _, key := range []string{"formatVersion", "kind", "title", "entrypoint", "parameters", "runtime", "reportingTimezone", "access", "dependencies", "instances"} {
		if !manifestKeys[key] {
			t.Errorf("manifestKeys is missing %q", key)
		}
	}
	// url and tenantID are Instance's own; the rest are flattened in from the
	// embedded Binding, which is the case a naive one-level walk would miss.
	for _, key := range []string{"url", "tenantID", "bind", "workflowID", "dataAppID", "featureID", "tables"} {
		if !instanceKeys[key] {
			t.Errorf("instanceKeys is missing %q", key)
		}
	}
	if len(manifestKeys) != reflect.TypeOf(Manifest{}).NumField()-1 {
		t.Errorf("manifestKeys has %d keys for %d exported fields — a new field needs a case above", len(manifestKeys), reflect.TypeOf(Manifest{}).NumField()-1)
	}
}

// TestBindingHasNoJSONMethods guards the one way Instance's preservation can be
// dismantled from another file, and the reason it looks unnecessary is the
// reason it is not.
//
// Binding is EMBEDDED in Instance. Reading only Instance, a MarshalJSON on
// Binding looks harmless: Instance defines its own at depth 0, and a method
// declared on the outer type SHADOWS the promoted one. But Instance.MarshalJSON
// encodes through plainInstance, which has no methods of its own and still
// embeds Binding — so a Binding.MarshalJSON IS promoted there, wins, and
// json.Marshal(plainInstance(i)) emits the binding alone. Every entry in the
// customer's committed manifest would lose the url and tenantID that say which
// instance and organization it belongs to, and the unknown keys would be
// spliced onto the truncated object. The unmarshal direction fails the same
// way, swallowing the whole entry so url and tenantID never decode.
//
// Nothing about adding that method fails to compile, and the tests that only
// check known fields on a manifest built in code would not notice either. This
// assertion is what notices.
func TestBindingHasNoJSONMethods(t *testing.T) {
	binding := reflect.TypeOf(Binding{})
	for _, method := range []string{"MarshalJSON", "UnmarshalJSON"} {
		if _, ok := binding.MethodByName(method); ok {
			t.Errorf("Binding must not define %s — it is embedded in Instance, so the method is promoted onto the plainInstance twin and wins there", method)
		}
		if _, ok := reflect.PointerTo(binding).MethodByName(method); ok {
			t.Errorf("*Binding must not define %s — it is embedded in Instance, so the method is promoted onto the plainInstance twin and wins there", method)
		}
	}
	// And the entry really does serialize as one flat object.
	body, err := json.Marshal(Instance{URL: "https://app.ronja.tech", TenantID: "ten-acme", Binding: Binding{WorkflowID: "wf-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), `{"url":"https://app.ronja.tech","tenantID":"ten-acme","workflowID":"wf-1"}`; got != want {
		t.Errorf("instance encoding = %s, want %s", got, want)
	}
}

// TestUnknownKeysDoNotMigrateBetweenEntries: each entry's sidecar belongs to
// THAT (instance, organization), and SetBinding rebuilds the slice — dropping
// the entry it matched, appending the replacement, then sorting. If the
// carry-across ever keyed on position rather than on the entry it matched, a
// push against one instance would stamp another instance's unknown keys onto
// this one's entry, and the file would still be valid JSON with
// plausible-looking contents. That is the quietest failure in this area, so it
// gets its own assertion — with the sort really moving the entries, which is
// when a positional bug bites.
func TestUnknownKeysDoNotMigrateBetweenEntries(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[{"url":"https://a.test","tenantID":"ten-a","workflowID":"wf-a","bindA":{"orders":"table-a"}},{"url":"https://b.test","tenantID":"ten-b","workflowID":"wf-b","bindB":{"orders":"table-b"}}]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The FIRST entry, so the replacement is appended past the second one and
	// the sort has to carry it back to the front.
	m.SetBinding(InstanceKey{URL: "https://a.test", TenantID: "ten-a"}, Binding{FeatureID: "feat-a"})
	if got := m.Instances[0].URL; got != "https://a.test" {
		t.Fatalf("expected the sort to put a.test back first, got %q — this test is no longer exercising the reorder", got)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Instances []map[string]json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(out.Instances) != 2 {
		t.Fatalf("expected two entries, got %d:\n%s", len(out.Instances), raw)
	}
	for i, want := range []struct{ has, hasNot string }{{"bindA", "bindB"}, {"bindB", "bindA"}} {
		if _, ok := out.Instances[i][want.has]; !ok {
			t.Errorf("entry %d lost its own %q:\n%s", i, want.has, raw)
		}
		if _, ok := out.Instances[i][want.hasNot]; ok {
			t.Errorf("entry %d picked up the OTHER entry's %q:\n%s", i, want.hasNot, raw)
		}
	}
}

// TestDuplicateKeysCollapseToTheLast: JSON permits a repeated key and
// encoding/json takes the last one, so the sidecar has to agree with the decode
// on both halves — the value kept AND the count. Remember the key twice and the
// rewrite emits a duplicate the author never typed; remember the wrong copy and
// a hand-edit is silently reverted.
func TestDuplicateKeysCollapseToTheLast(t *testing.T) {
	got := loadSave(t, canonicalManifest(t, `{"kind":"workflow","title":"first","title":"second","entrypoint":"main.py","future":1,"future":2,"instances":[]}`))
	for _, want := range []struct {
		key   string
		value string
	}{{`"title"`, `"title": "second"`}, {`"future"`, `"future": 2`}} {
		if n := strings.Count(got, want.key); n != 1 {
			t.Errorf("%s appears %d times, want once:\n%s", want.key, n, got)
		}
		if !strings.Contains(got, want.value) {
			t.Errorf("expected the LAST value to win (%s):\n%s", want.value, got)
		}
	}
}

// TestClearedParametersAndAccessAreNotResurrected is the resurrection guard on
// the two keys where getting it wrong is not cosmetic. `parameters` is a
// declaration push SYNCS, so replaying a cleared one would make the folder go
// on declaring parameters its author deleted — and the next push would put them
// back on the workflow. `access` is a capability grant, so a resurrected one
// re-grants what somebody deliberately revoked.
func TestClearedParametersAndAccessAreNotResurrected(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","future":true,"parameters":[{"name":"region","label":"Region","type":"string","required":false}],"access":{"allowedTableIDs":["table-1"],"allowedSecretIDs":[],"allowedAgentIDs":[],"allowedWorkflowIDs":[],"allowedCodexIDs":[],"allowedMetricIDs":[],"capabilities":["query_ronja"]},"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Back to UNMANAGED — the three-state pointer going to nil, not to empty.
	m.Parameters = nil
	m.Access = nil
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"parameters", "access", "allowedTableIDs", "query_ronja"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("a cleared declaration must be dropped, not restored from the sidecar (%q):\n%s", key, raw)
		}
	}
	if !strings.Contains(string(raw), `"future"`) {
		t.Errorf("clearing known keys must not disturb the unknown one:\n%s", raw)
	}
	out, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if out.ManagesParameters() || out.ManagesAccess() {
		t.Errorf("a cleared declaration must read back as UNMANAGED: %+v", out)
	}
}

// TestHTMLCharactersInAPreservedValueAreEscaped pins the one way a preserved
// value does NOT come back byte for byte: the JSON encoder escapes &, < and >,
// so a hand-written query string is re-spelled the first time any command
// rewrites the manifest.
//
// Pinned rather than fixed. Turning the escaping off would change the output
// for every KNOWN field too — a title with an ampersand, a URL with a query —
// which is a larger change than the one this file is making, and the DATA is
// identical either way. It also does not ping-pong: every build escapes the
// same way, so it is one diff line, once.
func TestHTMLCharactersInAPreservedValueAreEscaped(t *testing.T) {
	body := canonicalManifest(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","futureURL":"https://x.test/?a=1&b=2","instances":[]}`)
	got := loadSave(t, body)
	if !strings.Contains(got, `?a=1\u0026b=2`) {
		t.Fatalf("expected the encoder's escaped spelling of the preserved value:\n%s", got)
	}
	// The DATA is unchanged, which is the actual contract.
	var out struct {
		FutureURL string `json:"futureURL"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if out.FutureURL != "https://x.test/?a=1&b=2" {
		t.Errorf("futureURL read back as %q, want the value the file was written with", out.FutureURL)
	}
	// And it settles: a second rewrite changes nothing, so two people on two
	// builds do not trade the same line back and forth.
	if again := loadSave(t, got); again != got {
		t.Errorf("escaping must be stable across rewrites\n got:\n%s\nwant:\n%s", again, got)
	}
}

// TestDecodeErrorsNameTypesAPersonCanRecognise: a manifest is a file people
// hand-edit, so a mistyped value has to produce a sentence about THEIR file.
// Routing the decode through the plainManifest twin put an internal type name
// into encoding/json's message, where it reads like a bug in the CLI rather
// than a typo in the manifest — these pin the rename that takes it back out.
// TestEmbeddedStructNameIsNotInTheFieldPath pins that a path names where the
// value sits in the FILE, not in the Go structure.
//
// Binding flattens into an instance entry, so a manifest has no "binding" key.
// A path that names one sends the reader looking for something that was never
// there — the same defect as leaking the decode twin's name, one level down.
func TestEmbeddedStructNameIsNotInTheFieldPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"T","entrypoint":"main.py","instances":[{"url":"http://x","tenantID":"t","workflowID":{}}]}`)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected a decode error for an object in workflowID")
	}
	msg := err.Error()
	if strings.Contains(msg, "Binding") {
		t.Errorf("field path names the embedded Go type, which is not a key in the file:\n  %s", msg)
	}
	if !strings.Contains(msg, "Manifest.instances.workflowID") {
		t.Errorf("want the flattened path Manifest.instances.workflowID, got:\n  %s", msg)
	}
}

func TestDecodeErrorsNameTypesAPersonCanRecognise(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a mistyped field inside an entry",
			body: `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[{"url":{"host":"app.ronja.tech"},"tenantID":"ten-acme"}]}`,
			want: "Manifest.instances.url",
		},
		{
			name: "a mistyped root field",
			body: `{"kind":"workflow","title":5,"entrypoint":"main.py","instances":[]}`,
			want: "Manifest.title",
		},
		{
			// No struct and no field at all, so encoding/json names the decoded
			// TYPE instead — the other spelling the rename has to cover.
			name: "a manifest that is not an object",
			body: `[{"kind":"workflow","title":"x"}]`,
			want: "wfdir.Manifest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, tc.body)
			_, err := LoadManifest(root, WorkflowKind)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to name %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "plain") {
				t.Errorf("refusal names a type that exists only inside this package: %v", err)
			}
			if !strings.Contains(err.Error(), ManifestName) {
				t.Errorf("refusal = %v, want it to name the file", err)
			}
		})
	}
}

// TestFormatVersionSpellingsAllReachTheGate: the version gate is the escape
// hatch for a format this build cannot read, so it must not be dodgeable by how
// the number was typed. An int-typed probe fails on every spelling below and
// then falls THROUGH the gate, handing the author a type error about a Go field
// instead of the one sentence that helps: upgrade the CLI.
func TestFormatVersionSpellingsAllReachTheGate(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "integer", value: `4`, wantErr: "format version 4"},
		{name: "trailing zero", value: `4.0`, wantErr: "format version 4.0"},
		{name: "exponent", value: `4e1`, wantErr: "format version 4e1"},
		{name: "fractional", value: `3.5`, wantErr: "format version 3.5"},
		{name: "past float range", value: `1e999`, wantErr: "format version 1e999"},
		// Not a number at all: refused on its own terms rather than ignored,
		// because what the reader needs to know is that this file's format could
		// not be established — not that they picked the wrong Go type.
		{name: "quoted", value: `"2"`, wantErr: "is not a number"},
		{name: "boolean", value: `true`, wantErr: "is not a number"},
		{name: "object", value: `{"major":2}`, wantErr: "is not a number"},
		// Spellings the version gate lets past because they ARE versions this
		// build reads. The typed decode then refuses them, and that message names
		// both the file and the key, which is enough to fix a hand-edit.
		{name: "one, spelled long", value: `1.0`, wantErr: "formatVersion"},
		{name: "two, spelled long", value: `2.0`, wantErr: "formatVersion"},
		{name: "three, spelled long", value: `3.0`, wantErr: "formatVersion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := `{"formatVersion":` + tc.value + `,"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`
			write(t, root, ManifestName, body)
			_, err := LoadManifest(root, WorkflowKind)
			if err == nil {
				t.Fatalf("expected %s to be refused", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal = %v, want it to say %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), ManifestName) {
				t.Errorf("refusal = %v, want it to name the file", err)
			}
			raw, err := os.ReadFile(ManifestPath(root))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Errorf("a refused manifest must be left untouched:\n%s", raw)
			}
		})
	}
}

// TestNullFormatVersionMeansAbsent: null is the one non-number that loads. It
// is what "no value" looks like everywhere else in this decode — null into a
// typed field is a documented no-op — so a manifest carrying it says exactly
// what one omitting the key says, and refusing it would refuse a file whose
// meaning is not in doubt.
func TestNullFormatVersionMeansAbsent(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"formatVersion":null,"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("a null formatVersion must load as version 1: %v", err)
	}
	if m.FormatVersion != 0 {
		t.Errorf("formatVersion decoded as %d, want the zero value that means 1", m.FormatVersion)
	}
}
