package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
)

// `ronja context` is the handoff from CLI to HTTP.
//
// The CLI is deliberately NOT a wrapper around the whole API — wrapping it
// would mean every new endpoint needs a new subcommand, and an agent would be
// limited to whatever we had got around to wrapping. Instead the CLI does the
// one thing an agent genuinely cannot do for itself (an interactive browser
// sign-in), and then hands over everything needed to call the API directly.
//
// "Everything needed" is three things, and only the first two are local:
//
//	1. Which instance, and who you are on it (the CLI knows; the API does not
//	   advertise it).
//	2. How to authenticate (the header shape, and where the token lives).
//	3. What the API can do — which is /llms.txt, already curated and
//	   server-authoritative. We fetch and inline it rather than restating it,
//	   so this command cannot drift from the API it describes.

// docPaths are the agent-readable surfaces served at the instance root. They
// are unauthenticated, so an agent can fetch them with any HTTP client.
var docPaths = []struct{ path, what string }{
	{"/llms.txt", "index: guides, capabilities, and where everything else is"},
	{"/docs/api/endpoints.md", "every endpoint with the token scope it requires"},
	{"/docs/api/skills.md", "Ronja's own agent expertise — read before building a resource"},
	{"/docs/api/openapi.json", "request/response schemas"},
}

