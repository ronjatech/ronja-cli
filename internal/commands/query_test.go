package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// queryServer answers POST /api/v2/duckdb/query with a scripted envelope.
type queryServer struct {
	t      *testing.T
	server *httptest.Server

	// answer's zero value is an empty successful result.
	answer api.QueryResult
	// status overrides the HTTP status; 0 is 200.
	status int

	inputs []api.QueryInput
	// raws are the same bodies as JSON objects, to see whether a key was sent
	// at all.
	raws []map[string]any
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
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read query body: %v", err)
		}
		var in api.QueryInput
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode query body: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode query body: %v", err)
		}
		q.inputs = append(q.inputs, in)
		q.raws = append(q.raws, m)
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

// The wire format (csv_meta) is the CLI's decision, not a flag, so it is
// asserted.
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
	// Present-but-false, not absent: a shape that changes with the answer is
	// one a consumer must branch on.
	if _, ok := payload["truncated"]; !ok {
		t.Errorf("truncated missing from the envelope: %v", payload)
	}
	if payload["instanceKind"] != "duckdb" {
		t.Errorf("instanceKind = %v", payload["instanceKind"])
	}
}

// --- the 200-with-an-error trap ---------------------------------------------

// A failed query is HTTP 200 with an error in the envelope; exiting non-zero on
// it is why this command exists beside `ronja api`.
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

// The envelope is emitted before the failure is returned, so a caller need not
// re-run the query to learn why.
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

func TestQueryFailsOnAnHTTPError(t *testing.T) {
	q := newQueryServer(t)
	q.status = http.StatusForbidden
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "SELECT 1"); err == nil {
		t.Fatal("exit code was zero on a 403")
	}
}

// --- truncation -------------------------------------------------------------

// Truncation is a note on stderr, not a failure: stdout is a CSV somebody is
// parsing.
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
	if !strings.Contains(stderr, "2 rows written to") {
		t.Errorf("no narration on stderr:\n%s", stderr)
	}
	// Owner-only: tenant data nobody else on the machine may read.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// os.WriteFile's mode applies only on create, so an existing 0644 file would
// keep that mode and expose the result to every account on the machine.
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
	// The atomic write's temp file must be cleaned up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the output file: %v", len(entries), entries)
	}
}

// --- the deadline -----------------------------------------------------------

// Only the flag reaching the client; that a value past the client's own
// ceiling applies is pinned in internal/api.
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

// Zero means no deadline, never the already-passed one context.WithTimeout(ctx, 0)
// would give.
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

// No source on a terminal is an immediate error: never prompt, never block on a
// terminal read.
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

// --- the --jq flag-order footgun ---------------------------------------------

// With "-r" bound as the expression, '.rowCount' would become the SQL, pass the
// arity check, and fail server-side with a syntax error nobody typed.
func TestQueryRefusesAFlagAsTheJQExpression(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())

	_, err := runCLI(t, t.TempDir(), "query", "--jq", "-r", ".rowCount")
	if err == nil {
		t.Fatal("a flag was accepted as the --jq expression")
	}
	if !strings.Contains(err.Error(), "--jq") || !strings.Contains(err.Error(), `"-r"`) {
		t.Errorf("the refusal names neither the flag nor what it got: %v", err)
	}
	if len(q.inputs) != 0 {
		t.Errorf("a query went out anyway: %+v", q.inputs)
	}
}

func TestQueryAcceptsJQWithTheFlagAfterIt(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	out, err := runCLI(t, t.TempDir(), "query", "--jq", ".rowCount", "-r", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != "2\n" {
		t.Errorf("stdout = %q", out)
	}
}

// --- the hidden --attach flag ---------------------------------------------------

// Each --attach alias=secret splits on the first = only.
func TestQueryAttachSendsEachDatabase(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "--attach", "crm=secret-a", "--attach", "erp=secret=e", "SELECT 1"); err != nil {
		t.Fatalf("query: %v", err)
	}
	q.only()
	got, _ := q.raws[0]["attach"].(map[string]any)
	if len(got) != 2 || got["crm"] != "secret-a" || got["erp"] != "secret=e" {
		t.Fatalf("attach = %v, want both databases", q.raws[0]["attach"])
	}
}

func TestQueryAttachRefusesABadValue(t *testing.T) {
	q := newQueryServer(t)
	signInTo(t, q.URL())
	for _, args := range [][]string{
		{"--attach", "crm"},
		{"--attach", "=secret-a"},
		{"--attach", "crm="},
		{"--attach", "crm=secret-a", "--attach", "crm=secret-b"},
	} {
		_, err := runCLI(t, t.TempDir(), append(append([]string{"query"}, args...), "SELECT 1")...)
		if err == nil || !strings.Contains(err.Error(), "--attach") || !(strings.Contains(err.Error(), "alias=secret") || strings.Contains(err.Error(), "more than once")) {
			t.Errorf("%v: want a usage error naming --attach, got %v", args, err)
		}
	}
	if len(q.inputs) != 0 {
		t.Fatalf("a refused --attach still sent %d request(s)", len(q.inputs))
	}
}

func TestQueryWithoutAttachSendsNoAttachKey(t *testing.T) {
	q := newQueryServer(t)
	q.answer = api.QueryResult{Result: sampleCSV, RowCount: 2}
	signInTo(t, q.URL())

	if _, err := runCLI(t, t.TempDir(), "query", "SELECT 1"); err != nil {
		t.Fatalf("query: %v", err)
	}
	q.only()
	if _, ok := q.raws[0]["attach"]; ok {
		t.Fatalf("a query without --attach sent an attach key: %v", q.raws[0])
	}
}

// --attach stays hidden from --help while the HTTP join is unpublished.
func TestQueryAttachFlagIsHidden(t *testing.T) {
	f := newQueryCmd().Flags().Lookup("attach")
	if f == nil {
		t.Fatal("ronja query has no --attach flag")
	}
	if !f.Hidden {
		t.Fatal("--attach is visible in --help; it stays hidden until the HTTP guides document joins")
	}
}
