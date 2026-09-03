package wfdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A v1 workflow manifest, in the exact shape the CLI has been writing.
const legacyManifest = `{"kind":"workflow","title":"Region Report","entrypoint":"main.py","instances":[{"url":"http://localhost:8098","tenantID":"development","workflowID":"workflow-1","featureID":"collection-1"}]}`

func loadWorkflow(t *testing.T, root string) (*Manifest, *Lock) {
	t.Helper()
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	l, err := LoadLock(root)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	return m, l
}

func devKey() InstanceKey {
	return InstanceKey{URL: "http://localhost:8098", TenantID: "development"}
}

// TestLegacyFolderStaysV1: the property the whole migration rests on. A folder
// nobody has named a stack in must round-trip through the writer that now knows
// about stacks and come back BYTE-IDENTICAL — no formatVersion, no "stacks", and
// no lock file appearing beside it. Anything less forces every colleague onto a
// new CLI for a push that changed nothing meaningful.
func TestLegacyFolderStaysV1(t *testing.T) {
	root := t.TempDir()
	body := canonicalManifest(t, legacyManifest)
	write(t, root, ManifestName, body)

	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, devKey(), "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Named() {
		t.Fatalf("a legacy entry must select as unnamed, got %q", sel.Name)
	}
	if sel.Binding.WorkflowID != "workflow-1" || sel.Binding.FeatureID != "collection-1" {
		t.Fatalf("legacy binding not resolved: %+v", sel.Binding)
	}
	// The push shape: record the binding it already had, then save both files.
	m.Record(lock, sel, sel.Binding)
	if err := SaveFolder(root, m, lock); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, root); got != body {
		t.Errorf("a v1 folder was rewritten:\n got %s\nwant %s", got, body)
	}
	if _, err := os.Stat(LockPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a legacy folder must not grow a %s (stat err = %v)", LockName, err)
	}
}

// TestLegacyFolderStaysV1AfterANewBinding: the same property when the push
// actually changes something — a first push recording a workflow id. It must
// still be version 1: what makes a manifest v2 is a named STACK, not a write.
func TestLegacyFolderStaysV1AfterANewBinding(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-1"}]}`))

	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, devKey(), "")
	if err != nil {
		t.Fatal(err)
	}
	m.Record(lock, sel, Binding{WorkflowID: "workflow-9", FeatureID: "collection-1"})
	if err := SaveFolder(root, m, lock); err != nil {
		t.Fatal(err)
	}
	raw := readBack(t, root)
	for _, unwanted := range []string{"formatVersion", "stacks"} {
		if strings.Contains(raw, unwanted) {
			t.Errorf("a legacy push wrote %q:\n%s", unwanted, raw)
		}
	}
	if !strings.Contains(raw, "workflow-9") {
		t.Errorf("the new id was not recorded:\n%s", raw)
	}
}

