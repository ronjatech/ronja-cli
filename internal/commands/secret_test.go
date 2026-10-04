package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The `ronja secret create` tests. The property under test above all others is
// that a value goes into exactly one request body and into NOTHING the command
// prints — stdout, stderr or the error it returns (which run() prints) — so
// every test that supplies a value supplies a sentinel and looks for it
// everywhere.

const secretSentinel = "sk-live-SENTINEL-7f3a9c-do-not-print"

// secretFake is a Ronja serving POST /api/v2/secret and nothing else. Any other
// request fails the test: this command makes one request, and a second one is a
// regression whatever it is.
type secretFake struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []string
	bodies   [][]byte

	// status and body replace the success answer, for the forwarded-error cases.
	status int
	body   string
}

func newSecretFake(t *testing.T) *secretFake {
	t.Helper()
	f := &secretFake{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	signInTo(t, f.server.URL)
	return f
}

func (f *secretFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, raw)
	f.mu.Unlock()

	if r.Method != http.MethodPost || r.URL.Path != "/api/v2/secret" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	secretType, _ := in["secretType"].(string)
	writeJSON(w, map[string]any{
		"id":         "secret-abc123",
		"name":       in["name"],
		"secretType": secretType,
		"featureID":  in["featureID"],
		"status":     "ready",
	})
}

// only returns the single request body, failing unless exactly one was sent.
func (f *secretFake) only(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 {
		t.Fatalf("want exactly one request, got %d: %v", len(f.bodies), f.requests)
	}
	var body map[string]any
	if err := json.Unmarshal(f.bodies[0], &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	return body
}

// sentinelCount is how many times the sentinel appears across every body sent.
func (f *secretFake) sentinelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.bodies {
		n += strings.Count(string(b), secretSentinel)
	}
	return n
}

func (f *secretFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// runSecretCLI runs one invocation and returns stdout, stderr and the error
// text run() would print (empty on success).
func runSecretCLI(t *testing.T, args ...string) (stdout, stderr, errText string) {
	t.Helper()
	var err error
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, t.TempDir(), args...)
	})
	if err != nil {
		errText = err.Error()
	}
	return stdout, stderr, errText
}

// assertNoSentinel is the core assertion: the value is in none of the three
// places the command can speak.
func assertNoSentinel(t *testing.T, stdout, stderr, errText string) {
	t.Helper()
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "error": errText} {
		if strings.Contains(s, secretSentinel) {
			t.Fatalf("the secret value reached %s:\n%s", name, s)
		}
	}
}

// withFakePassword makes the echo-off prompt answer with values in order, and
// counts the prompts.
func withFakePassword(t *testing.T, values ...string) *int {
	t.Helper()
	calls := 0
	saved := readPassword
	readPassword = func(int) ([]byte, error) {
		calls++
		if calls > len(values) {
			t.Fatalf("prompted %d times, want %d", calls, len(values))
		}
		return []byte(values[calls-1]), nil
	}
	t.Cleanup(func() { readPassword = saved })
	return &calls
}

func credentialsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	creds, ok := body["credentials"].(map[string]any)
	if !ok {
		t.Fatalf("body has no credentials object: %v", body)
	}
	return creds
}

