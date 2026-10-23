package commands

import "testing"

// The three arms of the name rule, plus how a baseline is found when nothing
// is recorded. Each arm is its own case so deleting one fails here.
func TestDecideNameArms(t *testing.T) {
	d := func(s string) *string { return &s }
	cases := []struct {
		name     string
		declared *string
		live     string
		base     nameBaseline
		want     nameAction
	}{
		{"absent is unmanaged", nil, "anything", nameBaseline{Stem: "orders"}, nameUnmanaged},
		{"live equals desired", d("Sales orders"), "Sales orders", nameBaseline{Recorded: "stale"}, nameAgrees},
		{"live equals desired, trimmed", d(" Sales orders "), "Sales orders", nameBaseline{Stem: "orders"}, nameAgrees},
		{"live is the recorded baseline", d("Sales orders"), "Old", nameBaseline{Recorded: "Old", Stem: "orders"}, nameRename},
		{"live is the stem", d("Sales orders"), "orders", nameBaseline{Stem: "orders"}, nameRename},
		{"recorded outranks the stem", d("Sales orders"), "orders", nameBaseline{Recorded: "Old", Stem: "orders"}, nameDrift},
		// No slug equivalence: a live name that merely SLUGS to the stem may be a
		// colleague's punctuation-only rename, and reading it as the baseline
		// would revert it. A clone records the exact name instead.
		{"a name that slugs to the stem is not the stem", d("X"), "Sales: orders", nameBaseline{Stem: "Sales-orders"}, nameDrift},
		{"case is not folded", d("Sales orders"), "ORDERS", nameBaseline{Stem: "orders"}, nameDrift},
		{"renamed elsewhere", d("Sales orders"), "Web", nameBaseline{Recorded: "Old"}, nameDrift},
		{"no baseline at all", d("Title"), "Other", nameBaseline{}, nameUnanchored},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideName(c.declared, c.live, c.base); got != c.want {
				t.Errorf("decideName = %q, want %q", got, c.want)
			}
		})
	}
}
