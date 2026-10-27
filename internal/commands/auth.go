package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ⚠️ No command prints a token (it would land in scrollback, CI logs and agent
// transcripts); `ronja context` reports the credential file instead, and
// `ronja token create` hands the mint to the browser.

func newLoginCmd() *cobra.Command {
	var withToken bool
	var noBrowser bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authorize this machine against a Ronja instance",
		Long: `Authorize this machine against a Ronja instance.

Prints a short code and opens your browser. Approve there and the CLI receives
a personal access token, stored as a named profile in the CLI config directory
with owner-only permissions ('ronja context' prints the exact, platform-specific
path).

The token acts as you and carries your current permissions — it can never do
more than you can, and it shrinks automatically if your role changes. It expires
after 90 days; run this again to renew. Revoke it sooner from
Account -> Access tokens.

A token belongs to ONE organization: the one you are signed in to in the browser
when you approve. To add a second organization, switch to it in the web app and
run this again — the new token is stored as its own profile beside the first,
never on top of it. The profile is named after the organization unless
--profile says otherwise. An organization already signed in here keeps its
profile's name; 'ronja profile rename' changes it.

With --with-token, a token is read from stdin instead. That is the path for CI
and for machines with no browser:

  echo "$TOKEN" | ronja login --url https://app.ronja.tech --with-token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			target := flagURL
			if target == "" {
				// Deliberately not the current profile's URL: logging in
				// chooses an instance, and the last one may be the wrong one.
				target = os.Getenv(config.EnvURL)
			}
			if target == "" {
				target = config.DefaultURL
			}
			normalized, err := config.NormalizeURL(target)
			if err != nil {
				return err
			}

			// Checked before anything is minted.
			if flagProfile != "" {
				if err := checkRequestedProfile(flagProfile, normalized); err != nil {
					return err
				}
			}

			if withToken {
				return loginWithToken(ctx, normalized)
			}
			return loginWithDeviceFlow(ctx, normalized, noBrowser)
		},
	}

	cmd.Flags().BoolVar(&withToken, "with-token", false,
		"read an existing access token from stdin instead of opening a browser")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false,
		"print the URL instead of opening a browser")
	return cmd
}

func loginWithDeviceFlow(ctx context.Context, baseURL string, noBrowser bool) error {
	client := api.New(baseURL, "")

	start, err := client.StartDevice(ctx, clientName())
	if err != nil {
		return fmt.Errorf("start login against %s: %w", baseURL, err)
	}

	// Stderr, so `--json` stdout stays one object.
	out := os.Stderr
	fmt.Fprintf(out, "\n  Your code: %s\n\n", start.UserCode)

	// Only the URL handler's start is known, not that a browser appeared, so
	// the prefilled URL is printed either way.
	spawned := false
	if !noBrowser {
		spawned = openBrowser(start.VerificationURIComplete) == nil
	}
	if spawned {
		fmt.Fprintf(out, "  Opening %s in your browser.\n", start.VerificationURI)
		fmt.Fprintf(out, "  If nothing opened, approve here instead:\n  %s\n\n", start.VerificationURIComplete)
	} else {
		fmt.Fprintf(out, "  Open this URL to approve:\n  %s\n\n", start.VerificationURIComplete)
	}
	fmt.Fprintf(out, "  Waiting for approval... (Ctrl-C to cancel)\n")

	token, err := client.PollDevice(ctx, start)
	if err != nil {
		switch {
		case errors.Is(err, api.ErrDeviceDenied):
			return errors.New("login was declined in the browser")
		case errors.Is(err, api.ErrDeviceExpired):
			return errors.New("the code expired — run `ronja login` again")
		case errors.Is(err, context.Canceled):
			return errors.New("login cancelled")
		}
		return err
	}

	// ⚠️ Persist before verifying: the server hands the plaintext over exactly
	// once, so a /me failure first would orphan a live credential. The poll
	// response carries the organization; only the profile's name waits on /me.
	claim, err := claimLogin(baseURL, token.TenantID, func(p *config.Profile) {
		p.Token = token.Token
		p.TokenID = token.TokenID
		p.TokenName = token.TokenName
		p.TokenExpiry = token.ExpiresAt
		p.LoggedIn = time.Now().UTC().Format(time.RFC3339)
	})
	if err != nil {
		return fmt.Errorf("store the token that was just issued: %w", err)
	}
	name := claim.name

	me, err := api.New(baseURL, token.Token).Me(ctx)
	if err != nil {
		// Stored but unverified, which is not a failed login.
		if flagJSON {
			payload := map[string]any{
				"url":            baseURL,
				"profile":        name,
				"authenticated":  true,
				"verified":       false,
				"tokenFromEnv":   false,
				"tokenExpiresAt": token.ExpiresAt,
				"warning":        fmt.Sprintf("token stored but not verified: %v", err),
			}
			claim.addTo(payload)
			return emitJSON(payload)
		}
		fmt.Fprintf(os.Stderr,
			"\n  Warning: the token was issued and stored as profile %q, but %s did\n"+
				"  not answer when we checked it (%v). Run `ronja whoami` to confirm.\n\n",
			name, baseURL, err)
		return nil
	}

	// A failure here only warns: the token is stored and working, and a
	// non-zero exit would prompt a second login that orphans it. The first
	// write, which persists the token, must keep failing loudly.
	final, err := finishLogin(name, claim.auto, baseURL, me)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"\n  Warning: signed in, but the identity details could not be written\n"+
				"  to the credential file (%v). The token itself is stored under\n"+
				"  profile %q and works.\n", err, name)
		final = name
	}
	claim.name = final
	reportLogin(baseURL, me, token.ExpiresAt, claim)
	return nil
}

// checkRequestedProfile refuses an unusable --profile name before anything is
// minted. Only the URL can be judged this early; an organization mismatch is
// handled after approval by claimLogin.
func checkRequestedProfile(name, instanceURL string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	f, err := config.Load()
	if err != nil {
		return err
	}
	if p, ok := f.Profiles[name]; ok && p != nil && p.URL != instanceURL {
		return fmt.Errorf("profile %q is signed in to %s, not %s — pick another name, or drop --url to renew it where it is",
			name, p.URL, instanceURL)
	}
	return nil
}

// loginClaim is where a login's credential landed and why it has that name.
type loginClaim struct {
	name string
	// auto: nobody chose this name, so finishLogin names the profile after the
	// organization. A --profile name another organization holds leaves the
	// choice open too.
	auto bool
	// declined: the requested --profile name was not used. kept says why: this
	// organization already has a profile, and a re-login renews it in place
	// rather than renaming it, since scripts and RONJA_PROFILE refer to it.
	declined, kept bool
	// nameHeld: another sign-in holds the requested name, so no rename can free it.
	nameHeld bool
}

// addTo records a declined --profile name in a login's --json output.
func (c loginClaim) addTo(payload map[string]any) {
	if c.declined {
		payload["requestedProfile"] = flagProfile
		payload["profileNameDeclined"] = true
		payload["organizationAlreadyStored"] = c.kept
	}
}

// claimLogin stores a freshly minted credential in one locked step: allocating
// the name and writing the token cannot be separated, or two concurrent logins
// take the same free name and the second evicts the first.
func claimLogin(instanceURL, tenantID string, fill func(*config.Profile)) (loginClaim, error) {
	preferred := flagProfile
	var c loginClaim
	err := config.Update(func(f *config.File) error {
		name, p, created := f.ClaimProfile(instanceURL, tenantID, preferred)
		c = loginClaim{name: name, declined: preferred != "" && name != preferred}
		c.kept = c.declined && !created
		_, c.nameHeld = f.Profiles[preferred]
		c.nameHeld = c.nameHeld && c.declined
		c.auto = created && (preferred == "" || c.declined)
		fill(p)
		f.Current = name
		return nil
	})
	return c, err
}

// finishLogin backfills the identity snapshot and, for a provisional name,
// renames the profile after the organization. It never touches TenantID: that
// is half the profile's identity, and rewriting it could collide with another
// profile's slot.
func finishLogin(name string, provisional bool, instanceURL string, me *api.Me) (string, error) {
	final := name
	err := config.Update(func(f *config.File) error {
		p, ok := f.Profiles[name]
		if !ok || p == nil {
			return fmt.Errorf("profile %q disappeared while signing in", name)
		}
		p.UserID = me.User.ID
		p.UserEmail = me.User.Email
		if me.Tenant != nil {
			p.TenantName = me.Tenant.Name
		}
		// Re-derived inside this lock: another login may have taken the name
		// since the first write.
		if provisional {
			if better := f.AutoName(instanceURL, p.TenantName, name); better != name {
				if err := f.Rename(name, better); err != nil {
					return err
				}
				final = better
			}
		}
		f.Current = final
		return nil
	})
	return final, err
}

// loginWithToken stores a token supplied on stdin, verifying it first: the
// opposite order from the device flow is safe because a pasted token can be
// pasted again.
func loginWithToken(ctx context.Context, baseURL string) error {
	token, err := readTokenFromStdin()
	if err != nil {
		return err
	}

	me, err := api.New(baseURL, token).Me(ctx)
	if err != nil {
		if api.StatusOf(err) == 401 || api.StatusOf(err) == 403 {
			return fmt.Errorf("that token was rejected by %s", baseURL)
		}
		return fmt.Errorf("verify token against %s: %w", baseURL, err)
	}

	tenantID := ""
	if me.Tenant != nil {
		tenantID = me.Tenant.ID
	}
	claim, err := claimLogin(baseURL, tenantID, func(p *config.Profile) {
		p.Token = token
		p.UserID = me.User.ID
		p.UserEmail = me.User.Email
		if me.Tenant != nil {
			p.TenantName = me.Tenant.Name
		}
		p.LoggedIn = time.Now().UTC().Format(time.RFC3339)
	})
	if err != nil {
		return err
	}
	if claim.auto {
		if final, err := finishLogin(claim.name, true, baseURL, me); err == nil {
			claim.name = final
		}
	}
	reportLogin(baseURL, me, "", claim)
	return nil
}

func reportLogin(baseURL string, me *api.Me, expiresAt string, claim loginClaim) {
	profile := claim.name
	if flagJSON {
		payload := statusPayload(baseURL, profile, me, false)
		if expiresAt != "" {
			payload["tokenExpiresAt"] = expiresAt
		}
		claim.addTo(payload)
		_ = emitJSON(payload)
		return
	}
	path, _ := config.Path()
	fmt.Fprintf(os.Stderr, "\n  Signed in to %s as %s", baseURL, describeUser(me))
	if me.Tenant != nil {
		fmt.Fprintf(os.Stderr, " (%s)", me.Tenant.Name)
	}
	fmt.Fprintf(os.Stderr, "\n  Profile %q — now current. Token stored in %s\n", profile, path)
	if when := describeExpiry(expiresAt); when != "" {
		fmt.Fprintf(os.Stderr, "  Expires %s — run `ronja login` again to renew.\n", when)
	}

	if me.Tenant == nil {
		fmt.Fprintf(os.Stderr,
			"\n  Note: this login has no organization. Most of the API needs one —\n"+
				"  join or create one in the web app, then run `ronja login` again.\n")
	}

	// Said out loud: a credential silently landing under another name is how
	// someone later runs a command against the wrong organization.
	switch {
	case claim.kept && claim.nameHeld:
		fmt.Fprintf(os.Stderr,
			"\n  Note: this organization was already signed in here as profile %q, so its\n"+
				"  token was renewed there. Profile %q already holds another sign-in. To reach\n"+
				"  that one, switch organization in the web app and run\n"+
				"  `ronja login --profile %s` again.\n",
			profile, flagProfile, flagProfile)
	case claim.kept:
		fmt.Fprintf(os.Stderr,
			"\n  Note: this organization was already signed in here as profile %q, so its\n"+
				"  token was renewed there and not stored as %q. To use the new name, run\n"+
				"  `ronja profile rename %s %s`. To reach another organization, switch to it\n"+
				"  in the web app and run `ronja login` again.\n",
			profile, flagProfile, profile, flagProfile)
	case claim.declined:
		fmt.Fprintf(os.Stderr,
			"\n  Note: profile %q already holds a different organization on this instance,\n"+
				"  so nothing was overwritten — this login is stored as %q instead.\n"+
				"  A token belongs to whichever organization you were signed in to when you\n"+
				"  approved. To reach the other one, switch organization in the web app and\n"+
				"  run `ronja login` again, or rename with `ronja profile rename`.\n",
			flagProfile, profile)
	}

	warnLegacyStore()

	fmt.Fprintf(os.Stderr, "\n  Next: run `ronja context` for everything needed to call the API.\n\n")
}

// describeExpiry renders an RFC 3339 expiry as a date plus a day count, or an
// empty string when there is none.
func describeExpiry(raw string) string {
	if raw == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	days := int(time.Until(at).Hours() / 24)
	switch {
	case days < 0:
		return at.Local().Format("2006-01-02") + " (already expired)"
	case days == 0:
		return at.Local().Format("2006-01-02") + " (today)"
	case days == 1:
		return at.Local().Format("2006-01-02") + " (in 1 day)"
	default:
		return fmt.Sprintf("%s (in %d days)", at.Local().Format("2006-01-02"), days)
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the CLI is signed in as",
		Long: `Show who the CLI is signed in as.

Calls the server rather than reporting what is on disk, so a revoked or expired
token is reported as such. Exits non-zero when not authenticated.

Reports the selected profile. To see them all without contacting a server, use
'ronja profile list'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			if resolved.Token == "" {
				return notSignedIn(resolved)
			}

			me, err := api.New(resolved.URL, resolved.Token).Me(cmd.Context())
			if err != nil {
				if api.StatusOf(err) == 401 || api.StatusOf(err) == 403 {
					// Name the token that failed: logging in again does not help
					// while an env token overrides the stored one.
					if resolved.FromEnv {
						return fmt.Errorf("the token in $%s was rejected by %s", config.EnvToken, resolved.URL)
					}
					return fmt.Errorf("the stored token for %s is no longer valid — run `ronja login` again", resolved.URL)
				}
				return err
			}

			// An env-supplied token's lifetime is unknown.
			expiry := ""
			if !resolved.FromEnv && resolved.Entry != nil {
				expiry = resolved.Entry.TokenExpiry
			}

			// A recorded organization that disagrees with the server's breaks
			// every workflow-folder binding keyed on it, so it is reported.
			drifted := !resolved.FromEnv && resolved.Entry != nil &&
				me.Tenant != nil && resolved.Entry.TenantID != "" &&
				resolved.Entry.TenantID != me.Tenant.ID

			if flagJSON {
				payload := statusPayload(resolved.URL, resolved.Profile, me, resolved.FromEnv)
				if expiry != "" {
					payload["tokenExpiresAt"] = expiry
				}
				if drifted {
					payload["recordedTenantID"] = resolved.Entry.TenantID
					payload["tenantMismatch"] = true
				}
				return emitJSON(payload)
			}
			fmt.Printf("  %s\n", resolved.URL)
			if resolved.Profile != "" {
				fmt.Printf("  Profile: %s\n", resolved.Profile)
			}
			fmt.Printf("  User:  %s\n", describeUser(me))
			if me.Tenant != nil {
				fmt.Printf("  Org:   %s\n", me.Tenant.Name)
			}
			if me.Role != nil {
				fmt.Printf("  Role:  %s\n", me.Role.Name)
			}
			if when := describeExpiry(expiry); when != "" {
				fmt.Printf("  Expires: %s\n", when)
			}
			if resolved.FromEnv {
				fmt.Printf("  Token: from $%s\n", config.EnvToken)
			}
			if drifted {
				fmt.Fprintf(os.Stderr,
					"\n  Warning: profile %q records organization %s, but this token reaches %s.\n"+
						"  Sign in again to correct it — workflow folders bound under the recorded\n"+
						"  organization will not match until you do.\n",
					resolved.Profile, resolved.Entry.TenantID, me.Tenant.ID)
			}
			return nil
		},
	}
}

