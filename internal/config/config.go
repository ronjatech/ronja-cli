// Package config owns the CLI's on-disk credential store: one PROFILE per
// (instance URL, organization), because a token is bound to one organization.
// A profile's name is a human handle; its identity is (URL, TenantID). See
// cli/README.md → "Profiles".
//
// A plain 0600 file is deliberate over an OS keychain: agents, CI jobs and
// containers have no keychain or block on its unlock prompt.
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

// EnvURL and EnvToken let an agent or CI job authenticate without a login or
// anything on disk; EnvProfile selects a stored profile by name.
const (
	EnvURL     = "RONJA_URL"
	EnvToken   = "RONJA_TOKEN"
	EnvProfile = "RONJA_PROFILE"
	EnvConfig  = "RONJA_CONFIG_DIR"
)

// DefaultURL is the hosted instance, used when nothing says otherwise. Not a
// local backend: almost nobody running the CLI has one, and "connection
// refused" says nothing about what to do.
const DefaultURL = "https://cloud.ronja.tech"

// FileName is the credential store's basename.
const FileName = "config.json"

// Profile is one authenticated (instance, organization) pair.
type Profile struct {
	// URL and TenantID are the identity a login matches on, so two
	// organizations can coexist on one instance.
	URL      string `json:"url"`
	TenantID string `json:"tenantID,omitempty"`
	Token    string `json:"token"`
	// The rest is a login-time snapshot that makes the file legible; it is never
	// read back in place of asking the server (`whoami` always calls /me), since
	// a role can change or a token be revoked unnoticed. TokenExpiry is the one
	// exception.
	TenantName string `json:"tenantName,omitempty"`
	UserID     string `json:"userID,omitempty"`
	UserEmail  string `json:"userEmail,omitempty"`
	TokenID    string `json:"tokenID,omitempty"`
	TokenName  string `json:"tokenName,omitempty"`
	// TokenExpiry is RFC 3339, kept because the server reports it only once, at
	// login. Empty for a --with-token credential, whose lifetime is unknown.
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
	// FromEnv reports that the token came from RONJA_TOKEN rather than the file.
	FromEnv bool
	// Profile is the selected profile's name, or empty. It can be set alongside
	// FromEnv: the profile then chose the URL but not the credential.
	Profile string
	Entry   *Profile
	// TenantID is the organization the credential reaches, and is deliberately
	// empty whenever FromEnv is true: an environment token and the selected
	// profile are independent, and trusting the profile's tenant would let
	// `wf push` send one organization's workflow id under another's token.
	// Callers needing it must ask the server (ensureTenant).
	TenantID string
	// TenantName is the server's name for that organization, set only by
	// ensureTenant's /me call; a stored profile carries it on Entry.TenantName.
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
	// The path is kept (an instance may live under a subpath), minus a pasted
	// trailing /api or /api/v2 that would otherwise double into ".../api/api/v2/…".
	path := strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{"/api/v2", "/api"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	// Fold the host's case so two spellings are one store key; the path is
	// case-sensitive and left alone.
	host := strings.ToLower(u.Host)
	return strings.TrimRight(u.Scheme+"://"+host+path, "/"), nil
}

// isLoopbackAuthority reports whether a scheme-less URL names this machine, so
// http is the right guess. The host match is exact: a prefix test would accept
// "localhost.evil.com" and send it a bearer token in cleartext.
func isLoopbackAuthority(raw string) bool {
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	host := raw
	// A bare "::1" fails to split and is already the host.
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// Dir is where the credential file lives; RONJA_CONFIG_DIR overrides it.
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

// Load reads the credential store; a missing file is an empty store.
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

// Save writes the credential store 0600, atomically (temp file + rename), so a
// crash cannot leave a truncated file that loses every token.
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
	// Without the fsync, a power loss after the rename can leave a zero-length
	// config.json: the rename orders metadata, not content.
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
	// Flush the directory for the rename's durability. Best-effort: Windows
	// refuses to sync a directory, and the content is already on disk.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Update runs a read-modify-write of the store under the lock; an error from fn
// aborts the write. fn gets the whole File because choosing a free name and
// taking it must be one indivisible step.
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

// ErrNoProfile reports that the named profile does not exist.
var ErrNoProfile = errors.New("no such profile")

// AmbiguousURLError is returned when a URL alone does not pick out one profile;
// it carries the candidates so the caller can list them.
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
// invocation. RONJA_TOKEN, when set, always supplies the token.
//
// An explicitly named URL is a FILTER, never a fallback: a URL with no profile
// resolves to no token, never the current profile's, which would send that
// credential to a host the user named. Two profiles on one URL differ by
// organization, so ambiguity is refused rather than picked. A typed flag beats
// an exported variable; a URL/profile conflict within one layer is an error.
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

	// Drop the weaker of two conflicting layers before either is used.
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
			if names := f.Names(); len(names) > 0 {
				return nil, fmt.Errorf("%w %q — stored profiles are: %s",
					ErrNoProfile, name, strings.Join(names, ", "))
			}
			return nil, fmt.Errorf("%w %q — there are none stored yet, run `ronja login`",
				ErrNoProfile, name)
		}
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
			// Several match, but one is current: that counts as having chosen.
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
		// See Resolved.TenantID.
		res.TenantID = ""
	}
	return res, nil
}

// NamesOn lists the profiles on one normalized URL, sorted so every selection
// over it is deterministic.
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

// FindIdentity locates the profile for one (URL, organization) pair. An empty
// tenantID is an identity of its own (no organization yet), not a wildcard.
func (f *File) FindIdentity(url, tenantID string) (string, *Profile) {
	for _, name := range f.Names() {
		if p := f.Profiles[name]; p != nil && p.URL == url && p.TenantID == tenantID {
			return name, p
		}
	}
	return "", nil
}

// ClaimProfile finds or creates the profile for one identity, returning its
// name, the entry to fill in, and whether it was created. It must run inside
// Update. Only a created profile's name is provisional; an existing identity
// keeps its name. A `preferred` name held by a different identity is not
// taken: the credential lands under a derived name, since overwriting would
// evict a live token and refusing would discard a freshly minted one.
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

// Remove forgets one profile, re-pointing Current if it was the one removed,
// and reports whether it existed.
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

// pickCurrent chooses the fallback current profile, deterministically: a
// random pick after `logout` could point the next command at production
// instead of a local backend. The most recent login wins (LoggedIn is RFC 3339
// UTC, so a string compare is a time compare).
func (f *File) pickCurrent() string {
	best, bestLogin := "", ""
	// Sorted Names() plus a strict > make a tie go to the alphabetically first
	// name; ranging the map or relaxing to >= reintroduces the coin flip.
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
