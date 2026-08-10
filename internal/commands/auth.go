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

// Sign-in lives at the TOP LEVEL — `ronja login`, not `ronja login`.
//
// The CLI is a bootstrap, and this is the bootstrap act; burying it one level
// down understates what the tool is for. It also matches the surface around it,
// which was already flat: `context`, `env`, `api` and `query` are all
// auth-adjacent and none of them is nested.
//
// `whoami` rather than `status` for the identity read. `ronja status` would
// read as instance health, and this CLI already has a `status` that means
// something else entirely (`wf status`, about a folder).

// There is deliberately NO `ronja token` command.
//
// A command whose whole purpose is printing a live credential puts that
// credential into a terminal scrollback, a CI log, or an agent transcript every
// time it runs. Being able to READ the token and having it ECHOED are different
// things, and only the second is a leak.
//
// Instead `ronja context` reports the credential FILE — its path, the JSON key
// for this instance, and a substitution that pipes the value straight into an
// Authorization header without it ever being printed. A caller that needs the
// value has it; nothing routinely writes it down.

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
--profile says otherwise.

With --with-token, a token is read from stdin instead. That is the path for CI
and for machines with no browser:

  echo "$TOKEN" | ronja login --url https://app.ronja.tech --with-token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			target := flagURL
			if target == "" {
				// Deliberately does NOT inherit the current profile's URL:
				// logging in is how you choose an instance, so falling back to
				// whatever was used last would silently re-authenticate the
				// wrong one.
				target = os.Getenv(config.EnvURL)
			}
			if target == "" {
				target = config.DefaultURL
			}
			normalized, err := config.NormalizeURL(target)
			if err != nil {
				return err
			}

			// Check the requested name BEFORE the browser dance. Everything
			// after StartDevice mints a real 90-day credential, and discovering
			// then that the name is unusable means reporting a problem the user
			// could have been told about for free.
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

// loginWithDeviceFlow runs the browser handshake.
func loginWithDeviceFlow(ctx context.Context, baseURL string, noBrowser bool) error {
	client := api.New(baseURL, "")

	start, err := client.StartDevice(ctx, clientName())
	if err != nil {
		return fmt.Errorf("start login against %s: %w", baseURL, err)
	}

	// Human-facing instructions go to STDERR, so that `--json` stdout stays a
	// clean single object even while the flow is narrating progress.
	out := os.Stderr
	fmt.Fprintf(out, "\n  Your code: %s\n\n", start.UserCode)

	// "spawned" rather than "opened": all we know is that the platform's URL
	// handler started. Whether a browser window actually appeared is not
	// something any of these helpers reports, so the prefilled URL is printed
	// either way and the wording does not promise more than it knows.
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

	// PERSIST BEFORE VERIFYING. The server burns the single-use latch before
	// it mints, and hands the plaintext over exactly once — so this token
	// exists, is live, is full-access, and nobody but this process has ever
	// seen it. If we called /me first and it failed (a blip, a rolling
	// deploy), the token would be dropped on the floor: a live credential
	// bound to the user, visible in Account -> Access tokens, that they cannot
	// use and would have no idea how to reason about. Writing it to disk first
	// means the worst case is a stored token plus a clear warning, which
	// `ronja whoami` can then diagnose and `ronja logout` can clear.
	//
	// The organization is already known here — the poll response carries it —
	// so the profile's IDENTITY is complete at this point and only its NAME
	// waits on /me.
	name, provisional, declined, err := claimLogin(baseURL, token.TenantID, func(p *config.Profile) {
		p.Token = token.Token
		p.TokenID = token.TokenID
		p.TokenName = token.TokenName
		p.TokenExpiry = token.ExpiresAt
		p.LoggedIn = time.Now().UTC().Format(time.RFC3339)
	})
	if err != nil {
		return fmt.Errorf("store the token that was just issued: %w", err)
	}

	me, err := api.New(baseURL, token.Token).Me(ctx)
	if err != nil {
		// Stored but unverified: say so precisely rather than implying the
		// login failed outright, because it did not.
		if flagJSON {
			// --json stdout stays exactly one object, and `verified:false` is
			// the flag a caller branches on.
			return emitJSON(map[string]any{
				"url":            baseURL,
				"profile":        name,
				"authenticated":  true,
				"verified":       false,
				"tokenFromEnv":   false,
				"tokenExpiresAt": token.ExpiresAt,
				"warning":        fmt.Sprintf("token stored but not verified: %v", err),
			})
		}
		fmt.Fprintf(os.Stderr,
			"\n  Warning: the token was issued and stored as profile %q, but %s did\n"+
				"  not answer when we checked it (%v). Run `ronja whoami` to confirm.\n\n",
			name, baseURL, err)
		return nil
	}

	// Backfill the identity snapshot, and give the profile its real name now
	// that the organization is known.
	//
	// A failure here DEGRADES rather than aborting, for the same reason the /me
	// failure above does: by this point the token is already stored and working.
	// Returning an error would exit non-zero with a message that reads like the
	// login failed — most plausibly "timed out waiting for …config.json.lock",
	// which says nothing about tokens at all — and the user's obvious next move
	// is to run `ronja login` again, minting a second PAT and orphaning a
	// perfectly good first one. The snapshot is cosmetic: it makes the
	// credential file legible to a human, and `whoami` round-trips to /me
	// for anything that matters.
	//
	// The FIRST write, which persists the token itself, must keep failing
	// loudly — there the alternative is a live credential nobody has a copy of.
	final, err := finishLogin(name, provisional, baseURL, me)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"\n  Warning: signed in, but the identity details could not be written\n"+
				"  to the credential file (%v). The token itself is stored under\n"+
				"  profile %q and works.\n", err, name)
		final = name
	}
	reportLogin(baseURL, final, me, token.ExpiresAt, declined)
	return nil
}