// C1 — a prompted value reaches the body once and nothing printed.
func TestSecretCreatePromptedValue(t *testing.T) {
	f := newSecretFake(t)
	withTerminalStdin(t)
	calls := withFakePassword(t, secretSentinel)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	if *calls != 1 {
		t.Fatalf("prompted %d times, want 1", *calls)
	}
	body := f.only(t)
	if got := credentialsOf(t, body)["apiKey"]; got != secretSentinel {
		t.Fatalf("credentials.apiKey = %v, want the prompted value", got)
	}
	if f.sentinelCount() != 1 {
		t.Fatalf("value sent %d times, want exactly once", f.sentinelCount())
	}
	if body["secretType"] != "api_key" || body["featureID"] != "collection-1" {
		t.Fatalf("body = %v", body)
	}
	if hosts, _ := body["allowedURLs"].([]any); len(hosts) != 1 || hosts[0] != "api.stripe.com" {
		t.Fatalf("allowedURLs = %v", body["allowedURLs"])
	}
	if fields, _ := body["agentFields"].([]any); len(fields) != 1 || fields[0] != "apiKey" {
		t.Fatalf("agentFields = %v, want [apiKey] — a field a Workflow reads must be listed", body["agentFields"])
	}
	want := "secret-abc123  Stripe (api_key, feature collection-1, 1 field: apiKey — 36 chars)\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "apiKey (input hidden): ") {
		t.Fatalf("the prompt was not on stderr:\n%s", stderr)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// C1 — a piped value, with its one trailing newline stripped.
func TestSecretCreatePipedValue(t *testing.T) {
	f := newSecretFake(t)
	withStdin(t, secretSentinel+"\n")
	calls := withFakePassword(t)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	if *calls != 0 {
		t.Fatalf("prompted with a pipe on stdin")
	}
	if got := credentialsOf(t, f.only(t))["apiKey"]; got != secretSentinel {
		t.Fatalf("credentials.apiKey = %q, want the piped value without its newline", got)
	}
	if f.sentinelCount() != 1 {
		t.Fatalf("value sent %d times, want exactly once", f.sentinelCount())
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// C1 — a --from-file value, and several fields as a JSON object.
func TestSecretCreateFromFile(t *testing.T) {
	f := newSecretFake(t)
	withTerminalStdin(t) // a file wins over the terminal: no prompt
	withFakePassword(t)
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte(`{"apiKey":"`+secretSentinel+`","apiSecret":"other-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Acme", "--host", "api.acme.test",
		"--field", "apiKey", "--field", "apiSecret", "--from-file", path)
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	creds := credentialsOf(t, f.only(t))
	if creds["apiKey"] != secretSentinel || creds["apiSecret"] != "other-value" {
		t.Fatalf("credentials = %v", creds)
	}
	if f.sentinelCount() != 1 {
		t.Fatalf("value sent %d times, want exactly once", f.sentinelCount())
	}
	if !strings.Contains(stdout, "2 fields: apiKey — 36 chars, apiSecret — 11 chars") {
		t.Fatalf("stdout = %q", stdout)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// C1 — a database login from a file goes as the object it was, with the dialect
// and no hosts or fields of the CLI's own.
func TestSecretCreateDialectFromFile(t *testing.T) {
	f := newSecretFake(t)
	path := filepath.Join(t.TempDir(), "db.json")
	login := `{"host":"db.example.com","port":5432,"database":"sales","username":"reader","password":"` + secretSentinel + `"}`
	if err := os.WriteFile(path, []byte(login+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Warehouse", "--dialect", "postgres", "--from-file", path)
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	body := f.only(t)
	if body["dialect"] != "postgres" || body["secretType"] != "database" {
		t.Fatalf("body = %v", body)
	}
	for _, k := range []string{"allowedURLs", "agentFields", "fieldSchema"} {
		if _, ok := body[k]; ok {
			t.Fatalf("a database login sent %s — the server owns the schema", k)
		}
	}
	if creds := credentialsOf(t, body); creds["password"] != secretSentinel || creds["port"] != float64(5432) {
		t.Fatalf("credentials = %v", creds)
	}
	if !strings.Contains(stdout, "(database, feature collection-1, 5 fields: ") || !strings.Contains(stdout, "password — 36 chars") {
		t.Fatalf("stdout = %q", stdout)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// C1 — a server 400 is forwarded, and a body quoting the value still does not
// put it on screen: the CLI scrubs every value from what it reports.
func TestSecretCreateForwardsServerErrorWithoutTheValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"api error field", `{"error":"invalid apiKey ` + secretSentinel + `: not a Stripe key"}`, "invalid apiKey [redacted]: not a Stripe key"},
		{"raw body", `{"detail":{"apiKey":"` + secretSentinel + `"}}`, "[redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSecretFake(t)
			f.status, f.body = http.StatusBadRequest, tc.body
			withStdin(t, secretSentinel)

			stdout, stderr, errText := runSecretCLI(t, "secret", "create",
				"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
			if errText == "" {
				t.Fatalf("a 400 succeeded")
			}
			if !strings.Contains(errText, tc.want) || !strings.Contains(errText, "400") {
				t.Fatalf("the server's error was not forwarded: %q", errText)
			}
			if stdout != "" {
				t.Fatalf("stdout on failure: %q", stdout)
			}
			assertNoSentinel(t, stdout, stderr, errText)
		})
	}
}

// A value with a quote or a backslash in it reaches a raw error body in its
// JSON-escaped spelling, which is not the value's own bytes — so that spelling
// is scrubbed too.
func TestSecretCreateRedactsTheJSONEscapedValue(t *testing.T) {
	value := secretSentinel + `"quoted\path`
	escaped, _ := json.Marshal(value)
	f := newSecretFake(t)
	f.status = http.StatusBadRequest
	f.body = `{"detail":{"apiKey":` + string(escaped) + `}}`
	withStdin(t, value)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if !strings.Contains(errText, "[redacted]") || !strings.Contains(errText, "400") {
		t.Fatalf("the server's error was not forwarded scrubbed: %q", errText)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// I1 — a database login's ordinary fields are the words of the server's own
// refusal. Only a sensitive field's value is scrubbed, so "postgres" as a
// username does not eat the dialect's name out of the message, while the
// password in the same body still is.
func TestSecretCreateDialectRedactsOnlySensitiveFields(t *testing.T) {
	const serverSays = "postgres credentials missing required fields: database (sslMode require)"
	for _, tc := range []struct {
		name  string
		login string
		echo  string // what the server quotes back, beside serverSays
	}{
		{
			name:  "a string password",
			login: `{"host":"db.example.com","port":5432,"username":"postgres","sslMode":"require","password":"` + secretSentinel + `"}`,
			echo:  secretSentinel,
		},
		{
			name:  "a number under a sensitive name",
			login: `{"host":"db.example.com","username":"postgres","sslMode":"require","pinToken":73195824610}`,
			echo:  "73195824610",
		},
		{
			name:  "an object under a sensitive name",
			login: `{"host":"db.example.com","username":"postgres","sslMode":"require","privateKey":{"pem":"` + secretSentinel + `"}}`,
			echo:  secretSentinel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSecretFake(t)
			f.status = http.StatusBadRequest
			f.body = `{"error":"` + serverSays + `; got ` + tc.echo + `"}`
			withStdin(t, tc.login)

			stdout, stderr, errText := runSecretCLI(t, "secret", "create",
				"--feature", "collection-1", "--name", "Warehouse", "--dialect", "postgres")
			if !strings.Contains(errText, serverSays) {
				t.Fatalf("the server's message was not forwarded byte for byte:\n got %q\nwant it to contain %q", errText, serverSays)
			}
			if strings.Contains(errText, tc.echo) || !strings.Contains(errText, "got [redacted]") {
				t.Fatalf("the sensitive value was not scrubbed: %q", errText)
			}
			assertNoSentinel(t, stdout, stderr, errText)
		})
	}
}

// C2 — the refusals, each before any request goes out.
func TestSecretCreateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stdin    *string // nil = a terminal
		args     []string
		want     string
		noPrompt bool
	}{
		{
			name: "a value in --field",
			args: []string{"--host", "api.stripe.com", "--field", "apiKey=" + secretSentinel},
			want: "values are never flags — they end up in your shell history. Run without a value to be prompted, or pipe it on stdin.",
		},
		{name: "empty piped stdin", stdin: ptr(""), args: []string{"--host", "api.stripe.com"}, want: "no value on stdin — pipe one in, or pass --in-browser to enter it yourself in Ronja"},
		{name: "a bare newline on stdin", stdin: ptr("\n"), args: []string{"--host", "api.stripe.com"}, want: "no value on stdin — pipe one in, or pass --in-browser to enter it yourself in Ronja"},
		{name: "empty prompt", args: []string{"--host", "api.stripe.com"}, want: "no value entered for apiKey"},
		{
			name: "dialect on a terminal without a file",
			args: []string{"--dialect", "postgres"},
			want: "database credentials take a JSON object: use --from-file, pipe it on stdin, or pass --in-browser to enter it in Ronja",
			// The terminal is never prompted for a database login.
			noPrompt: true,
		},
		{name: "dialect with --host", args: []string{"--dialect", "postgres", "--host", "db.example.com"}, want: "drop --host and --field"},
		{name: "dialect with --field", args: []string{"--dialect", "postgres", "--field", "password"}, want: "drop --host and --field"},
		{name: "neither host nor dialect", args: []string{}, want: "pass --host <host> for an API key"},
		{name: "an unknown key", stdin: ptr(`{"apiKey":"x1234","nope":"y1234"}`), args: []string{"--host", "h", "--field", "apiKey", "--field", "apiSecret"}, want: "unexpected field nope in the input — the fields are: apiKey, apiSecret"},
		{name: "a missing key", stdin: ptr(`{"apiKey":"x1234"}`), args: []string{"--host", "h", "--field", "apiKey", "--field", "apiSecret"}, want: "the input has no apiSecret — the fields are: apiKey, apiSecret"},
		{name: "not JSON", stdin: ptr(secretSentinel), args: []string{"--host", "h", "--field", "a", "--field", "b"}, want: "stdin is not a JSON object of field → value"},
		{name: "a --field given twice", stdin: ptr(secretSentinel), args: []string{"--host", "h", "--field", "apiKey", "--field", "apiKey"}, want: "--field apiKey is given twice"},
		{name: "an empty --field", stdin: ptr(secretSentinel), args: []string{"--host", "h", "--field", " "}, want: "--field cannot be empty"},
		{name: "a number where a string goes", stdin: ptr(`{"a":12345678,"b":"y1234"}`), args: []string{"--host", "h", "--field", "a", "--field", "b"}, want: "the value for a must be a JSON string"},
		// A repeated key: encoding/json keeps the last value, so the first would
		// go out unchecked — and, as raw bytes on the database path, unredacted.
		{name: "a key given twice (API key)", stdin: ptr(`{"a":"` + secretSentinel + `","a":"y1234","b":"z1234"}`), args: []string{"--host", "h", "--field", "a", "--field", "b"}, want: `stdin names the field "a" twice — give each field once`},
		{name: "a key given twice (database)", stdin: ptr(`{"host":"db","password":"` + secretSentinel + `","password":"other-1234"}`), args: []string{"--dialect", "postgres"}, want: `stdin names the field "password" twice — give each field once`},
		// M1 — over the cap on stdin: the remedy is NOT --from-file, which has
		// the same cap.
		{name: "stdin over the cap", stdin: ptr(strings.Repeat("a", maxSecretInput+1)), args: []string{"--host", "h"}, want: "standard input carried more than 1 MiB, which no credential is — is that the right input?"},
		// M4 — a host is matched against a request's bare hostname, so anything
		// else would be stored and never match.
		{name: "a URL as --host", stdin: ptr(secretSentinel), args: []string{"--host", "https://api.stripe.com/v1"}, want: `--host "https://api.stripe.com/v1" is not a host: pass the host only, e.g. api.stripe.com or *.example.com`},
		{name: "a path in --host", stdin: ptr(secretSentinel), args: []string{"--host", "api.stripe.com/v1"}, want: "is not a host"},
		{name: "a port in --host", stdin: ptr(secretSentinel), args: []string{"--host", "api.stripe.com:443"}, want: "is not a host"},
		{name: "a user in --host", stdin: ptr(secretSentinel), args: []string{"--host", "me@api.stripe.com"}, want: "is not a host"},
		{name: "a query in --host", stdin: ptr(secretSentinel), args: []string{"--host", "api.stripe.com?x"}, want: "is not a host"},
		{name: "a fragment in --host", stdin: ptr(secretSentinel), args: []string{"--host", "api.stripe.com#x"}, want: "is not a host"},
		{name: "a space in --host", stdin: ptr(secretSentinel), args: []string{"--host", "api stripe.com"}, want: "is not a host"},
		{name: "a wildcard not as a prefix", stdin: ptr(secretSentinel), args: []string{"--host", "api.*.com"}, want: "is not a host"},
		{name: "a bare wildcard", stdin: ptr(secretSentinel), args: []string{"--host", "*."}, want: "is not a host"},
		{name: "an empty --host", stdin: ptr(secretSentinel), args: []string{"--host", " "}, want: "--host cannot be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSecretFake(t)
			if tc.stdin == nil {
				withTerminalStdin(t)
			} else {
				withStdin(t, *tc.stdin)
			}
			prompts := 0
			saved := readPassword
			readPassword = func(int) ([]byte, error) {
				prompts++
				return []byte(""), nil
			}
			t.Cleanup(func() { readPassword = saved })

			args := append([]string{"secret", "create", "--feature", "collection-1", "--name", "X"}, tc.args...)
			stdout, stderr, errText := runSecretCLI(t, args...)
			if !strings.Contains(errText, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", errText, tc.want)
			}
			if n := f.requestCount(); n != 0 {
				t.Fatalf("%d request(s) went out on a refusal", n)
			}
			if tc.noPrompt && prompts != 0 {
				t.Fatalf("prompted %d times", prompts)
			}
			if stdout != "" {
				t.Fatalf("stdout on a refusal: %q", stdout)
			}
			assertNoSentinel(t, stdout, stderr, errText)
		})
	}
}

// C2 — there is no flag a value could be passed in. A flag is the one input
// that is always in argv and usually in a shell history.
func TestSecretCreateHasNoValueFlag(t *testing.T) {
	cmd := newSecretCreateCmd()
	for _, name := range []string{"value", "secret", "password", "token", "credentials", "api-key"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("secret create has a --%s flag — values are never flags", name)
		}
	}
}

// C8 (part) — --json is metadata: it decodes, and nothing in it is named like
// a value.
func TestSecretCreateJSONIsMetadataOnly(t *testing.T) {
	newSecretFake(t)
	withStdin(t, secretSentinel)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create", "--json",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	payload := decodeJSON(t, stdout)
	if payload["id"] != "secret-abc123" || payload["secretType"] != "api_key" || payload["featureID"] != "collection-1" {
		t.Fatalf("payload = %v", payload)
	}
	fields, _ := payload["fields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("fields = %v", payload["fields"])
	}
	if f, _ := fields[0].(map[string]any); f["name"] != "apiKey" || f["length"] != float64(len(secretSentinel)) {
		t.Fatalf("fields[0] = %v", fields[0])
	}
	assertNoForbiddenKeys(t, payload)
	assertNoSentinel(t, stdout, stderr, errText)
}

// assertNoForbiddenKeys walks a decoded JSON value for a key that names a
// credential, at any depth.
func assertNoForbiddenKeys(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			switch strings.ToLower(k) {
			case "token", "credentials", "value":
				t.Fatalf("--json carries a %q field", k)
			}
			assertNoForbiddenKeys(t, child)
		}
	case []any:
		for _, child := range x {
			assertNoForbiddenKeys(t, child)
		}
	}
}

