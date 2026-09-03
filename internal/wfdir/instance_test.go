package wfdir

import (
	"errors"
	"testing"
)

// A folder bound to two organizations on ONE instance is the case this key
// exists for. Keyed by URL alone, both would share an entry — and a push made
// while signed in to the second would send the FIRST one's workflow id under
// the second's token.
func TestBindingSeparatesOrganizationsOnOneInstance(t *testing.T) {
	const url = "https://app.ronja.tech"
	m := &Manifest{Kind: KindWorkflow, Title: "x"}
	m.SetBinding(InstanceKey{URL: url, TenantID: "ten-acme"}, Binding{WorkflowID: "wf-acme", FeatureID: "feat-a"})
	m.SetBinding(InstanceKey{URL: url, TenantID: "ten-north"}, Binding{WorkflowID: "wf-north", FeatureID: "feat-n"})

	if len(m.Instances) != 2 {
		t.Fatalf("instances = %+v, want one per organization", m.Instances)
	}
	for _, tt := range []struct{ tenant, want string }{
		{"ten-acme", "wf-acme"},
		{"ten-north", "wf-north"},
	} {
		b, ok, err := m.Binding(InstanceKey{URL: url, TenantID: tt.tenant})
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", tt.tenant, ok, err)
		}
		if b.WorkflowID != tt.want {
			t.Errorf("%s resolved to %s, want %s", tt.tenant, b.WorkflowID, tt.want)
		}
	}

	// A third organization on the same instance is unbound, not "bound to
	// whichever entry sorted first".
	if _, ok, _ := m.Binding(InstanceKey{URL: url, TenantID: "ten-third"}); ok {
		t.Error("an organization with no entry reported as bound")
	}
}

// Re-pushing under the same organization updates its entry rather than adding a
// second one beside it.
func TestSetBindingReplacesSameOrganization(t *testing.T) {
	key := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	m := &Manifest{Kind: KindWorkflow, Title: "x"}
	m.SetBinding(key, Binding{WorkflowID: "wf-1", FeatureID: "feat-1"})
	m.SetBinding(key, Binding{WorkflowID: "wf-2", FeatureID: "feat-1"})

	if len(m.Instances) != 1 {
		t.Fatalf("instances = %+v, want exactly one", m.Instances)
	}
	if m.Instances[0].WorkflowID != "wf-2" {
		t.Errorf("workflowID = %s, want the updated wf-2", m.Instances[0].WorkflowID)
	}
}

// A signed-out `wf status` has no organization to match on. It is a read-only
// command that worked fine before bindings were organization-keyed, so an
// unambiguous folder must keep answering.
func TestUnknownOrganizationMatchesOnURLAlone(t *testing.T) {
	const url = "https://app.ronja.tech"
	m := &Manifest{Kind: KindWorkflow, Title: "x"}
	m.SetBinding(InstanceKey{URL: url, TenantID: "ten-acme"}, Binding{WorkflowID: "wf-acme"})
	m.SetBinding(InstanceKey{URL: "https://staging.ronja.tech", TenantID: "ten-acme"}, Binding{WorkflowID: "wf-staging"})

	entry, ok, err := m.BindingEntry(InstanceKey{URL: url})
	if err != nil || !ok {
		t.Fatalf("unambiguous lookup: ok=%v err=%v", ok, err)
	}
	if entry.WorkflowID != "wf-acme" {
		t.Errorf("workflowID = %s, want wf-acme", entry.WorkflowID)
	}
	// The matched entry names the organization, which is how a caller that did
	// not know it learns it without asking the server.
	if entry.Key().TenantID != "ten-acme" {
		t.Errorf("matched key = %+v, want it to carry the organization", entry.Key())
	}
}

// ...but two organizations on that URL is a genuine coin flip, and picking one
// would show another organization's workflow as if it were this folder's.
func TestUnknownOrganizationRefusesToPickAmongSeveral(t *testing.T) {
	const url = "https://app.ronja.tech"
	m := &Manifest{Kind: KindWorkflow, Title: "x"}
	m.SetBinding(InstanceKey{URL: url, TenantID: "ten-acme"}, Binding{WorkflowID: "wf-acme"})
	m.SetBinding(InstanceKey{URL: url, TenantID: "ten-north"}, Binding{WorkflowID: "wf-north"})

	// Repeatedly: a nondeterministic implementation passes a single check by
	// luck about half the time.
	for i := 0; i < 20; i++ {
		b, ok, err := m.Binding(InstanceKey{URL: url})
		if !errors.Is(err, ErrAmbiguousInstance) {
			t.Fatalf("Binding = %+v (ok %v), err = %v, want ErrAmbiguousInstance", b, ok, err)
		}
		if ok {
			t.Fatal("an ambiguous lookup reported a binding anyway")
		}
	}
}

