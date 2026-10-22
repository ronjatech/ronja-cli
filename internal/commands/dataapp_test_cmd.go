package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// The file is dataapp_test_cmd.go, not dataapp_test.go, because the latter is
// already a TEST file — and would never be compiled into the binary anyway. Same
// reason workflow_testcmd.go carries its suffix.

// `ronja app test` renders the app and reports what the browser saw.
//
// It is NOT the analogue of `ronja wf test`, and the difference decides the exit
// code. A workflow run has an outcome — it succeeded or it failed — so `wf test`
// exits zero only on success. A render has no such verdict, and the endpoint
// behind this command deliberately carries no pass/fail field: a compile
// failure, a blank page and an app that painted perfectly are all OBSERVATIONS,
// and which of them counts as a failure depends on what the caller was asking.
// So this exits ZERO whenever the harness ran, and --fail-on-errors is how a CI
// job opts into the stricter reading.
//
// What it is non-zero for is the harness not running: a transport failure, an
// HTTP error, or a busy pool that stayed busy through the one retry. Those are
// the cases where NOTHING about the app was observed, and reporting them as a
// clean render would be the one genuinely misleading answer available.

// The two viewport presets the endpoint knows. Refused locally rather than
// passed through, because the server resolves an unknown preset to desktop
// instead of failing — so `--viewport mobil` would silently answer a question
// about the desktop layout and look like a successful mobile test.
const (
	viewportDesktop = "desktop"
	viewportMobile  = "mobile"
)

// viewportPresetSizes is what each preset renders at, so an echoed preset name
// can be compared with a WxH this command asked for.
var viewportPresetSizes = map[string]api.PreviewViewport{
	viewportDesktop: {Width: 1280, Height: 800},
	viewportMobile:  {Width: 390, Height: 844},
}

// The range of one side of a --viewport WxH, mirroring the server's own
// refusal (and the render harness's clamp). Checked locally so a typo costs no
// round trip.
const (
	viewportMinSide = 240
	viewportMaxSide = 2560
)

// appTestStepsTimeout is the render budget sent with --steps when --timeout is
// not given: the endpoint's ceiling. A step plan is carved out of the same
// budget the render settles in, so the plain-render default starves both.
const appTestStepsTimeout = 40 * time.Second

// The busy-retry schedule. Package vars, like runPollInterval, so the tests can
// exercise the whole retry path without sleeping through it.
var (
	// previewBusyRetryDelay is used when the 429 named no Retry-After. Sized
	// against a typical render (15 s by default, 40 s at most), since the usual
	// cause of a busy answer is the tenant's concurrency slots being held by
	// renders about to finish.
	previewBusyRetryDelay = 15 * time.Second
	// previewBusyRetryCap bounds what a Retry-After can ask for. A server is
	// entitled to name an interval; it is not entitled to park an interactive
	// command for an hour, and a caller who wants to wait that long can loop.
	previewBusyRetryCap = 60 * time.Second
)

// Names of the files written into --out-dir. Fixed rather than derived from the
// app id or the run: a caller scripting this needs to know where the report is
// without parsing anything, and re-running overwrites rather than accumulating.
// How many interaction steps the human report lists before it stops. The
// artefact names, their permissions and where they go all live with the code
// that writes them, in dataapp_test_output.go.
const appTestMaxStepsShown = 20

