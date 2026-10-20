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

// The PGPASSWORD assertion matters most: `db env` must hand over the profile's
// own token, never mint a second credential nobody knows to revoke.
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
		// Never `require`: the token travels as the password, so a mode that
		// accepts any certificate hands it to whoever answers the socket.
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

// The two refusals must stay different messages. "No proxy block" has two
// causes the server does not distinguish (no proxy, or not an admin), so it
// names both; "no read role" means the proxy is there and is a different fix.
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

	// The printed mint command must work as pasted: POST :id/user requires
	// featureID. The payload is decoded, not substring-matched, so invalid JSON
	// fails too; the backend guard stays the authority and this pins the shape.
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
		// A string assertion: fmt.Sprint(nil) is "<nil>", so {"featureID":null}
		// would pass a non-empty-rendering check while the server answers 400.
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

		// featureID has no default and is deliberately not borrowed from an
		// existing role (see proxyForDatabase), so the refusal must say where
		// to find one.
		if !strings.Contains(msg, "/api/v2/feature/query") {
			t.Errorf("refusal must say where to find a featureID, got:\n%s", msg)
		}
		// `--jq` takes the next argument, so `--jq -r '<expr>'` would bind `-r`
		// as the expression.
		jq, r := strings.Index(msg, "--jq '"), strings.LastIndex(msg, " -r")
		if jq < 0 || r < jq {
			t.Errorf("the discovery command must pass --jq '<expr>' BEFORE -r, got:\n%s", msg)
		}
	})
}

// mintPayload is anchored on `-d '` and the NEXT quote: the message holds a
// second single-quoted `--jq '...'` argument a wider span would swallow.
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

// `db sql` and `db migrate` take --env, so it will be typed here; the refusal
// says a dev copy is addressed at the proxy by its own id, since the username
// is the id and there is no prod→dev overlay to redirect.
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

// libpq has no environment variable for keepalives_idle, so only a built
// conninfo survives the load balancer's idle timer. The password must not be in
// it: it reaches psql through the child's environment, out of `ps`.
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

// psql already gets Ctrl-C's SIGINT from the terminal; a child built with
// exec.CommandContext on the root's signal context would also be SIGKILLed on
// it, leaving the tty wrecked. A nil Cancel is what "no context" looks like.
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

// A token on argv is visible to every process on the box through `ps`; it
// travels on the CHILD's environment, not this process's.
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
	if os.Getenv("PGPASSWORD") == token {
		t.Error("the token was set on THIS process's environment")
	}
}

// A flag after -- that looks like one of ours (--env) must reach psql unchanged.
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

// The refusal must name psql and offer `db env`, which works with any Postgres
// client.
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

// The boundary comes from cobra, not from scanning the slice, so an id that
// looks like a flag is never mistaken for one.
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
