package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// seedManifest writes a valid manifest of a kind at dir, creating dir.
func seedManifest(t *testing.T, dir string, kind wfdir.Kind, m *wfdir.Manifest) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if m == nil {
		m = &wfdir.Manifest{Title: filepath.Base(dir)}
	}
	m.Kind = kind.Name
	if m.Entrypoint == "" {
		m.Entrypoint = kind.DefaultEntrypoint
	}
	if err := wfdir.SaveManifest(dir, m); err != nil {
		t.Fatalf("write manifest at %s: %v", dir, err)
	}
	return dir
}

// seedRawManifest writes whatever bytes it is given as a folder's ronja.json,
// which is how the unreadable cases are staged — a manifest LoadManifest
// refuses cannot be produced by SaveManifest.
func seedRawManifest(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, wfdir.ManifestName), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}
	return dir
}

// The walk is the one part of `ronja sync` that decides WHICH folders the
// command is about, so its edge cases are the ones where a wrong answer is
// invisible: a folder missed reads exactly like a folder that is fine.
func TestSyncDiscoverFolders(t *testing.T) {
	tests := []struct {
		name string
		// seed lays out a tree under base and returns nothing; assertions are
		// on the relative paths and reasons that come back.
		seed func(t *testing.T, base string)
		// want is the relative path of every folder discovered, in order.
		want []string
		// reasons maps a relative path to the not-checked reason it must carry.
		// A path absent from the map must be checkable.
		reasons map[string]string
	}{
		{
			name: "an empty tree finds nothing",
			seed: func(t *testing.T, base string) {
				if err := os.MkdirAll(filepath.Join(base, "src", "lib"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: nil,
		},
		{
			name: "the walk root itself is a folder",
			seed: func(t *testing.T, base string) {
				seedManifest(t, base, wfdir.WorkflowKind, nil)
			},
			want: []string{"."},
		},
		{
			name: "siblings of three kinds are all found",
			seed: func(t *testing.T, base string) {
				seedManifest(t, filepath.Join(base, "app"), wfdir.DataAppKind, nil)
				seedManifest(t, filepath.Join(base, "pipe"), wfdir.PipelineKind, nil)
				seedManifest(t, filepath.Join(base, "wf"), wfdir.WorkflowKind, nil)
			},
			want: []string{"app", "pipe", "wf"},
		},
		{
			name: "dot directories and build output are pruned by name",
			seed: func(t *testing.T, base string) {
				seedManifest(t, filepath.Join(base, "keep"), wfdir.WorkflowKind, nil)
				// Each of these would be discovered by a walk that did not
				// prune, and .git is the one that would make the walk expensive
				// as well as wrong.
				seedManifest(t, filepath.Join(base, ".git", "hidden"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, ".cache"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, "node_modules", "pkg"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, "dist"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, "build", "out"), wfdir.WorkflowKind, nil)
			},
			want: []string{"keep"},
		},
		{
			name: "a nested root is reported, never silently dropped",
			seed: func(t *testing.T, base string) {
				seedManifest(t, filepath.Join(base, "outer"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, "outer", "inner"), wfdir.DataAppKind, nil)
			},
			want:    []string{"outer", "outer/inner"},
			reasons: map[string]string{"outer/inner": syncReasonNestedRoot},
		},
		{
			name: "a sibling whose name PREFIXES another is not nested inside it",
			seed: func(t *testing.T, base string) {
				// The bug a bare strings.HasPrefix produces: `order` is not an
				// ancestor of `orders`, and reading it as one would mark a
				// perfectly ordinary folder not_checked for ever.
				seedManifest(t, filepath.Join(base, "order"), wfdir.WorkflowKind, nil)
				seedManifest(t, filepath.Join(base, "orders"), wfdir.WorkflowKind, nil)
			},
			want: []string{"order", "orders"},
		},
		{
			name: "a manifest from the future is unreadable, not skipped",
			seed: func(t *testing.T, base string) {
				seedManifest(t, filepath.Join(base, "ok"), wfdir.WorkflowKind, nil)
				seedRawManifest(t, filepath.Join(base, "future"),
					`{"formatVersion": 9999, "kind": "workflow", "title": "t"}`)
			},
			want:    []string{"future", "ok"},
			reasons: map[string]string{"future": syncReasonUnreadable},
		},
		{
			name: "a manifest that will not parse is unreadable",
			seed: func(t *testing.T, base string) {
				seedRawManifest(t, filepath.Join(base, "broken"), `{"kind": "workflow"`)
			},
			want:    []string{"broken"},
			reasons: map[string]string{"broken": syncReasonUnreadable},
		},
		{
			// A `ronja db` folder keeps no committed manifest by design, but a
			// hand-written one must not be read as a workflow — which is what
			// the kind probe's fallback would do if the walk took it.
			name: "a manifest of a kind this build has no loop for is unreadable",
			seed: func(t *testing.T, base string) {
				seedRawManifest(t, filepath.Join(base, "db"), `{"kind": "database", "title": "Ops"}`)
			},
			want:    []string{"db"},
			reasons: map[string]string{"db": syncReasonUnreadable},
		},
		{
			name: "a manifest declaring no kind at all is unreadable",
			seed: func(t *testing.T, base string) {
				seedRawManifest(t, filepath.Join(base, "nokind"), `{"title": "Ops"}`)
			},
			want:    []string{"nokind"},
			reasons: map[string]string{"nokind": syncReasonUnreadable},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tc.seed(t, base)

			found, err := discoverFolders(base)
			if err != nil {
				t.Fatalf("discoverFolders: %v", err)
			}
			var got []string
			for _, f := range found {
				got = append(got, f.Rel)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("folders = %v, want %v", got, tc.want)
			}
			for _, f := range found {
				want := tc.reasons[f.Rel]
				if f.Reason != want {
					t.Errorf("%s: reason = %q, want %q (detail: %s)", f.Rel, f.Reason, want, f.Detail)
				}
				// A folder with no reason must have been loaded, because the
				// stack resolution reads the manifest off it rather than
				// re-reading the file.
				if f.Reason == "" && f.Manifest == nil {
					t.Errorf("%s: checkable folder carries no manifest", f.Rel)
				}
			}
		})
	}
}

// A folder's kind must come from its own manifest — a tree command that guessed
// would run a data app's status computation over a pipeline's SQL.
func TestSyncDiscoverFoldersReportsKind(t *testing.T) {
	base := t.TempDir()
	seedManifest(t, filepath.Join(base, "app"), wfdir.DataAppKind, nil)
	seedManifest(t, filepath.Join(base, "pipe"), wfdir.PipelineKind, nil)
	seedManifest(t, filepath.Join(base, "wf"), wfdir.WorkflowKind, nil)

	found, err := discoverFolders(base)
	if err != nil {
		t.Fatalf("discoverFolders: %v", err)
	}
	want := map[string]string{
		"app":  wfdir.KindDataApp,
		"pipe": wfdir.KindPipeline,
		"wf":   wfdir.KindWorkflow,
	}
	for _, f := range found {
		if f.Kind.Name != want[f.Rel] {
			t.Errorf("%s: kind = %q, want %q", f.Rel, f.Kind.Name, want[f.Rel])
		}
	}
}

// The kind probe falls back to "workflow" for a manifest that declares
// something else, which is right for `ronja bind` — it wants LoadManifest's own
// refusals — and wrong to REPORT. A tree report that printed `kind: workflow`
// beside a folder whose file says `database` would be a confident lie in the
// one field a reader uses to tell the folders apart.
func TestSyncUnknownKindClaimsNoKind(t *testing.T) {
	base := t.TempDir()
	seedRawManifest(t, filepath.Join(base, "db"), `{"kind": "database", "title": "Ops"}`)
	seedRawManifest(t, filepath.Join(base, "nokind"), `{"title": "Ops"}`)

	found, err := discoverFolders(base)
	if err != nil {
		t.Fatalf("discoverFolders: %v", err)
	}
	for _, f := range found {
		if f.Reason != syncReasonUnreadable {
			t.Errorf("%s: reason = %q, want %q", f.Rel, f.Reason, syncReasonUnreadable)
		}
		if f.Kind.Name != "" {
			t.Errorf("%s: claims kind %q", f.Rel, f.Kind.Name)
		}
	}
}