func newDataAppTestCmd() *cobra.Command {
	var (
		route        string
		stepsFile    string
		viewport     string
		timeout      time.Duration
		outDir       string
		failOnErrors bool
	)
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Render the app in a headless browser and report what was seen",
		Long: `Render the app in a headless browser and report what was seen.

Renders what is ON THE SERVER — your own open draft when you have one (the draft
"ronja app push" writes to), the app itself otherwise — and reports a screenshot
filmstrip, runtime errors with App.tsx:line:col frames, the queries the app ran
and how many rows they returned, console errors, network failures, and a settle
report saying why the harness stopped waiting.

  ronja app test
  ronja app test --route '#/orders' --viewport mobile
  ronja app test --viewport 1280x2400   # a tall frame, to see below the fold
  ronja app test --out-dir ./preview    # anywhere you like, this folder included

--viewport is desktop (1280x800, the default), mobile (390x844), or WxH with
each side 240–2560. --route puts the hash in the URL before the app's first
script runs, as a real deep link does, so the app must read location.hash at
mount; no hashchange fires for it.

This is PERCEPTION, NOT A PASS/FAIL GATE. There is no verdict field and this
command exits zero whenever the harness ran, errors and all — what it saw is
yours to judge. Read the answer in this order: look at the frame, read
settle.signal ("sdk" = the app reported every operation finished, "heuristic" =
things went quiet, "deadline" = the budget ran out and the frame shows a page
still working), read the ops before concluding a blank screen is a bug (0 rows
means the data may be empty, not the code broken), then read the errors. A
bundle that did not compile comes back as diagnostics and no frame.

Use --fail-on-errors to make CI treat runtime errors, or a bundle that did not
build, as a failure.

Written into --out-dir, which defaults to .ronja/test/ inside the folder — the
local-only directory "ronja app push" never syncs and git never sees. The path
is printed on every run. Name --out-dir yourself and it is used exactly as
given, the folder itself included.

  report.json        the server's answer verbatim
  screenshot-1.png   every captured frame, in capture order
  screenshot.png     the representative frame, also written under this name

--steps runs an interaction plan after the render, from a JSON file holding
either a bare array of steps or the same {"steps": [...]} object the HTTP
endpoint takes (with "steps" as its only key — route, viewport and timeout are
flags):

  [{"action": "click", "text": "Refresh"}, {"action": "screenshot"}]

  click / waitFor   selector or text; waitFor waits for the element to be VISIBLE
  fill              selector (or text) and a non-empty value
  screenshot        nothing for the viewport, or a selector (or text) to capture
                    that element, scrolled into view

A step's own "timeoutMs" (default 5000, max 10000) is how long it waits for its
element. Every step shares the render budget: with --steps and no --timeout,
40s is sent (the endpoint's ceiling). A step reported skipped "budget" never
ran; raise --timeout or that step's timeoutMs.

"text" is Playwright's text= engine: a case-insensitive SUBSTRING, matched
against the FIRST element in document order, so "Orders" can hit a heading
before the tab you meant. Prefer an id ("#orders-tab"),
"role=tab[name=\"Orders\"]", or the exact form "text=\"Orders\"".

⚠️ --steps REQUIRES AN ADMINISTRATOR and EXECUTES FOR REAL, with the same
authority a viewer of this app has. A click runs the app's own code: it can
dispatch workflows and agents, upload files and call external systems. Nothing
is mocked and nothing is rolled back.

With --json, the server's envelope plus the paths written, on stdout; every
note and warning goes to stderr either way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppTest(cmd.Context(), appTestOptions{
				Route:         route,
				StepsFile:     stepsFile,
				Viewport:      viewport,
				Timeout:       timeout,
				TimeoutChosen: cmd.Flags().Changed("timeout"),
				OutDir:        outDir,
				FailOnErrors:  failOnErrors,
			})
		},
	}
	cmd.Flags().StringVar(&route, "route", "",
		"in-app hash route to open, e.g. '#/orders' (default: the app's own default route)")
	cmd.Flags().StringVar(&stepsFile, "steps", "",
		"JSON file holding an interaction plan to run after the render: a steps array, or {\"steps\": [...]} (admin only; the steps really execute)")
	cmd.Flags().StringVar(&viewport, "viewport", viewportDesktop,
		"render size: desktop, mobile, or WxH (each side 240-2560), e.g. 1280x2400")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second,
		"render budget, steps included (default 40s with --steps; 0 leaves it to the server; the server clamps it either way)")
	cmd.Flags().StringVar(&outDir, "out-dir", "",
		"directory to write report.json and the screenshots into (default: .ronja/test inside the app folder, which is never synced)")
	cmd.Flags().BoolVar(&failOnErrors, "fail-on-errors", false,
		"exit non-zero when the render reported runtime errors or the bundle did not build")
	return cmd
}

type appTestOptions struct {
	Route     string
	StepsFile string
	Viewport  string
	Timeout   time.Duration
	// TimeoutChosen is whether --timeout was given at all, which is what decides
	// whether --steps may raise the default.
	TimeoutChosen bool
	OutDir        string
	FailOnErrors  bool
}

// appTestResult is the --json surface.
//
// Preview is json.RawMessage rather than a decoded struct so that stdout carries
// the server's answer VERBATIM, fields this CLI's hand-mirror predates included.
// The wrapper is what keeps the CLI's own additions (where the files went)
// distinguishable from what the server said — merging them into the envelope
// would leave a reader unable to tell which half came from where.
type appTestResult struct {
	DataAppID string          `json:"dataAppID"`
	OutDir    string          `json:"outDir"`
	Files     []appTestFile   `json:"files"`
	Preview   json.RawMessage `json:"preview"`
}

func runAppTest(ctx context.Context, opts appTestOptions) error {
	// Everything local first, so a typo costs no round trip and does not even
	// need a credential.
	preset, size, err := parseAppTestViewport(opts.Viewport)
	if err != nil {
		return err
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to leave the render budget to the server", opts.Timeout)
	}
	steps, err := loadPreviewSteps(opts.StepsFile)
	if err != nil {
		return err
	}
	timeout := opts.Timeout
	if len(steps) > 0 && !opts.TimeoutChosen {
		timeout = appTestStepsTimeout
	}

	resolved, err := resolveInstance()
	if err != nil {
		return err
	}
	// A folder of another kind is refused here: LoadManifest checks the manifest's
	// `kind`, which is the only thing that tells the three folder shapes apart.
	f, err := openFolder(ctx, resolved, wfdir.DataAppKind)
	if err != nil {
		return err
	}
	// Where the answer will go is settled BEFORE anything is rendered, and that
	// ordering is the point rather than tidiness. Resolving it afterwards meant a
	// refusal here threw away a render that had already been paid for — and, with
	// --steps, one whose clicks had already dispatched workflows and written data
	// for real. It is also the last local check, so it keeps this command's own
	// contract: a folder that cannot be written to costs no round trip, and a
	// non-zero exit still means nothing about the app was observed. The narration
	// now lands before the render's wait rather than after it, which is
	// where a reader is actually looking.
	out, err := resolveAppTestOutDir(opts.OutDir, f.Root)
	if err != nil {
		return err
	}
	client := api.New(resolved.URL, resolved.Token)

	// The row to render is resolved HERE, not by the server: POST :id/preview
	// renders exactly the row it is named (a draft is its own address; nothing
	// substitutes the caller's draft for a live id), so rendering the open draft
	// means naming it. inspectAppTarget reads the binding's row AND the caller's
	// draft, and appTarget.Row picks the draft when there is one. An unbound
	// folder, a deleted app and an unworkable lifecycle each still get their own
	// message instead of a bare 404.
	target, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		return err
	}
	if target.App == nil {
		return fmt.Errorf("nothing to test — this folder has no data app on %s yet.\n  Run `ronja app push` first: it creates the app and the draft this would render",
			resolved.URL)
	}
	// A dirty folder is a WARNING, not a refusal, for the same reason it is on
	// `app validate`: rendering the draft as it stands is a reasonable thing to
	// want, but it is also the only way to look at a picture of code you are not
	// looking at.
	if err := warnIfAppDirty(f); err != nil {
		return err
	}

	req := api.PreviewRequest{
		Route:          opts.Route,
		ViewportPreset: preset,
		Viewport:       size,
		TimeoutMs:      int(timeout / time.Millisecond),
		Steps:          steps,
	}
	outcome, err := previewWithOneRetry(ctx, client, target.FilesRow().ID, req)
	if err != nil {
		return err
	}
	result := outcome.Result

	// Said off the server's answer rather than off our own choice: the id it
	// reports is the only authority on what these pixels are a picture of.
	if result.DataAppID != "" && result.DataAppID != target.App.ID {
		fmt.Fprintf(os.Stderr, "  Rendered your own draft %s of %s.\n", result.DataAppID, target.App.ID)
	}

	files, err := writeAppTestOutputs(ctx, client, out.Path, outcome)
	if err != nil {
		return err
	}

	report := &appTestResult{
		DataAppID: result.DataAppID,
		OutDir:    out.Path,
		Files:     files,
		Preview:   json.RawMessage(outcome.Raw),
	}
	if flagJSON {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else {
		printAppTestReport(result, files)
	}
	// Both on stderr, so a --json caller is told as well.
	warnIfViewportNotApplied(size, result.Viewport)
	hintIfStepsRanOutOfBudget(result.Steps)
	// Said again, last, because it is now said first: the ignore warning is
	// printed before a render that can take tens of seconds and then fills the
	// terminal, and one line that far up the scrollback is one nobody reads. The
	// files it is about have only just been written.
	out.WarnIfNotIgnored()

	// The exit code, and the only place this command takes a position. Note what
	// it does NOT do by default: an app full of runtime errors still exits zero,
	// because the harness ran and answered the question it was asked.
	if opts.FailOnErrors {
		if reason := appTestFailureReason(result); reason != "" {
			fmt.Fprintf(os.Stderr, "  --fail-on-errors: %s\n", reason)
			return errAlreadyReported
		}
	}
	return nil
}

// appTestFailureReason says why --fail-on-errors should bite, or "" when it
// should not.
//
// It counts COMPILE DIAGNOSTICS as well as runtime errors, and that is a
// deliberate addition rather than an oversight. A bundle that did not build
// comes back with an EMPTY errors list — there was no render to produce one —
// so a gate reading errors alone would pass the most broken state a data app
// can be in. Console errors are deliberately excluded: they are noisy, an app
// can log one on purpose, and a flag called --fail-on-errors that fired on a
// third-party library's warning would be turned off and never turned back on.
func appTestFailureReason(r *api.PreviewResult) string {
	if n := len(r.CompileDiagnostics); n > 0 {
		return fmt.Sprintf("the bundle did not build (%d %s)", n, plural(n, "diagnostic"))
	}
	if n := len(r.Errors); n > 0 {
		return fmt.Sprintf("the render reported %d runtime %s", n, plural(n, "error"))
	}
	return ""
}

// previewWithOneRetry renders, honouring a single Retry-After.
//
// ONE retry, not a loop. A 429 here means the shared harness (or this token's
// per-minute budget) is spent, and the honest thing to do about it is to wait
// the interval the server named and ask once more; a client that kept retrying
// would be adding load to the exact thing it was told is at capacity, and a
// client that gave up immediately would fail a CI job for a two-second queue.
//
// Busy after the retry is a HARNESS-AVAILABILITY failure, not an app verdict,
// and it exits non-zero with a message that says so. That asymmetry is the whole
// point: an app with a hundred runtime errors exits zero because it was
// observed, and a perfect app whose render never happened exits non-zero because
// it was not. Reporting an unobserved app as clean is the one answer that would
// send someone away believing something false.
func previewWithOneRetry(ctx context.Context, client *api.Client, appID string, req api.PreviewRequest) (*api.PreviewOutcome, error) {
	outcome, err := client.PreviewDataApp(ctx, appID, req)
	var busy *api.BusyError
	if !errors.As(err, &busy) {
		return outcome, err
	}

	wait := previewBusyWait(busy.RetryAfter)
	fmt.Fprintf(os.Stderr, "  The render harness is busy — %s\n  Waiting %s and trying once more. Nothing about your app was observed, so change nothing in response to this.\n",
		busyMessage(busy), wait.Round(time.Second))
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("stopped waiting for the render harness: %w", ctx.Err())
	case <-time.After(wait):
	}

	outcome, err = client.PreviewDataApp(ctx, appID, req)
	if errors.As(err, &busy) {
		fmt.Fprintf(os.Stderr, "error: the render harness was unavailable — %s\n  Nothing about your app was observed: it was never rendered, so this says nothing about your code and no change to it would help. Try again shortly.\n",
			busyMessage(busy))
		return nil, errAlreadyReported
	}
	return outcome, err
}

// busyMessage prefers the server's own wording, which explains WHICH bound was
// hit (the shared pool, or this caller's own per-minute budget).
func busyMessage(busy *api.BusyError) string {
	if busy.Message != "" {
		return busy.Message
	}
	return "the shared render pool is at capacity, or this credential has rendered too often in the last minute."
}

// previewBusyWait resolves what the server asked for into what we will actually
// wait: its interval when it named one, our default when it did not, and never
// more than the cap.
func previewBusyWait(retryAfter time.Duration) time.Duration {
	wait := retryAfter
	if wait <= 0 {
		wait = previewBusyRetryDelay
	}
	if wait > previewBusyRetryCap {
		wait = previewBusyRetryCap
	}
	return wait
}

// stepsFileKeyFlags names the flag that carries each other field of the HTTP
// request body. Such a key in a steps file is REFUSED, never honoured and
// never ignored: honouring it would give the file a second say over what the
// command line says, and ignoring a `route` would render a different page
// from the one its author wrote down.
var stepsFileKeyFlags = map[string]string{
	"route":          "--route",
	"viewportPreset": "--viewport",
	"viewport":       "--viewport",
	"timeoutMs":      "--timeout",
}

// unwrapStepsObject returns the array inside a {"steps": [...]} steps file,
// refusing an object with any other key, or with none.
func unwrapStepsObject(path string, obj []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(obj))
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("read the interaction plan in %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s holds more than one JSON value — an interaction plan is a SINGLE array of steps (or one {\"steps\": [...]} object), and everything after the first would have been silently ignored", path)
	}
	if _, isStep := fields["action"]; isStep {
		return nil, fmt.Errorf("%s holds a single step object — an interaction plan is an ARRAY of steps, or {\"steps\": [...]}", path)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "steps" {
			continue
		}
		if flag, ok := stepsFileKeyFlags[k]; ok {
			return nil, fmt.Errorf("%s sets %q — a steps file carries only \"steps\"; pass %s on the command line instead", path, k, flag)
		}
		return nil, fmt.Errorf("%s has the key %q — a steps file is a JSON array of steps, or an object whose only key is \"steps\"", path, k)
	}
	inner, ok := fields["steps"]
	if !ok {
		return nil, fmt.Errorf("%s is an object with no \"steps\" key — an interaction plan is a JSON array of steps, or {\"steps\": [...]}", path)
	}
	inner = bytes.TrimSpace(inner)
	if len(inner) == 0 || inner[0] != '[' {
		return nil, fmt.Errorf("%s: \"steps\" must be a JSON array of steps", path)
	}
	return inner, nil
}

// parseAppTestViewport reads --viewport: a preset name, sent as
// viewportPreset, or WxH, sent as an explicit viewport. Exactly one of the two
// results is set.
func parseAppTestViewport(v string) (preset string, size *api.PreviewViewport, err error) {
	if v == viewportDesktop || v == viewportMobile {
		return v, nil, nil
	}
	w, h, ok := parseWxH(v)
	if !ok {
		return "", nil, fmt.Errorf("--viewport must be %s, %s or WxH such as 1280x2400 (got %q) — the server resolves an unknown preset to desktop rather than failing, so a typo would silently answer a question about the wrong layout",
			viewportDesktop, viewportMobile, v)
	}
	if w < viewportMinSide || w > viewportMaxSide || h < viewportMinSide || h > viewportMaxSide {
		return "", nil, fmt.Errorf("--viewport %s is out of range: each side must be %d–%d px", v, viewportMinSide, viewportMaxSide)
	}
	return "", &api.PreviewViewport{Width: w, Height: h}, nil
}

// parseWxH reads "<digits>x<digits>" and nothing else: no sign, no spaces,
// no units.
func parseWxH(v string) (w, h int, ok bool) {
	ws, hs, found := strings.Cut(v, "x")
	if !found {
		return 0, 0, false
	}
	w, okW := parseSide(ws)
	h, okH := parseSide(hs)
	return w, h, okW && okH
}

func parseSide(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// warnIfViewportNotApplied says so when a WxH was asked for and the harness
// confirmed a different size — an instance older than explicit sizes ignores
// the field and renders desktop. An absent echo says nothing either way (the
// harness confirmed nothing), so it is not a warning.
func warnIfViewportNotApplied(asked *api.PreviewViewport, echoed string) {
	if asked == nil || echoed == "" {
		return
	}
	if echoed == fmt.Sprintf("%dx%d", asked.Width, asked.Height) {
		return
	}
	if preset, ok := viewportPresetSizes[echoed]; ok && preset == *asked {
		return
	}
	fmt.Fprintf(os.Stderr, "  warning: --viewport %dx%d: this server does not support custom sizes; the frame is %s.\n",
		asked.Width, asked.Height, displayLine(echoed))
}

// hintIfStepsRanOutOfBudget prints one line when any step was skipped for
// budget. No number in it: an older server clamps silently and echoes no
// applied budget, so any figure named here could be one it would not grant.
func hintIfStepsRanOutOfBudget(steps []api.PreviewStepResult) {
	for _, s := range steps {
		if s.Skipped == "budget" {
			fmt.Fprintf(os.Stderr, "  hint: a step was skipped because the render budget ran out — raise --timeout, or that step's timeoutMs.\n")
			return
		}
	}
}

// loadPreviewSteps reads an interaction plan, or returns nil when --steps was
// not given.
//
// NOTHING is ever synthesised. A step clicks and types inside the app with
// viewer authority — it can dispatch a workflow, run an agent, write data — so
// the only plan that ever runs is one a person wrote in a file and named on the
// command line.
func loadPreviewSteps(path string) ([]api.PreviewStep, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the interaction plan: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	example := `[{"action": "click", "text": "Refresh"}, {"action": "screenshot"}]`
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%s is empty — an interaction plan is a JSON array of steps, or {\"steps\": [...]}, e.g.\n    %s", path, example)
	}
	// Two shapes, one plan. The bare array is what this flag has always read;
	// the {"steps": [...]} object is the HTTP request body the guide shows,
	// and a person copying it from there meant exactly the same plan. Checked
	// before decoding so any other shape is named rather than reported as a
	// type error against a field nobody wrote.
	stepsJSON := trimmed
	switch trimmed[0] {
	case '[':
	case '{':
		inner, err := unwrapStepsObject(path, trimmed)
		if err != nil {
			return nil, err
		}
		stepsJSON = inner
	default:
		return nil, fmt.Errorf("%s must hold a JSON array of steps, or {\"steps\": [...]}, e.g.\n    %s", path, example)
	}

	var steps []api.PreviewStep
	dec := json.NewDecoder(bytes.NewReader(stepsJSON))
	// Strict, deliberately. The step grammar is four actions and five fields,
	// and a mistyped key is the failure this catches: decoded leniently,
	// `"selctor"` becomes a click with no selector, which the harness reports
	// as a step that could not find its target — i.e. as a fact about the APP.
	// A refusal naming the key is a better answer than a plausible wrong one.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("read the interaction plan in %s: %w", path, err)
	}
	// A Decoder stops at the end of the first VALUE, so two arrays in one file
	// would silently run the first and discard the rest. Same reason the decoder
	// is strict about keys: for a plan whose steps really execute, a refusal
	// naming the problem beats running a plausible subset of what was written.
	if dec.More() {
		return nil, fmt.Errorf("%s holds more than one JSON value — an interaction plan is a SINGLE array of steps (or one {\"steps\": [...]} object), and everything after the first would have been silently ignored", path)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("%s names no steps — drop --steps to render the app without interacting with it", path)
	}
	return steps, nil
}
