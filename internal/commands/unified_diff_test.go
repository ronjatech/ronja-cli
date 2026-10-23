package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

// TestUnifiedDiffMatchesGit: the pull's diff is the one `git diff` prints, so
// the author reads it as they will read the commit. Checked against git itself
// over edits at the start, the middle and the end, an added and a deleted file,
// and a missing final newline.
func TestUnifiedDiffMatchesGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git binary")
	}
	long := ""
	for i := 1; i <= 30; i++ {
		long += "line " + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + "\n"
	}
	cases := []struct {
		name          string
		before, after *string
	}{
		{"middle", strp("a\nb\nc\nd\ne\nf\ng\n"), strp("a\nb\nc\nD\ne\nf\ng\n")},
		{"start and end", strp(long), strp("new first\n" + strings.Replace(long, "line xd", "line XD", 1) + "tail\n")},
		{"two hunks", strp(long), strp(strings.Replace(strings.Replace(long, "line b", "B", 1), "line xxe", "E", 1))},
		{"added", nil, strp("one\ntwo\n")},
		{"deleted", strp("one\ntwo\n"), nil},
		{"no final newline", strp("a\nb"), strp("a\nc")},
		{"insert only", strp("a\nb\n"), strp("a\nx\nb\n")},
		{"emptied", strp("a\n"), strp("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name string, s *string) string {
				p := filepath.Join(dir, name)
				if s == nil {
					return "/dev/null"
				}
				if err := os.WriteFile(p, []byte(*s), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			}
			pa, pb := write("a", c.before), write("b", c.after)
			out, _ := exec.Command(git, "diff", "--no-index", "--no-color", "-U3", pa, pb).Output()
			// Keep git's hunks only: its header lines name temp paths.
			var want []string
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") ||
					strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "new file") || strings.HasPrefix(line, "deleted file") {
					continue
				}
				// git appends a function-context heading after a hunk's range.
				if strings.HasPrefix(line, "@@ ") {
					if end := strings.Index(line[3:], " @@"); end >= 0 {
						line = line[:3+end+3]
					}
				}
				want = append(want, line)
			}
			got := unifiedDiff("f.txt", c.before, c.after)
			lines := strings.Split(got, "\n")
			if len(lines) < 2 {
				t.Fatalf("no diff: %q", got)
			}
			if g, w := strings.Join(lines[2:], "\n"), strings.Join(want, "\n"); g != w {
				t.Fatalf("hunks differ from git's\n--- got\n%s\n--- want\n%s", g, w)
			}
		})
	}
	if d := unifiedDiff("f", strp("same\n"), strp("same\n")); d != "" {
		t.Fatalf("an unchanged file diffed: %q", d)
	}
}