func newContextCmd() *cobra.Command {
	var noDocs bool

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Print everything needed to call the Ronja API directly",
		Long: `Print everything needed to call the Ronja API directly.

Intended to be the second thing an agent runs, after 'ronja login'. It
reports which instance you are signed in to and as whom, how to authenticate,
and then inlines the instance's own API index (/llms.txt) — so from here on you
can work over plain HTTP and never touch this CLI again.

The token is never printed. This shows how to LOAD it instead —
'eval "$(ronja env)"' puts it in $RONJA_TOKEN without displaying it — and
reports the credential file for callers that are not a POSIX shell.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			// Identity is best-effort: the docs half of this output is useful
			// even when signed out, and an agent that has just been handed a
			// URL should still learn where to read.
			var me *api.Me
			var authErr error
			if resolved.Token != "" {
				me, authErr = client.Me(cmd.Context())
			}

			var llms string
			if !noDocs {
				// Unauthenticated, so this works even when signed out.
				llms, err = client.GetText(cmd.Context(), "/llms.txt")
				if err != nil {
					// Not fatal: the local facts are still worth printing, and
					// the URLs below tell the caller where to look.
					llms = ""
				}
			}

			here := localWork()
			if flagJSON {
				payload := contextPayload(resolved, me, llms)
				if here != nil {
					payload["localWorkflow"] = here.payload()
				}
				return emitJSON(payload)
			}
			printContext(resolved, me, authErr, llms, !noDocs)
			printLocalWork(here)
			return nil
		},
	}

	cmd.Flags().BoolVar(&noDocs, "no-docs", false,
		"skip the inlined API index; print only the connection details")
	return cmd
}

// storedExpiry is the recorded lifetime of the credential actually in play, or
// "" when there isn't one. A token from $RONJA_TOKEN was never minted through
// this CLI, so any expiry on disk describes a different credential entirely —
// reporting it would be worse than saying nothing.
func storedExpiry(resolved *config.Resolved) string {
	if resolved.FromEnv || resolved.Entry == nil {
		return ""
	}
	return resolved.Entry.TokenExpiry
}

func contextPayload(resolved *config.Resolved, me *api.Me, llms string) map[string]any {
	docs := map[string]string{}
	for _, d := range docPaths {
		docs[strings.TrimPrefix(d.path, "/")] = resolved.URL + d.path
	}
	payload := map[string]any{
		"baseURL":       resolved.URL,
		"apiBase":       resolved.URL + "/api/v2",
		"authHeader":    "Authorization: Bearer <token>",
		"tokenEnvVar":   config.EnvToken,
		"tokenFromEnv":  resolved.FromEnv,
		"authenticated": me != nil,
		"docs":          docs,
	}
	// The credential is located, never quoted. A consumer reads the file at
	// tokenFile and takes the value at tokenJSONPath; nothing here carries the
	// secret, so this payload is safe to log.
	//
	// Omitted entirely when the token came from $RONJA_TOKEN — same branch the
	// human-readable output takes. The stored file then describes a DIFFERENT
	// credential from the one in play, and may not exist at all, so naming it
	// would point a non-shell consumer at the wrong token.
	if !resolved.FromEnv && resolved.Profile != "" {
		if path, err := config.Path(); err == nil {
			payload["tokenFile"] = path
			payload["tokenJSONPath"] = fmt.Sprintf("profiles[%q].token", resolved.Profile)
		}
	}
	if resolved.Profile != "" {
		payload["profile"] = resolved.Profile
	}
	if exp := storedExpiry(resolved); exp != "" {
		payload["tokenExpiresAt"] = exp
	}
	if me != nil {
		payload["userID"] = me.User.ID
		payload["userEmail"] = me.User.Email
		if me.Tenant != nil {
			payload["tenantID"] = me.Tenant.ID
			payload["tenantName"] = me.Tenant.Name
		}
		if me.Role != nil {
			payload["role"] = me.Role.Name
		}
	}
	if llms != "" {
		payload["llmsText"] = llms
	}
	return payload
}

// docsWanted distinguishes "the index was skipped" from "the index could not be
// fetched" — reporting a failure that never happened would send someone
// debugging their network for no reason.
func printContext(resolved *config.Resolved, me *api.Me, authErr error, llms string, docsWanted bool) {
	out := os.Stdout

	fmt.Fprintf(out, "# Ronja API access\n\n")
	fmt.Fprintf(out, "Base URL:  %s\n", resolved.URL)
	fmt.Fprintf(out, "API base:  %s/api/v2\n", resolved.URL+"")
	if resolved.Profile != "" {
		fmt.Fprintf(out, "Profile:   %s\n", resolved.Profile)
	}

	switch {
	case me != nil:
		fmt.Fprintf(out, "Signed in: %s", describeUser(me))
		if me.Tenant != nil {
			fmt.Fprintf(out, " — organization %q", me.Tenant.Name)
		}
		if me.Role != nil {
			fmt.Fprintf(out, ", role %s", me.Role.Name)
		}
		fmt.Fprintln(out)
	case authErr != nil:
		fmt.Fprintf(out, "Signed in: NO — the stored token was rejected. Run `ronja login`.\n")
	default:
		fmt.Fprintf(out, "Signed in: NO — run `ronja login` first.\n")
	}

	// Named distinctly from the "## Authentication" section inside the inlined
	// llms.txt below: that one explains where a credential comes from in
	// general, this one is the concrete, runnable form for THIS instance.
	fmt.Fprintf(out, "\n## Use your credentials\n\n")
	fmt.Fprintf(out, "Send a bearer token on every /api/v2 request.\n\n")
	// The two built-in verbs attach the credential themselves — nothing to
	// load, interpolate or echo — so they lead, and the curl form below is
	// the fallback for every other HTTP client.
	fmt.Fprintf(out, "The simplest way is to let the CLI attach it for you:\n\n")
	fmt.Fprintf(out, "    ronja api /api/v2/authentication/me\n")
	fmt.Fprintf(out, "    ronja query \"SELECT region, COUNT(*) FROM {{ ref('table-...') }} GROUP BY 1\"\n\n")
	fmt.Fprintf(out, "For any other HTTP client:\n\n")

	if resolved.FromEnv {
		// The environment already holds the credential, so there is nothing to
		// load — and pointing at the stored file here would send a reader to a
		// value that is not the one in play.
		fmt.Fprintf(out, "$%s and $%s are already set in this environment:\n\n",
			config.EnvURL, config.EnvToken)
		fmt.Fprintf(out, "    curl -H \"Authorization: Bearer $%s\" \"$%s/api/v2/authentication/me\"\n",
			config.EnvToken, config.EnvURL)
	} else {
		fmt.Fprintf(out, "Load the stored credential into your environment:\n\n")
		fmt.Fprintf(out, "    eval \"$(ronja env)\"\n\n")
		fmt.Fprintf(out, "That sets $%s and $%s. `eval` consumes the output, so the token is\n",
			config.EnvURL, config.EnvToken)
		fmt.Fprintf(out, "never displayed — unlike running `ronja env` on its own.\n\n")
		// The single most useful line in this output for an agent: shell state
		// does not survive between tool calls, so the load and the work have to
		// be one command or the variable is empty when it is read.
		fmt.Fprintf(out, "Shell state does not persist between separate commands, so keep the load\n")
		fmt.Fprintf(out, "and the request together:\n\n")
		fmt.Fprintf(out, "    eval \"$(ronja env)\" && \\\n")
		fmt.Fprintf(out, "      curl -H \"Authorization: Bearer $%s\" \"$%s/api/v2/authentication/me\"\n",
			config.EnvToken, config.EnvURL)
		if path, err := config.Path(); err == nil && resolved.Profile != "" {
			fmt.Fprintf(out, "\nThe credential itself lives at %s\n", path)
			fmt.Fprintf(out, "under `profiles[%q].token` (owner-only) — read it there directly if you\n", resolved.Profile)
			fmt.Fprintf(out, "are not driving a POSIX shell.\n")
		}
		fmt.Fprintf(out, "\nDo not echo the token or paste it into a message.\n")
	}

	// The token acts as a person, so say so where someone deciding what to do
	// with it will actually read it.
	fmt.Fprintf(out, "\nThe token acts as you, with your current permissions — it can do no more\n")
	fmt.Fprintf(out, "than you can, and a route your role can't reach returns 403.\n")
	// An agent reading this needs to know the credential has a shelf life, or
	// it will one day report a mysterious 401 rather than "log in again".
	if when := describeExpiry(storedExpiry(resolved)); when != "" {
		fmt.Fprintf(out, "It expires %s — re-run `ronja login` to renew.\n", when)
	}

	fmt.Fprintf(out, "\n## Reference\n\n")
	for _, d := range docPaths {
		fmt.Fprintf(out, "    %-44s %s\n", resolved.URL+d.path, d.what)
	}
	fmt.Fprintf(out, "\nThese are unauthenticated — fetch them with any HTTP client.\n")

	switch {
	case llms != "":
		fmt.Fprintf(out, "\n---\n\n")
		fmt.Fprint(out, strings.TrimSpace(llms))
		fmt.Fprintln(out)
	case docsWanted:
		fmt.Fprintf(out, "\n(Could not fetch %s/llms.txt — start there once the instance is reachable.)\n", resolved.URL)
	}
}
