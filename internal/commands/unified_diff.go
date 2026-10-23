package commands

import (
	"fmt"
	"strings"
)

// unifiedDiff renders one file's change as a unified diff — the format `git
// diff` prints, so the author reads `ronja <kind> pull`'s output the way they
// will read the commit it asks them to make. nil is an absent file: before nil
// is a file the pull adds, after nil one it deletes. Three lines of context.
//
// The line diff is Myers' O(ND) algorithm, which stays linear in the size of
// the files for the small edits a pull carries. "" for no change.
func unifiedDiff(path string, before, after *string) string {
	if before != nil && after != nil && *before == *after {
		return ""
	}
	if before == nil && after == nil {
		return ""
	}
	a, b := splitDiffLines(before), splitDiffLines(after)
	from, to := "a/"+path, "b/"+path
	if before == nil {
		from = "/dev/null"
	}
	if after == nil {
		to = "/dev/null"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for _, h := range diffHunks(myersDiff(a.lines, b.lines), 3) {
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(h.aStart, h.aLen), hunkRange(h.bStart, h.bLen))
		for _, op := range h.ops {
			switch op.kind {
			case diffEqual:
				out.WriteString(" " + a.lines[op.a] + "\n")
			case diffDelete:
				out.WriteString("-" + a.lines[op.a] + "\n")
				if op.a == len(a.lines)-1 && a.noNewline {
					out.WriteString("\\ No newline at end of file\n")
				}
			case diffInsert:
				out.WriteString("+" + b.lines[op.b] + "\n")
				if op.b == len(b.lines)-1 && b.noNewline {
					out.WriteString("\\ No newline at end of file\n")
				}
			}
		}
	}
	return out.String()
}

type diffText struct {
	lines     []string
	noNewline bool
}

func splitDiffLines(s *string) diffText {
	if s == nil || *s == "" {
		return diffText{}
	}
	text := *s
	noNewline := !strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	return diffText{lines: strings.Split(text, "\n"), noNewline: noNewline}
}

func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffDelete
	diffInsert
)

// diffOp is one line of an edit script: a's index for equal and delete, b's
// for equal and insert.
type diffOp struct {
	kind diffKind
	a, b int
}

// myersDiff is the shortest edit script from a to b.
func myersDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil
	}
	offset := max
	v := make([]int, 2*max+2)
	var trace [][]int
	for d := 0; d <= max; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, offset, d)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int, a, b []string, offset, dEnd int) []diffOp {
	x, y := len(a), len(b)
	var ops []diffOp
	for d := dEnd; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			ops = append(ops, diffOp{kind: diffEqual, a: x, b: y})
		}
		if d > 0 {
			if x == prevX {
				y--
				ops = append(ops, diffOp{kind: diffInsert, a: x, b: y})
			} else {
				x--
				ops = append(ops, diffOp{kind: diffDelete, a: x, b: y})
			}
		}
	}
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

type diffHunk struct {
	aStart, aLen, bStart, bLen int
	ops                        []diffOp
}

// diffHunks groups an edit script into hunks with `context` equal lines around
// each change, merging changes whose context would overlap.
func diffHunks(ops []diffOp, context int) []diffHunk {
	var hunks []diffHunk
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == diffEqual {
			i++
		}
		if i >= len(ops) {
			break
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(ops) {
			if ops[end].kind != diffEqual {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == diffEqual {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end += min(context, run-end)
				break
			}
			end = run
		}
		h := diffHunk{ops: ops[start:end]}
		first := ops[start]
		h.aStart, h.bStart = first.a, first.b
		for _, op := range h.ops {
			switch op.kind {
			case diffEqual:
				h.aLen++
				h.bLen++
			case diffDelete:
				h.aLen++
			case diffInsert:
				h.bLen++
			}
		}
		hunks = append(hunks, h)
		i = end
	}
	return hunks
}
