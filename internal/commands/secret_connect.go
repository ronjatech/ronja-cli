package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// `ronja secret connect` is the second credential handshake on `secret`: it
// connects a Ronja-managed OAuth service (Gmail, Google Sheets, HubSpot, …) by
// sending a person through the provider's own consent screen.
//
// No new flow. The CLI calls the same initiate route the app does and waits on
// GET /secret/:id. What it opens is the secret's page in Ronja (initiate's
// `url`), never the provider directly: the page says what is being connected
// and who asked, and the provider's sign-in opens only when the person presses
// Continue there — a sign-in window jumping up out of a terminal reads like
// phishing. The page calls initiate again from the browser, so the state row
// minted for the CLI's call expires unused. Against a server that returns no
// `url` the command opens the authURL, as it always did. Either way the
// provider redirects to Ronja's registered callback — the CLI never sees a
// provider token, and the secret's value is never on any route it reads.
//
// What makes the wait honest is the scope set initiate now reports:
//
//   - "already connected" is claimed only when the secret is ready AND its
//     recorded grant COVERS what this run would ask for (no --scope), or
//     EQUALS it (--scope, which replaces the grant) — alreadyConnected;
//   - completion is a status change from pending/reauth_required, or a CHANGE
//     in grantedScopes from a ready secret — the column only the callback
//     writes — and never updatedAt, which a background token refresh bumps;
//   - after completion, a grant missing some of what was asked for (a granular
//     consent screen with a box unticked) exits 1 naming them.

// connectPollInterval and connectWaitLimit are package vars so the tests can
// collapse them, as the login tests collapse the device-flow cadence. The limit
// matches the state row's 10-minute TTL: past it the link is dead whatever the
// person does.
var (
	connectPollInterval = 3 * time.Second
	connectWaitLimit    = 10 * time.Minute
	// connectPageWaitLimit is the wait when the person starts from Ronja's
	// page: its link does not expire, and Continue mints a fresh 10-minute
	// state row, so the wait matches `secret create --in-browser`.
	connectPageWaitLimit = 15 * time.Minute
)

// offlineAccess is the OIDC pseudo-scope that asks for a refresh token. It must
// be requested (Microsoft and Salesforce need it in the authorize URL) and is
// never echoed back as granted — the same exception the server's
// oauthregistry.ScopesCover makes. Without it here, every Microsoft connect
// would read as "did not grant: offline_access" and never as already connected.
const offlineAccess = "offline_access"