// checkRequestedProfile refuses an unusable --profile name before anything is
// minted. Only the URL can be judged this early: whether the ORGANIZATION
// matches is not knowable until the human has approved in the browser, and that
// case is handled after the fact by claimLogin.
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

// claimLogin stores a freshly minted credential against its identity, in ONE
// locked step: finding or allocating the name and writing the token cannot be
// separated, or two concurrent logins both take the same free name and the
// second silently evicts the first.
//
// It reports whether the profile was newly CREATED — meaning its name is still
// provisional and may be improved once /me names the organization — and whether
// a requested --profile name had to be DECLINED because a different
// organization already holds it.
func claimLogin(instanceURL, tenantID string, fill func(*config.Profile)) (name string, created, declined bool, err error) {
	preferred := flagProfile
	err = config.Update(func(f *config.File) error {
		var p *config.Profile
		name, p, created = f.ClaimProfile(instanceURL, tenantID, preferred)
		declined = preferred != "" && name != preferred
		fill(p)
		f.Current = name
		return nil
	})
	return name, created, declined, err
}

// finishLogin backfills the identity snapshot and, for a profile whose name is
// still provisional, renames it after the organization.
//
// It deliberately does NOT touch TenantID. That was set when the profile was
// claimed and is half of its identity; rewriting it here from a second source
// could move a profile into a slot another one already occupies, which is how
// two logins end up fighting over one entry again.
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
		// Re-derived INSIDE this lock rather than passed in: another login can
		// have taken the name since the first write, and allocating against a
		// stale snapshot is the same lost-credential race one step later.
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

