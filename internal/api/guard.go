package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ReadOnlyGuard refuses, before it is sent, every request on a context that
// carries it except a read (GET, HEAD) and the writes its Allow admits. It is
// how a command that must change nothing on the server but a narrow set of
// records — `ronja <kind> pull`, which records anchors and nothing else —
// can run the deploy paths' own reads and re-anchoring and still be unable to
// write a row, whatever those paths decide.
//
// A refused request never leaves the process: it fails with ErrWriteRefused (a
// plain error, so no caller reads it as a timeout to reconcile or a status to
// interpret), and is recorded so the command can say what it would have sent.
type ReadOnlyGuard struct {
	// Allow admits a write by method and path (relative to /api/v2/, no
	// leading slash). Nil admits none.
	Allow func(method, path string) bool

	mu      sync.Mutex
	blocked []string
}

// ErrWriteRefused is what a guarded request answers.
var ErrWriteRefused = errors.New("refused before it was sent: this command writes nothing on the server but its anchors")

type guardKey struct{}

// WithReadOnlyGuard returns a context whose requests g guards.
func WithReadOnlyGuard(ctx context.Context, g *ReadOnlyGuard) context.Context {
	return context.WithValue(ctx, guardKey{}, g)
}

// Blocked lists the writes the guard refused, as "METHOD /api/v2/path".
func (g *ReadOnlyGuard) Blocked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.blocked...)
}

// check answers nil for a request the guard on ctx (if any) lets through. The
// path is relative to /api/v2/.
func checkGuard(ctx context.Context, method, path string) error {
	path = strings.TrimPrefix(path, "/")
	return checkGuardAt(ctx, method, path, "/api/v2/"+path)
}

// checkGuardRaw is checkGuard for a path relative to the instance ROOT, as
// DoRaw takes it: an /api/v2/ route is judged by Allow like any other, and a
// write anywhere else (/service-api/…) is never one Allow admits.
func checkGuardRaw(ctx context.Context, method, path string) error {
	path = strings.TrimPrefix(path, "/")
	if rest, ok := strings.CutPrefix(path, "api/v2/"); ok {
		return checkGuard(ctx, method, rest)
	}
	return checkGuardAt(ctx, method, "", "/"+path)
}

// checkGuardAt judges one request: allowPath is what Allow is asked about (""
// asks nothing — refused), shown is how a refusal names it.
func checkGuardAt(ctx context.Context, method, allowPath, shown string) error {
	g, _ := ctx.Value(guardKey{}).(*ReadOnlyGuard)
	if g == nil || method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	if allowPath != "" && g.Allow != nil && g.Allow(method, allowPath) {
		return nil
	}
	g.mu.Lock()
	g.blocked = append(g.blocked, method+" "+shown)
	g.mu.Unlock()
	return fmt.Errorf("%s %s: %w", method, shown, ErrWriteRefused)
}
