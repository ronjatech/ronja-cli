package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// How one `ronja app test` observation is narrated to a terminal. ⚠️ Every
// server string is untrusted tenant-derived text and reaches stdout only
// through displayLine.

// printAppTestReport is the human summary. Step DOM text and console messages
// are deliberately not printed beyond a count: a terminal cannot tell untrusted
// text from the CLI's own words; they are in report.json.
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
	// A skipped step is called out separately: it never ran, and its ok:false is
	// not an app failure.
	if len(r.Steps) > 0 {
		printAppTestSteps(out, r.Steps)
	}

	if r.Hint != "" {
		// The hint can quote the app's own text.
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
	// Only report.json on disk has two causes: no frames captured (the render),
	// or none fetchable (this machine's reach to file storage).
	if len(files) == 1 {
		if len(r.Screenshots) == 0 {
			fmt.Fprintf(out, "\n  No frames were captured — the render produced none, or this instance has no\n  file storage wired for them.\n")
		} else {
			fmt.Fprintf(out, "\n  The render captured %d %s, but none of them could be fetched — see the\n  warnings above. The observation itself is complete and is in report.json.\n",
				len(r.Screenshots), plural(len(r.Screenshots), "frame"))
		}
	}

	fmt.Fprintf(out, "\n  This is what the browser saw, not a verdict. `ronja app publish` is unaffected\n  by it either way.\n")

	// The server's calibration qualifies the whole observation, so it comes
	// last. These are static server prose but still go through displayLine:
	// the rule has no exceptions.
	if r.AppOutputNote != "" {
		fmt.Fprintf(out, "\n  %s\n", displayLine(r.AppOutputNote))
	}
	if r.Calibration != "" {
		fmt.Fprintf(out, "\n  %s\n", displayLine(r.Calibration))
	}
}

// describeSettleSignal spells out the signals, since "deadline" reads as a
// failure and is not one.
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
	// The path is app-authored. The bundler namespace is stripped before the
	// disarm, for the same reason displayLine strips the envelope first.
	where := displayLine(stripBundleNamespacePath(d.File))
	if where == "" {
		where = "?"
	}
	return fmt.Sprintf("%s:%d:%d  %s", where, d.Line, d.Column, displayLine(d.Message))
}

// displayLine is what every server string goes through on its way to stdout.
// ⚠️ Strip-then-disarm, deliberately: disarming first would let app text that
// starts with an invisible character slide its own tag into the anchored
// position and delete the envelope. Display only; report.json keeps the
// envelope (it is written from PreviewOutcome.Raw).
func displayLine(s string) string {
	return oneLine(stripUntrustedEnvelope(s))
}

// The two halves of an <untrusted_*> envelope, matched by prefix rather than
// name so envelopes the server adds later are stripped too.
const (
	untrustedOpenPrefix  = "<untrusted_"
	untrustedClosePrefix = "</untrusted_"
)

// stripUntrustedEnvelope removes one leading open tag and one trailing close
// tag, both anchored, never every occurrence: an app message containing a tag
// must not be able to erase part of itself. A second layer behind the backend's
// lib/untrusted, which disarms markers on the way in.
func stripUntrustedEnvelope(s string) string {
	if strings.HasPrefix(s, untrustedOpenPrefix) {
		if end := strings.IndexByte(s, '>'); end > 0 && isUntrustedTagName(s[len(untrustedOpenPrefix):end]) {
			s = s[end+1:]
		}
	}
	if strings.HasSuffix(s, ">") {
		// A tag name holds no '<', so the last '<' either opens the close tag
		// or there is none.
		if start := strings.LastIndexByte(s, '<'); start >= 0 &&
			strings.HasPrefix(s[start:], untrustedClosePrefix) &&
			isUntrustedTagName(s[start+len(untrustedClosePrefix):len(s)-1]) {
			s = s[:start]
		}
	}
	return s
}

// isUntrustedTagName holds the tag grammar to what the server emits (lowercase
// letters, digits, underscores), so arbitrary app text is never taken for an
// envelope.
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

// oneLine flattens a message to one line and disarms it: control characters
// (an ESC could repaint lines already printed), bidi overrides and zero-width
// characters are dropped. Server strings reach it through displayLine. The rune
// classes mirror backend/lib/untrusted, restated because this module cannot
// import the backend.
func oneLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		// C0, DEL and C1 (which holds a one-byte CSI); whitespace becomes a
		// space so words do not run together.
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			continue
		// Bidi overrides and isolates.
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			continue
		// Zero-width and invisible formatting characters.
		case r >= 0x200b && r <= 0x200f, r == 0x2060, r == 0xfeff,
			r >= 0x2061 && r <= 0x2064:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
