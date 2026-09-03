package commands

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/jqf"
	"github.com/spf13/cobra"
)

// `ronja api` is a request runner, not an API wrapper.
//
// The distinction is the whole reason it is allowed to exist. A wrapper is a
// subcommand per endpoint: it has to be extended for every new route, it is
// permanently behind the API, and each verb is a second definition of a shape
// that already has one. This command has NO per-endpoint knowledge at all. It
// carries the two things a caller cannot supply for itself — which instance,
// and the credential — and passes the path straight through. There is nothing
// here to drift, because there is nothing here that describes the API.
//
// It is `gh api`'s bargain, and the alternative it replaces is worse: the
// documented form today is
//
//	eval "$(ronja env)" && curl -H "Authorization: Bearer $RONJA_TOKEN" "$RONJA_URL/api/v2/..."
//
// which puts a live credential into a shell variable, an interpolation and
// (when the shell echoes, or the command is logged) a transcript. `ronja api`
// loads the same credential and never exposes it.
//
// What it still does not do: list, browse or delete. Discovery stays on HTTP
// behind /llms.txt.
func newAPICmd() *cobra.Command {
	var (
		method       string
		data         string
		form         []string
		headers      []string
		timeout      time.Duration
		jqExpr       string
		rawOutput    bool
		outFile      string
		retries      int
		failOnError  bool
		waitUntil    string
		waitInterval time.Duration
		waitTimeout  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "api <path>",
		Short: "Make an authenticated request to this Ronja instance",
		Long: `Make an authenticated request to this Ronja instance.

The path is joined to the instance you are signed in to, and the credential is
attached for you — so nothing has to load, interpolate or echo a token:

  ronja api /api/v2/authentication/me
  ronja api -X POST /api/v2/feature -d '{"name":"Sales","scope":"private"}'
  ronja api -X POST /api/v2/feature -d @feature.json
  cat feature.json | ronja api -X POST /api/v2/feature -d @-

The method defaults to GET, or to POST when a body is given. A body sets
Content-Type: application/json unless -H overrides it. A "Host: ..." header is
honoured rather than dropped, which is not what a bare Go client does.

Upload a file with -F, which sends multipart/form-data — the encoding the
upload endpoints require and -d cannot produce:

  ronja api -X POST /api/v2/file/upload/uploads -F file=@quarterly.pdf
  ronja api -X POST /api/v2/file/upload/uploads -F file=@- -F note=draft

A file is streamed from disk, at any size the instance accepts; content piped on
standard input is held in memory and capped at 16 MiB, because a pipe cannot be
re-opened to re-send on a retry. Both -d and -F work this way. --timeout bounds
the whole request, the upload included, so raise it (or pass --timeout 0) for a
file large enough that sending it takes longer than the default.

Pull one field out of the answer with --jq, so nothing has to be piped through
another interpreter to read it. -r prints strings unquoted, which is what makes
the result safe to substitute:

  ronja api /api/v2/feature/query --jq '.result[] | select(.scope=="shared") | .id'
  id=$(ronja api -X POST /api/v2/workflow -d @wf.json --jq '.id' -r)

Wait for asynchronous work with --wait-until, which re-issues the request until
a jq condition holds. It works for anything that finishes eventually — workflow
runs, table builds, connector syncs, exports:

  ronja api /api/v2/workflow/run/$id --wait-until '.status != "running"' --jq '.status' -r

Note jq's truthiness: [] and 0 are TRUE, so write an explicit comparison
(.errors | length > 0) rather than relying on a bare field.

--jq prints EVERY value the expression matched, or it fails and prints nothing.
There is no cap and no prefix, so a script can trust what it captured. Two
bounds can make it fail, and each says which one it was and what to do:

  * a work and memory budget, scaled to the size of the response. A filter that
    manufactures data (range, recurse, repeat, a string repeated with *) is
    refused rather than allowed to exhaust the machine, and so is an honest
    filter over a response too big for it — that one says to narrow the REQUEST.
  * a 10-second deadline on one application of the filter, separate from
    --timeout, which bounds the REQUEST.

Other output handling:

  -o, --out <file>    write the response body to a file (0600) instead of stdout
      --retry N       retry a 429 or 5xx up to N times, honouring Retry-After
      --fail-on-error exit non-zero when a 200 carries an "error" in its body

--fail-on-error covers the trap that makes a runner unsafe in an && chain: some
endpoints report failure IN the envelope, so a query that never ran comes back
as HTTP 200 and a plain runner exits zero. It is off by default because reading
inside the body means holding all of it, and the default path streams.

The path is passed through VERBATIM, query string included — nothing is escaped
for you, so percent-encode anything that needs it (a raw space in ?q= produces a
malformed request line and an opaque 400):

  ronja api '/api/v2/search?q=monthly%20revenue'

The response body is written to stdout verbatim — it is already the
machine-readable output, so --json is accepted and does nothing. On an HTTP
error the body is still printed, one "HTTP <status> <method> <path>" line goes
to stderr, and the command exits non-zero.

--timeout bounds ONE request, the response body included; 0 waits as long as the
endpoint takes, which with --retry leaves the command no time limit at all. Because the body is streamed as it arrives, a call that runs out
of time has already written part of it — so raise --timeout for an endpoint that
does real work rather than treating a truncated payload as the answer.
--wait-timeout separately bounds a --wait-until loop as a whole.

This is a transport, not a wrapper: it knows no endpoints. Read what to call at
` + "`ronja context`" + ` or the instance's /llms.txt.`,
		// The flag-order guard runs BEFORE the arity check rather than in RunE,
		// because the arity check is where this mistake lands: `--jq -r .id`
		// swallows -r as the expression, .id becomes a second positional, and
		// cobra answers "accepts 1 arg(s), received 2" — a message about the
		// wrong flag entirely. RunE never runs, so a check inside it is a check
		// that never fires.
		Args: func(cmd *cobra.Command, args []string) error {
			if err := refuseFlagAsJQExpr(cmd, jqExpr); err != nil {
				return err
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			// Checked before anything else, because it is the mistake this
			// command's shape invites: a caller reaches for the full URL they
			// would have given curl. Naming both halves is what turns that into
			// a one-word fix.
			if !strings.HasPrefix(path, "/") {
				return fmt.Errorf("path must start with \"/\" — the instance base URL is added automatically, so write /api/v2/authentication/me rather than a full URL (got %q)",
					path)
			}
			// Mirrors `ronja query`: a negative duration has no meaning, and
			// zero already means "no deadline", so a negative one is a typo
			// rather than an intent worth guessing at.
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the request takes", timeout)
			}
			if retries < 0 {
				return fmt.Errorf("--retry cannot be negative (got %d)", retries)
			}
			// --timeout bounds ONE attempt, and --retry makes more of them, so
			// the pair has no overall wall-clock budget at all: an instance
			// that accepts the connection and then stalls mid-body wedges
			// every attempt forever, with no further output. A terminal has
			// Ctrl-C; a CI step or an agent-driven call just hangs, which is
			// worth one line of warning rather than a surprise. Stderr, so it
			// never lands in --json output on stdout.
			if timeout == 0 && retries > 0 {
				fmt.Fprintf(os.Stderr, "  Note: --timeout 0 with --retry %d leaves no time limit of any kind — an attempt that stalls will not time out, so the command can hang indefinitely.\n", retries)
			}
			if data != "" && len(form) > 0 {
				return errors.New("-d and -F build the request body two different ways — use one or the other")
			}
			if err := checkStdinSources(data, form); err != nil {
				return err
			}
			if rawOutput && jqExpr == "" {
				return errors.New("--raw only means something with --jq: it unquotes the STRINGS a filter produces")
			}

			// Compiled before the request goes out, not after it comes back. A
			// typo in a filter should cost nothing, and with --wait-until it
			// would otherwise be found only once the first poll had landed.
			var filter *jqf.Filter
			if jqExpr != "" {
				var err error
				if filter, err = jqf.Compile(jqExpr); err != nil {
					return err
				}
			}
			plan, err := parseWaitPlan(waitUntil, waitInterval, waitTimeout)
			if err != nil {
				return err
			}
			if err := checkFormPaths(form); err != nil {
				return err
			}

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			header, err := parseHeaders(headers)
			if err != nil {
				return err
			}
			body, err := requestBody(data)
			if err != nil {
				return err
			}
			if len(form) > 0 {
				formSrc, contentType, err := buildForm(form)
				if err != nil {
					return err
				}
				body = formSrc
				// The boundary is generated with the body, so the header has
				// to carry THAT one — see formContentType, which reconciles a
				// -H the caller also gave.
				resolved, err := formContentType(header.Get("Content-Type"), contentType)
				if err != nil {
					return err
				}
				header.Set("Content-Type", resolved)
			}
			// curl's defaulting, and for curl's reason: a caller who supplies a
			// body has already said what they mean, and making them say -X POST
			// as well is a step whose only outcome is forgetting it.
			if method == "" {
				method = http.MethodGet
				if body != nil {
					method = http.MethodPost
				}
			}
			method = strings.ToUpper(method)

			// A wait re-sends the SAME request, so it is only safe on a method
			// that a repeat cannot change the meaning of. Polling a POST would
			// create one resource per attempt — silently, and up to the whole
			// --wait-timeout worth of them.
			if plan != nil && method != http.MethodGet && method != http.MethodHead {
				return fmt.Errorf("--wait-until re-sends the request until the condition holds, so it only accepts GET and HEAD (got %s) — start the work with one call, then wait on the GET that reports its status",
					method)
			}

			request := apiRequest{
				client:  api.New(resolved.URL, resolved.Token),
				method:  method,
				path:    path,
				header:  header,
				body:    body,
				timeout: timeout,
				retries: retries,
			}
			out := apiOutput{filter: filter, raw: rawOutput, out: outFile, failOnError: failOnError}

			var resp *api.RawResponse
			if plan != nil {
				resp, err = request.wait(cmd.Context(), *plan)
			} else {
				resp, err = request.send(cmd.Context())
			}
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			emitErr := out.emit(cmd.Context(), resp, method, path)
			// The re-stat runs AFTER emit, and the ordering is the whole point:
			// the request DID succeed on the wire, so the server's answer is
			// real and the caller must see it. What follows is the second half
			// of the truth — that the bytes the answer describes may be a prefix
			// of the file. Printing the response and then failing is the only
			// shape that reports both. See checkFilesUnchanged.
			//
			// It runs on an HTTP FAILURE too, and that case is the one it was
			// missing: a file that shrank mid-upload is a plausible cause of the
			// 400 the server just answered with, and "the bytes sent were a
			// prefix" is the half of that story only the CLI can tell. An emit
			// that failed on local I/O — an unwritable --out, a closed stdout —
			// is a different story entirely, and the response status is what
			// separates the two.
			if emitErr != nil && resp.Status < 400 {
				return emitErr
			}
			checkErr := checkFilesUnchanged(body)
			if checkErr == nil {
				return emitErr
			}
			// errAlreadyReported means emit has ALREADY written its "HTTP <status>"
			// line to stderr and wants nothing else printed — so joining it here
			// would suppress this message along with it (run() prints nothing for
			// an error that says it has spoken for itself). The status line is on
			// stderr either way; what has to survive is the truncation.
			if emitErr == nil || errors.Is(emitErr, errAlreadyReported) {
				return checkErr
			}
			return errors.Join(emitErr, checkErr)
		},
	}

	cmd.Flags().StringVarP(&method, "method", "X", "",
		"HTTP method (default GET, or POST when a body is given)")
	cmd.Flags().StringVarP(&data, "data", "d", "",
		"request body; @file reads a file, @- reads standard input")
	cmd.Flags().StringArrayVarP(&form, "form", "F", nil,
		"multipart form part as name=value or name=@path (repeatable; @- reads standard input)")
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil,
		`extra request header as "Name: value" (repeatable; "Host" is honoured)`)
	cmd.Flags().DurationVar(&timeout, "timeout", api.DefaultRawTimeout,
		"how long to wait for one request, response body included (0 removes the only bound there is and waits indefinitely)")
	cmd.Flags().StringVarP(&jqExpr, "jq", "q", "",
		"filter the JSON response through a jq expression")
	cmd.Flags().BoolVarP(&rawOutput, "raw", "r", false,
		"with --jq, print string results unquoted")
	cmd.Flags().StringVarP(&outFile, "out", "o", "",
		"write the response body to this file (owner-only) instead of stdout")
	cmd.Flags().IntVar(&retries, "retry", 0,
		"retry a 429 or 5xx up to this many times, honouring Retry-After")
	cmd.Flags().BoolVar(&failOnError, "fail-on-error", false,
		`exit non-zero when a 2xx response carries a non-empty "error" field`)
	cmd.Flags().StringVar(&waitUntil, "wait-until", "",
		"re-send the request until this jq condition holds (GET/HEAD only)")
	cmd.Flags().DurationVar(&waitInterval, "wait-interval", DefaultWaitInterval,
		"how long to pause between --wait-until attempts")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", DefaultWaitTimeout,
		"how long a --wait-until loop may run in total (0 waits indefinitely)")
	return cmd
}