// A key that names an organization must never adopt an entry that names a
// different one, or none at all — a hand-written entry with no organization
// could belong to anybody.
func TestKnownOrganizationNeverAdoptsAnotherEntry(t *testing.T) {
	const url = "https://app.ronja.tech"
	m := &Manifest{
		Kind: KindWorkflow, Title: "x",
		Instances: []Instance{
			{URL: url, Binding: Binding{WorkflowID: "wf-untagged"}},
			{URL: url, TenantID: "ten-other", Binding: Binding{WorkflowID: "wf-other"}},
		},
	}
	if b, ok, err := m.Binding(InstanceKey{URL: url, TenantID: "ten-mine"}); ok || err != nil {
		t.Errorf("Binding = %+v (ok %v, err %v), want unbound", b, ok, err)
	}
}

// The baseline is keyed the same way, so one organization's sync state cannot
// be read as another's — which would report every file as drifted, or worse, as
// unchanged when it is not.
func TestStateSeparatesOrganizations(t *testing.T) {
	const url = "https://app.ronja.tech"
	acme := InstanceKey{URL: url, TenantID: "ten-acme"}
	north := InstanceKey{URL: url, TenantID: "ten-north"}

	s := &State{}
	s.Set(acme, &InstanceState{SourceID: "wf-acme"})
	s.Set(north, &InstanceState{SourceID: "wf-north"})

	if got := s.For(acme); got == nil || got.SourceID != "wf-acme" {
		t.Errorf("acme baseline = %+v", got)
	}
	if got := s.For(north); got == nil || got.SourceID != "wf-north" {
		t.Errorf("northwind baseline = %+v", got)
	}
	if got := s.For(InstanceKey{URL: url, TenantID: "ten-third"}); got != nil {
		t.Errorf("unrelated organization got baseline %+v, want none", got)
	}
}

// ronja.json is committed and hand-edited, so the two spellings people reach
// for must still match — and must not accumulate a second entry on write.
func TestKeyMatchingToleratesURLSpelling(t *testing.T) {
	m := &Manifest{
		Kind: KindWorkflow, Title: "x",
		Instances: []Instance{
			{URL: "https://APP.ronja.tech/", TenantID: "ten-acme", Binding: Binding{WorkflowID: "wf-1"}},
		},
	}
	canonical := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	if b, ok, _ := m.Binding(canonical); !ok || b.WorkflowID != "wf-1" {
		t.Errorf("Binding = %+v (%v), want wf-1 despite case and trailing slash", b, ok)
	}
	m.SetBinding(canonical, Binding{WorkflowID: "wf-2"})
	if len(m.Instances) != 1 {
		t.Fatalf("instances = %+v, want the equivalent spelling replaced, not duplicated", m.Instances)
	}
	if m.Instances[0].URL != "https://app.ronja.tech" {
		t.Errorf("URL = %q, want the canonical spelling", m.Instances[0].URL)
	}
}

// KeysOn answers two questions whose disagreement is invisible: "which
// organizations does this folder name here" (what a refusal enumerates) and "is
// there anything here to adopt" (what decides whether the caller pays for an
// organization lookup). Both have to see exactly what Find sees, spelling
// included — a rawer comparison would leave a folder that Find matches looking
// empty, and the lookup would go unguarded.
func TestKeysOnSeesWhatFindSees(t *testing.T) {
	instances := []Instance{
		{URL: "https://APP.ronja.tech/", TenantID: "ten-acme"},
		{URL: "https://app.ronja.tech", TenantID: "ten-northwind"},
		{URL: "https://other.ronja.tech", TenantID: "ten-elsewhere"},
	}
	got := KeysOn(instances, Instance.Key, "https://app.ronja.tech/")
	if len(got) != 2 {
		t.Fatalf("KeysOn = %+v, want both spellings of the same instance", got)
	}
	if got[0].TenantID != "ten-acme" || got[1].TenantID != "ten-northwind" {
		t.Errorf("KeysOn = %+v, want slice order preserved", got)
	}
	// The pairing that matters: an unknown organization on this URL is ambiguous
	// exactly when KeysOn reports more than one.
	if _, err := Find(instances, Instance.Key, InstanceKey{URL: "https://app.ronja.tech"}); err != ErrAmbiguousInstance {
		t.Errorf("Find err = %v, want the ambiguity KeysOn just enumerated", err)
	}
	if none := KeysOn(instances, Instance.Key, "https://nothing.ronja.tech"); len(none) != 0 {
		t.Errorf("KeysOn = %+v on an instance the folder does not name", none)
	}
}