// loginWithToken stores a token supplied on stdin, verifying it FIRST.
//
// The opposite order from the device flow, deliberately: a pasted token is
// re-pastable, so refusing to store one we could not use costs the caller
// nothing and saves them a confusing failure on the next command. A
// device-flow token is handed over exactly once and cannot be recovered, so
// there it must be written down before anything else can go wrong.
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

	// No poll response here, so /me is where the organization comes from — and
	// it is known BEFORE the claim, which is why this path needs no rename
	// afterwards.
	tenantID := ""
	if me.Tenant != nil {
		tenantID = me.Tenant.ID
	}
	name, created, declined, err := claimLogin(baseURL, tenantID, func(p *config.Profile) {
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
	if created && flagProfile == "" {
		// The organization was known at claim time, so improving the name is
		// just the same rename, with nothing to wait for.
		if final, err := finishLogin(name, true, baseURL, me); err == nil {
			name = final
		}
	}
	reportLogin(baseURL, name, me, "", declined)
	return nil
}

// reportLogin is the shared success output for both login paths.
func reportLogin(baseURL, profile string, me *api.Me, expiresAt string, declined bool) {
	if flagJSON {
		payload := statusPayload(baseURL, profile, me, false)
		if expiresAt != "" {
			payload["tokenExpiresAt"] = expiresAt
		}
		if declined {
			payload["requestedProfile"] = flagProfile
			payload["profileNameDeclined"] = true
		}
		_ = emitJSON(payload)
		return
	}
	path, _ := config.Path()
	fmt.Fprintf(os.Stderr, "\n  Signed in to %s as %s", baseURL, describeUser(me))
	if me.Tenant != nil {
		fmt.Fprintf(os.Stderr, " (%s)", me.Tenant.Name)
	}
	fmt.Fprintf(os.Stderr, "\n  Profile %q — now current. Token stored in %s\n", profile, path)
	// Say when it dies, at the moment it is created. A credential that stops
	// working with no warning is the worst version of this.
	if when := describeExpiry(expiresAt); when != "" {
		fmt.Fprintf(os.Stderr, "  Expires %s — run `ronja login` again to renew.\n", when)
	}

	// A token with no organization reaches almost nothing. Better to say so at
	// the moment it is created than to let every later command fail obscurely.
	if me.Tenant == nil {
		fmt.Fprintf(os.Stderr,
			"\n  Note: this login has no organization. Most of the API needs one —\n"+
				"  join or create one in the web app, then run `ronja login` again.\n")
	}

	// The requested name belonged to a different organization. Say where the
	// credential actually went, and why: silently landing it elsewhere is how
	// someone later runs a command against the wrong organization.
	if declined {
		fmt.Fprintf(os.Stderr,
			"\n  Note: profile %q already holds a different organization on this instance,\n"+
				"  so nothing was overwritten — this login is stored as %q instead.\n"+
				"  A token belongs to whichever organization you were signed in to when you\n"+
				"  approved. To reach the other one, switch organization in the web app and\n"+
				"  run `ronja login` again, or rename with `ronja profile rename`.\n",
			flagProfile, profile)
	}

	// Raised here rather than before the login: the moment someone has just
	// replaced the credential is the moment the stale one is worth mentioning.
	warnLegacyStore()

	// The handoff. Without this line an agent has a working credential and no
	// idea what to do with it; `ronja context` is the whole answer.
	fmt.Fprintf(os.Stderr, "\n  Next: run `ronja context` for everything needed to call the API.\n\n")
}

// describeExpiry renders an RFC 3339 expiry as a date plus a day count, or ""
// when there is no expiry to report. "2026-10-25 (in 90 days)" beats either
// half alone: the date is what you diary, the count is what you react to.
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
					// Point at whichever token actually failed: telling someone
					// to log in again would not help if the credential came
					// from the environment and will just override it anyway.
					if resolved.FromEnv {
						return fmt.Errorf("the token in $%s was rejected by %s", config.EnvToken, resolved.URL)
					}
					return fmt.Errorf("the stored token for %s is no longer valid — run `ronja login` again", resolved.URL)
				}
				return err
			}

			// Only meaningful for a stored login: an env-supplied token was
			// never minted through us, so we know nothing about its lifetime.
			expiry := ""
			if !resolved.FromEnv && resolved.Entry != nil {
				expiry = resolved.Entry.TokenExpiry
			}

			// The credential file records an organization; the server has just
			// stated one. They can only disagree if the file was hand-edited or
			// the token was re-pointed — but if they do, every workflow-folder
			// binding keyed on the recorded one is wrong, so say it out loud
			// rather than letting it surface as a confusing 404 later.
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

// notSignedIn explains what to do, in terms of whichever selector the caller
// actually used — telling someone who passed --profile to run `login --url` is
// an instruction that does not fit the command they typed.
func notSignedIn(resolved *config.Resolved) error {
	if flagProfile != "" || os.Getenv(config.EnvProfile) != "" {
		return fmt.Errorf("profile %q has no stored token — run `ronja login --url %s --profile %s`",
			resolved.Profile, resolved.URL, resolved.Profile)
	}
	// On the default instance the --url is noise, and this is the very first
	// message a new install produces — the shortest true command wins.
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
				// Otherwise the next command still works and it looks like
				// logout silently failed.
				fmt.Fprintf(os.Stderr, "  Note: $%s is still set, so the CLI remains authenticated.\n",
					config.EnvToken)
			}
			return nil
		},
	}
}

// statusPayload is the --json shape shared by login and status.
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

// describeUser names the signed-in human. Email first because it is unique and
// is what a person recognises; the composed first/last name is the fallback for
// an account with none (an SSO user whose email the directory did not share),
// and the opaque ID is the last resort so this never returns "".
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

// readTokenFromStdin accepts a token piped in, or prompts when attached to a
// terminal. Prompting only on a TTY is what keeps the command safe to use in a
// pipeline: a script that forgets to pipe anything gets EOF, not a hang.
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

// openBrowser is best-effort; every caller has a printed-URL fallback.
//
// The URL arrives verbatim from whichever instance --url pointed at, and the
// platform openers dispatch on SCHEME: `open` will happily hand a non-http URL
// to whatever local application has registered that scheme, or open a path. So
// the scheme is checked before anything is spawned. Host-matching is
// deliberately NOT attempted — the approval page legitimately lives on the
// frontend origin, which differs from the API origin in every real deployment.
func openBrowser(raw string) error {
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
	// A successful Start proves only that the helper SPAWNED — it says nothing
	// about a browser appearing, which is why the caller still prints the URL.
	// Reap it so it does not sit as a zombie for the several minutes a login
	// can take.
	go func() { _ = proc.Wait() }()
	return nil
}