// notSignedIn explains what to do in terms of the selector the caller used.
func notSignedIn(resolved *config.Resolved) error {
	if flagProfile != "" || os.Getenv(config.EnvProfile) != "" {
		return fmt.Errorf("profile %q has no stored token — run `ronja login --url %s --profile %s`",
			resolved.Profile, resolved.URL, resolved.Profile)
	}
	if resolved.URL == config.DefaultURL {
		return errors.New("not signed in — run `ronja login`")
	}
	return fmt.Errorf("not signed in to %s — run `ronja login --url %s`",
		resolved.URL, resolved.URL)
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget a stored profile",
		Long: `Forget a stored profile.

Removes ONE profile — the selected one, which is the current profile unless
--profile or --url says otherwise. Other organizations on the same instance keep
their own logins.

This removes the token from disk. It does NOT revoke it — do that from
Account -> Access tokens if the token may have been exposed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			removed := false
			if resolved.Profile != "" {
				if err := config.Update(func(f *config.File) error {
					removed = f.Remove(resolved.Profile)
					return nil
				}); err != nil {
					return err
				}
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"url":        resolved.URL,
					"profile":    resolved.Profile,
					"removed":    removed,
					"tokenInEnv": resolved.FromEnv,
				})
			}
			if !removed {
				fmt.Fprintf(os.Stderr, "  No stored credentials for %s\n", resolved.URL)
			} else {
				fmt.Fprintf(os.Stderr, "  Removed profile %q (%s)\n", resolved.Profile, resolved.URL)
			}
			if resolved.FromEnv {
				fmt.Fprintf(os.Stderr, "  Note: $%s is still set, so the CLI remains authenticated.\n",
					config.EnvToken)
			}
			return nil
		},
	}
}

// statusPayload is the --json shape shared by login and whoami.
func statusPayload(url, profile string, me *api.Me, fromEnv bool) map[string]any {
	payload := map[string]any{
		"url":           url,
		"userID":        me.User.ID,
		"userEmail":     me.User.Email,
		"authenticated": true,
		"tokenFromEnv":  fromEnv,
	}
	if profile != "" {
		payload["profile"] = profile
	}
	if me.Tenant != nil {
		payload["tenantID"] = me.Tenant.ID
		payload["tenantName"] = me.Tenant.Name
	}
	if me.Role != nil {
		payload["role"] = me.Role.Name
	}
	return payload
}

// describeUser names the signed-in human: email, else full name, else the ID,
// so it is never empty.
func describeUser(me *api.Me) string {
	if me.User.Email != "" {
		return me.User.Email
	}
	if name := strings.TrimSpace(me.User.FirstName + " " + me.User.LastName); name != "" {
		return name
	}
	return me.User.ID
}

// clientName is the self-description shown on the approval screen so a human
// can recognise their own terminal. The server treats it as untrusted.
func clientName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "ronja-cli"
	}
	return "ronja-cli on " + host
}

// readTokenFromStdin accepts a token piped in, or prompts on a terminal only, so
// a script that pipes nothing gets EOF rather than a hang.
func readTokenFromStdin() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Paste your access token: ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}

	raw, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("no token on stdin")
	}
	return token, nil
}

// openBrowser is best-effort; every caller has a printed-URL fallback. A
// package var so tests can pin when and with what it is called.
var openBrowser = openBrowserURL

// openBrowserURL refuses any scheme but http(s): the URL comes from the server
// and the platform openers dispatch on scheme to any registered local handler.
// The host is deliberately not matched, since the approval page lives on the
// frontend origin, not the API's.
func openBrowserURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("unusable verification URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing to open a %q URL", u.Scheme)
	}

	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}

	proc := exec.Command(name, append(args, raw)...)
	if err := proc.Start(); err != nil {
		return err
	}
	// Reaped so it does not sit as a zombie for the length of a login.
	go func() { _ = proc.Wait() }()
	return nil
}