// parseWaitPlan validates the three --wait-* flags together, returning nil when
// no wait was asked for.
//
// The interval and timeout are checked even though they have defaults, because
// the failure they prevent is expensive and silent: a zero interval polls as
// fast as the network allows, which is a self-inflicted rate limit rather than
// a wait.
func parseWaitPlan(expr string, interval, timeout time.Duration) (*waitPlan, error) {
	if expr == "" {
		return nil, nil
	}
	if interval <= 0 {
		return nil, fmt.Errorf("--wait-interval must be positive (got %s) — a wait with no pause is a denial-of-service against your own instance", interval)
	}
	if timeout < 0 {
		return nil, fmt.Errorf("--wait-timeout cannot be negative (got %s) — pass 0 to wait indefinitely", timeout)
	}
	until, err := jqf.Compile(expr)
	if err != nil {
		return nil, err
	}
	return &waitPlan{until: until, interval: interval, timeout: timeout}, nil
}

// maxStdinBody caps a body read from a PIPE, and only from a pipe.
//
// A file on disk is streamed: it can be stat'ed for a size and re-opened for
// every retry, so there is nothing to hold and nothing to bound — the only
// ceiling is whatever the instance itself accepts. Standard input has neither
// property. It cannot be re-opened, so a retry can only re-send what was kept;
// keeping it means holding it in memory; and a producer that never stops (a
// `yes`, a tail of a growing log) would fill that memory with nothing to
// report. This is the ceiling on that one case.
//
// 16 MiB is deliberately far above the read side's maxDocBody: a docs page is a
// few KB, while a legitimate piped API body can carry an embedded file.
const maxStdinBody = 16 << 20