// TestNamingAStackMigratesTheEntry is the migration, forwards: a human supplies
// the one thing an instances[] entry never had — a name — and the entry moves
// across whole. The ids land in the lock, the config stays in the manifest, and
// the legacy entry is GONE so the two shapes can never both answer for one
// organization.
func TestNamingAStackMigratesTheEntry(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, legacyManifest))

	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, devKey(), "dev")
	if err != nil {
		t.Fatalf("naming a legacy entry: %v", err)
	}
	if !sel.Bound {
		t.Fatal("naming an existing legacy entry must resolve as BOUND — an unbound answer would create a second workflow")
	}
	if sel.Binding.WorkflowID != "workflow-1" {
		t.Fatalf("the legacy entry's ids must come across, got %+v", sel.Binding)
	}
	m.Record(lock, sel, sel.Binding)
	if err := SaveFolder(root, m, lock); err != nil {
		t.Fatal(err)
	}

	raw := readBack(t, root)
	if !strings.Contains(raw, `"formatVersion": 2`) {
		t.Errorf("a stack manifest must declare version 2, so an older CLI refuses it:\n%s", raw)
	}
	if strings.Index(raw, "formatVersion") > strings.Index(raw, `"kind"`) {
		t.Errorf("formatVersion must lead the file — it is what an older CLI's gate reads:\n%s", raw)
	}
	if strings.Contains(raw, `"instances"`) {
		t.Errorf("the migrated entry must leave instances[] entirely:\n%s", raw)
	}
	if strings.Contains(raw, "workflow-1") {
		t.Errorf("the created id is STATE and belongs in %s, not in the manifest:\n%s", LockName, raw)
	}
	if !strings.Contains(raw, "collection-1") {
		t.Errorf("featureID is CONFIG and stays in the manifest:\n%s", raw)
	}

	lockRaw, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatalf("the lock file must exist after a stack push: %v", err)
	}
	if !strings.Contains(string(lockRaw), "workflow-1") {
		t.Errorf("the lock must carry the created id:\n%s", lockRaw)
	}

	// And backwards, in the sense that matters: the migrated folder still
	// resolves to the same row from the same credential, with no --stack.
	m2, lock2 := loadWorkflow(t, root)
	sel2, err := m2.Select(lock2, devKey(), "")
	if err != nil {
		t.Fatalf("implicit selection after migration: %v", err)
	}
	if sel2.Name != "dev" || sel2.Binding.WorkflowID != "workflow-1" || sel2.Binding.FeatureID != "collection-1" {
		t.Fatalf("a migrated folder must resolve identically without --stack, got %+v", sel2)
	}
}

// TestStackAndLegacyEntryCoexist: migration is per (instance, organization), so
// a folder halfway through it has both shapes. Both must answer, or the
// un-named half would read as unbound and its next push would create a second
// set of resources.
func TestStackAndLegacyEntryCoexist(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"dev":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-1"}},
	  "instances":[{"url":"http://localhost:8098","tenantID":"other-org","workflowID":"workflow-2","featureID":"collection-2"}]}`))
	write(t, root, LockName, `{"stacks":{"dev":{"workflowID":"workflow-1"}}}`+"\n")

	m, lock := loadWorkflow(t, root)
	named, err := m.Select(lock, devKey(), "")
	if err != nil || named.Name != "dev" || named.Binding.WorkflowID != "workflow-1" {
		t.Fatalf("the stack half did not resolve: %+v (%v)", named, err)
	}
	legacy, err := m.Select(lock, InstanceKey{URL: "http://localhost:8098", TenantID: "other-org"}, "")
	if err != nil || legacy.Named() || legacy.Binding.WorkflowID != "workflow-2" {
		t.Fatalf("the legacy half did not resolve: %+v (%v)", legacy, err)
	}
}

// TestSelectRefusesAStackThisCredentialCannotReach: --stack and --profile name
// two different things, and when they disagree acting on either is a guess. The
// outcome without this refusal is the worst one available: this folder's files
// pushed into the named stack's feature under the other organization's token.
func TestSelectRefusesAStackThisCredentialCannotReach(t *testing.T) {
	root := t.TempDir()
	// The SAME instance, a different organization — which is the case the
	// organization half of the check owns. The instance half has its own test
	// below, because the two are compared under different conditions.
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"prod":{"url":"http://localhost:8098","tenantID":"ten-prod","featureID":"collection-p"}}}`))

	m, lock := loadWorkflow(t, root)
	_, err := m.Select(lock, devKey(), "prod")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"prod", "ten-prod", "development"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to name %q", err, want)
		}
	}
}

// TestSelectRefusesATypoedStackName: an undeclared name against a place this
// folder already calls something else is a typo, and treating it as a new stack
// creates a second workflow beside the first and reports success.
func TestSelectRefusesATypoedStackName(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"prod":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-1"}}}`))

	m, lock := loadWorkflow(t, root)
	_, err := m.Select(lock, devKey(), "prd")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--stack prod") {
		t.Errorf("refusal = %v, want it to name the stack that was meant", err)
	}
}

