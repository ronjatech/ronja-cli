package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The copy's caps, as the server enforces them (backend/engine/echangelist,
// MaxArchiveBytes, MaxArchiveFiles, MaxUnpackedBytes): refused here, before
// the upload, with the number that was too large.
const (
	proposeMaxArchiveBytes  = 32 << 20
	proposeMaxArchiveFiles  = 5000
	proposeMaxUnpackedBytes = 256 << 20
)

// packProposal is the copy: a gzipped tar of base holding, for each folder,
// exactly the files its push reads (proposeFolderPaths) beside ronja.json and
// ronja.lock.json, and each proposed migrations folder's .sql files. The
// readers skip every dot-name, so neither .ronja/ nor .env nor .git is in it.
//
// It also answers, by folder root, the sha256 of each of a folder's files as
// the copy holds them, without the manifest and the lock.
func packProposal(base string, folders []discoveredFolder, migrationDirs []string) ([]byte, map[string]map[string]string, error) {
	files := map[string]string{}
	// copied hashes the bytes as they go into the archive, not the working
	// tree: the record has to hold what the deploy writes, and the folder may
	// change after propose reads it.
	type folderFile struct{ root, path string }
	owned := map[string]folderFile{}
	copied := map[string]map[string]string{}
	add := func(abs string) error {
		info, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nothing was proposed: %s is not a regular file (a link?), and a change list carries only files — replace it with the file itself", abs)
		}
		rel, err := filepath.Rel(base, abs)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = abs
		return nil
	}
	for _, folder := range folders {
		paths, err := proposeFolderPaths(folder)
		if err != nil {
			return nil, nil, err
		}
		copied[folder.Root] = map[string]string{}
		for _, p := range paths {
			abs := filepath.Join(folder.Root, filepath.FromSlash(p))
			if err := add(abs); err != nil {
				return nil, nil, err
			}
			if p == wfdir.ManifestName || p == wfdir.LockName {
				continue
			}
			if rel, err := filepath.Rel(base, abs); err == nil {
				owned[filepath.ToSlash(rel)] = folderFile{folder.Root, p}
			}
		}
	}
	for _, dir := range migrationDirs {
		entries, err := os.ReadDir(filepath.Join(dir, dbdir.MigrationsDir))
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
				continue
			}
			if err := add(filepath.Join(dir, dbdir.MigrationsDir, e.Name())); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(files) > proposeMaxArchiveFiles {
		return nil, nil, fmt.Errorf("nothing was proposed: the copy would hold %d files, more than a change list takes (%d) — propose a smaller directory", len(files), proposeMaxArchiveFiles)
	}
	if err := refuseCredentialDir(base, files); err != nil {
		return nil, nil, err
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	sizes := make(map[string]int64, len(names))
	var total int64
	for _, name := range names {
		info, err := os.Stat(files[name])
		if err != nil {
			return nil, nil, err
		}
		sizes[name] = info.Size()
		total += info.Size()
		if total > proposeMaxUnpackedBytes {
			return nil, nil, fmt.Errorf("nothing was proposed: the copy would be larger than a change list takes (%d MB) — propose a smaller directory", proposeMaxUnpackedBytes>>20)
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		body, err := os.ReadFile(files[name])
		if err != nil {
			return nil, nil, err
		}
		if int64(len(body)) != sizes[name] {
			return nil, nil, fmt.Errorf("nothing was proposed: %s changed while it was being copied — propose again", name)
		}
		if o, ok := owned[name]; ok {
			copied[o.root][o.path] = wfdir.HashString(string(body))
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, err
	}
	if buf.Len() > proposeMaxArchiveBytes {
		return nil, nil, fmt.Errorf("nothing was proposed: the copy compresses to %d MB, more than a change list takes (%d MB) — propose a smaller directory", buf.Len()>>20, proposeMaxArchiveBytes>>20)
	}
	return buf.Bytes(), copied, nil
}

// proposeFolderPaths is every file of a folder its deploy reads, by the
// push's own readers (the enumeration readLocalFiles and readPipelineFiles
// read, and a pipeline's metric, checks and docs readers), with ronja.json
// and ronja.lock.json. Paths are relative to the folder.
func proposeFolderPaths(folder discoveredFolder) ([]string, error) {
	paths := []string{wfdir.ManifestName}
	if _, err := os.Lstat(filepath.Join(folder.Root, wfdir.LockName)); err == nil {
		paths = append(paths, wfdir.LockName)
	}
	enumeration, err := enumerateFolder(folder.Root, folder.Kind)
	if err != nil {
		return nil, err
	}
	for p := range enumeration.Files {
		paths = append(paths, p)
	}
	if folder.Kind.Name != wfdir.KindPipeline {
		return paths, nil
	}
	metrics, err := readMetricFiles(folder.Root)
	if err != nil {
		return nil, err
	}
	for _, m := range metrics {
		paths = append(paths, m.Path)
	}
	sql := make(map[string]string, len(enumeration.Files))
	for p := range enumeration.Files {
		sql[p] = ""
	}
	checks, err := readChecksFiles(folder.Root, sql)
	if err != nil {
		return nil, err
	}
	for _, c := range checks {
		paths = append(paths, c.Path)
	}
	sidecars, err := readTableDocsSidecars(folder.Root)
	if err != nil {
		return nil, err
	}
	for _, d := range sidecars {
		paths = append(paths, d.Path)
	}
	return paths, nil
}

// refuseCredentialDir refuses a copy that would hold the CLI's own credential
// store: RONJA_CONFIG_DIR can point anywhere, a folder of the tree included.
func refuseCredentialDir(base string, files map[string]string) error {
	dir, err := config.Dir()
	if err != nil {
		return nil
	}
	creds, tree := realPath(dir), realPath(base)
	if !pathWithin(tree, creds) {
		return nil
	}
	for name := range files {
		if pathWithin(creds, filepath.Join(tree, filepath.FromSlash(name))) {
			return errors.New("nothing was proposed: the CLI's credentials (" + dir + ") are inside the directory being proposed, and a change list must not carry them — move them out (RONJA_CONFIG_DIR) and propose again")
		}
	}
	return nil
}
