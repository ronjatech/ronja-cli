package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The GIT half of `ronja <kind> pull`: the rules that keep a pull from
// overwriting work — the dirty rule, files git could not bring back, paths
// through a symbolic link — and the one way this package runs git
// (gitWithin). Split from pull.go.

// refuseUncommitted refuses a folder with uncommitted local changes, and one
// outside a git work tree.
//
// OUTSIDE GIT IS A REFUSAL TOO, and on purpose. Pull overwrites files with what
// is live; inside git, a committed file is recoverable and the refusal above
// protects everything that is not. Outside git nothing can say which edits are
// work, and nothing could bring an overwritten one back — so the pull asks for
// --force, the author's word that there is nothing to lose. The diff a pull
// prints is no substitute: it shows what was replaced, not a way back to it.
//
// The lock file and the git-ignored .ronja/ are not work: the lock is a cache
// of the server's state, which a pull rewrites from the server anyway.
func refuseUncommitted(f *folder) error {
	changed, inGit, err := folderUncommitted(f.Root)
	if err != nil {
		return fmt.Errorf("check %s for uncommitted changes: %w — commit or stash them and pull again, or pull --force to overwrite them", f.Root, err)
	}
	if !inGit {
		return &managedRefusal{Reason: pullReasonOutsideGit, Detail: fmt.Sprintf(
			"%s is not in a git work tree, so nothing can tell which of its files are committed, and a pull overwrites them with what is live. Put the folder under git and commit it, preview with `%s pull --dry-run`, or pull --force if there is nothing in it to lose",
			f.Root, f.Kind.Command)}
	}
	if len(changed) > 0 {
		return &managedRefusal{Reason: pullReasonDirty, Detail: fmt.Sprintf(
			"this folder has uncommitted changes (%s), and a pull overwrites files with what is live. Commit or stash them and pull again, preview with `%s pull --dry-run`, or pull --force to overwrite them",
			strings.Join(changed, ", "), f.Kind.Command)}
	}
	return nil
}

// folderUncommitted answers the folder's uncommitted paths (folder-relative)
// per git, and whether the folder is in a git work tree at all. A var so the
// tests can stand a folder in a clean or dirty repository without one.
var folderUncommitted = gitFolderUncommitted

func gitFolderUncommitted(root string) (changed []string, inGit bool, err error) {
	if gitOutput(root, "rev-parse", "--is-inside-work-tree") != "true" {
		return nil, false, nil
	}
	// Porcelain v1, NUL-separated, untracked files listed one by one (an
	// untracked file is uncommitted work as much as a modified one, and the
	// pull may write over it).
	entries, err := gitFolderStatus(root, "--untracked-files=all")
	if err != nil {
		return nil, true, err
	}
	for _, e := range entries {
		if e.path == wfdir.LockName || e.path == wfdir.StateDirName || strings.HasPrefix(e.path, wfdir.StateDirName+"/") {
			continue
		}
		changed = append(changed, e.path)
	}
	sort.Strings(changed)
	return changed, true, nil
}

// gitStatusEntry is one line of `git status --porcelain=v1 -z`, its path
// relative to the folder.
type gitStatusEntry struct {
	status string
	path   string
}

// gitFolderStatus is `git status --porcelain=v1 -z` scoped to the folder, its
// paths made folder-relative (git answers them relative to the repository's
// top).
func gitFolderStatus(root string, args ...string) ([]gitStatusEntry, error) {
	top := gitOutput(root, "rev-parse", "--show-toplevel")
	if top == "" {
		return nil, fmt.Errorf("git names no top-level directory for %s", root)
	}
	out, err := gitRaw(root, append(append([]string{"status", "--porcelain=v1", "-z"}, args...), "--", ".")...)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(evalPath(top), evalPath(root))
	if err != nil {
		return nil, err
	}
	prefix := ""
	if rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}
	var entries []gitStatusEntry
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		e := fields[i]
		if len(e) < 4 {
			continue
		}
		status, path := e[:2], e[3:]
		if status[0] == 'R' || status[0] == 'C' {
			// A rename names its origin in the next field.
			i++
		}
		entries = append(entries, gitStatusEntry{status: status, path: strings.TrimPrefix(path, prefix)})
	}
	return entries, nil
}