// TestSelectCreatesAStackSomewhereNew: the legitimate other half of the rule
// above — an undeclared name for an organization this folder has never been
// pushed to is how a second environment is born.
func TestSelectCreatesAStackSomewhereNew(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"dev":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-1"}}}`))

	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-prod"}, "prod")
	if err != nil {
		t.Fatalf("naming a new stack: %v", err)
	}
	if sel.Bound || sel.Name != "prod" {
		t.Fatalf("a new stack must select unbound and named, got %+v", sel)
	}
}

// TestCheckStacksRefusals: the PER-STACK invariants, which are the only ones
// load enforces. The cross-stack rules moved to selection — see
// TestLoadAcceptsCrossStackCollisionsAndSelectionResolvesThem.
func TestCheckStacksRefusals(t *testing.T) {
	cases := []struct {
		name    string
		stacks  string
		wantErr string
	}{
		{
			name:    "a name that is not typeable",
			stacks:  `{"prod stack":{"url":"http://a","tenantID":"t1"}}`,
			wantErr: "letters, digits",
		},
		{
			// A stack is an (instance, ORGANIZATION). One without the second half
			// is invisible to the implicit match and refused by every explicit
			// one, so it is refused here — the only place that can name the key
			// that is missing.
			name:    "a stack naming no organization",
			stacks:  `{"prod":{"url":"http://a","featureID":"collection-1"}}`,
			wantErr: `no "tenantID"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t,
				`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":`+tc.stacks+`}`))
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

// TestLockPreservesUnknownKeys: the lock is committed and shared, so it carries
// the identical hazard the manifest does — a colleague on an older CLI rewrites
// it and whatever the newer format recorded is gone from the repo. Preservation
// is per object, and the lock has two: the root and each stack entry.
func TestLockPreservesUnknownKeys(t *testing.T) {
	root := t.TempDir()
	// The stack entry's unknown key is a key from the FUTURE, deliberately not
	// headVersionID: that is a field this build owns now, and a rebinding
	// legitimately clears it (see TestLockRebindingClearsTheAnchor). A fixture
	// that used it would be testing the rebind rule while claiming to test
	// preservation.
	body := `{"formatVersion":1,"generatedBy":"ronja 9.9","stacks":{"dev":{"workflowID":"workflow-1","promotedFrom":"staging"}}}` + "\n"
	write(t, root, LockName, body)

	lock, err := LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.SetBinding("dev", Binding{WorkflowID: "workflow-2"})
	if err := SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"generatedBy": "ronja 9.9"`, `"promotedFrom": "staging"`, `"workflowID": "workflow-2"`} {
		if !strings.Contains(got, want) {
			t.Errorf("rewrite dropped %s:\n%s", want, got)
		}
	}
	// Position, not only presence: a key put back on the end makes an old CLI
	// and a new one fight over the same file for ever.
	if strings.Index(got, "generatedBy") > strings.Index(got, `"stacks"`) {
		t.Errorf("an unknown key must keep its place:\n%s", got)
	}
}

// TestLockRefusesAFutureFormat: the same gate the manifest has, on the same
// reasoning. Without it a newer lock is read with whatever this build
// understands and then written back stripped.
func TestLockRefusesAFutureFormat(t *testing.T) {
	root := t.TempDir()
	write(t, root, LockName, `{"formatVersion":2,"stacks":{}}`+"\n")
	_, err := LoadLock(root)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "format version 2") || !strings.Contains(err.Error(), LockName) {
		t.Errorf("refusal = %v, want it to name the file and the version", err)
	}
}

// TestSaveLockWritesNothingForALegacyFolder: the mechanism behind "v1 stays
// v1". Every write path calls SaveFolder, so if this wrote an empty file every
// legacy push would drop an unexplained ronja.lock.json into the customer's repo.
func TestSaveLockWritesNothingForALegacyFolder(t *testing.T) {
	root := t.TempDir()
	if err := SaveLock(root, &Lock{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LockPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty lock must not create a file (stat err = %v)", err)
	}
	// But one that already exists is kept in step, or removing the last stack
	// would leave a stale entry behind for ever.
	write(t, root, LockName, `{"stacks":{"dev":{"workflowID":"workflow-1"}}}`+"\n")
	if err := SaveLock(root, &Lock{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "workflow-1") {
		t.Errorf("an existing lock must be rewritten:\n%s", raw)
	}
}

// TestLockKeepsLiveFingerprintsAcrossABindingWrite: SetBinding takes path →
// table id, which carries no fingerprints. Writing the map wholesale would drop
// every LiveSHA256 on the first push after a clone, and the drift guard would
// silently stop having anything to compare against.
func TestLockKeepsLiveFingerprintsAcrossABindingWrite(t *testing.T) {
	lock := &Lock{}
	lock.SetTableLive("dev", "orders.sql", "table-1", "sha-live")
	lock.SetBinding("dev", Binding{WorkflowID: "", Tables: map[string]string{"orders.sql": "table-1"}})
	if got := lock.TableLive("dev", "orders.sql"); got != "sha-live" {
		t.Errorf("live fingerprint = %q, want it preserved", got)
	}
	// A path REBOUND to a different table drops the old row's fingerprint with
	// it: a hash is only ever compared against the row it was taken from.
	lock.SetBinding("dev", Binding{Tables: map[string]string{"orders.sql": "table-2"}})
	if got := lock.TableLive("dev", "orders.sql"); got != "" {
		t.Errorf("rebinding must drop the old row's fingerprint, got %q", got)
	}
}

// TestSaveFolderWritesTheLockFirst: the two files have no transaction between
// them, so the order decides which half is left behind by a crash. A lock entry
// no stack names is inert; a stack with no lock entry reads as unbound and its
// next push creates a second set of resources.
func TestSaveFolderWritesTheLockFirst(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Kind: KindWorkflow, Title: "x", Entrypoint: "main.py"}
	lock := &Lock{}
	m.Record(lock, Selection{Name: "dev", Key: devKey()}, Binding{WorkflowID: "workflow-1", FeatureID: "collection-1"})
	// The manifest write is made to fail by putting a directory where the file
	// goes, which is the only local failure this code can actually produce.
	if err := os.MkdirAll(filepath.Join(root, ManifestName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveFolder(root, m, lock); err == nil {
		t.Fatal("expected the manifest write to fail")
	}
	if _, err := os.Stat(LockPath(root)); err != nil {
		t.Errorf("the lock must already be on disk when the manifest write fails: %v", err)
	}
}

// TestSelectRefusesAStackOnAnotherInstanceWithNoOrganization is the same
// refusal as above, on the credential that has no organization at all — which
// is what $RONJA_TOKEN always is, and the documented CI path.
//
// It has its own test because it is the case a single "do the keys match"
// comparison silently skips: with no organization to compare, such a check
// passes, and `--stack prod` under a localhost token adopts prod's featureID and
// pushes to localhost with it.
func TestSelectRefusesAStackOnAnotherInstanceWithNoOrganization(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"prod":{"url":"https://app.ronja.tech","tenantID":"ten-prod","featureID":"collection-p"}}}`))

	m, lock := loadWorkflow(t, root)
	_, err := m.Select(lock, InstanceKey{URL: "http://localhost:8098"}, "prod")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "app.ronja.tech") || !strings.Contains(err.Error(), "localhost:8098") {
		t.Errorf("refusal = %v, want it to name both instances", err)
	}
}

