package commands

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
)

// `ronja token create` never touches the credential: POST /authentication/token
// refuses every machine credential (a login token is full access, so "no wider
// than itself" would bound nothing). It opens the pre-filled form from GET
// /authentication/token-link and waits for a matching new token in the caller's
// list, which never carries the secret (api.MyToken has no field for it).

// Package vars so tests can collapse them.
var (
	tokenPollInterval = 3 * time.Second
	tokenWaitLimit    = 10 * time.Minute
)

// tokenScopeOrder is the Account page's print order; keep it in step with the
// backend's gt.AllScopes, though a scope missing here still prints and is accepted.
var tokenScopeOrder = []string{"data", "structure", "analytics", "automation", "agents", "secrets", "admin"}

var (
	tokenExpiresDaysRe = regexp.MustCompile(`^[0-9]{1,4}d$`)
	tokenExpiresDateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

const tokenScopedLogin = "this login can't manage tokens (its token lacks admin scope). Create the token in Ronja under Account → Access tokens."

func newTokenCmd() *cobra.Command {
	token := &cobra.Command{
		Use:   "token",
		Short: "Create a Ronja API token for an app, in your browser",
		Long: `Create a Ronja API token for an app, in your browser.

  ronja token create   open a pre-filled token form in Ronja and wait for it

The token is created by you, in Ronja, and shown there once. It is never sent
to this terminal. Listing and revoking tokens stay in Ronja (Account → Access
tokens) and on plain HTTP:

  ronja api /api/v2/authentication/token-mine
  ronja api -X DELETE /api/v2/authentication/token-mine/<id>`,
	}
	token.AddCommand(newTokenCreateCmd())
	return token
}

func newTokenCreateCmd() *cobra.Command {
	var (
		name      string
		scopes    []string
		expires   string
		noBrowser bool
		noWait    bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Open a pre-filled token form in Ronja and report the token you create",
		Long: `Open a pre-filled token form in Ronja and report the token you create.

  ronja token create --name "Sales dashboard" --scope data:read
  ronja token create --name nightly-sync --scope data:write --scope automation:read --expires 90d

Opens Account → Access tokens with the name, scopes and expiry filled in. Review
the form and click Create; Ronja shows the token there once — copy it into your
app. This command then prints the new token's id, name, scopes and expiry. It
never prints the token itself, and never receives it. --json prints one object
with status and a tokens array containing only this metadata.

--scope is <scope>:<read|write> and is repeatable; at least one is required.
Scopes: data, structure, analytics, automation, agents, secrets, admin. A token
with full access is a choice you make on the form, not here.

--expires is a number of days (90d, at most 3650d) or a date (YYYY-MM-DD). A
number of days counts from when you open the form, in your browser's timezone,
and a date is a date there. The default is never: an app built on the token
does not stop working on a date nobody chose.

--no-browser prints the link instead of opening it. --no-wait returns as soon
as the link is open, without waiting for the token (--json: status pending and
an empty tokens array).

The token reported is a new one in your token list whose name is --name, or
whose scopes are exactly the ones asked for (so renaming it on the form is
fine). Any other token that appears meanwhile — a sign-in from another
terminal, say — is named on stderr and not reported as this one.

Only an Admin can create a token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("--name is required: the token's name in the token list")
			}
			chosen := make([]string, 0, len(scopes))
			for _, s := range scopes {
				if s = strings.TrimSpace(s); s != "" {
					chosen = append(chosen, s)
				}
			}
			if len(chosen) == 0 {
				return errors.New("--scope is required, as <scope>:<read|write> (scopes: " + strings.Join(tokenScopeOrder, ", ") + "); a full-access token is created on the form in Ronja")
			}
			exp, err := parseTokenExpires(expires)
			if err != nil {
				return err
			}

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := newClient(resolved.URL, resolved.Token)
			return runTokenCreate(cmd.Context(), client, resolved, name, chosen, exp, noBrowser, noWait)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "the token's name, shown in the token list (required)")
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "a scope the token may use, as <scope>:<read|write> (repeatable; at least one)")
	cmd.Flags().StringVar(&expires, "expires", "never", "when the token stops working: <N>d (days from when the form is opened), a date as YYYY-MM-DD, or never")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening it")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the link is open, without waiting for the token")
	return cmd
}

// parseTokenExpires checks only the shape: range and past dates are the
// server's call, future dates the form's against the browser's today, so the
// terminal's clock decides nothing.
func parseTokenExpires(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch {
	case v == "" || strings.EqualFold(v, "never"):
		return "", nil
	case tokenExpiresDaysRe.MatchString(v), tokenExpiresDateRe.MatchString(v):
		return v, nil
	}
	return "", fmt.Errorf("--expires takes a number of days (90d), a date as YYYY-MM-DD, or never; got %q", raw)
}

func runTokenCreate(ctx context.Context, client *api.Client, resolved *config.Resolved, name string, scopes []string, expires string, noBrowser, noWait bool) error {
	out := os.Stderr

	link, err := client.GetTokenLink(ctx, name, scopes, expires)
	if err != nil {
		return tokenLinkError(err)
	}
	// The form creates the token in whichever organization the browser ends up
	// in, so name it here for comparison with the page's banner.
	if org := tokenOrgLabel(resolved, link.URL); org != "" {
		fmt.Fprintf(out, "Creating a token for organization %s.\n", org)
	}

	// Snapshot before the link opens: new is decided by id, so no two clocks
	// need agree.
	var seen map[string]bool
	if !noWait {
		before, err := client.ListMyTokens(ctx)
		if err != nil {
			return tokenListError(err)
		}
		seen = make(map[string]bool, len(before))
		for _, t := range before {
			seen[t.ID] = true
		}
	}

	spawned := false
	if !noBrowser {
		spawned = openBrowser(link.URL) == nil
	}
	if spawned {
		fmt.Fprintln(out, "Opening the token form in your browser.")
		fmt.Fprintf(out, "If nothing opened, open this link instead:\n  %s\n\n", link.URL)
	} else {
		fmt.Fprintf(out, "Open this link to create the token:\n  %s\n\n", link.URL)
	}
	fmt.Fprintln(out, "Review the token in your browser and click Create. Ronja shows the token there once; copy it into your app. It is never sent to this terminal.")
	if noWait {
		if flagJSON {
			return emitJSON(tokenCreateResult{Status: "pending", Tokens: []api.MyToken{}})
		}
		return nil
	}
	fmt.Fprintln(out, "Waiting for the token... (Ctrl-C to stop waiting)")

	created, err := waitForNewToken(ctx, client, seen, tokenRequest{name: name, grants: tokenRequestedGrants(scopes)})
	if err != nil {
		return err
	}
	if flagJSON {
		return emitJSON(tokenCreateResult{Status: "created", Tokens: created})
	}
	for _, t := range created {
		fmt.Fprintf(os.Stdout, "created: %s  %s  %s  expires %s\n", t.ID, t.Name, tokenScopeList(t.ScopeGrants), tokenExpiry(t.ExpiresAt))
	}
	return nil
}

// tokenCreateResult contains observed metadata only; MyToken never decodes a credential.
type tokenCreateResult struct {
	Status string        `json:"status"`
	Tokens []api.MyToken `json:"tokens"`
}

// tokenOrgLabel falls back to the link's ?org= id, the organization that
// answered: the one source that is right under an environment token too.
func tokenOrgLabel(resolved *config.Resolved, link string) string {
	orgID := ""
	if u, err := url.Parse(link); err == nil {
		orgID = u.Query().Get("org")
	}
	if resolved != nil && !resolved.FromEnv && resolved.Entry != nil && resolved.Entry.TenantName != "" &&
		(orgID == "" || resolved.Entry.TenantID == orgID) {
		return resolved.Entry.TenantName
	}
	return orgID
}

type tokenRequest struct {
	name   string
	grants map[string]string
}

// tokenRequestedGrants trusts its input: the server already refused anything
// malformed or contradictory.
func tokenRequestedGrants(scopes []string) map[string]string {
	grants := make(map[string]string, len(scopes))
	for _, s := range scopes {
		if key, lvl, ok := strings.Cut(strings.TrimSpace(s), ":"); ok {
			grants[key] = lvl
		}
	}
	return grants
}

// matches takes the name OR exactly the scopes, so the form may change one but
// not both. A sign-in token, the usual unbidden arrival, has no grant at all and
// never matches a request, which always names a scope.
func (r tokenRequest) matches(t api.MyToken) bool {
	return t.Name == r.name || maps.Equal(t.ScopeGrants, r.grants)
}

func tokenLinkError(err error) error {
	switch status := api.StatusOf(err); {
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return errors.New("this Ronja server is older than the CLI. Create the token in Ronja under Account → Access tokens.")
	case status == http.StatusForbidden && (api.WireCodeOf(err) == api.InsufficientScopeCode || scopeDenied(err)):
		return errors.New(tokenScopedLogin)
	case status == http.StatusForbidden:
		// A role short of Admin: the server's own sentence.
		if msg := api.CodeOf(err); msg != "" {
			return errors.New(msg)
		}
		return errors.New("Only an Admin can create a personal access token.")
	}
	return err
}

// A 403 from token-mine is only ever the login's scope: the route is open to
// every member.
func tokenListError(err error) error {
	switch api.StatusOf(err) {
	case http.StatusForbidden:
		return errors.New(tokenScopedLogin)
	case http.StatusNotFound:
		return errors.New("this Ronja server did not answer the token list (HTTP 404). Check Account → Access tokens in Ronja.")
	}
	return fmt.Errorf("read your tokens: %w", err)
}

// waitForNewToken names a non-matching new token on stderr and keeps waiting:
// something else created it, and reporting it would hand over the wrong id.
func waitForNewToken(ctx context.Context, client *api.Client, seen map[string]bool, req tokenRequest) ([]api.MyToken, error) {
	const cancelled = "stopped waiting. The form stays open in your browser; a token created there is listed under Account → Access tokens"
	deadline := time.Now().Add(tokenWaitLimit)
	interval := tokenPollInterval
	var failingSince time.Time
	var unmatched []api.MyToken

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New(cancelled)
		case <-time.After(interval):
		}

		tokens, err := client.ListMyTokens(ctx)
		switch {
		case err == nil:
			failingSince = time.Time{}
			interval = tokenPollInterval
			var byName, byScopes []api.MyToken
			for _, t := range tokens {
				if seen[t.ID] {
					continue
				}
				seen[t.ID] = true
				switch {
				case t.Name == req.name:
					byName = append(byName, t)
				case req.matches(t):
					byScopes = append(byScopes, t)
				default:
					unmatched = append(unmatched, t)
					fmt.Fprintf(os.Stderr, "A new token appeared that does not match: %s  %s (not reported as this one)\n", t.ID, t.Name)
				}
			}
			if created := append(byName, byScopes...); len(created) > 0 {
				return created, nil
			}
		case ctx.Err() != nil:
			return nil, errors.New(cancelled)
		case api.StatusOf(err) == http.StatusForbidden, api.StatusOf(err) == http.StatusNotFound:
			return nil, tokenListError(err)
		case api.IsTransient(err):
			t := time.Now()
			if failingSince.IsZero() {
				failingSince = t
			}
			if failing := t.Sub(failingSince); failing > client.MaxPollFailureWindow {
				return nil, fmt.Errorf("gave up after %s of consecutive failures: %w", failing.Round(time.Second), err)
			}
			if api.StatusOf(err) == http.StatusTooManyRequests {
				interval += client.RateLimitBackoff
			}
		default:
			return nil, fmt.Errorf("read your tokens: %w", err)
		}

		if time.Now().After(deadline) {
			msg := fmt.Sprintf("no new token named %q or with exactly the scopes asked for appeared within %s; if you created it, it is in Account → Access tokens", req.name, humanDuration(tokenWaitLimit))
			if len(unmatched) > 0 {
				names := make([]string, len(unmatched))
				for i, t := range unmatched {
					names[i] = t.ID + " (" + t.Name + ")"
				}
				msg += ". New tokens that did not match: " + strings.Join(names, ", ")
			}
			return nil, errors.New(msg)
		}
	}
}

// A NULL grant is unrestricted, hence "full access".
func tokenScopeList(grants map[string]string) string {
	if len(grants) == 0 {
		return "full access"
	}
	parts := make([]string, 0, len(grants))
	for _, s := range tokenScopeOrder {
		if lvl, ok := grants[s]; ok {
			parts = append(parts, s+":"+lvl)
		}
	}
	var rest []string
	for s, lvl := range grants {
		if !slices.Contains(tokenScopeOrder, s) {
			rest = append(rest, s+":"+lvl)
		}
	}
	slices.Sort(rest)
	return strings.Join(append(parts, rest...), ", ")
}

// tokenExpiry prints an instant with its offset, not a bare date: the form
// sends midnight in the browser's zone, which falls on the day before in
// another zone.
func tokenExpiry(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "never"
	}
	return at.Local().Format(time.RFC3339)
}