// refuseUnrecoverable refuses a pull that would overwrite or delete a file git
// could not bring back.
//
// The dirty check above protects what git sees as changed, and a committed file
// is recoverable from git whatever a pull does to it. Neither covers a file git
// IGNORES: it is never listed as uncommitted, and the folder's own enumeration
// does not read .gitignore, so a pull can delete or overwrite it — and nothing,
// not git and not the diff, brings it back. So every path the pull writes or
// deletes must be tracked by git, or absent. --force is the author's word that
// there is nothing to lose.
func refuseUnrecoverable(f *folder, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	lost, err := folderUnrecoverable(f.Root, paths)
	if err != nil {
		return fmt.Errorf("check which files git tracks in %s: %w — pull --force to write them anyway", f.Root, err)
	}
	if len(lost) == 0 {
		return nil
	}
	return &managedRefusal{Reason: pullReasonUnrecoverable, Detail: fmt.Sprintf(
		"a pull would overwrite or delete %s, which git does not track, so nothing could bring %s back. Move %s out of the folder, or track and commit %s, and pull again; preview with `%s pull --dry-run`, or pull --force if there is nothing in %s to lose",
		strings.Join(lost, ", "), agree(len(lost), "it", "them"), agree(len(lost), "it", "them"),
		agree(len(lost), "it", "them"), f.Kind.Command, agree(len(lost), "it", "them"))}
}

// refuseSymlinked refuses a pull that would write or delete a file through a
// symbolic link inside the folder: a link in any component of the path, the
// file itself included. A pull writes the live row INTO this folder, and a link
// would carry the write wherever it points — outside the folder, outside the
// repository. Not a question of lost work, so --force does not override it.
func refuseSymlinked(f *folder, paths []string) error {
	var linked []string
	for _, p := range paths {
		parts := strings.Split(p, "/")
		for i := range parts {
			at := strings.Join(parts[:i+1], "/")
			info, err := os.Lstat(filepath.Join(f.Root, filepath.FromSlash(at)))
			if err != nil {
				break // absent: written fresh from here down, through no link
			}
			if info.Mode()&os.ModeSymlink != 0 {
				linked = append(linked, fmt.Sprintf("%s (through the link %s)", p, at))
				break
			}
		}
	}
	if len(linked) == 0 {
		return nil
	}
	return &managedRefusal{Reason: pullReasonSymlinked, Detail: fmt.Sprintf(
		"a pull would write or delete %s, and a symbolic link would carry that out of this folder. Replace the link with a real directory or file and pull again; --force does not override this",
		strings.Join(linked, ", "))}
}

// folderUnrecoverable answers the paths among `paths` (folder-relative) that
// exist on disk and that git does not track, each with why ("ignored by git",
// "untracked"). Outside a git work tree it answers none: refuseUncommitted owns
// that case. A var so the tests can stand a folder in git without one.
var folderUnrecoverable = gitFolderUnrecoverable

func gitFolderUnrecoverable(root string, paths []string) ([]string, error) {
	if gitOutput(root, "rev-parse", "--is-inside-work-tree") != "true" {
		return nil, nil
	}
	// ls-files answers paths relative to the directory it runs in, which is
	// the folder.
	out, err := gitRaw(root, "ls-files", "-z", "--", ".")
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	entries, err := gitFolderStatus(root, "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var ignored []string
	for _, e := range entries {
		if e.status == "!!" {
			ignored = append(ignored, e.path)
		}
	}
	isIgnored := func(p string) bool {
		for _, ig := range ignored {
			if p == ig || (strings.HasSuffix(ig, "/") && strings.HasPrefix(p, ig)) {
				return true
			}
		}
		return false
	}
	var lost []string
	for _, p := range paths {
		if tracked[p] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); errors.Is(err, os.ErrNotExist) {
			continue
		}
		why := "untracked"
		if isIgnored(p) {
			why = "ignored by git"
		}
		lost = append(lost, fmt.Sprintf("%s (%s)", p, why))
	}
	return lost, nil
}

// gitRaw runs one read-only git query and returns its output verbatim — unlike
// gitOutput, whose "" cannot tell a clean answer from a failure.
func gitRaw(dir string, args ...string) (string, error) {
	return gitWithin(30*time.Second, dir, args...)
}

// gitWithin is the one git invocation both helpers share: `git -C dir args…`,
// bounded by timeout, the output verbatim, and stderr folded into the error.
func gitWithin(timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// evalPath resolves symlinks (a macOS temp dir is one), falling back to the
// path as given.
func evalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// gitOutput runs one read-only git query, "" on any failure — no git binary, no
// repository, no such config key. Bounded, because a label is never worth a
// hung command.
func gitOutput(dir string, args ...string) string {
	out, err := gitWithin(5*time.Second, dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
