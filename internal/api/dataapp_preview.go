package api

// POST /api/v2/dataapp/:id/preview — render a data app headlessly and report
// what was seen.
//
// A HAND-MIRROR, like every other type in this package, and the same hazard
// applies: a json tag that does not exist on the wire decodes to the zero value
// SILENTLY. Here that would show up as a report claiming a clean render of an
// app that errored, which is worse than a blank column. Every tag below is
// copied from one of four server files — keep them in step:
//
//	backend/api/v2/dataapp/preview.go           PreviewRequest, PreviewResult,
//	                                            PreviewScreenshotView, the 429 body
//	backend/agent/tools/dataapp_preview_core.go PreviewObservation, PreviewBusy,
//	                                            PreviewStepRequest
//	backend/agent/tools/dataapp_preview_hints.go the static notes (stepsNote,
//	                                            appOutputNote) and the <untrusted_*>
//	                                            envelopes those notes describe
//	backend/platform/jsvalidator/types.go       SettleInfo, OpRecord, RuntimeError,
//	                                            ConsoleError, NetworkFailure,
//	                                            HarnessRequest, StepResult, StepSettle
//
// Several fields below are decoded and nothing in this CLI reads them. That is
// deliberate rather than dead weight: report.json is written from
// PreviewOutcome.Raw, never from this struct, so a mirrored field's job is to
// state the shape the server sends and let a test pin it — which is exactly the
// drift check the whole convention exists for.
//
// ⚠️ The response JSON is FLAT: PreviewResult EMBEDS the shared observation
// struct server-side, so `rendered` and `dataAppID` are siblings on the wire.
// Mirroring it as a nested object would decode every observation field as zero.
//
// ⚠️ There is deliberately NO `ok` / pass-fail field, and there never will be.
// A preview is PERCEPTION, not a gate: a compile failure, a blank page and a
// busy harness are all observations the caller weighs. Nothing in this file may
// grow a verdict — that decision belongs to whoever reads the report.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PreviewRequest is the body of POST :id/preview. Every field is optional; an
// empty body renders the app's default route at desktop size.
type PreviewRequest struct {
	// Route is an in-app hash fragment ("#/orders"). The server adds a missing
	// '#' and refuses a control character.
	Route string `json:"route,omitempty"`
	// ViewportPreset is "desktop" (the default) or "mobile". The server resolves
	// anything else to desktop rather than failing, which is exactly why the CLI
	// refuses an unknown value locally: a typo that silently renders the wrong
	// size answers a question nobody asked.
	ViewportPreset string `json:"viewportPreset,omitempty"`
	// TimeoutMs is the render budget, clamped server-side to the deployed
	// harness's ceiling and never refused. Zero means "the server's default".
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// Steps is an optional interaction plan run after the app renders.
	//
	// ⚠️ USR_ADMIN only, and it EXECUTES FOR REAL with viewer authority: a click
	// runs the app's own code, so it can dispatch workflows and agents, upload
	// files and write data. Nothing is mocked and nothing is rolled back.
	Steps []PreviewStep `json:"steps,omitempty"`
}

