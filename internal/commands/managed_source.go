package commands

import (
	"path/filepath"
	"strings"
	"sync"
)

// A claim's SOURCE LABEL says where the folder that manages an address lives:
// the normalized git remote of the repository, `//`, and the folder's path
// inside it — `github.com/acme/data//pipelines/sales`. The server stores it for
// display (and the web app's "Managed in git" marker); this CLI reads one half of it
// back for one decision: whether a claim was made by THIS repository or by a
// copy of it (a fork, a template) that targets the same feature. See
// foreignClaim.
//
// ⚠️ Only the REMOTE half decides. A folder moved inside its repository keeps
// its id (that is what the id is for), so its path changes while it is
// still the same folder; reading the path as identity would refuse every
// folder move as a foreign repository. The path is relabelled quietly on the
// next anchor write instead, as is a label that was empty (a claim made from
// outside git) — see putManagedAnchor. A claim with no anchor change pending
// keeps its old label until one is.

// sourceLabelOf answers a folder root's source label. A var so the tests can
// stand a folder in a repository without a git binary.
var sourceLabelOf = gitSourceLabel

// localSourceSuffix ends the label of a folder with no git remote: the
// repository directory's name and the folder's path in it, marked as local —
// `roastery/apps/dashboard (local, no git remote)` — so a colleague reading
// "managed in git (…)" can tell which checkout it was, where a bare folder id
// said nothing. It names no REPOSITORY: sourceRemote reads no remote half in
// it, so it is never foreign to anything, and a git label is never replaced by
// one (relabelTo).
const localSourceSuffix = " (local, no git remote)"

// gitSourceLabel asks git, once per repository per process.
func gitSourceLabel(root string) string {
	// The path git compares its top level against: symlinks resolved, as git
	// resolves them (a macOS temporary directory is under one).
	root = realPath(root)
	top := gitOutput(root, "rev-parse", "--show-toplevel")
	if top == "" {
		return filepath.Base(root) + localSourceSuffix
	}
	remote := normalizeGitRemote(cachedRemote(top))
	if remote == "" {
		label := filepath.Base(top)
		if rel, err := filepath.Rel(top, root); err == nil && rel != "." {
			label += "/" + filepath.ToSlash(rel)
		}
		return label + localSourceSuffix
	}
	rel, err := filepath.Rel(top, root)
	if err != nil {
		// A root git says is outside its own top level: the remote alone still
		// names the repository, which is the half that decides anything.
		return remote
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return remote
	}
	return remote + "//" + rel
}

var (
	remoteMu    sync.Mutex
	remoteCache = map[string]string{}
)

func cachedRemote(top string) string {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if remote, ok := remoteCache[top]; ok {
		return remote
	}
	remote := gitOutput(top, "config", "--get", "remote.origin.url")
	remoteCache[top] = remote
	return remote
}

// normalizeGitRemote reduces the spellings of one remote to one string: no
// scheme, no credentials, no port, no `.git`, the scp form
// (`git@github.com:acme/data`) read as the URL it abbreviates, a host's known
// aliases folded onto it, and the whole thing LOWERCASED. Two clones of one
// repository over HTTPS and over SSH, or spelled `Acme/Data` by a CI checkout
// and `acme/data` by a laptop, are the same source.
//
// Case-folding the PATH is a choice: GitHub, GitLab, Bitbucket and Azure DevOps
// resolve repository paths case-insensitively, and a label that told
// `Acme/Data` from `acme/data` made CI and a laptop refuse each other's claims
// as foreign (and take them from each other with --adopt-foreign, back and
// forth). A self-hosted server with two repositories differing only in case
// would read as one source — the foreign-claim check is a slip guard, and that
// is the cheaper miss.
func normalizeGitRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
		r = stripUserinfo(r)
	} else if at := strings.Index(r, "@"); at >= 0 && strings.Contains(r[at:], ":") {
		// scp-like: user@host:path
		r = r[at+1:]
		r = strings.Replace(r, ":", "/", 1)
	} else {
		r = stripUserinfo(r)
	}
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	host, path, _ := strings.Cut(r, "/")
	// A port is part of where a repository lives only for the transport; the
	// scp form never has one, so dropping it keeps the two forms equal.
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	host, path = canonicalGitHost(strings.ToLower(host), path)
	path = strings.ToLower(path)
	if path == "" {
		return host
	}
	return host + "/" + path
}