// requestBody resolves -d into a body source, or nil for no body.
//
// The @ prefix is curl's, and so is the reason for it: a JSON body big enough
// to matter does not belong on a command line, where the shell's quoting rules
// get a vote on its contents.
func requestBody(data string) (api.BodySource, error) {
	if data == "" {
		return nil, nil
	}
	rest, isRef := strings.CutPrefix(data, "@")
	if !isRef {
		// A literal body already fits in a command line, which is a far tighter
		// bound than anything applied here would be.
		return api.BytesBody(data), nil
	}
	if rest == "-" {
		body, err := readStdinLimit("-d @-", "-d", maxStdinBody)
		if err != nil {
			return nil, err
		}
		return api.BytesBody(body), nil
	}
	return fileSource("-d @"+rest, "-d", rest)
}

// fileBody is a request body streamed off disk.
//
// The size is taken once, at build time, and becomes the request's
// Content-Length; Open re-opens the file for every attempt. Stat-then-send is
// therefore a window in which the file can change, and the two directions are
// caught in two different places: a file that SHRANK ends short of the
// Content-Length already written and fails at the transport (translated by
// explainBodySizeChange), while a file that GREW produces a complete, valid
// request carrying a PREFIX of it — the instance may store those bytes as a
// finished upload — and is caught by the re-stat in checkFilesUnchanged after
// the response has been emitted.
type fileBody struct {
	path string
	size int64
	// modTime is the file's modification time as it was when size was
	// measured. It is the only thing that can tell a same-length in-place
	// rewrite from an untouched file — see checkFilesUnchanged.
	modTime time.Time
}

