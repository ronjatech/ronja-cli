package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// How one observation is narrated to a terminal: what is printed, in what
// order, and what is withheld. Everything the server said is untrusted text
// rendered from tenant data, so every string that reaches stdout goes through
// displayLine first.

// printAppTestReport is the human summary: what was seen, in the order it is
// worth reading, and where the files went.
//
// Deliberately NOT printed: step DOM text and the app's own console messages
// beyond a count. Both are untrusted content rendered from tenant data, and a
// terminal gives a reader nothing to tell them apart from the CLI's own words.
// They are in report.json, which is a file somebody chose to open.
func printAppTestReport(r *api.PreviewResult, files []appTestFile) {
	out := os.Stdout

	fmt.Fprintf(out, "  App:      %s\n", r.DataAppID)
	if r.Route != "" {
		fmt.Fprintf(out, "  Route:    %s\n", r.Route)
	}
	if r.Viewport != "" {
		fmt.Fprintf(out, "  Viewport: %s\n", r.Viewport)
	}

	switch {
	case len(r.CompileDiagnostics) > 0:
		fmt.Fprintf(out, "  Rendered: no — the bundle did not build\n")
	case r.Rendered:
		fmt.Fprintf(out, "  Rendered: yes (%s)\n", (time.Duration(r.DurationMs) * time.Millisecond).Round(time.Millisecond))
	default:
		fmt.Fprintf(out, "  Rendered: no\n")
	}
	if r.Settle != nil && r.Settle.Signal != "" {
		fmt.Fprintf(out, "  Settled:  %s — %s\n", r.Settle.Signal, describeSettleSignal(r.Settle.Signal))
	}

	if len(r.CompileDiagnostics) > 0 {
		fmt.Fprintf(out, "\n  Build errors:\n")
		for _, d := range r.CompileDiagnostics {
			fmt.Fprintf(out, "    %s\n", formatCompileDiagnostic(d))
		}
	}

	fmt.Fprintf(out, "\n  Operations:      %d\n", len(r.Ops))
	fmt.Fprintf(out, "  Runtime errors:  %d\n", len(r.Errors))
	fmt.Fprintf(out, "  Console errors:  %d\n", len(r.ConsoleErrors))
	if len(r.NetworkFailures) > 0 {
		fmt.Fprintf(out, "  Network failed:  %d\n", len(r.NetworkFailures))
	}
	if len(r.Steps) > 0 {
		ok, skipped, failed := countSteps(r.Steps)
		fmt.Fprintf(out, "  Steps:           %d ok, %d skipped, %d failed\n", ok, skipped, failed)
	}

	// The errors themselves, with the source-mapped frame — the one thing
	// somebody reads this output for when it is not green.
	if len(r.Errors) > 0 {
		fmt.Fprintf(out, "\n  Errors:\n")
		for _, e := range r.Errors {
			if e.Source != "" {
				// The source path is app-authored, so it is disarmed too.
				fmt.Fprintf(out, "    %s:%d:%d  %s\n", displayLine(e.Source), e.Line, e.Column, displayLine(e.Message))
				continue
			}
			fmt.Fprintf(out, "    %s\n", displayLine(e.Message))
		}
	}
	// A failed step names what it could not do. A SKIPPED one is called out
	// separately because it never ran at all, and reading its ok:false as an app
	// failure is the single most likely misreading of this whole report.
	if len(r.Steps) > 0 {
		printAppTestSteps(out, r.Steps)
	}

	if r.Hint != "" {
		// Disarmed like every other server string here: the hint quotes what the
		// render saw, so it can carry the app's own text.
		fmt.Fprintf(out, "\n  %s\n", displayLine(r.Hint))
	}

	fmt.Fprintf(out, "\n  Wrote:\n")
	for _, f := range files {
		suffix := ""
		switch {
		case f.Kind == "screenshot" && filepath.Base(f.Path) == appTestSelectedName:
			suffix = "  (the representative frame)"
		case f.Phase != "":
			suffix = "  (" + f.Phase + ")"
		}
		fmt.Fprintf(out, "    %s%s\n", f.Path, suffix)
	}
	// Only the report on disk has two very different causes, and conflating them
	// sends a reader to look at the wrong thing: nothing to fetch is a fact about
	// the RENDER, everything failing to fetch is a fact about this machine's
	// reach to the instance's file storage.
	if len(files) == 1 {
		if len(r.Screenshots) == 0 {
			fmt.Fprintf(out, "\n  No frames were captured — the render produced none, or this instance has no\n  file storage wired for them.\n")
		} else {
			fmt.Fprintf(out, "\n  The render captured %d %s, but none of them could be fetched — see the\n  warnings above. The observation itself is complete and is in report.json.\n",
				len(r.Screenshots), plural(len(r.Screenshots), "frame"))
		}
	}

	fmt.Fprintf(out, "\n  This is what the browser saw, not a verdict. `ronja app publish` is unaffected\n  by it either way.\n")

	// The server's own calibration, last, beside the line that says the same
	// thing in this command's words — it qualifies the WHOLE observation, so
	// printing it above the evidence would put a caveat before the thing it
	// caveats. AppOutputNote goes first because it is narrower: it says what the
	// error messages printed further up are, and it arrives only when there were
	// some.
	//
	// Both are static server prose rather than app text. They still go through
	// displayLine, because the rule in this file is that nothing the server said
	// reaches stdout unfiltered — a rule with an exception for "the strings we
	// believe are ours" is a rule that stops being checked.
	if r.AppOutputNote != "" {
		fmt.Fprintf(out, "\n  %s\n", displayLine(r.AppOutputNote))
	}
	if r.Calibration != "" {
		fmt.Fprintf(out, "\n  %s\n", displayLine(r.Calibration))
	}
}

