package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// `ronja query` is a thin command over one endpoint, so the tests are about the
// three things that are NOT thin: which of the three SQL sources won, what
// landed on stdout versus stderr, and whether a failure was noticed at all —
// the last being the trap the command exists to close, since a failed query is
// HTTP 200.

// queryServer answers POST /api/v2/duckdb/query with a scripted envelope.
type queryServer struct {
	t      *testing.T
	server *httptest.Server

	// answer is what the endpoint returns. Zero value = an empty successful
	// result, which is enough for the tests that are about the request.
	answer api.QueryResult
	// status overrides the HTTP status; 0 is 200.
	status int

	inputs []api.QueryInput
}

func newQueryServer(t *testing.T) *queryServer {
	t.Helper()
	q := &queryServer{t: t}
	q.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/duckdb/query" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var in api.QueryInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatalf("decode query body: %v", err)
		}
		q.inputs = append(q.inputs, in)
		if q.status != 0 {
			http.Error(w, `{"error":"boom"}`, q.status)
			return
		}
		writeJSON(w, q.answer)
	}))
	t.Cleanup(q.server.Close)
	return q
}

func (q *queryServer) URL() string { return q.server.URL }

func (q *queryServer) only() api.QueryInput {
	q.t.Helper()
	if len(q.inputs) != 1 {
		q.t.Fatalf("served %d queries, want 1: %+v", len(q.inputs), q.inputs)
	}
	return q.inputs[0]
}

const sampleCSV = "id,amount\n1,10\n2,20\n"

// --- the happy path ---------------------------------------------------------

// CSV on stdout and nothing else there, and csv_meta on the wire — the format
// is the CLI's decision, not a flag, so it is asserted rather than assumed.
func TestQueryPrintsTheCSV(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != sampleCSV {
		t.Errorf("stdout = %q, want the CSV verbatim", out)
	}
	in := q.only()
	if in.SQL != "SELECT 1" {
		t.Errorf("sql = %q", in.SQL)
	}
	if in.Format != api.QueryFormatCSVMeta {
		t.Errorf("format = %q, want %q", in.Format, api.QueryFormatCSVMeta)
	}
	if in.MaxRows != 0 {
		t.Errorf("maxRows = %d, want it omitted so the server's cap applies", in.MaxRows)
	}
}

func TestQueryPassesMaxRows(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "--max-rows", "50", "SELECT 1"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := q.only().MaxRows; got != 50 {
		t.Errorf("maxRows = %d, want 50", got)
	}
}

// --json is the whole envelope, one object, on stdout. A caller wanting the row
// count or the truncation flag should not have to parse CSV to get it.
func TestQueryJSONEmitsTheWholeEnvelope(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{
		Result: sampleCSV, RowCount: 2, Truncated: false,
		SQL: "SELECT 1", InstanceKind: "duckdb",
	}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "--json", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["result"] != sampleCSV {
		t.Errorf("result = %v", payload["result"])
	}
	if payload["rowCount"] != float64(2) {
		t.Errorf("rowCount = %v", payload["rowCount"])
	}
	// Present-but-false rather than absent: a shape that changes with the
	// answer is a shape a consumer has to branch on.
	if _, ok := payload["truncated"]; !ok {
		t.Errorf("truncated missing from the envelope: %v", payload)
	}
	if payload["instanceKind"] != "duckdb" {
		t.Errorf("instanceKind = %v", payload["instanceKind"])
	}
}

// --- the 200-with-an-error trap ---------------------------------------------

// The reason this command exists rather than `ronja api -X POST
// /api/v2/duckdb/query`: the transport is perfectly happy, and a pipeline built
// on the generic runner would continue over a query that never ran.
func TestQueryFailsOnAnEnvelopeError(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Error: `Catalog Error: Table "orders" does not exist`}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "SELECT * FROM orders")
	if err == nil {
		t.Fatal("exit code was zero on a failed query")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error does not carry the server's message: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing — there is no result", out)
	}
}