// PreviewStep is one requested interaction, mirroring tools.PreviewStepRequest.
//
// The grammar, and what each action needs:
//
//	"click"      — selector (or text)
//	"fill"       — selector + a NON-EMPTY value
//	"waitFor"    — selector (or text); waits for VISIBLE, not merely present
//	"screenshot" — nothing; captures a frame tagged phase "step-<n>"
//
// Text is a SELECTOR shorthand (Playwright's text= engine), never a value alias
// on fill.
type PreviewStep struct {
	Action    string `json:"action"`
	Selector  string `json:"selector,omitempty"`
	Text      string `json:"text,omitempty"`
	Value     string `json:"value,omitempty"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
}

// PreviewResult is one render's observation — api/v2/dataapp.PreviewResult
// flattened, since the server embeds tools.PreviewObservation into it.
//
// Ops / Errors / ConsoleErrors carry no omitempty server-side, so "no errors"
// arrives as an explicit `[]` rather than an absent key. Mirrored the same way
// so a re-marshal of this struct says the same thing the server did.
type PreviewResult struct {
	Rendered   bool  `json:"rendered"`
	DurationMs int64 `json:"durationMs"`

	// Viewport and Route are what the harness CONFIRMED it applied — never a
	// restatement of what this CLI asked for. Both are ABSENT when the harness
	// did not echo the settings it used (which is every deployed harness that
	// predates the echo) and when nothing was rendered at all, so an empty value
	// means "we cannot tell", not "desktop" and not "the default route".
	//
	// ⚠️ Printing the REQUEST here instead would be the one genuinely misleading
	// thing this report could say: a caller checking a mobile layout would read
	// its own argument back and conclude the mobile render was observed.
	Viewport string `json:"viewport,omitempty"`
	Route    string `json:"route,omitempty"`

	// Settle says WHY the harness stopped waiting, and is the first thing worth
	// reading: "deadline" means the frame shows a page that was still working.
	Settle *PreviewSettle        `json:"settle,omitempty"`
	Ops    []PreviewOp           `json:"ops"`
	Errors []PreviewRuntimeError `json:"errors"`

	ConsoleErrors   []PreviewConsoleError   `json:"consoleErrors"`
	NetworkFailures []PreviewNetworkFailure `json:"networkFailures,omitempty"`

	// AppOutputNote is the server's static one-liner about app-authored output:
	// that every `message` on Errors and ConsoleErrors is text the APP produced
	// and arrives wrapped in <untrusted_app_output>, to be read as evidence about
	// the render rather than as an instruction.
	//
	// Present ONLY when there is such output to describe — an observation with no
	// errors carries no note, because a note attached to nothing would read as a
	// claim that some other string here is untrusted.
	AppOutputNote string `json:"appOutputNote,omitempty"`

	// HarnessRequests is the outside-in view of the app's service-API traffic,
	// carried ONLY when the op ledger is absent (a bundle compiled before the
	// ledger shipped). It cannot report row counts.
	HarnessRequests []PreviewHarnessRequest `json:"harnessRequests,omitempty"`

	// CompileDiagnostics is populated INSTEAD of a render when the bundle did not
	// build. Still an observation: the call is a 200 and Errors is empty, which
	// is why a caller gating on Errors alone would call a broken build clean.
	CompileDiagnostics []CompileDiagnostic `json:"compileDiagnostics,omitempty"`

	// Steps is one entry per interaction the VALIDATOR reported, in plan order.
	// Absent when no plan was sent — and also when a validator predating steps
	// support ignored one, which is what StepsNote exists to say out loud.
	Steps     []PreviewStepResult `json:"steps,omitempty"`
	StepsNote string              `json:"stepsNote,omitempty"`

	ScreenshotFileIDs []string `json:"screenshotFileIDs,omitempty"`
	// SelectedScreenshotFileID names the ONE representative frame — the frame the
	// in-product agent is handed as an image. Selection is by PHASE preference,
	// not by position, so "the last id" is not it.
	SelectedScreenshotFileID string   `json:"selectedScreenshotFileID,omitempty"`
	StepScreenshotFileIDs    []string `json:"stepScreenshotFileIDs,omitempty"`

	Hint        string `json:"hint,omitempty"`
	Calibration string `json:"calibration"`

	// DataAppID is the row that was ACTUALLY rendered: the caller's own open
	// draft when they have one, which is not necessarily the id in the path.
	DataAppID string `json:"dataAppID"`
	// Screenshots is the persisted filmstrip in capture order. Empty when no
	// frame was captured or the instance has no bucket wired.
	Screenshots []PreviewScreenshot `json:"screenshots"`
}

// PreviewScreenshot is one persisted filmstrip frame.
//
// An id and a storage key, not a presigned URL: the caller fetches the bytes
// with the credential it already has. See DownloadFrame.
type PreviewScreenshot struct {
	FileID string `json:"fileID"`
	// FileKey is what the bytes are actually fetched by, and it is the field to
	// prefer: /api/v2/file/download/:key is `data`-scoped like the preview
	// itself, whereas resolving an id through /api/v2/feature/file/:id needs a
	// route whose group carries no scope at all — which a SCOPED token is
	// refused on. Empty only against an instance older than this field.
	FileKey string `json:"fileKey"`
	// Phase is the NEWEST phase these pixels were observed at: "first_paint" /
	// "settled" / "deadline", or "step-<n>" for a `screenshot` step's frame.
	Phase string `json:"phase"`
	// Selected marks the ONE representative frame, on the wire so the CLI, the
	// chat card and the model all cite the same picture.
	Selected bool `json:"selected"`
}

// PreviewSettle mirrors jsvalidator.SettleInfo.
//
// ⚠️ TWO CLOCKS. DOMQuietMs is the HARNESS's clock; FirstRenderAt /
// LastSettledAt / ReportedReadyAt are the PAGE's (performance.now()). They are
// not comparable without PageTimeOriginMs. The CLI reads only Signal, and
// anything that starts subtracting one from the other must read that note in
// backend/platform/jsvalidator/types.go first.
type PreviewSettle struct {
	// Signal is "sdk" (the app's own op ledger reported everything finished),
	// "heuristic" (network and DOM went quiet) or "deadline" (the budget ran
	// out, so the frame shows a page still working).
	Signal              string   `json:"signal"`
	PendingOpsAtCapture int      `json:"pendingOpsAtCapture"`
	DOMQuietMs          int64    `json:"domQuietMs"`
	InFlightAtCapture   int      `json:"inFlightAtCapture"`
	PendingHosts        []string `json:"pendingHosts,omitempty"`
	OpsStarted          int      `json:"opsStarted"`
	FirstRenderAt       int64    `json:"firstRenderAt"`
	LastSettledAt       int64    `json:"lastSettledAt"`
	ReportedReadyAt     int64    `json:"reportedReadyAt"`
	PageTimeOriginMs    int64    `json:"pageTimeOriginMs"`
}

// PreviewOp is one entry of the SDK op ledger — a query, agent run or file read
// the app performed, timed from inside the page.
//
// Rows is a POINTER because nil ("this operation has no row count to report")
// and 0 ("it returned nothing") are different facts, and a preview exists partly
// to tell a blank page apart from an empty result.
type PreviewOp struct {
	Path      string `json:"path"`
	StartedAt int64  `json:"startedAt"`
	Ms        int64  `json:"ms"`
	Rows      *int64 `json:"rows,omitempty"`
	OK        bool   `json:"ok"`
}

// PreviewHarnessRequest is one service-API request as the BROWSER saw it — the
// fallback view for a bundle with no op ledger.
type PreviewHarnessRequest struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Status int    `json:"status"`
	Ms     int64  `json:"ms"`
}

// PreviewRuntimeError is one uncaught error or unhandled rejection. Source /
// Line / Column are the first source-map-resolved frame inside the author's own
// code, so a reader has "App.tsx:270:28" without parsing a stack.
type PreviewRuntimeError struct {
	// Message arrives WRAPPED in <untrusted_app_output> — see AppOutputNote. The
	// wrap is the server's, and the raw bytes keep it: report.json is written
	// from PreviewOutcome.Raw, so what a reader opens is exactly what the server
	// framed. Only the terminal summary unwraps it, and only for display.
	//
	// Stack / RawStack / Source / Line / Column are NOT wrapped. They are Ronja's
	// reading of the bundle's own source map rather than a string the app chose.
	Message  string `json:"message"`
	Stack    string `json:"stack,omitempty"`
	RawStack string `json:"rawStack,omitempty"`
	Source   string `json:"source,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

// PreviewConsoleError is a deduped console.error call. Message carries the same
// <untrusted_app_output> envelope PreviewRuntimeError.Message does, and for a
// blunter reason: a console.error commonly prints a whole tenant row.
type PreviewConsoleError struct {
	Message string `json:"message"`
}

// PreviewNetworkFailure is a 4xx/5xx or transport failure, after platform-CDN
// noise has been filtered out server-side.
type PreviewNetworkFailure struct {
	URL        string `json:"url"`
	Status     int    `json:"status"`
	StatusText string `json:"statusText"`
}

// PreviewStepResult is what happened at one interaction step.
//
// ⚠️ Read Skipped BEFORE OK. A skipped step NEVER RAN (the invocation's step
// window was spent), so its ok:false says nothing about the app; a step with
// ok:false and no skipped is one that ran and did not do what it asked.
type PreviewStepResult struct {
	Index       int                `json:"index"`
	Action      string             `json:"action"`
	OK          bool               `json:"ok"`
	Skipped     string             `json:"skipped,omitempty"`
	Error       string             `json:"error,omitempty"`
	SettleAfter *PreviewStepSettle `json:"settleAfter,omitempty"`
	// DOMText is document.body.innerText after the step.
	//
	// ⚠️ UNTRUSTED. It is the APP's content, rendered from tenant rows and
	// whatever external systems wrote into them. It reaches report.json because
	// that is a file the caller chose to write; it is deliberately NOT printed in
	// the terminal summary, where it would be indistinguishable from the CLI's
	// own words.
	DOMText string `json:"domText,omitempty"`
}

// PreviewStepSettle is the abbreviated settle report after a mutating step.
type PreviewStepSettle struct {
	Signal   string `json:"signal"`
	Ms       int64  `json:"ms"`
	InFlight int    `json:"inFlight"`
}

// PreviewBusy is the 429 body: the shared render pool (or this caller's own
// per-minute budget) was spent, so NOTHING about the app was observed.
type PreviewBusy struct {
	Busy        bool   `json:"busy"`
	Message     string `json:"message"`
	Calibration string `json:"calibration"`
}

// BusyError is a 429 from the preview endpoint, surfaced as its own type rather
// than as a plain *Error with StatusOf(err) == 429.
//
// Two reasons it earns a type. It is not a failure of the REQUEST — the app was
// never rendered, so nothing at all is known about it, and a caller must not
// report it as "your app is broken" or change any code in response. And it is
// the one answer that names its own remedy: the Retry-After the server sent,
// which a retry that ignored it would be a rate limiter's definition of abuse.
type BusyError struct {
	Message     string
	Calibration string
	// RetryAfter is the parsed Retry-After header, or 0 when the server sent
	// none (or something unparseable). The caller supplies its own default.
	RetryAfter time.Duration
	// Body is the 429 verbatim, so a --json caller can be shown what the server
	// actually said rather than this struct's reading of it.
	Body []byte
}

func (e *BusyError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "the data-app render harness is busy"
}

// PreviewOutcome pairs one decoded observation with the bytes it arrived as.
//
// The raw bytes are not a convenience. This file is a hand-mirror taken at one
// moment, and an instance newer than the CLI sends fields it does not know
// about; a report written by re-marshalling PreviewResult would silently drop
// exactly those, which is the drift the mirror convention exists to bound. So
// `ronja app test` writes report.json from Raw and reads Result only to
// summarise — the file is always at least as complete as the server's answer.
type PreviewOutcome struct {
	Result *PreviewResult
	Raw    []byte
}

// maxPreviewBody bounds the observation envelope. It carries no image bytes
// (frames are file ids), but it does carry an op ledger, a stack per runtime
// error and up to 20 steps' worth of captured page text — so it is generously
// sized, and the point of the cap is only that --url can be aimed at anything.
const maxPreviewBody = 8 << 20

// previewHTTPMargin is how long the client waits BEYOND the render budget it
// asked for.
//
// The render is not the whole call: the request may queue behind the tenant's
// concurrency slot, and the frames are uploaded and registered as files after
// the browser is done. Bounding the HTTP call at exactly the render budget
// would therefore kill a preview that was working, and report a healthy harness
// as a broken one.
const previewHTTPMargin = 60 * time.Second

// PreviewDataApp renders a data app headlessly and returns what was seen.
//
// A busy harness comes back as *BusyError, NOT as a *PreviewOutcome with a flag:
// the two are different events (an observation versus the absence of one), and a
// single return value would let a caller read a busy answer as an app with no
// errors. Everything else non-2xx is an ordinary *Error.
func (c *Client) PreviewDataApp(ctx context.Context, appID string, in PreviewRequest) (*PreviewOutcome, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("Content-Type", "application/json")

	path := "/api/v2/dataapp/" + url.PathEscape(appID) + "/preview"
	resp, err := c.DoRaw(ctx, "POST", path, header, body, previewCallTimeout(in))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the body for EVERY status, not just the successful one: the 429 is a
	// message worth quoting, and the 200 has to survive verbatim.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPreviewBody+1))
	if err != nil {
		return nil, fmt.Errorf("read the preview response from %s: %w", c.BaseURL+path, err)
	}
	if len(raw) > maxPreviewBody {
		return nil, fmt.Errorf("the preview response from %s exceeded %d bytes — is that URL a Ronja instance?",
			c.BaseURL+path, maxPreviewBody)
	}

	if resp.Status == http.StatusTooManyRequests {
		var busy PreviewBusy
		// A 429 whose body will not decode is still a 429. The status is the
		// fact; the words are decoration, and refusing to report the throttle
		// because its message was malformed would be the wrong trade.
		_ = json.Unmarshal(raw, &busy)
		return nil, &BusyError{
			Message:     busy.Message,
			Calibration: busy.Calibration,
			RetryAfter:  parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:        raw,
		}
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, errorFrom(resp.Status, raw)
	}

	var result PreviewResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode the preview response from %s: %w", c.BaseURL+path, err)
	}
	return &PreviewOutcome{Result: &result, Raw: raw}, nil
}