func (f fileBody) Len() int64 { return f.size }

func (f fileBody) Open() (io.ReadCloser, error) { return os.Open(f.path) }

func (f fileBody) paths() []sizedPath {
	return []sizedPath{{path: f.path, size: f.size, modTime: f.modTime}}
}

// fileSource builds a body from a path: streamed when it can be, buffered when
// it cannot.
//
// what names the flag and path as the caller wrote them ("-d @report.pdf"), and
// flag is the bare flag ("-d", "-F"), named only in the over-the-cap message.
func fileSource(what, flag, path string) (api.BodySource, error) {
	kind, fi, err := classifyBodyPath(what, path)
	if err != nil {
		return nil, err
	}
	if kind == pathRegular {
		return fileBody{path: path, size: fi.Size(), modTime: fi.ModTime()}, nil
	}
	body, err := readUnseekablePath(what, flag, path)
	if err != nil {
		return nil, err
	}
	return api.BytesBody(body), nil
}

// pathKind is how a named path's bytes have to be carried.
type pathKind int

const (
	// pathRegular is a file on disk: stat'ed for a size, opened lazily, and
	// re-opened for every retry.
	pathRegular pathKind = iota
	// pathUnseekable is everything else that can still be read: a FIFO, a
	// process substitution (`-d @<(gzip -c x)`, which the shell hands over as
	// /dev/fd/63), /dev/stdin, a character device.
	pathUnseekable
)