// stripUserinfo drops `user:password@` from a URL's authority. The userinfo
// ends at the LAST `@` before the host; a password may hold an unencoded `/`
// (`u:a/b@host/r`), which git cannot use but a label must still never carry —
// so an `@` after the first `/` is userinfo too when the text before that `/`
// is `user:password` rather than `host:port`.
func stripUserinfo(r string) string {
	at := strings.LastIndex(r, "@")
	if at < 0 {
		return r
	}
	slash := strings.Index(r, "/")
	if slash < 0 || at < slash {
		return r[at+1:]
	}
	if _, after, ok := strings.Cut(r[:slash], ":"); ok && strings.Trim(after, "0123456789") != "" {
		return r[at+1:]
	}
	return r
}

// canonicalGitHost folds the hosts that serve one repository under several
// names onto one: GitHub's and GitLab's SSH-over-443 hosts, and Azure DevOps'
// HTTPS (`dev.azure.com/o/p/_git/r`), SSH (`ssh.dev.azure.com:v3/o/p/r`) and
// legacy `o.visualstudio.com` forms, all read as `dev.azure.com/o/p/r`.
func canonicalGitHost(host, path string) (string, string) {
	switch host {
	case "ssh.github.com", "www.github.com":
		return "github.com", path
	case "altssh.gitlab.com", "www.gitlab.com":
		return "gitlab.com", path
	case "ssh.dev.azure.com", "vs-ssh.visualstudio.com":
		return "dev.azure.com", strings.TrimPrefix(path, "v3/")
	case "dev.azure.com":
		return host, strings.Replace(path, "/_git/", "/", 1)
	}
	if org, ok := strings.CutSuffix(host, ".visualstudio.com"); ok && org != "" {
		return "dev.azure.com", org + "/" + strings.Replace(path, "/_git/", "/", 1)
	}
	return host, path
}

// sourceRemote is the repository half of a source label.
func sourceRemote(label string) string {
	if isLocalSource(label) {
		return ""
	}
	remote, _, _ := strings.Cut(label, "//")
	return remote
}

// isLocalSource reports a label recorded from a checkout with no git remote.
func isLocalSource(label string) bool { return strings.HasSuffix(label, localSourceSuffix) }

// relabelTo reports whether a claim recorded under label should be relabelled
// to ours when its anchor is written: when they differ and are not two
// repositories (foreignClaim) — and never from a label to a LOCAL one, so a
// deploy from a checkout with no remote leaves a repository's label as it is.
func relabelTo(label, ours string) bool {
	switch {
	case ours == "" || label == ours || foreignClaim(label, ours):
		return false
	case isLocalSource(ours):
		return label == ""
	}
	return true
}

// foreignClaim reports whether a claim's source label names ANOTHER repository
// than this folder's — a fork or a template copy that kept the folder ids and
// targets the same feature. Unknown on either side is not foreign: a folder
// outside git, or a claim made by one, cannot be told apart from this one, and
// refusing on a guess would refuse every apply run from a tarball.
//
// Both halves are normalized again: a label an earlier build recorded kept the
// path's case and the host's alias, and must still read as this repository.
func foreignClaim(claimLabel, ours string) bool {
	theirs, mine := normalizeGitRemote(sourceRemote(claimLabel)), normalizeGitRemote(sourceRemote(ours))
	return theirs != "" && mine != "" && theirs != mine
}
