//go:build !windows

package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A path that is readable but not a REGULAR file is not a mistake to refuse —
// it is `-d @<(gzip -c dump.sql)`, `-F file=@/dev/stdin`, a named pipe a
// producer is writing into. Those cannot be streamed (their stat size is not
// the size of what reading them yields) and cannot be re-opened for a retry, so
// they are read into memory and sent from there, exactly as `@-` is.
//
// The assertion is that the bytes ARRIVE: a check that only looked at the exit
// code would pass just as well on a request that went out empty, which is the
// failure this shape actually invites.
func TestAPISendsANonRegularFileBody(t *testing.T) {
	const payload = "a,b\n1,2\n"

	for _, tc := range []struct {
		name string
		args func(fifo string) []string
	}{
		{"data", func(fifo string) []string { return []string{"-d", "@" + fifo} }},
		{"form", func(fifo string) []string { return []string{"-F", "file=@" + fifo} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEchoServer(t)
			signInTo(t, e.URL())
			dir := t.TempDir()
			fifo := filepath.Join(dir, "pipe")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			// A FIFO's reader blocks until a writer opens it, so the content
			// has to be produced concurrently — which is exactly the situation
			// a caller is in when they point the CLI at one.
			written := make(chan error, 1)
			go func() {
				f, err := os.OpenFile(fifo, os.O_WRONLY, 0o600)
				if err != nil {
					written <- err
					return
				}
				_, err = io.WriteString(f, payload)
				written <- errors.Join(err, f.Close())
			}()

			args := append([]string{"api", "-X", "POST"}, tc.args(fifo)...)
			if _, err := runCLI(t, dir, append(args, "/api/v2/file/upload/uploads")...); err != nil {
				t.Fatalf("a readable FIFO was refused: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatalf("writing the FIFO: %v", err)
			}
			if !strings.Contains(e.only().Body, payload) {
				t.Errorf("the FIFO's content never reached the instance: %q", e.only().Body)
			}
		})
	}
}
