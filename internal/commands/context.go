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

// `ronja context` is the handoff from CLI to HTTP: identity, the organization's
// policy, how to authenticate, then /llms.txt inlined rather than restated so
// it cannot drift from the API.

// docPaths are the unauthenticated agent-readable surfaces at the instance root.
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
policy (the standing rules its admins wrote for how work here is done) and
your own, how to authenticate, and then inlines the instance's own API index
(/llms.txt) — so from here on you can work over plain HTTP and never touch
this CLI again.

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

			// Identity is best-effort: the docs are useful even signed out.
			var me *api.Me
			var authErr error
			// The policy read is not gated on Me: a scoped token always fails
			// the admin-scoped /api/v2/authentication yet may read the policy.
			// Run in parallel so a degraded instance costs one timeout, not
			// two; each goroutine writes only its own pair.
			var policy *api.Policy
			var policyErr error
			var applied *api.AppliedPolicy
			var appliedErr error
			if resolved.Token != "" {
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					me, authErr = client.Me(cmd.Context())
				}()
				go func() {
					defer wg.Done()
					policy, policyErr, applied, appliedErr = readPolicies(cmd.Context(), client)
				}()
				wg.Wait()
			}
			// A Ctrl-C is not a verdict on the credential: exit non-zero
			// rather than report a rejection.
			if err := cmd.Context().Err(); err != nil {
				return err
			}

			var llms string
			if !noDocs {
				llms, err = client.GetText(cmd.Context(), "/llms.txt")
				if err != nil {
					llms = ""
				}
			}

			here := localWork()
			if flagJSON {
				payload := contextPayload(resolved, me, authErr, policy, policyErr, applied, appliedErr, llms)
				if here != nil {
					payload["localWorkflow"] = here.payload()
				}
				return emitJSON(payload)
			}
			printContext(resolved, me, authErr, policy, policyErr, applied, appliedErr, llms, !noDocs)
			printLocalWork(here)
			return nil
		},
	}

	cmd.Flags().BoolVar(&noDocs, "no-docs", false,
		"skip the inlined API index; print the connection details and the organization's policy only")
	return cmd
}

// storedExpiry is the recorded expiry of the credential in play, empty for a
// $RONJA_TOKEN token: the expiry on disk describes a different credential.
func storedExpiry(resolved *config.Resolved) string {
	if resolved.FromEnv || resolved.Entry == nil {
		return ""
	}
	return resolved.Entry.TokenExpiry
}

// identityStatus is the --json verdict on the credential and the fork the
// `Signed in:` line takes, so the two cannot disagree:
//
//	"ok"       — /me answered
//	"scoped"   — /me refused with the server's scope verdict (scopeDenied): a
//	             scoped token, still working for the routes it is scoped for
//	"rejected" — /me failed any other way: run `ronja login`
//	"none"     — there was no token
//
// `authenticated` alone would tell a consumer to replace a working scoped
// token.
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

func contextPayload(resolved *config.Resolved, me *api.Me, authErr error, policy *api.Policy, policyErr error, applied *api.AppliedPolicy, appliedErr error, llms string) map[string]any {
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
	// ⚠️ The credential is located, never quoted, so this payload is safe to
	// log. Omitted for a $RONJA_TOKEN token: the stored file holds a different
	// credential.
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
	// The document is carried whenever served, an empty one included.
	// organizationPolicyStatus is the closed-set verdict a consumer keys on,
	// never error text. Its version is carried only beside the whole document,
	// where it is the CAS token for the next write. Beside the general part it
	// is dropped (isGeneralPart instead): content, verbatim, is that part's CAS
	// token on PUT /policy/org/part, and a whole-document PUT of it at the
	// version would drop every section.
	if status := policyStatus(me, policy, policyErr); status != "" {
		payload["organizationPolicyStatus"] = status
	}
	if policy != nil && !noOrganization(me) {
		doc := map[string]any{"content": policy.Content}
		if policy.GeneralPart {
			doc["isGeneralPart"] = true
		} else {
			doc["version"] = policy.Version
		}
		if policy.UpdatedAt != "" {
			doc["updatedAt"] = policy.UpdatedAt
		}
		payload["organizationPolicy"] = doc
	}
	if status := propertyPoliciesStatus(me, applied, appliedErr); status != "" {
		payload["propertyPoliciesStatus"] = status
	}
	if applied != nil && !noOrganization(me) {
		list := make([]map[string]any, 0, len(applied.Sections))
		for _, sec := range applied.Sections {
			list = append(list, map[string]any{
				"propertyKey":   sec.PropertyKey,
				"valueKey":      sec.ValueKey,
				"propertyLabel": sec.PropertyLabel,
				"valueLabel":    sec.ValueLabel,
				"content":       sec.Content,
				"because":       sec.Because,
			})
		}
		payload["propertyPolicies"] = list
	}
	// No status key of its own: it rides on the property-policy read, whose
	// failures propertyPoliciesStatus already names, so absent means none.
	if own := ownPolicy(me, applied); own != nil {
		payload["ownPolicy"] = map[string]any{"content": own.Content, "version": own.Version}
	}
	if llms != "" {
		payload["llmsText"] = llms
	}
	return payload
}

// docsWanted distinguishes "the index was skipped" from "the index could not be
// fetched".
func printContext(resolved *config.Resolved, me *api.Me, authErr error, policy *api.Policy, policyErr error, applied *api.AppliedPolicy, appliedErr error, llms string, docsWanted bool) {
	out := os.Stdout

	fmt.Fprintf(out, "# Ronja API access\n\n")
	fmt.Fprintf(out, "Base URL:  %s\n", resolved.URL)
	fmt.Fprintf(out, "API base:  %s/api/v2\n", resolved.URL+"")
	if resolved.Profile != "" {
		fmt.Fprintf(out, "Profile:   %s\n", resolved.Profile)
	}

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
		fmt.Fprintf(out, "Signed in: as a scoped token — it cannot call /api/v2/authentication/me, so who you\n")
		fmt.Fprintf(out, "are is not shown; the calls it is scoped for still work.\n")
	case "rejected":
		fmt.Fprintf(out, "Signed in: NO — the stored token was rejected. Run `ronja login`.\n")
	default:
		fmt.Fprintf(out, "Signed in: NO — run `ronja login` first.\n")
	}

	// Before the credential recipe, so the rules are read before the first
	// request is composed.
	printOrganizationPolicy(out, me, authErr, policy, policyErr)
	printPropertyPolicies(out, me, authErr, applied, appliedErr)
	printOwnPolicy(out, me, applied)

	// Named apart from the "## Authentication" section in the inlined llms.txt.
	fmt.Fprintf(out, "\n## Use your credentials\n\n")
	fmt.Fprintf(out, "Send a bearer token on every /api/v2 request.\n\n")
	fmt.Fprintf(out, "The simplest way is to let the CLI attach it for you:\n\n")
	fmt.Fprintf(out, "    ronja api /api/v2/authentication/me\n")
	fmt.Fprintf(out, "    ronja query \"SELECT region, COUNT(*) FROM {{ ref('table-...') }} GROUP BY 1\"\n\n")
	fmt.Fprintf(out, "For any other HTTP client:\n\n")

	if resolved.FromEnv {
		// The stored file is not the credential in play, so it is not named.
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

	fmt.Fprintf(out, "\nThe token acts as you, with your current permissions — it can do no more\n")
	fmt.Fprintf(out, "than you can, and a route your role can't reach returns 403.\n")
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
