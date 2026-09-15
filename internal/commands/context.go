package commands

import (
	"fmt"
	"os"
	"strings"
	"sync"

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
// "Everything needed" is four things, and only the first two are local:
//
//	1. Which instance, and who you are on it (the CLI knows; the API does not
//	   advertise it).
//	2. How to authenticate (the header shape, and where the token lives).
//	3. The organization's policy — the standing rules its admins wrote for
//	   how work here is done. Ronja's own agent is given it on every turn; a
//	   coding agent working from outside was never told it existed, so what
//	   it built ignored the organization's own rules. Inlined, not pointed
//	   at, and BEFORE the credential recipe: identity, then the rules of the
//	   place you are acting in, then how to call it.
//	4. What the API can do — which is /llms.txt, already curated and
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
reports which instance you are signed in to and as whom, the organization's
policy (the standing rules its admins wrote for how work here is done), how
to authenticate, and then inlines the instance's own API index (/llms.txt) —
so from here on you can work over plain HTTP and never touch this CLI again.

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
			// The policy read is independent of Me, not gated on it:
			// /api/v2/authentication is admin-scoped, so a role-bound scoped
			// API token (say analytics:read in CI) always fails Me — and it
			// is exactly the caller the policy was invisible to. Both go out
			// whenever there is a token; each reports its own verdict.
			//
			// Side by side, not in sequence: each is bounded by the client's
			// own timeout, and a degraded instance that lets both run out
			// would otherwise cost the sum. Each goroutine writes its own
			// pair and nothing else, and the WaitGroup is the happens-before
			// for the reads below.
			var policy *api.Policy
			var policyErr error
			if resolved.Token != "" {
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					me, authErr = client.Me(cmd.Context())
				}()
				go func() {
					defer wg.Done()
					policy, policyErr = client.OrgPolicy(cmd.Context())
				}()
				wg.Wait()
			}
			// A Ctrl-C during the reads is not a verdict on the credential.
			// Both errors above would read as a rejection ("the stored token
			// was rejected", "could not read … context canceled") and the
			// command would exit zero on output that says nothing true; the
			// caller asked to stop, so stop, non-zero.
			if err := cmd.Context().Err(); err != nil {
				return err
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
				payload := contextPayload(resolved, me, authErr, policy, policyErr, llms)
				if here != nil {
					payload["localWorkflow"] = here.payload()
				}
				return emitJSON(payload)
			}
			printContext(resolved, me, authErr, policy, policyErr, llms, !noDocs)
			printLocalWork(here)
			return nil
		},
	}

	// The policy is NOT under this flag: it is context about the organization,
	// not the API docs, and the flag's text has to say so or it lies.
	cmd.Flags().BoolVar(&noDocs, "no-docs", false,
		"skip the inlined API index; print the connection details and the organization's policy only")
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

// identityStatus is the --json verdict on the credential, from a closed set,
// and the fork the human `Signed in:` line takes — one reading, so the two
// outputs cannot disagree about what a refused /me means:
//
//	"ok"       — /me answered; userID, tenantID and role are carried
//	"scoped"   — /me was refused with the server's SCOPE verdict (scopeDenied):
//	             a role-bound scoped token, which cannot call the admin-scoped
//	             authentication group at all and is still a working credential
//	             for the routes it is scoped for
//	"rejected" — /me failed any other way: a 401, a dead credential's 403, a
//	             network failure — run `ronja login`
//	"none"     — there was no token to ask with
//
// It exists because `authenticated` alone lied by omission: a scoped token
// reported `authenticated: false` beside `organizationPolicyStatus: "ok"`,
// and a consumer keyed on the boolean would sign in again to replace a token
// that had just read the policy. `authenticated` keeps its type and its
// meaning (/me answered) so nothing keyed on it changes; this is the finer
// answer beside it.
func identityStatus(resolved *config.Resolved, me *api.Me, authErr error) string {
	switch {
	case resolved.Token == "":
		return "none"
	case me != nil:
		return "ok"
	case scopeDenied(authErr):
		return "scoped"
	}
	return "rejected"
}

