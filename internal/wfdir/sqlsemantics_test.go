package wfdir

import "testing"

// A manifest has nothing to say about how its SQL reaches DuckDB: the server
// moves an artifact to raw escapes on its own the first time a write changes
// its code. A `"sqlSemantics"` key left in a committed ronja.json — written by a
// pre-release build of this CLI, or by hand — must therefore load like any
// other key this build does not know, whatever its value: never a refusal, and
// never read by a push.
func TestManifestIgnoresAStraySQLSemanticsKey(t *testing.T) {
	for _, value := range []string{"raw", "legacy", "nonsense"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, `{"kind":"workflow","title":"x","sqlSemantics":"`+value+`"}`)
			if _, err := LoadManifest(root, WorkflowKind); err != nil {
				t.Fatalf("load: %v — a stray sqlSemantics key must never be a refusal", err)
			}
		})
	}
}