// A piped value ending in a Windows line ending loses exactly that, and
// nothing else.
func TestSecretCreateStripsOneCRLF(t *testing.T) {
	f := newSecretFake(t)
	withStdin(t, secretSentinel+"\r\n")

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	if got := credentialsOf(t, f.only(t))["apiKey"]; got != secretSentinel {
		t.Fatalf("credentials.apiKey = %q, want the piped value without its CRLF", got)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// M1 (the file half) — a file over the cap is refused, naming the file.
func TestSecretCreateRefusesAnOversizedFile(t *testing.T) {
	f := newSecretFake(t)
	path := filepath.Join(t.TempDir(), "huge.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", maxSecretInput+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com", "--from-file", path)
	if want := "is larger than 1 MiB, which no credential is — is that the right file?"; !strings.Contains(errText, want) {
		t.Fatalf("error = %q, want it to contain %q", errText, want)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d request(s) went out", n)
	}
}

// M4 — a wildcard prefix is a host, and goes through as given.
func TestSecretCreateAcceptsAWildcardHost(t *testing.T) {
	f := newSecretFake(t)
	withStdin(t, secretSentinel)

	_, _, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Acme", "--host", "*.example.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	if hosts, _ := f.only(t)["allowedURLs"].([]any); len(hosts) != 1 || hosts[0] != "*.example.com" {
		t.Fatalf("allowedURLs = %v", hosts)
	}
}

// M2 — signed out, the command says so BEFORE it reads a value: a piped value
// is left unread rather than swallowed by a command that was always going to
// refuse.
func TestSecretCreateSignedOutReadsNoValue(t *testing.T) {
	f := newSecretFake(t)
	signOutFrom(t, f.server.URL)
	withStdin(t, secretSentinel)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if !strings.Contains(errText, "not signed in") {
		t.Fatalf("error = %q, want the sign-in refusal", errText)
	}
	if pos, err := os.Stdin.Seek(0, io.SeekCurrent); err != nil || pos != 0 {
		t.Fatalf("stdin was read (offset %d, %v) before the sign-in check", pos, err)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d request(s) went out signed out", n)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// M3 — Ctrl-C at the hidden prompt stops the command at once. The root context
// owns SIGINT, so without this the prompt sat waiting for Enter.
func TestSecretCreateCtrlCAtThePromptStops(t *testing.T) {
	f := newSecretFake(t)
	withTerminalStdin(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	saved := readPassword
	readPassword = func(int) ([]byte, error) {
		close(entered)
		// A person who never presses Enter. Bounded, so code that waits for
		// the read fails this test rather than hanging it.
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		return []byte(secretSentinel), nil
	}
	t.Cleanup(func() {
		close(release)
		readPassword = saved
	})
	go func() {
		// The prompt has been reached, so the root's handler is installed.
		<-entered
		if self, err := os.FindProcess(os.Getpid()); err == nil {
			_ = self.Signal(os.Interrupt)
		}
	}()

	start := time.Now()
	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if !strings.Contains(errText, "cancelled; nothing was stored") {
		t.Fatalf("error = %q, want the cancellation", errText)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("Ctrl-C took %s to land — the prompt waited for Enter", waited)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d request(s) went out after Ctrl-C", n)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// M5 — one field given what looks like a several-field object is warned about,
// and stored as given: a JSON value can be the real thing. The warning quotes
// none of the content.
func TestSecretCreateWarnsOnAnObjectForOneField(t *testing.T) {
	f := newSecretFake(t)
	input := `{"apiKey":"` + secretSentinel + `"}`
	withStdin(t, input)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if errText != "" {
		t.Fatalf("create failed: %s", errText)
	}
	if want := `warning: this looks like a JSON object with a "apiKey" key; with one --field the whole input is stored as the value of apiKey`; !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	if got := credentialsOf(t, f.only(t))["apiKey"]; got != input {
		t.Fatalf("credentials.apiKey = %v, want the input as given", got)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// A request that timed out may still have landed, so the error says to look
// before retrying — and is scrubbed like every other.
func TestSecretCreateTimeoutSaysItMayHaveLanded(t *testing.T) {
	f := newSecretFake(t)
	failOnceWithATimeout(t, http.MethodPost, "/api/v2/secret")
	withStdin(t, secretSentinel)

	stdout, stderr, errText := runSecretCLI(t, "secret", "create",
		"--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if want := `the secret may have been created anyway; look for "Stripe" in Ronja before running this again`; !strings.Contains(errText, want) {
		t.Fatalf("error = %q, want it to contain %q", errText, want)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d request(s) reached the fake after the timeout", n)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// Assert the wire schema, not only the builder, so both the direct and browser
// paths must send every requested field as a required secret input.
func TestSecretCreateRequiredAPIKeyFields(t *testing.T) {
	for _, inBrowser := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "browser"}[inBrowser], func(t *testing.T) {
			args := []string{"secret", "create", "--feature", "collection-1", "--name", "Key pair", "--host", "api.example.com", "--field", "apiKey", "--field", "apiSecret", "--json"}
			var body func() map[string]any
			if inBrowser {
				fastInBrowser(t, 5*time.Second)
				fakeBrowser(t)
				noPrompt(t)
				withStdin(t, secretSentinel)
				f := newPendingFake(t, secretAnswer{status: "ready"})
				body = func() map[string]any { return f.createBodyJSON(t) }
				args = append(args, "--in-browser")
			} else {
				f := newSecretFake(t)
				body = func() map[string]any { return f.only(t) }
				withStdin(t, `{"apiKey":"`+secretSentinel+`","apiSecret":"`+secretSentinel+`"}`)
			}
			stdout, stderr, errText := runSecretCLI(t, args...)
			if errText != "" {
				t.Fatal(errText)
			}
			assertNoSentinel(t, stdout, stderr, errText)
			schema, ok := body()["fieldSchema"].(map[string]any)
			if !ok || len(schema) != 2 {
				t.Fatalf("unexpected field schema: %v", schema)
			}
			for i, name := range []string{"apiKey", "apiSecret"} {
				field, ok := schema[name].(map[string]any)
				if !ok || field["required"] != true || field["secret"] != true || field["type"] != "secret" || field["label"] != name || field["order"] != float64(i) {
					t.Fatalf("field %s metadata = %v", name, field)
				}
			}
		})
	}
}
