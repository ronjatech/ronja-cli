package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// writeStreamFile copies r into path at 0600 (tenant data) via temp file and
// rename, not os.WriteFile: that keeps an existing file's mode, writes through a
// symlink, and can leave a truncated file that looks complete. Streamed, since
// `ronja api --out` downloads large binaries. No fsync, unlike config.Save: a
// lost response is one command from being re-fetched; a lost token is not.
func writeStreamFile(path string, r io.Reader) (int64, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	// Before the write, so the data is never briefly readable by anyone else.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	return n, nil
}

// writeResultFile is writeStreamFile for a body already in memory.
func writeResultFile(path, body string) error {
	_, err := writeStreamFile(path, strings.NewReader(body))
	return err
}