// classifyBodyPath decides which of the three things a named path is, and
// refuses only the one that has no bytes at all.
//
// The three-way split is the whole of it:
//
//   - A REGULAR file with a NON-ZERO stat size becomes a lazy path segment. It
//     is the only kind whose stat'ed size can be relied on in advance, which is
//     what lets the bytes go out with a real Content-Length, straight off disk,
//     at any size the instance accepts — and the only kind that can be re-opened
//     to the same bytes when --retry or --wait-until sends the request again.
//   - A REGULAR file whose stat size is ZERO is treated as unseekable and read,
//     because on this one value the stat cannot be told apart from a file that
//     reports no size and yields bytes anyway: procfs, sysfs and cgroup files
//     all do, and so do some FUSE mounts. Trusting the 0 sent an EMPTY body
//     with a zero exit — no Content-Length mismatch to fail on, and a re-stat
//     that compares 0 against 0 and sees nothing wrong — which is the silent
//     truncation this command is built to refuse. Reading it costs nothing a
//     pipe does not already cost, and a file that really is empty reads as
//     empty, so the outcome is unchanged for it.
//   - A NON-REGULAR but readable path is read into memory, bounded, exactly as
//     a pipe is. Its stat size is not the size of what reading it yields
//     (usually 0, for a stream with no end), so it cannot be streamed against a
//     Content-Length; and it is unseekable and un-replayable, so the only way a
//     retry can re-send it is if the bytes were kept. Memory is that keeping,
//     and the cap is what stops a device that never ends from filling it.
//   - A DIRECTORY is refused. It does have a size (96 bytes on APFS, 4096 on
//     ext4), but that is the size of the directory ENTRY; there are no bytes to
//     send, so there is nothing to buffer and nothing to stream.
func classifyBodyPath(what, path string) (pathKind, os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", what, err)
	}
	if fi.IsDir() {
		return 0, nil, fmt.Errorf("%s is a directory, not a file — point at a file inside it", what)
	}
	if !fi.Mode().IsRegular() {
		return pathUnseekable, fi, nil
	}
	if fi.Size() == 0 {
		// Not a shortcut for "empty". A stat size of 0 on a regular file is
		// the one size that may be a LIE about what reading it yields, so the
		// bytes decide instead of the number.
		return pathUnseekable, fi, nil
	}
	return pathRegular, fi, nil
}