// TestSelectAdoptsADeclaredStacksOrganization: the other half of the split — a
// credential that names no organization but is pointed at the right INSTANCE
// gets the stack's own organization out of the committed file, with no round
// trip. That is how a CI job pins the organization from the repo.
func TestSelectAdoptsADeclaredStacksOrganization(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{
	  "formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py",
	  "stacks":{"dev":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-1"}}}`))

	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, InstanceKey{URL: "http://localhost:8098"}, "dev")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Key.TenantID != "development" || !sel.Bound {
		t.Fatalf("a declared stack must supply its own organization, got %+v", sel)
	}
}

// TestLockFileIsNeverSource is the B1 regression. ronja.lock.json is COMMITTED,
// so unlike .ronja/ it is a real file sitting in the walk's way — and a workflow
// or data app has no SyncExt, so anything the structural rules allow is pushed.
// Without an exclusion the folder's own machine state is PUT into the customer's
// workflow (and into a data app's compiled bundle), a later clone writes the
// server's stale copy over the real lock file, and the folder then reports
// `modified ronja.lock.json` for ever.
//
// Enumeration and path-checking must AGREE, which is why both predicates are
// asserted here and why the round trip below is the real test.
func TestLockFileIsNeverSource(t *testing.T) {
	for _, kind := range []Kind{WorkflowKind, DataAppKind, PipelineKind} {
		t.Run(kind.Name, func(t *testing.T) {
			if reason := StructuralExclusion(LockName, kind); reason == "" {
				t.Errorf("StructuralExclusion(%s) allowed it into a %s folder", LockName, kind.Label)
			}
			if reason := NotSyncable(LockName, kind); reason == "" {
				t.Errorf("NotSyncable(%s) allowed it into a %s folder", LockName, kind.Label)
			}
			// A remote file set carrying it must be refused before a byte is
			// written, exactly as one carrying ronja.json is: laying it down
			// would overwrite the folder's real lock with the server's copy.
			if err := CheckLocalPaths([]string{LockName}, kind); err == nil {
				t.Errorf("CheckLocalPaths accepted %s for a %s folder", LockName, kind.Label)
			}
		})
	}

	// The round trip: a real folder with a real lock file in it, enumerated the
	// way a push enumerates. It must not appear in Files (it would be pushed)
	// and must not appear in Skipped either (it is the CLI's own bookkeeping,
	// and naming it on every command is pure noise).
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow"}`)
	write(t, root, LockName, `{"stacks":{"dev":{"workflowID":"workflow-1"}}}`)
	write(t, root, "main.py", "x = 1\n")
	enumeration, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if _, ok := enumeration.Files[LockName]; ok {
		t.Errorf("%s was enumerated as pushable source: %v", LockName, enumeration.Files)
	}
	if _, ok := enumeration.Files["main.py"]; !ok {
		t.Errorf("main.py should still be source: %v", enumeration.Files)
	}
	for _, skipped := range enumeration.Skipped {
		if skipped.Path == LockName {
			t.Errorf("%s was reported as a skipped user file (%s) — it is folder machinery", LockName, skipped.Reason)
		}
	}
}

// TestLoadAcceptsCrossStackCollisionsAndSelectionResolvesThem is the B3
// regression, and the invariant it pins is THE CLI MUST NEVER WRITE A MANIFEST
// IT WILL THEN REFUSE TO LOAD.
//
// Both collisions arrive without a writer: a git merge bringing two branches'
// stacks together is a clean textual merge. Enforced at load they made every
// command fail — `status` and an explicit `--stack prod` that resolves
// perfectly well included — with a recovery of hand-editing a committed file.
func TestLoadAcceptsCrossStackCollisionsAndSelectionResolvesThem(t *testing.T) {
	t.Run("two stacks on one organization", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t,
			`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"a":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-a"},"b":{"url":"http://LOCALHOST:8098/","tenantID":"development","featureID":"collection-b"}}}`))
		m, lock := loadWorkflow(t, root)

		// Naming one still works — that is the recovery, and it must exist.
		sel, err := m.Select(lock, devKey(), "b")
		if err != nil {
			t.Fatalf("--stack b on a merged folder: %v", err)
		}
		if sel.Name != "b" || sel.Binding.FeatureID != "collection-b" {
			t.Fatalf("--stack b resolved to %+v", sel)
		}
		// The IMPLICIT match is where the ambiguity is reported, and it names
		// both stacks plus what makes the pair a problem.
		_, err = m.Select(lock, devKey(), "")
		if err == nil {
			t.Fatal("an implicit selection across two stacks on one organization must refuse")
		}
		if !errors.Is(err, ErrAmbiguousStack) {
			t.Errorf("refusal = %v, want ErrAmbiguousStack", err)
		}
		for _, want := range []string{"--stack", "a, b", "local sync baseline"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %v, want it to say %q", err, want)
			}
		}
	})

	t.Run("names differing only by case", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t,
			`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"prod":{"url":"http://a","tenantID":"t1"},"Prod":{"url":"http://b","tenantID":"t2"}}}`))
		m, lock := loadWorkflow(t, root)
		sel, err := m.Select(lock, InstanceKey{URL: "http://a", TenantID: "t1"}, "prod")
		if err != nil {
			t.Fatalf("--stack prod on a folder that also names Prod: %v", err)
		}
		if sel.Name != "prod" {
			t.Fatalf("--stack prod resolved to %+v", sel)
		}
	})
}

