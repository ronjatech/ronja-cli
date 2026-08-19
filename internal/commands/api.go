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

Pull one field out of the answer with --jq, so nothing has to be piped through
another interpreter to read it. -r prints strings unquoted, which is what makes
the result safe to substitute:

  ronja api /api/v2/feature/query --jq '.items[] | select(.scope=="shared") | .id'
  id=$(ronja api -X POST /api/v2/workflow -d @wf.json --jq '.id' -r)

Wait for asynchronous work with --wait-until, which re-issues the request until
a jq condition holds. It works for anything that finishes eventually — workflow
runs, table builds, connector syncs, exports:

  ronja api /api/v2/workflow/run/$id --wait-until '.status != "running"' --jq '.status' -r

Note jq's truthiness: [] and 0 are TRUE, so write an explicit comparison
(.errors | length > 0) rather than relying on a bare field.

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
endpoint takes. Because the body is streamed as it arrives, a call that runs out
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
				formBody, contentType, err := buildForm(form)
				if err != nil {
					return err
				}
				body = formBody
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

			return out.emit(resp, method, path)
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
		"how long to wait for one request, response body included (0 waits as long as it takes)")
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

// maxRequestBody caps a body read from a file or a pipe.
//
// The whole body is held in memory and handed to the request as one buffer, so
// an unbounded read is an unbounded allocation driven by whatever path (or pipe)
// was named — `-d @video.mp4` or a mistyped `-d @/dev/urandom` should fail
// saying so, not swap the machine out. It is deliberately far above the read
// side's maxDocBody: a docs page is a few KB, while a legitimate API body can
// carry an embedded file, and refusing one of those would be the worse failure.
const maxRequestBody = 16 << 20

// requestBody resolves -d into bytes, or nil for no body.
//
// The @ prefix is curl's, and so is the reason for it: a JSON body big enough
// to matter does not belong on a command line, where the shell's quoting rules
// get a vote on its contents.
func requestBody(data string) ([]byte, error) {
	if data == "" {
		return nil, nil
	}
	rest, isRef := strings.CutPrefix(data, "@")
	if !isRef {
		// A literal body already fits in a command line, which is a far tighter
		// bound than anything applied here would be.
		return []byte(data), nil
	}
	if rest == "-" {
		// Capped like the file case, and for a stronger reason: a pipe has no
		// size to check in advance, so a producer that never stops (a `yes`, a
		// tail of a growing log) would otherwise read forever with nothing to
		// report.
		return readStdinLimit("-d @-", maxRequestBody)
	}
	return readBodyFile(rest)
}

// readBodyFile reads -d @file, refusing anything past maxRequestBody.
//
// Read through a LimitReader rather than stat-then-read: a size checked before
// the read is a different moment from the read itself, and the things most
// likely to be oversized here — a growing log, a character device — either
// change between the two or report no size at all.
func readBodyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	defer f.Close()
	// One byte past the cap, so "exactly at the limit" and "over it" are
	// distinguishable rather than both looking full.
	body, err := io.ReadAll(io.LimitReader(f, maxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(body) > maxRequestBody {
		return nil, fmt.Errorf("request body %s is larger than the %d MiB limit — is that the file you meant?",
			path, maxRequestBody>>20)
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
