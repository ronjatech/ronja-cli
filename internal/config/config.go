// Package config owns the CLI's on-disk credential store.
//
// The unit of storage is a PROFILE: a name over one (instance URL,
// organization) pair and the token that reaches it. Two facts force that shape
// rather than the obvious "one token per host":
//
//   - A Ronja access token is HARD-BOUND to one organization. Token auth always
//     carries the tenant recorded on the token row, and the only tenant-switch
//     mechanism in the product (POST /api/v2/tenant/set) sets a browser COOKIE,
//     which a token can neither see nor use. So a person who belongs to two
//     organizations needs two tokens ON THE SAME URL — something a store keyed
//     by URL alone cannot hold without silently evicting one of them.
//   - Nothing else names an instance, so selecting one means retyping
//     `--url https://…` forever, and "the default" drifts to whatever was
//     logged into last.
//
// A profile's NAME is a handle for humans and is never load-bearing; its
// IDENTITY is (URL, TenantID), which is what every lookup matches on.
//
// A plain 0600 file is deliberate over an OS keychain: the primary consumers
// are agents, CI jobs and containers, where a keychain is either absent or
// blocks on an interactive unlock prompt. Anything more sensitive should use
// RONJA_TOKEN, which never touches disk.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EnvURL and EnvToken override the stored configuration entirely. They exist so
// an agent or CI job can authenticate without a login step and without writing
// anything to disk. EnvProfile selects a stored profile by name, for a shell
// that wants to pin one without passing --profile to every command.
const (
	EnvURL     = "RONJA_URL"
	EnvToken   = "RONJA_TOKEN"
	EnvProfile = "RONJA_PROFILE"
	EnvConfig  = "RONJA_CONFIG_DIR"
)

// DefaultURL is where the CLI goes when nothing says otherwise — the hosted
// Ronja instance.
//
// Production rather than a local backend, because the overwhelming majority of
// people running this have no local backend at all, and a default they cannot
// reach fails as "connection refused" rather than as anything that suggests
// what to do. Working on Ronja itself is the rare case, and it is the one with
// an obvious remedy: `--url localhost:8080` once, after which that login is a
// profile like any other.
const DefaultURL = "https://cloud.ronja.tech"

// FileName is the credential store's basename. Named for what it holds rather
// than how it is keyed — an earlier "hosts.json" stopped being true the moment
// one host could carry several logins.
const FileName = "config.json"

// Profile is one authenticated (instance, organization) pair.
type Profile struct {
	// URL and TenantID are the IDENTITY. A login matches an existing profile on
	// both, which is what lets two organizations coexist on one instance.
	URL      string `json:"url"`
	TenantID string `json:"tenantID,omitempty"`
	Token    string `json:"token"`
	// The rest is a snapshot captured at login, kept so the credential file is
	// legible to a human (or an agent) opening it: "which token is this, whose
	// is it, and when did it get here". It is deliberately NOT read back as a
	// substitute for asking the server — `whoami` always round-trips to
	// /me, because a role can change and a token can be revoked without
	// anything on this machine noticing.
	//
	// TokenExpiry below is the one exception, and only because the server
	// states it exactly once.
	TenantName string `json:"tenantName,omitempty"`
	UserID     string `json:"userID,omitempty"`
	UserEmail  string `json:"userEmail,omitempty"`
	TokenID    string `json:"tokenID,omitempty"`
	TokenName  string `json:"tokenName,omitempty"`
	// TokenExpiry is RFC 3339, recorded at login. CLI-minted tokens are
	// time-bounded, and the expiry is reported exactly once by the server — so
	// if it is not kept here, nothing can tell the user their credential is
	// about to stop working. Empty for a token supplied with --with-token,
	// whose lifetime we were never told.
	TokenExpiry string `json:"tokenExpiresAt,omitempty"`
	LoggedIn    string `json:"loggedInAt,omitempty"`
}