// TestSelectRefusesANameThatOnlyDiffersByCase is the write half of B3: the pair
// above must be impossible to CREATE, since that is the only door this CLI has
// to it. `--stack Prod` against a folder naming `prod` used to write a second
// key, create a second set of resources, and report success — and the file it
// left behind was one LoadManifest then refused.
func TestSelectRefusesANameThatOnlyDiffersByCase(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"prod":{"url":"http://a","tenantID":"t1"}},"instances":[{"url":"http://localhost:8098","tenantID":"development","workflowID":"workflow-legacy"}]}`))
	m, lock := loadWorkflow(t, root)

	// The exact shape the reviewer reproduced: a legacy entry for ANOTHER key,
	// so the near-miss loop keyed on the ORGANIZATION never fires and the
	// migration path was reached with a name that collides.
	_, err := m.Select(lock, devKey(), "Prod")
	if err == nil {
		t.Fatal("--stack Prod against a folder naming prod must refuse")
	}
	if !strings.Contains(err.Error(), "capitalisation") || !strings.Contains(err.Error(), "--stack prod") {
		t.Errorf("refusal = %v, want it to name the existing spelling", err)
	}

	// And the file is still loadable, which is the whole point.
	if _, _, err := roundTripFolder(t, root); err != nil {
		t.Fatalf("the folder must stay loadable: %v", err)
	}
}

// roundTripFolder loads a folder, writes both files back unchanged, and loads
// them again — the "would this CLI refuse what it just wrote" check.
func roundTripFolder(t *testing.T, root string) (*Manifest, *Lock, error) {
	t.Helper()
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		return nil, nil, err
	}
	lock, err := LoadLock(root)
	if err != nil {
		return nil, nil, err
	}
	if err := SaveFolder(root, m, lock); err != nil {
		return nil, nil, err
	}
	m, err = LoadManifest(root, WorkflowKind)
	if err != nil {
		return nil, nil, err
	}
	lock, err = LoadLock(root)
	return m, lock, err
}

// TestSelectAdoptsAStrandedLockEntry is the B5 regression. SaveFolder writes the
// lock FIRST, so a push that created a workflow and then failed to write the
// manifest leaves the id recorded under a name no stack declares. Unreachable,
// that id is LOST and the next push creates a second workflow — which is exactly
// what the lock-first ordering was supposed to avoid.
func TestSelectAdoptsAStrandedLockEntry(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{}}`))
	write(t, root, LockName, `{"stacks":{"dev":{"workflowID":"workflow-stranded"}}}`+"\n")
	m, lock := loadWorkflow(t, root)

	sel, err := m.Select(lock, devKey(), "dev")
	if err != nil {
		t.Fatalf("--stack dev against a stranded lock entry: %v", err)
	}
	if !sel.Bound {
		t.Fatal("a stack whose ids the lock records must select as BOUND, or the next push creates a second workflow")
	}
	if sel.Binding.WorkflowID != "workflow-stranded" {
		t.Fatalf("binding = %+v, want the stranded workflow id", sel.Binding)
	}

	// A name the lock does NOT record is still an ordinary new stack.
	sel, err = m.Select(lock, devKey(), "staging")
	if err != nil {
		t.Fatalf("--stack staging: %v", err)
	}
	if sel.Bound {
		t.Fatalf("an unrecorded name must select unbound, got %+v", sel)
	}
}

