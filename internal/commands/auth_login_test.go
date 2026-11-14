package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
)

// loginFake answers the device flow and /me for one organization, approving on
// the first poll.
func loginFake(t *testing.T, tenantID, tenantName, token string) string {
	t.Helper()
	url, _ := recordingLoginFake(t, tenantID, tenantName, token)
	return url
}

// recordingLoginFake is loginFake that also keeps each device-start body. Its
// verificationUriComplete carries a marker the CLI could not have built, so a
// test can tell the CLI opened the server's link.
func recordingLoginFake(t *testing.T, tenantID, tenantName, token string) (string, *[]map[string]any) {
	t.Helper()
	starts := &[]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v2/authentication/cli/device":
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode device start: %v", err)
			}
			*starts = append(*starts, body)
			writeJSON(w, api.DeviceStart{DeviceCode: "dev", UserCode: "ABCD-EFGH", ExpiresIn: 600,
				VerificationURI: "http://x/cli-auth", VerificationURIComplete: "http://x/cli-auth?code=ABCD-EFGH&server=built"})
		case "POST /api/v2/authentication/cli/token":
			writeJSON(w, api.DeviceToken{Token: token, TokenID: "tok-" + token, TokenName: "ronja-cli on test",
				UserID: "user-1", TenantID: tenantID, ExpiresAt: "2027-01-01T00:00:00Z"})
		case "GET /api/v2/authentication/me":
			writeJSON(w, map[string]any{
				"user":   map[string]any{"id": "user-1", "email": "admin@midas.test"},
				"tenant": map[string]any{"id": tenantID, "name": tenantName},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, starts
}

func loginEnv(t *testing.T) {
	t.Helper()
	signOutFrom(t, "")
	saved := openBrowser
	openBrowser = func(string) error { return nil }
	t.Cleanup(func() { openBrowser = saved })
}

func loadProfiles(t *testing.T) *config.File {
	t.Helper()
	f, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return f
}

func TestLoginKeepsTheRequestedProfileNameForANewOrganization(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-midas", "Midas Bikes", "pat-1")

	if _, err := runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "work"); err != nil {
		t.Fatalf("login: %v", err)
	}
	f := loadProfiles(t)
	p, ok := f.Profiles["work"]
	if !ok {
		t.Fatalf("no profile %q after `login --profile work`; profiles: %v", "work", f.Names())
	}
	if p.Token != "pat-1" || p.TenantName != "Midas Bikes" {
		t.Errorf("profile work = token %q, organization %q; want pat-1, Midas Bikes", p.Token, p.TenantName)
	}
	if len(f.Profiles) != 1 || f.Current != "work" {
		t.Errorf("profiles %v, current %q; want only work, current", f.Names(), f.Current)
	}
}

// The organization keeps its name: a rename would break every script and
// RONJA_PROFILE that uses it, and the usual cause is approving the organization
// you already have when you meant to add another. The note says so instead.
func TestLoginRenewsAnOrganizationUnderItsExistingName(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-midas", "Midas Bikes", "pat-2")
	writeProfiles(t, "midas-bikes", testProfile{Name: "midas-bikes", URL: url, TenantID: "ten-midas", TenantName: "Midas Bikes", Token: "pat-old"})

	out, err := runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "work", "--json")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	f := loadProfiles(t)
	if p := f.Profiles["midas-bikes"]; len(f.Profiles) != 1 || p == nil || p.Token != "pat-2" {
		t.Fatalf("profiles %v; want midas-bikes alone, renewed with the new token", f.Names())
	}
	got := decodeJSON(t, out)
	if got["profile"] != "midas-bikes" || got["profileNameDeclined"] != true || got["organizationAlreadyStored"] != true {
		t.Errorf("--json = %v; want profile midas-bikes, the name declined because the organization is already stored", got)
	}
}