func newSecretConnectCmd() *cobra.Command {
	var (
		featureID string
		scopes    []string
		noBrowser bool
	)

	cmd := &cobra.Command{
		Use:   "connect <service>",
		Short: "Connect a service like Gmail or HubSpot by signing in to it",
		Long: `Connect a service like Gmail or HubSpot by signing in to it.

Opens Ronja in your browser, on a page that says what is being connected.
Press Continue there to sign in to the service; once you approve, Ronja stores
the connection and this command reports it:

  ronja secret connect gmail
  ronja secret connect hubspot --feature <feature-id>

A connection is one per provider and person: connecting a second Google service
adds its permissions to the Google connection you already have. If that
connection already holds everything the service needs, nothing opens.

--scope picks the permissions, for a service that offers a choice (one it does
not offer is refused with the list it does); it REPLACES what the connection
holds, which is the only way to make a connection narrower.

--no-browser prints the link instead of opening it. The page lets only you
connect, signed in to Ronja.

Prints the secret's id, its name and the permissions it holds — never a token.
An unknown service is refused with the list of service names.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := strings.TrimSpace(args[0])
			if service == "" {
				return errors.New("name the service to connect, e.g. gmail")
			}
			featureID = strings.TrimSpace(featureID)
			var in api.OAuthInitiateInput
			in.FeatureID = featureID
			if cmd.Flags().Changed("scope") {
				chosen := make([]string, 0, len(scopes))
				for _, s := range scopes {
					if s = strings.TrimSpace(s); s != "" {
						chosen = append(chosen, s)
					}
				}
				in.Scopes = &chosen
			}

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := newClient(resolved.URL, resolved.Token)
			return runSecretConnect(cmd.Context(), client, service, in, noBrowser)
		},
	}

	cmd.Flags().StringVar(&featureID, "feature", "", "the Feature a NEW connection is created in (default: one per provider). An existing connection stays where it is")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "a permission to ask for, on a service that offers a choice (repeatable). Replaces what the connection holds")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening it")
	return cmd
}

func runSecretConnect(ctx context.Context, client *api.Client, service string, in api.OAuthInitiateInput, noBrowser bool) error {
	out := os.Stderr

	started, err := client.InitiateOAuth(ctx, service, in)
	if err != nil {
		return connectInitiateError(err)
	}

	before, err := client.GetSecretStatus(ctx, started.SecretID)
	if err != nil {
		if api.StatusOf(err) == http.StatusNotFound {
			return fmt.Errorf("secret %s was deleted before the sign-in started", started.SecretID)
		}
		return fmt.Errorf("read secret %s: %w", started.SecretID, err)
	}
	provider := providerLabel(before.Name)

	// The connection is one per provider and person, so an existing one keeps
	// its feature whatever --feature says. Saying so is the difference between
	// the person finding it and thinking the flag did nothing.
	if in.FeatureID != "" && before.FeatureID != nil && *before.FeatureID != "" && *before.FeatureID != in.FeatureID {
		fmt.Fprintf(out, "%s already lives in feature %s; using it.\n", before.Name, *before.FeatureID)
	}

	requested := started.RequestedScopes
	if requested != nil && before.Status == "ready" && alreadyConnected(grantedOf(before), *requested, in.Scopes != nil) {
		// The state row this initiate minted is never used; it expires on
		// its own in 10 minutes.
		return reportConnected(true, before, []string{})
	}

	onPage := started.URL != ""
	link := started.AuthURL
	if onPage {
		link = started.URL
	}
	spawned := false
	if !noBrowser {
		spawned = openBrowser(link) == nil
	}
	switch {
	case onPage && spawned:
		fmt.Fprintf(out, "Opening Ronja so you can connect %s there.\n", provider)
		fmt.Fprintf(out, "If nothing opened, open this link instead:\n  %s\n\n", link)
	case onPage:
		fmt.Fprintf(out, "Open this link to connect %s in Ronja (only you can connect, signed in):\n  %s\n\n", provider, link)
	case spawned:
		fmt.Fprintf(out, "Opening the %s sign-in in your browser.\n", provider)
		fmt.Fprintf(out, "If nothing opened, sign in here instead:\n  %s\n\n", link)
	default:
		fmt.Fprintf(out, "Open this URL to sign in to %s:\n  %s\n\n", provider, link)
	}
	fmt.Fprintf(out, "Waiting for the sign-in... (Ctrl-C to cancel)\n")

	after, err := waitForConnect(ctx, client, before, onPage)
	if err != nil {
		return err
	}

	if requested == nil {
		fmt.Fprintln(out, "this server can't confirm the scopes; check the secret in Ronja")
		return reportConnected(false, after, nil)
	}
	missing := missingScopes(grantedOf(after), *requested)
	if err := reportConnected(false, after, missing); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("connected, but %s did not grant: %s. %s calls needing them will fail; re-run and leave them ticked",
			provider, strings.Join(missing, ", "), service)
	}
	return nil
}

// alreadyConnected decides whether a ready secret needs no consent at all.
//
// Without --scope the grant ACCUMULATES, and coverage is enough: if the
// recorded grant already covers everything this run would ask for, a consent
// could only REMOVE scopes the service does not use — for a selectable service
// the request is the required set plus the stored scopes still in its catalog,
// so a stale stored scope is exactly one that would be dropped. Equality here
// would open the browser needlessly whenever the grant holds a full Google
// scope and the service asks for its ".readonly" variant, and, since such a
// consent changes nothing the completion rule can see, then wait out the whole
// link lifetime.
//
// With --scope the grant is REPLACED, so only equality (offline_access aside)
// means nothing would change: a broad grant covers a narrowing request, and
// treating that as done would silently skip the narrowing asked for.
func alreadyConnected(granted, requested []string, replace bool) bool {
	if replace {
		return sameScopeSet(withoutOffline(granted), withoutOffline(requested))
	}
	return len(missingScopes(granted, requested)) == 0
}

// connectInitiateError turns initiate's refusals into what a person can act on.
// A 400 is forwarded as the server worded it — an unknown service lists the
// valid names, an unofferable --scope the allowed set.
func connectInitiateError(err error) error {
	if api.StatusOf(err) == http.StatusForbidden && (api.WireCodeOf(err) == api.InsufficientScopeCode || scopeDenied(err)) {
		return errors.New("this login's token cannot write secrets (needs secrets:write)")
	}
	return err
}

// waitForConnect polls the secret until the consent lands, the link expires,
// the secret disappears, or the person presses Ctrl-C.
//
// Transient failures — a dropped connection, a 429, a 5xx from a pod restarting
// — are tolerated for the login poll's window (client.MaxPollFailureWindow),
// measured in elapsed time, and a 429 backs the cadence off; a 404 is terminal,
// since a deleted secret will never complete.
//
// onPage is a wait that started on Ronja's page rather than the provider's:
// that link never expires, so the wait is longer and the messages say so.
func waitForConnect(ctx context.Context, client *api.Client, before *api.SecretStatus, onPage bool) (*api.SecretStatus, error) {
	limit, cancelled := connectWaitLimit, "cancelled; the sign-in link stays valid until it expires, and completing it still connects the secret"
	if onPage {
		limit, cancelled = connectPageWaitLimit, "cancelled; connecting on the page in Ronja still connects the secret"
	}
	deadline := time.Now().Add(limit)
	interval := connectPollInterval
	latest := before
	var failingSince time.Time

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New(cancelled)
		case <-time.After(interval):
		}

		now, err := client.GetSecretStatus(ctx, before.ID)
		switch {
		case err == nil:
			failingSince = time.Time{}
			interval = connectPollInterval
			latest = now
			if connectCompleted(before, now) {
				return now, nil
			}
		case ctx.Err() != nil:
			return nil, errors.New(cancelled)
		case api.StatusOf(err) == http.StatusNotFound:
			return nil, fmt.Errorf("the secret was deleted while waiting (%s)", before.ID)
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
			return nil, fmt.Errorf("read secret %s: %w", before.ID, err)
		}

		if time.Now().After(deadline) {
			// From ready, completion is seen only as a CHANGE in the grant, so a
			// consent that finished granting nothing new (the person unticked
			// exactly the new scopes) looks the same as one never finished.
			either := ""
			if before.Status == "ready" {
				either = ", or the consent finished without granting anything new: the recorded grant is unchanged"
			}
			if onPage {
				return nil, fmt.Errorf("the sign-in did not complete within %s%s. Currently recorded grant: %s. You can still connect on the page in Ronja, or re-run to wait again",
					humanDuration(limit), either, scopeList(grantedOf(latest)))
			}
			return nil, fmt.Errorf("the sign-in did not complete within %s (the link has expired)%s. Currently recorded grant: %s. Re-run to get a new link",
				humanDuration(limit), either, scopeList(grantedOf(latest)))
		}
	}
}

// connectCompleted is the completion rule. From pending or reauth_required the
// status reaching ready is unambiguous. From ready the only server-visible sign
// of a consent is a CHANGE in grantedScopes — the column only the callback
// writes. updatedAt is never read: a background token refresh bumps it.
func connectCompleted(before, now *api.SecretStatus) bool {
	if now.Status != "ready" {
		return false
	}
	if before.Status != "ready" {
		return true
	}
	return !sameScopeSet(grantedOf(before), grantedOf(now))
}

// reportConnected prints the one result line, or --json metadata. already is
// true when no sign-in was needed. missing is nil when the server could not say
// what was asked for, so the JSON can tell "nothing missing" ([]) from
// "unknown" (null).
func reportConnected(already bool, s *api.SecretStatus, missing []string) error {
	granted := grantedOf(s)
	verb := "connected"
	if already {
		verb = "already connected"
	}
	if flagJSON {
		featureID := ""
		if s.FeatureID != nil {
			featureID = *s.FeatureID
		}
		return emitJSON(map[string]any{
			"secretID":         s.ID,
			"name":             s.Name,
			"status":           s.Status,
			"grantedScopes":    granted,
			"missingScopes":    missing,
			"featureID":        featureID,
			"alreadyConnected": already,
		})
	}
	fmt.Fprintf(os.Stdout, "%s: %s (%s, scopes: %s)\n", verb, s.ID, s.Name, scopeList(granted))
	return nil
}

// grantedOf is the recorded grant as a non-nil slice.
func grantedOf(s *api.SecretStatus) []string {
	if s == nil || s.GrantedScopes == nil {
		return []string{}
	}
	return *s.GrantedScopes
}

func withoutOffline(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s != offlineAccess {
			out = append(out, s)
		}
	}
	return out
}

// sameScopeSet compares two scope lists as sets.
func sameScopeSet(a, b []string) bool {
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// missingScopes is what the grant lacks of what was asked for, by the server's
// coverage rule (oauthregistry.ScopesCover): offline_access is never granted
// back, and a full Google scope covers its ".readonly" variant.
func missingScopes(granted, requested []string) []string {
	have := make(map[string]bool, len(granted))
	for _, s := range granted {
		have[s] = true
	}
	missing := []string{}
	for _, s := range requested {
		if s == offlineAccess || have[s] {
			continue
		}
		if base := strings.TrimSuffix(s, ".readonly"); base != s && have[base] {
			continue
		}
		missing = append(missing, s)
	}
	return missing
}

func scopeList(scopes []string) string {
	if len(scopes) == 0 {
		return "none"
	}
	return strings.Join(scopes, ", ")
}

// providerLabel is "Google" for the "Google OAuth" secret every registry
// service of one provider shares.
func providerLabel(secretName string) string {
	if p := strings.TrimSpace(strings.TrimSuffix(secretName, " OAuth")); p != "" {
		return p
	}
	return "the provider"
}

// humanDuration renders the wait limit the way a person says it.
func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if m := int(d / time.Minute); m != 1 {
			return fmt.Sprintf("%d minutes", m)
		}
		return "1 minute"
	}
	return d.String()
}