// File is the whole credential store.
type File struct {
	// Current is the profile used when nothing on the command line or in the
	// environment selects one.
	Current  string              `json:"currentProfile,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
}

// Resolved is the effective configuration for one command invocation, after
// flags, environment and file have been layered.
type Resolved struct {
	URL   string
	Token string
	// FromEnv reports that the token came from RONJA_TOKEN rather than the
	// credential file. `logout` uses this to explain why clearing the file
	// will not actually log the caller out.
	FromEnv bool
	// Profile is the name of the selected profile, or "" when none was. It can
	// be set even when FromEnv is true: the profile still decided which URL to
	// talk to, it just did not supply the credential.
	Profile string
	Entry   *Profile
	// TenantID is the organization the credential reaches — and is deliberately
	// EMPTY whenever FromEnv is true, however confidently the selected profile
	// names one.
	//
	// An environment token and a profile are independent: `eval "$(ronja env)"`
	// sets RONJA_TOKEN and RONJA_URL, and a later `ronja profile use other-org`
	// in that same shell leaves a profile whose TenantID describes a DIFFERENT
	// organization from the token in play. Trusting it there does not fail
	// cleanly — `wf push` would match one organization's binding and send its
	// workflow id under the other's token. Callers that need the tenant under
	// an environment token must ask the server (see commands.resolveInstance).
	TenantID string
	// TenantName is what the SERVER calls that organization, filled in by the
	// one /me call ensureTenant already makes. Empty otherwise, including for
	// every stored profile — those carry the name on Entry.TenantName, and this
	// field exists precisely for the credential that has no entry to carry one.
	TenantName string
}

// NormalizeURL canonicalises a base URL so the same instance always maps to the
// same credential-store key. It tolerates a bare host ("localhost:8080") by
// assuming http for loopback and https otherwise, which is what people type.
func NormalizeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(trimmed, "://") {
		if isLoopbackAuthority(trimmed) {
			trimmed = "http://" + trimmed
		} else {
			trimmed = "https://" + trimmed
		}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid URL %q: no host", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	// Queries and fragments are never part of an instance's identity. The PATH
	// is kept, because an instance can legitimately be served under a subpath
	// — but strip a trailing /api or /api/v2, which is someone pasting an API
	// URL rather than an instance root. Left in place, those produce silently
	// doubled paths (".../api/api/v2/...") that 404 with no clue why.
	path := strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{"/api/v2", "/api"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	// Hostnames are case-insensitive, so "https://APP.ronja.tech" and
	// "https://app.ronja.tech" are the same instance. Without folding the case
	// they would be two credential-store keys, and a login under one spelling
	// would look like "not signed in" under the other. The PATH is left alone —
	// that half genuinely is case-sensitive.
	host := strings.ToLower(u.Host)
	return strings.TrimRight(u.Scheme+"://"+host+path, "/"), nil
}

// isLoopbackAuthority reports whether a scheme-less URL names this machine, in
// which case http rather than https is the right guess.
//
// The match is EXACT on the host. A prefix test ("does it start with
// localhost") also accepts "localhost.evil.com", which is a perfectly
// registerable domain — and would then be contacted in cleartext, with a bearer
// token, because of a naming trick.
func isLoopbackAuthority(raw string) bool {
	// Everything from the first path/query/fragment separator is not the host.
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	host := raw
	// Tolerates "localhost:8080" and "[::1]:8080"; a bare "::1" has too many
	// colons to split, and is already the host.
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// Dir is where the credential file lives. RONJA_CONFIG_DIR overrides it, which
// is what lets tests and sandboxed agents stay off the real profile.
func Dir() (string, error) {
	if custom := os.Getenv(EnvConfig); custom != "" {
		return custom, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(base, "ronja"), nil
}

// Path is the credential file itself.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the credential store. A missing file is not an error — it is a
// first run.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &File{Profiles: map[string]*Profile{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete it to start over)", path, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	return &f, nil
}

// Save writes the credential store with owner-only permissions.
//
// The write is atomic (temp file + rename) so an interrupted save cannot leave
// a truncated file that the next run refuses to parse — losing a token to a
// crash would be a bad way to learn about non-atomic writes.
func Save(f *File) error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	// Set the mode before writing so the token is never briefly world-readable.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	// Without this, rename-for-atomicity only orders the metadata: the file
	// name flips over instantly while its CONTENT is still in the page cache,
	// so a power loss between the two leaves a zero-length config.json — the
	// truncated file the temp-and-rename dance was supposed to rule out.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	// And the rename itself is a directory operation, so the directory needs
	// flushing too. Best-effort: opening a directory for sync is not portable
	// (Windows refuses), and a failure here costs durability on a crash, not
	// correctness — the data is already safely on disk by this point.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Update runs a read-modify-write of the store under the lock, saving whatever
// fn leaves behind. fn returning an error aborts the write entirely.
//
// The whole File is handed over rather than a narrow mutator per operation
// because the interesting operations are not single writes: allocating a unique
// profile name and storing under it is one indivisible decision, and so is
// "rename this profile unless someone took the name while we were away".
func Update(fn func(f *File) error) error {
	return withLock(func() error {
		f, err := Load()
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
		return Save(f)
	})
}

// ErrNoProfile reports that the named profile does not exist. Callers match on
// it to add "here is what does exist" to the message.
var ErrNoProfile = errors.New("no such profile")

// AmbiguousURLError is returned when a URL alone does not pick out one profile.
//
// A type rather than a formatted string because the CANDIDATES are the useful
// part: the caller prints them so the human can pick, instead of being told
// only that their command was unclear.
type AmbiguousURLError struct {
	URL        string
	Candidates []string
}

func (e *AmbiguousURLError) Error() string {
	return fmt.Sprintf("%s has %d profiles (%s) — pass --profile to choose one",
		e.URL, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// ProfileURLMismatchError is returned when an explicitly named profile and an
// explicitly named URL disagree.
type ProfileURLMismatchError struct {
	Profile      string
	ProfileURL   string
	RequestedURL string
}

func (e *ProfileURLMismatchError) Error() string {
	return fmt.Sprintf("profile %q is on %s, not %s — pass one or the other, not both",
		e.Profile, e.ProfileURL, e.RequestedURL)
}

// Resolve layers flags, environment and file into the configuration for one
// invocation.
//
// The token still comes from RONJA_TOKEN whenever it is set, ahead of anything
// stored: an agent or CI job that sets it must never accidentally act as
// whoever last ran `ronja login` on the machine.
//
// Profile selection obeys ONE invariant, which is more important than the order
// of the rules below:
//
//	An explicitly named URL is a FILTER, never a fallback.
//
// If --url or RONJA_URL names an instance with no profile, the answer is "not
// signed in TO THAT INSTANCE" — never "fall back to the current profile". The
// alternative sends whatever token happens to be current to a host the user
// deliberately named, which is exactly how a production credential ends up on a
// staging box. The old URL-keyed store got this right by accident (a map miss
// yielded no token); here it has to be deliberate.
//
// Ambiguity is likewise never resolved by picking: two profiles on one URL
// differ by ORGANIZATION, and the gap between them is the gap between reading
// test data and writing to a customer's.
//
// When a URL and a profile are both given and disagree, the more DELIBERATE one
// wins — a typed flag over an exported variable — and only a conflict WITHIN one
// layer is an error. Erroring on `--profile acme` in a shell that happens to
// export RONJA_URL would punish the user for being specific; erroring on
// `--profile acme --url elsewhere` is refusing to guess which half of a typed
// command was meant.
func Resolve(flagURL, flagProfile string) (*Resolved, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}

	rawURL, urlFromFlag := flagURL, flagURL != ""
	if rawURL == "" {
		rawURL = strings.TrimSpace(os.Getenv(EnvURL))
	}
	name, nameFromFlag := flagProfile, flagProfile != ""
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvProfile))
	}

	// Drop the weaker of two conflicting layers before either is used, so the
	// checks below only ever see a coherent request.
	if rawURL != "" && name != "" && urlFromFlag != nameFromFlag {
		if nameFromFlag {
			rawURL = ""
		} else {
			name = ""
		}
	}

	wantURL := ""
	if rawURL != "" {
		if wantURL, err = NormalizeURL(rawURL); err != nil {
			return nil, err
		}
	}

	var chosen *Profile
	chosenName := ""
	switch {
	case name != "":
		p, ok := f.Profiles[name]
		if !ok {
			// Name what DOES exist. "No such profile" alone leaves the reader to
			// run a second command to find out what they should have typed.
			if names := f.Names(); len(names) > 0 {
				return nil, fmt.Errorf("%w %q — stored profiles are: %s",
					ErrNoProfile, name, strings.Join(names, ", "))
			}
			return nil, fmt.Errorf("%w %q — there are none stored yet, run `ronja login`",
				ErrNoProfile, name)
		}
		// Both named and disagreeing: refuse rather than silently ranking one
		// above the other. Either answer would be a guess at which half of the
		// command the user meant.
		if wantURL != "" && wantURL != p.URL {
			return nil, &ProfileURLMismatchError{Profile: name, ProfileURL: p.URL, RequestedURL: wantURL}
		}
		chosen, chosenName = p, name

	case wantURL != "":
		candidates := f.NamesOn(wantURL)
		switch {
		case len(candidates) == 0:
			// Deliberately nothing: unauthenticated on the named URL.
		case len(candidates) == 1:
			chosenName = candidates[0]
			chosen = f.Profiles[chosenName]
		case contains(candidates, f.Current):
			// Several match, but one of them is already the current profile —
			// that is a real signal of intent, not a coin flip.
			chosenName = f.Current
			chosen = f.Profiles[chosenName]
		default:
			return nil, &AmbiguousURLError{URL: wantURL, Candidates: candidates}
		}

	case f.Current != "":
		if p, ok := f.Profiles[f.Current]; ok {
			chosen, chosenName = p, f.Current
		}
	}

	res := &Resolved{Profile: chosenName, Entry: chosen}
	switch {
	case chosen != nil:
		res.URL = chosen.URL
		res.Token = chosen.Token
		res.TenantID = chosen.TenantID
	case wantURL != "":
		res.URL = wantURL
	default:
		res.URL = DefaultURL
	}

	if envToken := strings.TrimSpace(os.Getenv(EnvToken)); envToken != "" {
		res.Token = envToken
		res.FromEnv = true
		// See Resolved.TenantID: the environment token and the profile are
		// independent, so the profile's organization says nothing about which
		// one this credential actually reaches.
		res.TenantID = ""
	}
	return res, nil
}

// NamesOn lists the profiles on one normalized URL, sorted. Sorted because the
// result reaches both an error message and a single-candidate selection, and Go
// randomises map iteration — an un-sorted version would pick a different
// profile on different runs.
func (f *File) NamesOn(url string) []string {
	var out []string
	for name, p := range f.Profiles {
		if p != nil && p.URL == url {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Names lists every profile, sorted.
func (f *File) Names() []string {
	out := make([]string, 0, len(f.Profiles))
	for name := range f.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FindIdentity locates the profile for one (URL, organization) pair — the
// IDENTITY match a login uses, so that re-authenticating updates the profile
// you already have instead of minting a second one beside it.
//
// An empty tenantID is a legitimate identity of its own (a login by someone who
// belongs to no organization yet), not a wildcard: it matches only other
// org-less profiles on that URL.
func (f *File) FindIdentity(url, tenantID string) (string, *Profile) {
	for _, name := range f.Names() {
		if p := f.Profiles[name]; p != nil && p.URL == url && p.TenantID == tenantID {
			return name, p
		}
	}
	return "", nil
}

// ClaimProfile finds or creates the profile for one identity, returning its
// name, the entry to fill in, and whether it was newly created. MUST be called
// inside Update, because deciding the name and taking it is one indivisible
// step (see Allocate).
//
// `created` is what tells a caller whether the name is still PROVISIONAL and
// may be improved once the organization's name is known. Renaming without that
// check would rename the profile an existing user deliberately chose, every
// time they re-authenticate.
//
// Three behaviours, each load-bearing:
//
//   - An existing identity KEEPS ITS NAME. Re-authenticating updates the
//     profile you already have; it never mints a second one beside it, and
//     never renames the one your scripts refer to.
//   - A free `preferred` name (from --profile) is used as given.
//   - A `preferred` name already held by a DIFFERENT identity is NOT taken.
//     The new credential lands under a derived name instead, and the caller
//     reports that. Overwriting would evict a live token, and refusing outright
//     would throw away one the server has already minted and shown to nobody —
//     both worse than a slightly surprising name.
func (f *File) ClaimProfile(instanceURL, tenantID, preferred string) (string, *Profile, bool) {
	if name, p := f.FindIdentity(instanceURL, tenantID); p != nil {
		return name, p, false
	}
	name := preferred
	if name == "" || f.taken(name, "") {
		name = f.Allocate(HostLabel(instanceURL), "")
	}
	p := &Profile{URL: instanceURL, TenantID: tenantID}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	f.Profiles[name] = p
	return name, p, true
}

// Remove forgets one profile, re-pointing Current if it was the one removed. It
// reports whether anything was actually stored, so `logout` can tell the
// truth rather than always claiming success.
func (f *File) Remove(name string) bool {
	if _, ok := f.Profiles[name]; !ok {
		return false
	}
	delete(f.Profiles, name)
	if f.Current == name {
		f.Current = f.pickCurrent()
	}
	return true
}

// Rename moves a profile to a new name, carrying Current with it.
func (f *File) Rename(from, to string) error {
	p, ok := f.Profiles[from]
	if !ok {
		return fmt.Errorf("%w %q", ErrNoProfile, from)
	}
	if from == to {
		return nil
	}
	if _, taken := f.Profiles[to]; taken {
		return fmt.Errorf("a profile named %q already exists", to)
	}
	delete(f.Profiles, from)
	f.Profiles[to] = p
	if f.Current == from {
		f.Current = to
	}
	return nil
}

// pickCurrent chooses the fallback current profile once the previous one is
// gone.
//
// Deterministically, which matters more than it looks: ranging over the map and
// taking the first key promotes an ARBITRARY profile, because Go randomises map
// iteration. On a laptop signed in to production and a local backend, one
// `ronja logout` would leave the next command pointed at either — and the
// difference between those two is the difference between reading test data and
// writing to a customer's organization.
//
// Most-recent login is the best guess at intent; LoggedIn is RFC 3339 UTC, so a
// string compare is a time compare. Ties, and profiles with no recorded login
// (never empty in practice, but the file is user-editable), fall back to sorted
// name order so the answer is at least stable.
func (f *File) pickCurrent() string {
	best, bestLogin := "", ""
	// Two halves make this deterministic, and BOTH are load-bearing: Names() is
	// sorted, and the comparison below is strictly greater. Together they mean a
	// tie is won by the alphabetically first name, because a later profile with
	// an equal stamp never displaces an earlier one. Ranging the map instead, or
	// relaxing this to >=, silently reintroduces the coin flip.
	for _, name := range f.Names() {
		login := ""
		if p := f.Profiles[name]; p != nil {
			login = p.LoggedIn
		}
		if best == "" || login > bestLogin {
			best, bestLogin = name, login
		}
	}
	return best
}

func contains(haystack []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