// A --profile name another organization holds is refused, and the login is
// then named after its own organization, as if no name had been asked for.
func TestClaimLoginNamesADeclinedLoginAfterItsOrganization(t *testing.T) {
	loginEnv(t)
	const url = "https://cloud.ronja.tech"
	writeProfiles(t, "acme", testProfile{Name: "acme", URL: url, TenantID: "ten-acme", TenantName: "Acme", Token: "pat-acme"})
	saved := flagProfile
	flagProfile = "acme"
	t.Cleanup(func() { flagProfile = saved })

	claim, err := claimLogin(url, "ten-north", func(p *config.Profile) { p.Token = "pat-north" })
	if err != nil {
		t.Fatalf("claimLogin: %v", err)
	}
	if !claim.declined || claim.kept || !claim.auto {
		t.Fatalf("claim = %+v; want declined, not kept, named automatically", claim)
	}
	me := &api.Me{}
	me.Tenant = &struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{ID: "ten-north", Name: "North Retail"}
	final, err := finishLogin(claim.name, claim.auto, url, me)
	if err != nil {
		t.Fatalf("finishLogin: %v", err)
	}
	if final != "north-retail" {
		t.Errorf("declined login named %q, want the organization's name north-retail", final)
	}
}

// The note's advice must work: with the requested name held by another sign-in,
// renaming onto it would be refused, so the note points at that sign-in instead.
func TestLoginNoteForAnOrganizationAlreadyStored(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workHeld bool
		want     []string
		wantNot  []string
	}{
		{"requested name free", false, []string{"ronja profile rename midas-bikes work"}, []string{"already holds another sign-in"}},
		{"requested name held", true, []string{`Profile "work" already holds another sign-in`, "ronja login --profile work"}, []string{"profile rename"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loginEnv(t)
			url := loginFake(t, "ten-midas", "Midas Bikes", "pat-new")
			profiles := []testProfile{{Name: "midas-bikes", URL: url, TenantID: "ten-midas", TenantName: "Midas Bikes", Token: "pat-old"}}
			if tc.workHeld {
				profiles = append(profiles, testProfile{Name: "work", URL: url, TenantID: "ten-acme", TenantName: "Acme", Token: "pat-acme"})
			}
			writeProfiles(t, "midas-bikes", profiles...)

			var err error
			stderr := captureStderr(t, func() {
				_, err = runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "work")
			})
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr lacks %q:\n%s", w, stderr)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(stderr, w) {
					t.Errorf("stderr has %q:\n%s", w, stderr)
				}
			}
			f := loadProfiles(t)
			if p := f.Profiles["midas-bikes"]; p == nil || p.Token != "pat-new" {
				t.Errorf("midas-bikes was not renewed: %+v", p)
			}
			if tc.workHeld {
				if p := f.Profiles["work"]; p == nil || p.Token != "pat-acme" {
					t.Errorf("the other sign-in work was changed: %+v", p)
				}
			}
		})
	}
}

func TestLoginNeverTakesARequestedNameFromAnotherOrganization(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-north", "North Retail", "pat-north")
	writeProfiles(t, "acme", testProfile{Name: "acme", URL: url, TenantID: "ten-acme", TenantName: "Acme", Token: "pat-acme"})

	out, err := runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "acme", "--json")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	f := loadProfiles(t)
	if p := f.Profiles["acme"]; p == nil || p.Token != "pat-acme" || p.TenantID != "ten-acme" {
		t.Fatalf("the other organization's profile acme was changed: %+v", p)
	}
	name, p := f.FindIdentity(url, "ten-north")
	if p == nil || p.Token != "pat-north" {
		t.Fatalf("the new organization's token was not stored; profiles %v", f.Names())
	}
	got := decodeJSON(t, out)
	if got["profileNameDeclined"] != true || got["profile"] != name {
		t.Errorf("--json = %v; want profileNameDeclined and profile %q", got, name)
	}
}