// previewCallTimeout is the HTTP deadline for one preview call.
//
// Never shorter than the raw default, so an empty request (which asks the
// server for its own budget) still gets the slow bound rather than a read's.
func previewCallTimeout(in PreviewRequest) time.Duration {
	d := time.Duration(in.TimeoutMs)*time.Millisecond + previewHTTPMargin
	if d < DefaultRawTimeout {
		return DefaultRawTimeout
	}
	return d
}

// parseRetryAfter reads the delta-seconds form of Retry-After, which is what
// this endpoint sends. Anything else — absent, an HTTP-date, a negative number,
// junk — comes back as 0, meaning "the server named no interval"; picking a
// default is the caller's business, and guessing one here would hide the
// difference between a server that asked for a wait and one that did not.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// File is a HAND-MIRROR of the sliver of rdb.File a preview frame needs.
//
// Narrow on purpose, and for the same reason Feature is: this is not a general
// file client, and discovery belongs on plain HTTP. It exists because a preview
// hands back FILE IDS and the bytes live behind a second call keyed on
// FileKey — which is the only field here anything branches on.
//
// Tags copied from backend/ronja/rdb/table_file.go.
type File struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	FileKey     string `json:"fileKey"`
	ContentType string `json:"contentType"`
	ContentSize int64  `json:"contentSize"`
}