// describeSettleSignal spells out the three signals, because "deadline" reads
// as a failure and is not one — it means the frame shows a page that was still
// working when the budget ran out.
func describeSettleSignal(signal string) string {
	switch signal {
	case "sdk":
		return "the app reported every operation finished"
	case "heuristic":
		return "network and DOM went quiet"
	case "deadline":
		return "the render budget ran out, so the frame shows a page still working"
	default:
		return "a signal this version of the CLI does not know; see report.json"
	}
}

func countSteps(steps []api.PreviewStepResult) (ok, skipped, failed int) {
	for _, s := range steps {
		switch {
		case s.Skipped != "":
			skipped++
		case s.OK:
			ok++
		default:
			failed++
		}
	}
	return ok, skipped, failed
}

func printAppTestSteps(out *os.File, steps []api.PreviewStepResult) {
	shown := steps
	if len(shown) > appTestMaxStepsShown {
		shown = shown[:appTestMaxStepsShown]
	}
	printed := false
	for _, s := range shown {
		if s.OK && s.Skipped == "" {
			continue
		}
		if !printed {
			fmt.Fprintf(out, "\n  Steps:\n")
			printed = true
		}
		if s.Skipped != "" {
			fmt.Fprintf(out, "    step %d %-10s skipped — %s (it never ran, so this says nothing about the app)\n",
				s.Index, s.Action, displayLine(s.Skipped))
			continue
		}
		fmt.Fprintf(out, "    step %d %-10s failed — %s\n", s.Index, s.Action, displayLine(s.Error))
	}
}

func formatCompileDiagnostic(d api.CompileDiagnostic) string {
	// The path is app-authored too — it is whatever the source imported. The
	// bundler's namespace comes off first, in the form the SERVER wrote it and
	// before the disarm can change the bytes, exactly as displayLine takes the
	// untrusted envelope off first and for the same reason.
	where := displayLine(stripBundleNamespacePath(d.File))
	if where == "" {
		where = "?"
	}
	return fmt.Sprintf("%s:%d:%d  %s", where, d.Line, d.Column, displayLine(d.Message))
}

// displayLine is what every server string goes through on its way to stdout:
// the untrusted envelope off, then the disarm.
//
// THE ORDER IS DELIBERATE, AND IT IS STRIP-THEN-DISARM. The envelope is bytes
// the SERVER wrote, so it is recognised in the form the server wrote it —
// before anything has had a chance to change the string. Disarming first would
// make the anchor test depend on the app's own content: a message beginning
// with a NUL or a zero-width space would have that character dropped, the tag
// underneath would slide into first position, and app text could delete its own
// envelope simply by prefixing an invisible character to it.
//
// ⚠️ This is DISPLAY only. report.json is written from PreviewOutcome.Raw and
// keeps the envelope, because the file is what a reader — or another agent —
// opens to see exactly how the server framed each string.
func displayLine(s string) string {
	return oneLine(stripUntrustedEnvelope(s))
}

