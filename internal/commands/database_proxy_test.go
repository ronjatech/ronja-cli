package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// proxyServer answers GET /api/v2/database/<id>/roles with a canned body and
// records the path it was asked for.
func proxyServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotPath
}

const proxyRolesBody = `{
  "host": "cluster.internal",
  "port": "5432",
  "sslMode": "require",
  "database": "t_x_crm",
  "roles": [],
  "proxy": {
    "host": "pg.ronja.tech",
    "port": "5432",
    "username": "mdb-abc.read",
    "database": "t_x_crm",
    "connectionString": "postgresql://mdb-abc.read:<YOUR_PAT>@pg.ronja.tech:5432/t_x_crm?sslmode=verify-full&sslrootcert=system&keepalives_idle=60",
    "readRoleMinted": true
  }
}`

// TestDatabaseEnvEmitsPGVariables is the whole contract of `db env`: the six PG*
// variables psql and pg_dump read, plus the profile's own token as PGPASSWORD.
//
// The token assertion is the one that matters. `db env` mints nothing and asks
// for nothing — it moves the credential the CLI already holds into the place a
// different program reads it from, exactly as `ronja env` does. A command that
// obtained a SECOND credential here would be a new thing to revoke that nobody
// knows exists.
func TestDatabaseEnvEmitsPGVariables(t *testing.T) {
	srv, gotPath := proxyServer(t, proxyRolesBody)
	signInTo(t, srv.URL)

	out, err := runCLI(t, t.TempDir(), "db", "env", "mdb-abc")
	if err != nil {
		t.Fatalf("db env: %v", err)
	}
	if *gotPath != "/api/v2/database/mdb-abc/roles" {
		t.Errorf("asked %q", *gotPath)
	}

	for _, want := range []string{
		"export PGHOST='pg.ronja.tech'",
		"export PGPORT='5432'",
		"export PGUSER='mdb-abc.read'",
		"export PGDATABASE='t_x_crm'",
		// verify-full and the OS trust store, never `require`: the token travels
		// as the password, so a mode that accepts any certificate hands it to
		// whoever answers the socket.
		"export PGSSLMODE='verify-full'",
		"export PGSSLROOTCERT='system'",
		"export PGPASSWORD='test-token'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The placeholder belongs on the connection card a human reads, never in
	// something a shell is about to evaluate.
	if strings.Contains(out, "YOUR_PAT") {
		t.Errorf("db env emitted the card's placeholder:\n%s", out)
	}
}

func TestDatabaseEnvJSON(t *testing.T) {
	srv, _ := proxyServer(t, proxyRolesBody)
	signInTo(t, srv.URL)

	out, err := runCLI(t, t.TempDir(), "--json", "db", "env", "mdb-abc")
	if err != nil {
		t.Fatalf("db env --json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got["PGUSER"] != "mdb-abc.read" || got["PGPASSWORD"] != "test-token" {
		t.Errorf("unexpected object: %v", got)
	}
}

// TestDatabaseProxyRefusals covers the two ways there is nothing to connect to,
// and pins that they stay DIFFERENT messages.
//
// "No proxy block" has two causes the server deliberately does not distinguish
// (no proxy deployed, or a caller who is not an admin), so the message names
// both. "No read role" is a different problem entirely — the proxy is there and
// the login would authenticate before being refused — and an admin told the
// wrong one of these goes looking for a deployment problem that does not exist.
func TestDatabaseProxyRefusals(t *testing.T) {
	t.Run("no proxy block", func(t *testing.T) {
		srv, _ := proxyServer(t, `{"host":"h","port":"5432","database":"t_x_crm","roles":[]}`)
		signInTo(t, srv.URL)

		_, err := runCLI(t, t.TempDir(), "db", "env", "mdb-abc")
		if err == nil {
			t.Fatal("want a refusal when the instance publishes no proxy")
		}
		if !strings.Contains(err.Error(), "no psql proxy configured") ||
			!strings.Contains(err.Error(), "not admin") {
			t.Errorf("refusal must name both causes, got: %v", err)
		}
	})

	// The refusal is presented as THE FIX, not as prose, so the command it
	// prints has to be one that works. It did not: it omitted `featureID`,
	// which POST :id/user requires (backend agent/tools/database_core.go,
	// createManagedDatabaseUser), so pasting it verbatim returned 400.
	//
	// The payload is therefore decoded rather than substring-matched — a
	// substring check passes on a command that is no longer valid JSON, which
	// is the other way this can regress. It is not proof the SERVER accepts
	// it: `cli` is its own Go module and cannot import the backend guard, so
	// that guard stays the authority and this pins the shape.
	t.Run("no read role minted", func(t *testing.T) {
		body := strings.Replace(proxyRolesBody, `"readRoleMinted": true`, `"readRoleMinted": false`, 1)
		srv, _ := proxyServer(t, body)
		signInTo(t, srv.URL)

		_, err := runCLI(t, t.TempDir(), "db", "env", "mdb-abc")
		if err == nil {
			t.Fatal("want a refusal when no read role is minted")
		}
		msg := err.Error()
		if !strings.Contains(msg, "no read role") {
			t.Fatalf("refusal must say what is missing, got: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(mintPayload(t, msg)), &payload); err != nil {
			t.Fatalf("the -d payload the refusal prints must be valid JSON: %v\nmessage:\n%s", err, msg)
		}
		// A STRING assertion, not a non-empty-rendering one. fmt.Sprint(nil) is
		// "<nil>", so a payload of {"featureID":null} would satisfy a
		// TrimSpace(Sprint(v)) != "" guard while the server answers 400 on
		// TrimSpace(opts.FeatureID) == "" — i.e. the exact regression this test
		// exists to catch would walk straight through it. Same for false and 0.
		for _, key := range []string{"access", "featureID"} {
			v, ok := payload[key]
			if !ok {
				t.Errorf("the printed mint command omits %q — without it POST :id/user answers 400.\nmessage:\n%s", key, msg)
				continue
			}
			if str, isStr := v.(string); !isStr || strings.TrimSpace(str) == "" {
				t.Errorf("the printed mint command's %q must be a non-empty string, got %#v.\nmessage:\n%s", key, v, msg)
			}
		}

		// The second half of the remedy. featureID has no default, and the CLI
		// deliberately does not borrow one from an existing role — see
		// proxyForDatabase's comment for why that is a choice rather than a
		// limitation. Naming a value is therefore the operator's job, and
		// nothing else in the CLI tells them where to look.
		if !strings.Contains(msg, "/api/v2/feature/query") {
			t.Errorf("refusal must say where to find a featureID, got:\n%s", msg)
		}
		// The message prints TWO commands, and the branch's premise is that a
		// printed remedy has to work — so pin the second one's flag order too.
		// `--jq` takes the next argument, so the widely-mistyped `--jq -r '<expr>'`
		// binds `-r` AS the expression; it is caught at runtime rather than
		// silently, but not by any assertion above.
		jq, r := strings.Index(msg, "--jq '"), strings.LastIndex(msg, " -r")
		if jq < 0 || r < jq {
			t.Errorf("the discovery command must pass --jq '<expr>' BEFORE -r, got:\n%s", msg)
		}
	})
}

// mintPayload returns the JSON the refusal's `-d '...'` argument carries.
//
// Anchored on `-d '` and the NEXT quote specifically. The message holds a
// second single-quoted argument — the `--jq '...'` expression on the discovery
// line — so a first-quote-to-last-quote span would swallow both commands and
// decode as nothing.
func mintPayload(t *testing.T, msg string) string {
	t.Helper()
	const anchor = "-d '"
	i := strings.Index(msg, anchor)
	if i < 0 {
		t.Fatalf("refusal prints no -d payload:\n%s", msg)
	}
	rest := msg[i+len(anchor):]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		t.Fatalf("refusal's -d payload is unterminated:\n%s", msg)
	}
	return rest[:j]
}

// TestDatabaseConnectRefusesEnvFlag: `db sql` and `db migrate` both take --env,
// so somebody who used it five minutes ago will type it here. The honest answer
// is not "unknown flag" — a dev copy IS a database at the proxy, addressed by
// its own id, because the username is the id and there is nothing left for the
// server's prod→dev overlay to redirect.
func TestDatabaseConnectRefusesEnvFlag(t *testing.T) {
	srv, _ := proxyServer(t, proxyRolesBody)
	signInTo(t, srv.URL)

	for _, cmd := range []string{"env", "connect"} {
		_, err := runCLI(t, t.TempDir(), "db", cmd, "--env", "dev", "mdb-abc")
		if err == nil {
			t.Fatalf("db %s --env dev was accepted", cmd)
		}
		if !strings.Contains(err.Error(), "OWN database id") {
			t.Errorf("db %s: refusal must say how a dev copy IS reached, got: %v", cmd, err)
		}
	}
}

// TestProxyConninfoCarriesTheKeepalive is why `db connect` exists at all beside
// `db env`: libpq has no environment variable for keepalives_idle, so only a
// command that builds the conninfo itself can keep a long interactive session
// alive through the load balancer's idle timer.
//
// It also pins that the password is NOT in the string. It reaches psql through
// the child's environment, which is what keeps it out of `ps` and out of the
// shell history.
func TestProxyConninfoCarriesTheKeepalive(t *testing.T) {
	got := proxyConninfo(&api.DatabaseProxy{
		Host: "pg.ronja.tech", Port: "5432",
		Username: "mdb-abc.read", Database: "t_x_crm",
	})
	want := "postgresql://mdb-abc.read@pg.ronja.tech:5432/t_x_crm" +
		"?sslmode=verify-full&sslrootcert=system&keepalives_idle=60"
	if got != want {
		t.Fatalf("conninfo =\n %q\nwant\n %q", got, want)
	}
	if strings.Contains(got, "YOUR_PAT") || strings.Contains(got, ":@") {
		t.Errorf("the conninfo must carry no password field at all: %q", got)
	}
}

// TestPsqlCommandLeavesSIGINTToPsql is the reason psqlCommand exists as its own
// function.
//
// Every command in this tree runs under the root's signal context, cancelled on
// SIGINT. On Ctrl-C the terminal delivers SIGINT to psql anyway — it is in this
// process's foreground process group — so a child built with
// exec.CommandContext would ALSO have exec.Cmd's default Cancel fire on the same
// keystroke and SIGKILL psql mid-session: no query cancellation, and no chance
// to restore the terminal's tty settings, which leaves the user's shell wrecked
// rather than back at a prompt.
//
// A nil Cancel is exactly what "no context is attached" looks like from the
// outside, so that is what this asserts. Re-adding cmd.Context() here is a
// one-line, entirely reasonable-looking change.
func TestPsqlCommandLeavesSIGINTToPsql(t *testing.T) {
	child := psqlCommand("/usr/bin/psql", &api.DatabaseProxy{
		Host: "pg.ronja.tech", Port: "5432",
		Username: "mdb-abc.read", Database: "t_x_crm",
	}, "test-token", nil)

	if child.Cancel != nil {
		t.Error("psql must own SIGINT: a Cancel func means the parent's context SIGKILLs it mid-session")
	}
	if child.WaitDelay != 0 {
		t.Errorf("WaitDelay = %v, want 0 — the same kill-on-cancel path by another name", child.WaitDelay)
	}
}

// TestPsqlCommandKeepsThePasswordOffTheCommandLine is the bug this whole shape
// exists to prevent: a token on argv is visible to every process on the box
// through `ps`, and to anything reading /proc.
//
// It travels on the CHILD's environment instead — not this process's, which is
// why it also never leaks into anything else `db connect` might later spawn.
func TestPsqlCommandKeepsThePasswordOffTheCommandLine(t *testing.T) {
	const token = "pat-super-secret"
	child := psqlCommand("/usr/bin/psql", &api.DatabaseProxy{
		Host: "pg.ronja.tech", Port: "5432",
		Username: "mdb-abc.read", Database: "t_x_crm",
	}, token, []string{"-c", "select 1"})

	for _, arg := range child.Args {
		if strings.Contains(arg, token) {
			t.Fatalf("the token reached argv (%q) — it is visible in `ps`", arg)
		}
	}
	found := 0
	for _, kv := range child.Env {
		if kv == "PGPASSWORD="+token {
			found++
		}
	}
	if found != 1 {
		t.Errorf("PGPASSWORD=<token> appears %d times on the child env, want exactly 1", found)
	}
	// The parent process must not be carrying it either: `db env` is the command
	// that exports, and this one exports nothing.
	if os.Getenv("PGPASSWORD") == token {
		t.Error("the token was set on THIS process's environment")
	}
}

// TestPsqlCommandForwardsArgsVerbatim: everything after -- is psql's, unchanged
// and in order, after the conninfo this command builds. A flag that looked like
// one of ours must not be intercepted, reordered or dropped.
func TestPsqlCommandForwardsArgsVerbatim(t *testing.T) {
	args := []string{"-n", "public", "-f", "report.sql", "--env", "dev"}
	child := psqlCommand("/usr/bin/psql", &api.DatabaseProxy{
		Host: "pg.ronja.tech", Port: "5432",
		Username: "mdb-abc.read", Database: "t_x_crm",
	}, "test-token", args)

	want := append([]string{"/usr/bin/psql", proxyConninfo(&api.DatabaseProxy{
		Host: "pg.ronja.tech", Port: "5432",
		Username: "mdb-abc.read", Database: "t_x_crm",
	})}, args...)
	if strings.Join(child.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", child.Args, want)
	}
}

// TestDatabaseConnectWithoutPsql pins the message a caller gets when the tool
// this command drives is not installed.
//
// It must name psql AND offer the way out that needs no psql at all — `db env`
// works with any Postgres client — because "exec: psql: executable file not
// found in $PATH" is a diagnostic about our implementation, not an answer to
// what the caller was trying to do.
func TestDatabaseConnectWithoutPsql(t *testing.T) {
	srv, _ := proxyServer(t, proxyRolesBody)
	signInTo(t, srv.URL)
	// An empty directory as the whole PATH: LookPath reads it at call time.
	t.Setenv("PATH", t.TempDir())

	_, err := runCLI(t, t.TempDir(), "db", "connect", "mdb-abc")
	if err == nil {
		t.Fatal("db connect with no psql on PATH must fail")
	}
	if !strings.Contains(err.Error(), "psql is not on your PATH") {
		t.Errorf("refusal must name psql, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ronja db env mdb-abc") {
		t.Errorf("refusal must offer the client-agnostic way out, got: %v", err)
	}
}

// TestSplitAtDash: everything before -- is ours, everything after is psql's. The
// boundary comes from cobra rather than from scanning the slice, so a database
// id that happens to look like a flag is never mistaken for one.
func TestSplitAtDash(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		atDash  int
		own     []string
		wantLen int
	}{
		{"no dash at all", []string{"mdb-abc"}, -1, []string{"mdb-abc"}, 0},
		{"dash with args", []string{"mdb-abc", "-c", "select 1"}, 1, []string{"mdb-abc"}, 2},
		{"dash with nothing after", []string{"mdb-abc"}, 1, []string{"mdb-abc"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			own, rest := splitAtDash(tc.args, tc.atDash)
			if strings.Join(own, ",") != strings.Join(tc.own, ",") {
				t.Errorf("own = %v, want %v", own, tc.own)
			}
			if len(rest) != tc.wantLen {
				t.Errorf("rest = %v, want %d entries", rest, tc.wantLen)
			}
		})
	}
}