// In --json the envelope is still the answer to what was asked, so it is
// emitted before the failure is returned. A caller left with only an exit code
// has to run the query again to find out why.
func TestQueryJSONStillEmitsTheEnvelopeOnAFailure(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Error: "syntax error at or near \"SELCT\""}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "--json", "SELCT 1")
	if err == nil {
		t.Fatal("exit code was zero on a failed query")
	}
	if payload := decodeJSON(t, out); !strings.Contains(payload["error"].(string), "SELCT") {
		t.Errorf("envelope did not carry the error: %v", payload)
	}
}

// An HTTP-level failure is the other non-zero path.
func TestQueryFailsOnAnHTTPError(t *testing.T) {
	q := newQueryServer(t)
	q.status = http.StatusForbidden
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "SELECT 1"); err == nil {
		t.Fatal("exit code was zero on a 403")
	}
}

// --- truncation -------------------------------------------------------------

// Truncation is a note, not a failure: the rows are real, there are simply more
// of them. It goes to stderr because stdout is a CSV somebody is parsing.
func TestQueryNotesTruncationOnStderrAndExitsZero(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 100000, Truncated: true}
	signInTo(t, q.URL())

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, t.TempDir(), "query", "SELECT 1")
	})
	if err != nil {
		t.Fatalf("truncation was treated as a failure: %v", err)
	}
	if out != sampleCSV {
		t.Errorf("stdout = %q, want the CSV alone", out)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, "100000") {
		t.Errorf("no usable truncation note on stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--max-rows") {
		t.Errorf("the note does not say what to do:\n%s", stderr)
	}
}

// ...and silence when it is not truncated. A note on every query is a note
// nobody reads.
func TestQuerySaysNothingWhenTheResultIsComplete(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, t.TempDir(), "query", "SELECT 1"); err != nil {
			t.Fatalf("query: %v", err)
		}
	})
	if strings.Contains(stderr, "truncated") {
		t.Errorf("truncation note on a complete result:\n%s", stderr)
	}
}

// --- --out ------------------------------------------------------------------

func TestQueryWritesToAFileAndLeavesStdoutEmpty(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "rows.csv")

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, dir, "query", "--out", path, "SELECT 1")
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing when --out is given", out)
	}
	if got := readFile(t, path); got != sampleCSV {
		t.Errorf("file = %q, want the CSV", got)
	}
	// The narration belongs on stderr, so `--out` composes with a redirect.
	if !strings.Contains(stderr, "2 rows written to") {
		t.Errorf("no narration on stderr:\n%s", stderr)
	}
	// Owner-only: this is tenant data the caller was authorised to read and
	// nobody else on the machine was.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// Writing over an EXISTING file is the case os.WriteFile gets wrong: its mode
// argument applies only on create, so a rows.csv an earlier tool left at 0644
// would keep that mode and publish the result to every account on the machine.
func TestQueryOutTightensAnExistingFilesMode(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "rows.csv")
	if err := os.WriteFile(path, []byte("stale,content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureStderr(t, func() {
		if _, err := runCLI(t, dir, "query", "--out", path, "SELECT 1"); err != nil {
			t.Fatalf("query: %v", err)
		}
	})

	if got := readFile(t, path); got != sampleCSV {
		t.Errorf("file = %q, want the new CSV", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600 — a pre-existing file kept its own mode", perm)
	}
	// The temp file the atomic write goes through is a detail, but one it must
	// clean up: a directory slowly filling with .rows.csv-* is a bug a user
	// finds long after the run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the output file: %v", len(entries), entries)
	}
}

// --- the deadline -----------------------------------------------------------

// A heavy query is routed to bigger compute server-side and takes minutes, so
// the deadline has to be movable. The plumbing that makes a value past the
// client's own ceiling actually apply is pinned in internal/api; this is the
// flag reaching it.
func TestQueryTimeoutFlagIsAccepted(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "--timeout", "10m", "SELECT 1"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(q.inputs) != 1 {
		t.Errorf("the query did not go out: %+v", q.inputs)
	}
}