func contextPayload(resolved *config.Resolved, me *api.Me, authErr error, policy *api.Policy, policyErr error, llms string) map[string]any {
	docs := map[string]string{}
	for _, d := range docPaths {
		docs[strings.TrimPrefix(d.path, "/")] = resolved.URL + d.path
	}
	payload := map[string]any{
		"baseURL":        resolved.URL,
		"apiBase":        resolved.URL + "/api/v2",
		"authHeader":     "Authorization: Bearer <token>",
		"tokenEnvVar":    config.EnvToken,
		"tokenFromEnv":   resolved.FromEnv,
		"authenticated":  me != nil,
		"identityStatus": identityStatus(resolved, me, authErr),
		"docs":           docs,
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
	// The documented contract is `content` + `version`, carried whenever the
	// server served the document — a document with no rules INCLUDED, at its
	// served `version`, because that version is the CAS token an admin's agent
	// needs for the first write and there is nowhere else to get it without a
	// second read. Beside it, `organizationPolicyStatus` is the verdict a
	// consumer keys on: "no rules" (build freely) is `none`, never an empty
	// `content` it has to trim for itself, and it is distinct from "the read
	// was refused" (ask the admin for the rules before building), which
	// absence alone would conflate with it. The status is a closed set, never
	// the error text; the human output is still the only place the reason is
	// spelled out. Absent, like the document, when no token was in play —
	// there was no organization to ask. And absent when Me says there is no
	// organization: the status is `none` there too, but there is no document
	// and no version to write against.
	if status := policyStatus(me, policy, policyErr); status != "" {
		payload["organizationPolicyStatus"] = status
	}
	if policy != nil && !noOrganization(me) {
		// Verbatim, as GET /api/v2/policy/org serves it.
		doc := map[string]any{"content": policy.Content, "version": policy.Version}
		if policy.UpdatedAt != "" {
			doc["updatedAt"] = policy.UpdatedAt
		}
		payload["organizationPolicy"] = doc
	}
	if llms != "" {
		payload["llmsText"] = llms
	}
	return payload
}

// docsWanted distinguishes "the index was skipped" from "the index could not be
// fetched" — reporting a failure that never happened would send someone
// debugging their network for no reason.
func printContext(resolved *config.Resolved, me *api.Me, authErr error, policy *api.Policy, policyErr error, llms string, docsWanted bool) {
	out := os.Stdout

	fmt.Fprintf(out, "# Ronja API access\n\n")
	fmt.Fprintf(out, "Base URL:  %s\n", resolved.URL)
	fmt.Fprintf(out, "API base:  %s/api/v2\n", resolved.URL+"")
	if resolved.Profile != "" {
		fmt.Fprintf(out, "Profile:   %s\n", resolved.Profile)
	}

	// The same closed-set verdict --json carries as identityStatus, so the
	// prose and the key cannot fork on what a refused /me means.
	switch identityStatus(resolved, me, authErr) {
	case "ok":
		fmt.Fprintf(out, "Signed in: %s", describeUser(me))
		if me.Tenant != nil {
			fmt.Fprintf(out, " — organization %q", me.Tenant.Name)
		}
		if me.Role != nil {
			fmt.Fprintf(out, ", role %s", me.Role.Name)
		}
		fmt.Fprintln(out)
	case "scoped":
		// /api/v2/authentication is admin-scoped, so a role-bound scoped API
		// token (analytics:read in CI, say) is refused THERE and nowhere else
		// it is scoped for. Telling it to run `ronja login` would send an
		// agent off to replace a credential that works — and the policy
		// section right below may well have just been read with it.
		//
		// Keyed on the server's SCOPE verdict, not on the status: platform/auth
		// answers 403 for a dead credential too (a removed user, a deleted
		// organization, a PAT with no bound user), and "still works" is the
		// wrong thing to tell every one of those.
		fmt.Fprintf(out, "Signed in: as a scoped token — it cannot call /api/v2/authentication/me, so who you\n")
		fmt.Fprintf(out, "are is not shown; the calls it is scoped for still work.\n")
	case "rejected":
		fmt.Fprintf(out, "Signed in: NO — the stored token was rejected. Run `ronja login`.\n")
	default:
		fmt.Fprintf(out, "Signed in: NO — run `ronja login` first.\n")
	}

	// Identity, then the rules of the place you are acting in, then how to
	// call it: the policy sits between the two so it is read before the first
	// request is composed, not found after the index.
	printOrganizationPolicy(out, me, authErr, policy, policyErr)

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
