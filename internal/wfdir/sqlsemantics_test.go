package wfdir

import "testing"

// The server moves an artifact to raw escapes on its own, so a stray
// `"sqlSemantics"` key in a committed ronja.json must load like any unknown key,
// whatever its value.
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