// TestSelectRefusesAStrandedLockEntryWithNoStackFlag is the other half of the
// test above, on the door the recovery does NOT cover.
//
// selectNamed adopts a stranded lock entry, which is what makes SaveFolder's
// lock-first ordering safe — but only behind an explicit --stack. The IMPLICIT
// path reads the folder through m.entries(lock), which walks m.Stacks, so a
// manifest naming no stacks reports "never pushed here" while the lock holds a
// real workflow id. Flagless, that is the create path, and it makes exactly the
// duplicate the ordering exists to prevent.
//
// A refusal rather than a second recovery: which of the lock's stacks a flagless
// command meant is a guess, and guessing wrong pushes one environment's files
// into another's.
func TestSelectRefusesAStrandedLockEntryWithNoStackFlag(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{}}`))
	write(t, root, LockName, `{"stacks":{"dev":{"workflowID":"workflow-stranded"}}}`+"\n")
	m, lock := loadWorkflow(t, root)

	sel, err := m.Select(lock, devKey(), "")
	if err == nil {
		t.Fatalf("a flagless select against a stranded lock entry must be REFUSED — it took the "+
			"create path instead and would push a second workflow beside workflow-stranded (got %+v)", sel)
	}
	// The message has to hand the reader the way out, which is the name only the
	// lock knows.
	for _, want := range []string{"dev", "--stack", LockName, ManifestName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so it does not tell the reader what to do:\n%v", want, err)
		}
	}
	if sel.Bound {
		t.Errorf("a refused selection must not come back bound: %+v", sel)
	}

	// And --stack still adopts, so the refusal is a signpost rather than a wall.
	if sel, err := m.Select(lock, devKey(), "dev"); err != nil || sel.Binding.WorkflowID != "workflow-stranded" {
		t.Fatalf("--stack dev must still adopt the stranded ids: sel=%+v err=%v", sel, err)
	}
}

// TestSelectAllowsALockWithNoStacks pins the two shapes the refusal above must
// NOT touch: a folder with no lock file at all, and a v1 folder carrying a lock
// file that names no stacks. Both are ordinary, both select flaglessly, and a
// refusal on either would break every legacy folder on the first upgrade.
func TestSelectAllowsALockWithNoStacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		lock string // "" writes no lock file at all
	}{
		{name: "no lock file"},
		{name: "empty lock", lock: `{"stacks":{}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t, legacyManifest))
			if tc.lock != "" {
				write(t, root, LockName, tc.lock)
			}
			m, lock := loadWorkflow(t, root)
			sel, err := m.Select(lock, devKey(), "")
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if sel.Binding.WorkflowID != "workflow-1" {
				t.Fatalf("legacy binding not resolved: %+v", sel.Binding)
			}
		})
	}
}