func TestLoginWithTokenKeepsTheRequestedProfileName(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-midas", "Midas Bikes", "unused")
	stdin := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(stdin, []byte("pat-pasted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = saved; in.Close() })

	if _, err := runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "work", "--with-token"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if p := loadProfiles(t).Profiles["work"]; p == nil || p.Token != "pat-pasted" {
		t.Fatalf("`login --with-token --profile work` did not store profile work")
	}
}

func TestLoginWithoutAProfileNameNamesItAutomatically(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-midas", "Midas Bikes", "pat-3")

	if _, err := runCLI(t, t.TempDir(), "login", "--url", url); err != nil {
		t.Fatalf("login: %v", err)
	}
	f := loadProfiles(t)
	// A loopback instance is named after its host, not its organization.
	want := config.HostLabel(url)
	if p := f.Profiles[want]; p == nil || p.Token != "pat-3" {
		t.Fatalf("profiles %v; want the automatic name %q", f.Names(), want)
	}
}

// openedURLs replaces openBrowser with a recorder.
func openedURLs(t *testing.T) *[]string {
	t.Helper()
	opened := &[]string{}
	saved := openBrowser
	openBrowser = func(u string) error { *opened = append(*opened, u); return nil }
	t.Cleanup(func() { openBrowser = saved })
	return opened
}

// Renewing a stored profile by name tells the server which organization it is
// for, and opens the link the server built from that.
func TestLoginWithAStoredProfileNamesItsOrganization(t *testing.T) {
	loginEnv(t)
	opened := openedURLs(t)
	url, starts := recordingLoginFake(t, "ten-presso", "Presso", "pat-new")
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: url, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})

	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "presso")
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(*starts) != 1 || (*starts)[0]["tenantID"] != "ten-presso" {
		t.Fatalf("device start bodies %v, want one naming tenantID ten-presso", *starts)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://x/cli-auth?code=ABCD-EFGH&server=built" {
		t.Errorf("opened %v, want the server's verificationUriComplete", *opened)
	}
	for _, want := range []string{
		"Approve in Presso (profile presso).",
		"press Ctrl-C and run ronja login without --profile",
		"run `ronja login --profile presso` again to renew",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if p := loadProfiles(t).Profiles["presso"]; p == nil || p.Token != "pat-new" {
		t.Errorf("presso was not renewed: %+v", p)
	}
}

// Negative controls: a plain login is how a second organization is added, and a
// name nothing is stored under, or a stored profile with no organization
// recorded, knows no organization. None of them sends one.
func TestLoginWithoutAStoredOrganizationSendsNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		args   []string
	}{
		{"plain login", false, nil},
		{"unknown profile", false, []string{"--profile", "unknown"}},
		{"stored profile with no organization", true, []string{"--profile", "presso"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loginEnv(t)
			url, starts := recordingLoginFake(t, "ten-presso", "Presso", "pat-new")
			stored := testProfile{Name: "presso", URL: url, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"}
			if tc.legacy {
				stored.TenantID, stored.TenantName = "", ""
			}
			writeProfiles(t, "presso", stored)

			var err error
			stderr := captureStderr(t, func() {
				_, err = runCLI(t, t.TempDir(), append([]string{"login", "--url", url}, tc.args...)...)
			})
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			if len(*starts) != 1 {
				t.Fatalf("%d device starts, want 1", len(*starts))
			}
			if v, sent := (*starts)[0]["tenantID"]; sent {
				t.Errorf("device start sent tenantID %v, want none", v)
			}
			if strings.Contains(stderr, "Approve in") {
				t.Errorf("stderr names a target organization:\n%s", stderr)
			}
		})
	}
}

// The renewal hint prints `ronja login --profile <name>`, which must work for a
// profile stored on another instance: with no --url and no $RONJA_URL, the
// login goes to that profile's instance.
func TestLoginWithAStoredProfileRenewsAtItsInstance(t *testing.T) {
	loginEnv(t)
	url, starts := recordingLoginFake(t, "ten-presso", "Presso", "pat-new")
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: url, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})

	if _, err := runCLI(t, t.TempDir(), "login", "--profile", "presso"); err != nil {
		t.Fatalf("login --profile presso with no --url: %v", err)
	}
	if len(*starts) != 1 || (*starts)[0]["tenantID"] != "ten-presso" {
		t.Fatalf("device start bodies %v, want one at the stored instance naming ten-presso", *starts)
	}
	if p := loadProfiles(t).Profiles["presso"]; p == nil || p.Token != "pat-new" || p.URL != url {
		t.Errorf("presso = %+v, want renewed at %s", p, url)
	}
}

// An explicit --url still decides: a stored profile on another instance is
// refused rather than renewed somewhere it does not live.
func TestLoginRefusesAStoredProfileAtAnotherExplicitURL(t *testing.T) {
	loginEnv(t)
	url, starts := recordingLoginFake(t, "ten-presso", "Presso", "pat-new")
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: "https://elsewhere.ronja.test", TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})

	_, err := runCLI(t, t.TempDir(), "login", "--url", url, "--profile", "presso")
	if err == nil || !strings.Contains(err.Error(), "drop --url to renew it where it is") {
		t.Fatalf("login = %v, want the refusal naming the stored instance", err)
	}
	if len(*starts) != 0 {
		t.Errorf("a refused login started %d device flows", len(*starts))
	}
}