// The two halves of an <untrusted_*> envelope, matched by PREFIX rather than by
// name. The server already has three of them (<untrusted_app_output> around an
// error or console message, <untrusted_page_text> around a step's DOM snapshot,
// <untrusted_viewer_error> elsewhere in the agent surface) and will grow more; a
// CLI that knew only the one it was written for would print the others' tags at
// a reader, which is the exact confusion the tags exist to prevent.
const (
	untrustedOpenPrefix  = "<untrusted_"
	untrustedClosePrefix = "</untrusted_"
)

// stripUntrustedEnvelope removes ONE leading open tag and ONE trailing close
// tag, both ANCHORED — never every occurrence anywhere in the string.
//
// That restraint is the whole design. The envelope wraps text the app chose, and
// an app whose error message literally contains "</untrusted_app_output>" must
// not be able to erase the rest of its own message from this report by saying
// so. Anchored, the server's frame comes off and the app's copy of it stays
// visible as the ordinary characters it is.
//
// The backend's lib/untrusted already disarms marker prefixes where these
// strings enter the system, so a message carrying a tag should not reach us at
// all; this is the second layer, held to the same rule the first one is.
func stripUntrustedEnvelope(s string) string {
	if strings.HasPrefix(s, untrustedOpenPrefix) {
		if end := strings.IndexByte(s, '>'); end > 0 && isUntrustedTagName(s[len(untrustedOpenPrefix):end]) {
			s = s[end+1:]
		}
	}
	if strings.HasSuffix(s, ">") {
		// The last '<' in a string ending in '>' opens the final tag, and a tag
		// name holds no '<' of its own — so this either is the close tag or is
		// not a tag at all.
		if start := strings.LastIndexByte(s, '<'); start >= 0 &&
			strings.HasPrefix(s[start:], untrustedClosePrefix) &&
			isUntrustedTagName(s[start+len(untrustedClosePrefix):len(s)-1]) {
			s = s[:start]
		}
	}
	return s
}

// isUntrustedTagName holds the tag grammar to what the server actually emits —
// a non-empty run of lowercase letters, digits and underscores. It bounds what
// counts as an envelope, so "<untrusted_ whatever the app typed>" is text that
// gets printed rather than a frame that gets believed.
func isUntrustedTagName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// oneLine flattens a message so a multi-line stack or diagnostic cannot break
// the report's shape, and DISARMS it so it cannot rewrite the terminal.
//
// Not called directly from the report: server strings go through displayLine,
// which takes the untrusted envelope off first.
//
// Everything this is applied to — a runtime error's message, a compile
// diagnostic, a step's error or skip reason, the server's hint — is text the
// APP produced, which means it can carry tenant data or anything an author put
// in a string literal. Flattening newlines was never enough: an ESC starts an
// ANSI sequence that repaints lines the CLI already printed (so a failing render
// can print itself a clean one), and a bidi override or a zero-width character
// reorders or hides what is left. A terminal gives a reader nothing to tell that
// apart from the command's own words.
//
// The rune classes mirror backend/lib/untrusted, which is the reference for what
// counts as dangerous here. They are re-stated rather than imported because the
// CLI is its own Go module and does not import the backend — a leaf filter of a
// dozen lines is the right price for that boundary.
func oneLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		// C0 controls and DEL, plus the C1 block (0x80–0x9F, which holds a
		// single-byte CSI). Whitespace becomes a space so words do not run
		// together; everything else is dropped.
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			continue
		// Bidi overrides and isolates: they reorder the rest of the line.
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			continue
		// Zero-width and invisible formatting: ZWSP/ZWNJ/ZWJ, LRM/RLM, the word
		// joiner, BOM/ZWNBSP, and the invisible-operator block.
		case r >= 0x200b && r <= 0x200f, r == 0x2060, r == 0xfeff,
			r >= 0x2061 && r <= 0x2064:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