// TestRecordKeepsADeclaredFeatureID is the B6 regression: Record writes the
// binding a caller hands it, and a state-shaped one carrying no featureID must
// not wipe the line a person wrote with `init --feature <id>`.
func TestRecordKeepsADeclaredFeatureID(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":2,"kind":"workflow","title":"x","entrypoint":"main.py","stacks":{"dev":{"url":"http://localhost:8098","tenantID":"development","featureID":"collection-chosen"}}}`))
	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, devKey(), "dev")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	m.Record(lock, sel, Binding{WorkflowID: "workflow-1"})
	if got := m.Stacks["dev"].FeatureID; got != "collection-chosen" {
		t.Fatalf("featureID = %q, want the declared one kept", got)
	}
}

// TestMigrationCarriesTheLegacyEntrysUnknownKeys is the B7 regression. Naming a
// legacy entry MOVES it into stacks and drops the instances[] entry — and
// SetBinding carries an entry's unknown keys across whenever it replaces one, so
// the migration, which is the same move by another name, must too. Otherwise
// naming a stack is the one rewrite that silently loses what a newer CLI wrote.
func TestMigrationCarriesTheLegacyEntrysUnknownKeys(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t,
		`{"formatVersion":1,"kind":"workflow","title":"x","entrypoint":"main.py","instances":[{"url":"http://localhost:8098","tenantID":"development","workflowID":"workflow-1","featureID":"collection-1","deployPolicy":"manual"}]}`))
	m, lock := loadWorkflow(t, root)
	sel, err := m.Select(lock, devKey(), "dev")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !sel.Bound {
		t.Fatal("naming a legacy entry must select bound")
	}
	m.Record(lock, sel, sel.Binding)
	if err := SaveFolder(root, m, lock); err != nil {
		t.Fatalf("SaveFolder: %v", err)
	}
	body, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), `"deployPolicy": "manual"`) {
		t.Fatalf("the migration dropped the legacy entry's unknown keys:\n%s", body)
	}
}

