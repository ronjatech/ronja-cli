package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Writing a response to a file, for `--out` on both `query` and `api`.
//
// 0600, not 0644: this is tenant data the caller was authorised to read and
// nobody else on the machine was. os.WriteFile cannot promise that, and the
// three ways it fails are all realistic here:
//
//   - Its mode argument applies only when the file is CREATED. Writing over a
//     pre-existing rows.csv left at 0644 by an earlier tool leaves it 0644, and
//     the data inside it world-readable.
//   - It follows a SYMLINK, so the bytes land wherever that points, under
//     whatever mode the target already has.
//   - A short write (a full disk, an interrupt) leaves a TRUNCATED file that is
//     indistinguishable from a complete result — the worst outcome available,
//     because the caller goes on to use a prefix as if it were the whole thing.
//
// Temp file plus rename closes all three: the mode is set on a file this process
// just created, the rename replaces a symlink rather than writing through it,
// and the result appears whole or not at all. Same mechanism as the credential
// store (internal/config.Save), minus its fsync — a lost token is unrecoverable,
// while a lost response is one command away from being re-fetched.

// writeStreamFile copies r into path atomically at 0600, returning how many
// bytes landed.
//
// Streamed rather than buffered because `ronja api --out` is how a large binary
// comes down (a feature export is a zip, a table extract is parquet), and
// holding one of those whole to write it out would put a ceiling on downloads
// for no reason.
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