// GetFile reads a file row. 404s for an id the caller cannot reach.
func (c *Client) GetFile(ctx context.Context, id string) (*File, error) {
	var out File
	if err := c.Do(ctx, "GET", "feature/file/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DownloadFrame streams one preview frame's bytes.
//
// It prefers the KEY the preview already handed over — one call, on the same
// scope the preview itself needs — and falls back to resolving the id only when
// the instance predates the key (which costs an extra call AND a route a scoped
// token cannot reach; see PreviewScreenshot.FileKey).
func (c *Client) DownloadFrame(ctx context.Context, shot PreviewScreenshot) (io.ReadCloser, error) {
	if shot.FileKey != "" {
		return c.downloadFileByKey(ctx, shot.FileKey)
	}
	return c.DownloadFileByID(ctx, shot.FileID)
}

// downloadFileByKey streams the bytes at a storage key.
//
// ⚠️ The key is escaped WHOLE, slashes included, into one path segment — the
// route is `/download/:key`, not a wildcard, and gin unescapes the parameter on
// the other side. Joining the key's own slashes into the path would produce a
// URL that matches no route at all.
func (c *Client) downloadFileByKey(ctx context.Context, fileKey string) (io.ReadCloser, error) {
	header := http.Header{}
	header.Set("Accept", "*/*")
	path := "/api/v2/file/download/" + url.PathEscape(fileKey)
	resp, err := c.DoRaw(ctx, "GET", path, header, nil, DefaultRawTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		return nil, errorFrom(resp.Status, raw)
	}
	return resp.Body, nil
}

// DownloadFileByID streams a file's bytes, given its id. The caller owns the
// returned reader and MUST close it — closing is also what releases the
// request's deadline.
//
// Two calls, because the download route is keyed on the storage KEY rather than
// on the file id: the row first, then the bytes.
//
// ⚠️ The key is escaped WHOLE, slashes included, into one path segment — the
// route is `/download/:key`, not a wildcard, and gin unescapes the parameter on
// the other side. Joining the key's own slashes into the path would produce a
// URL that matches no route at all.
func (c *Client) DownloadFileByID(ctx context.Context, fileID string) (io.ReadCloser, error) {
	file, err := c.GetFile(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fileID, err)
	}
	if file.FileKey == "" {
		return nil, fmt.Errorf("file %s has no storage key, so its bytes cannot be fetched", fileID)
	}
	return c.downloadFileByKey(ctx, file.FileKey)
}