// TestLockTablePreservesUnknownKeys is the B4 regression. The lock's sidecars
// stopped one nesting level short: a key a newer CLI wrote inside
// stacks.<name>.tables.<path> was stripped by any load/save this build did. The
// next thing that goes in there is a per-table version pointer, so the level
// gets its sidecar now, while no file in the field carries one.
func TestLockTablePreservesUnknownKeys(t *testing.T) {
	root := t.TempDir()
	body := `{"stacks":{"dev":{"tables":{"a.sql":{"tableID":"table-1","liveSHA256":"abc","builtAt":"2026-08-31T00:00:00Z"}}}}}` + "\n"
	write(t, root, LockName, body)
	lock, err := LoadLock(root)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	// Rewritten by an ordinary recording, not just an untouched round trip:
	// SetTableLive rebuilds the entry, and building a fresh value there is how
	// the key would be dropped even with the sidecar in place.
	lock.SetTableLive("dev", "a.sql", "table-1", "def")
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(written), `"builtAt"`) {
		t.Fatalf("a load/save stripped an unknown key from a table entry:\n%s", written)
	}
	if !strings.Contains(string(written), `"liveSHA256": "def"`) {
		t.Fatalf("the recorded fingerprint did not survive:\n%s", written)
	}

	// A path rebound to a DIFFERENT table keeps nothing, by the invariant on
	// LockTable: the old row's keys describe something else entirely.
	lock.SetTableLive("dev", "a.sql", "table-2", "ghi")
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, err = os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(written), `"builtAt"`) {
		t.Fatalf("a rebinding kept the old row's keys:\n%s", written)
	}
}

// TestLockRebindingClearsTheAnchor: the head pointer names a version in ONE
// row's history, so a stack repointed at a different row must not keep it. Kept,
// it would be compared against a history it never belonged to — where it can
// only ever fail to match, refusing every push and naming a version the reader
// cannot find. Same invariant SetTableLive enforces for a rebound path.
func TestLockRebindingClearsTheAnchor(t *testing.T) {
	lock := &Lock{}
	lock.SetBinding("dev", Binding{WorkflowID: "workflow-1"})
	lock.SetHeadVersion("dev", "wfv-7")

	// Re-recording the SAME binding keeps it: an ordinary push writes the
	// binding it already had, and dropping the anchor there would disarm the
	// guard on every push.
	lock.SetBinding("dev", Binding{WorkflowID: "workflow-1"})
	if got := lock.HeadVersion("dev"); got != "wfv-7" {
		t.Fatalf("re-recording the same binding dropped the anchor: %q", got)
	}

	lock.SetBinding("dev", Binding{WorkflowID: "workflow-2"})
	if got := lock.HeadVersion("dev"); got != "" {
		t.Errorf("HeadVersion after a rebind = %q, want it cleared", got)
	}
}

// TestLockAnchorRoundTrips: it is a COMMITTED value, so the only thing that
// makes it worth anything is surviving a write and a read by another checkout.
func TestLockAnchorRoundTrips(t *testing.T) {
	root := t.TempDir()
	lock := &Lock{}
	lock.SetBinding("prod", Binding{DataAppID: "dataapp-1"})
	lock.SetHeadVersion("prod", "dav-3")
	if err := SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.HeadVersion("prod"); got != "dav-3" {
		t.Errorf("HeadVersion after a round trip = %q, want dav-3", got)
	}
	// And nothing for a stack that has none — an absent anchor must read as
	// "no answer", never as agreement.
	if got := reloaded.HeadVersion("dev"); got != "" {
		t.Errorf("HeadVersion of an unrecorded stack = %q, want empty", got)
	}
}