// Zero is "wait as long as it takes", so it must not be read as a deadline that
// has already passed — the shape context.WithTimeout(ctx, 0) would give it.
func TestQueryTimeoutZeroWaits(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "--timeout", "0", "SELECT 1")
	if err != nil {
		t.Fatalf("--timeout 0 failed the query: %v", err)
	}
	if out != sampleCSV {
		t.Errorf("stdout = %q", out)
	}
}

// Negative has no meaning, and zero already means "no deadline" — so it is a
// typo rather than an intent worth guessing at. Mirrors `wf test`.
func TestQueryRefusesANegativeTimeout(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())

	_, err := runCLI(t, t.TempDir(), "query", "--timeout", "-5s", "SELECT 1")
	if err == nil {
		t.Fatal("a negative timeout was accepted")
	}
	if !strings.Contains(err.Error(), "pass 0") {
		t.Errorf("error does not say what 0 means: %v", err)
	}
	if len(q.inputs) != 0 {
		t.Errorf("a query went out anyway: %+v", q.inputs)
	}
}

// --- the SQL sources --------------------------------------------------------

func TestQueryReadsSQLFromAFile(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "report.sql")
	const sql = "SELECT *\nFROM {{ ref('table-abc') }}\nWHERE amount > 10\n"
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "query", "--file", path); err != nil {
		t.Fatalf("query: %v", err)
	}
	// Verbatim, newlines and all — the multi-line case is the reason the flag
	// exists rather than everyone quoting it into a shell argument.
	if got := q.only().SQL; got != sql {
		t.Errorf("sql = %q, want the file verbatim", got)
	}
}

func TestQueryReadsSQLFromAPipe(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())
	withStdin(t, "SELECT 42\n")

	if _, err := runCLI(t, t.TempDir(), "query"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := q.only().SQL; got != "SELECT 42\n" {
		t.Errorf("sql = %q", got)
	}
}

func TestQueryReadsSQLFromStdinWithFileDash(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())
	withStdin(t, "SELECT 7")

	if _, err := runCLI(t, t.TempDir(), "query", "--file", "-"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := q.only().SQL; got != "SELECT 7" {
		t.Errorf("sql = %q", got)
	}
}

// No source and a terminal: an error, immediately. Prompting is the one thing
// this must never do, and blocking on a read of the terminal is worse.
func TestQueryRefusesWithNoSourceOnATerminal(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())
	withTerminalStdin(t)

	_, err := runCLI(t, t.TempDir(), "query")
	if err == nil {
		t.Fatal("no SQL was accepted")
	}
	if !strings.Contains(err.Error(), "no SQL given") {
		t.Errorf("error does not say what to supply: %v", err)
	}
	if len(q.inputs) != 0 {
		t.Errorf("a query went out anyway: %+v", q.inputs)
	}
}

// An empty pipe is a source that produced nothing, which is a different mistake
// from giving no source at all — and both stop before a round trip.
func TestQueryRefusesBlankSQL(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())
	withStdin(t, "   \n")

	_, err := runCLI(t, t.TempDir(), "query")
	if err == nil {
		t.Fatal("blank SQL was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v", err)
	}
	if len(q.inputs) != 0 {
		t.Errorf("a query went out anyway: %+v", q.inputs)
	}
}

func TestQueryRefusesTwoSources(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())

	_, err := runCLI(t, t.TempDir(), "query", "--file", "report.sql", "SELECT 1")
	if err == nil {
		t.Fatal("an argument and --file together were accepted")
	}
	if len(q.inputs) != 0 {
		t.Errorf("a query went out anyway: %+v", q.inputs)
	}
}

func TestQueryRefusesWhenSignedOut(t *testing.T) {
	q := newQueryServer(t)
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	t.Setenv("RONJA_URL", q.URL())
	t.Setenv("RONJA_TOKEN", "")

	_, err := runCLI(t, t.TempDir(), "query", "SELECT 1")
	if err == nil {
		t.Fatal("an unauthenticated query was attempted")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("error does not say what to do: %v", err)
	}
}