// readUnseekablePath reads a non-regular path whole, under the same ceiling a
// pipe gets.
//
// The message when it is exceeded says the same three things readStdinLimit's
// does — what was read, that the content is held in memory because a retry has
// to be able to re-send it, and that a regular file has neither limitation —
// and additionally names the path, because unlike standard input the caller
// wrote one and may not realise it is not an ordinary file.
func readUnseekablePath(what, flag, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer f.Close()

	body, over, err := readLimit(f, maxStdinBody)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if over {
		return nil, fmt.Errorf("%s read more than %d MiB from %s and stopped: %s is not a regular file, so its bytes cannot be re-read for a retry and are held in memory instead, which the CLI caps at %d MiB. Write it to a file and point %s at the path — a file streams from disk at any size the instance accepts.",
			what, maxStdinBody>>20, path, path, maxStdinBody>>20, flag)
	}
	return body, nil
}

// refuseFlagAsJQExpr catches the one flag-order mistake `--jq` invites.
//
// `--jq` takes a value, so `--jq -r '.id'` binds "-r" as the EXPRESSION and
// leaves '.id' as a positional argument. What the caller then sees is an error
// about argument counts (or, on `ronja query`, a query whose SQL is ".rowCount")
// — neither of which mentions the flag that actually went wrong, and both of
// which look like a bug in the command rather than a space in the wrong place.
//
// The flag set is read off the COMMAND rather than listed here, so a flag added
// later is covered without anybody remembering this function exists. It also
// keeps the check honest per command: `ronja query` has no -F and no --retry,
// and refusing an expression on a flag that command does not have would be
// inventing a mistake.
//
// A leading dash alone is not enough to refuse: "-1" is an ordinary jq
// expression and no command has a -1 flag, so it passes.
func refuseFlagAsJQExpr(cmd *cobra.Command, expr string) error {
	if !isOwnFlagToken(cmd, expr) {
		return nil
	}
	return fmt.Errorf("--jq was given %q, which is one of this command's own flags — --jq takes a value, so it swallowed the flag and left the expression as a stray argument.\n  Quote the expression and pass the flag after it: --jq '.id' %s",
		expr, expr)
}

// isOwnFlagToken reports whether a token is a flag THIS command defines, in
// either its long or its short spelling.
//
// Persistent flags inherited from the root (--json, --url, --profile) are
// included: cmd.Flags() is the complete set that applies here, and `--jq --json`
// is the same mistake as `--jq -r`.
func isOwnFlagToken(cmd *cobra.Command, token string) bool {
	if long, ok := strings.CutPrefix(token, "--"); ok {
		return long != "" && cmd.Flags().Lookup(long) != nil
	}
	short, ok := strings.CutPrefix(token, "-")
	// Exactly one character: ShorthandLookup PANICS on anything longer, and a
	// multi-letter "-abc" is not a shorthand this CLI defines anyway.
	return ok && len(short) == 1 && cmd.Flags().ShorthandLookup(short) != nil
}

// parseHeaders turns repeated -H values into a header set.
//
// Add rather than Set, so a header given twice is sent twice — that is what the
// protocol allows, and silently keeping only the last one would be a surprise
// nobody could debug from the outside.
func parseHeaders(raw []string) (http.Header, error) {
	header := http.Header{}
	for _, entry := range raw {
		name, value, ok := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("bad header %q — write it as \"Name: value\"", entry)
		}
		header.Add(name, strings.TrimSpace(value))
	}
	return header, nil
}