// whoami on a rejected stored token and `profile list` on an expired one send
// the person to the renewal that names the profile.
func TestRenewalHintsNameTheProfile(t *testing.T) {
	t.Run("whoami", func(t *testing.T) {
		loginEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		writeProfiles(t, "presso", testProfile{Name: "presso", URL: srv.URL, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-dead"})

		_, err := runCLI(t, t.TempDir(), "whoami")
		if err == nil || !strings.Contains(err.Error(), "run `ronja login --profile presso` again") {
			t.Fatalf("whoami = %v, want the hint to name profile presso", err)
		}
	})
	t.Run("profile list", func(t *testing.T) {
		loginEnv(t)
		writeProfiles(t, "presso", testProfile{Name: "presso", URL: "https://app.ronja.test", TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})
		if err := config.Update(func(f *config.File) error {
			f.Profiles["presso"].TokenExpiry = "2020-01-01T00:00:00Z"
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		out, err := runCLI(t, t.TempDir(), "profile", "list")
		if err != nil {
			t.Fatalf("profile list: %v", err)
		}
		if !strings.Contains(out, "(token expired — run `ronja login --profile presso`)") {
			t.Errorf("profile list lacks the named renewal hint:\n%s", out)
		}
	})
}

// $RONJA_URL chooses an instance the way --url does, so the refusal names the
// one the person actually set.
func TestLoginRefusesAStoredProfileAtAnotherEnvironmentURL(t *testing.T) {
	loginEnv(t)
	url, starts := recordingLoginFake(t, "ten-presso", "Presso", "pat-new")
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: "https://elsewhere.ronja.test", TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})
	t.Setenv(config.EnvURL, url)

	_, err := runCLI(t, t.TempDir(), "login", "--profile", "presso")
	if err == nil || !strings.Contains(err.Error(), "unset $"+config.EnvURL+" to renew it where it is") {
		t.Fatalf("login = %v, want the refusal to name $%s", err, config.EnvURL)
	}
	if len(*starts) != 0 {
		t.Errorf("a refused login started %d device flows", len(*starts))
	}
}

// An expired code is retried with the command that started it, whatever its
// flags, so a renewal keeps its guard.
func TestLoginExpiryAsksForTheSameCommand(t *testing.T) {
	loginEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v2/authentication/cli/device":
			writeJSON(w, api.DeviceStart{DeviceCode: "dev", UserCode: "ABCD-EFGH", ExpiresIn: 600,
				VerificationURI: "http://x/cli-auth", VerificationURIComplete: "http://x/cli-auth?code=ABCD-EFGH"})
		case "POST /api/v2/authentication/cli/token":
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": api.ErrExpiredToken})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: srv.URL, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})

	for _, args := range [][]string{
		{"login", "--profile", "presso"},
		{"login", "--url", srv.URL, "--profile", "fresh"},
		{"login", "--url", srv.URL},
	} {
		_, err := runCLI(t, t.TempDir(), args...)
		if err == nil || err.Error() != "the code expired — run the same command again" {
			t.Errorf("%v = %v, want the expiry to ask for the same command", args, err)
		}
	}
}

// --with-token renewing a stored profile verifies the token at that profile's
// instance, as the browser path does, when no --url names another.
func TestLoginWithTokenRenewsAStoredProfileAtItsOwnInstance(t *testing.T) {
	loginEnv(t)
	url := loginFake(t, "ten-presso", "Presso", "unused")
	writeProfiles(t, "presso", testProfile{Name: "presso", URL: url, TenantID: "ten-presso", TenantName: "Presso", Token: "pat-old"})
	withStdin(t, "pat-pasted\n")

	if _, err := runCLI(t, t.TempDir(), "login", "--profile", "presso", "--with-token"); err != nil {
		t.Fatalf("login: %v", err)
	}
	f := loadProfiles(t)
	if p := f.Profiles["presso"]; len(f.Profiles) != 1 || p == nil || p.Token != "pat-pasted" || p.URL != url {
		t.Fatalf("profiles %v; want presso alone, at %s, with the pasted token", f.Names(), url)
	}
}
