package api

import (
	"fmt"
	"net/http"
	"sync"
)

// HeaderSpace names the Space a personal token's request acts in. The server
// answers it in kind: a response to a request it acted on in that Space carries
// the same header naming it, a handler's own 4xx included.
const HeaderSpace = "X-Ronja-Space"

// The wire codes of the middleware's two refusals of a named Space, mirrored
// from backend/platform/auth/token_space.go.
const (
	SpaceUnavailableCode   = "space_unavailable"
	SpaceNotSelectableCode = "space_not_selectable"
)

// SpaceErrorKind is why a request naming a Space was not acted on there.
type SpaceErrorKind int

const (
	// SpaceUnconfirmed is a response that does not echo the Space: an instance
	// that does not honour the header, which may have acted in its default
	// Space.
	SpaceUnconfirmed SpaceErrorKind = iota + 1
	// SpaceUnavailable is the server's 409: the token's person cannot enter
	// the Space, or it no longer exists.
	SpaceUnavailable
	// SpaceNotSelectable is the server's 400: a credential that may not choose
	// a Space, such as an organization API token.
	SpaceNotSelectable
)

// SpaceError is the answer to a request that named the client's Space when the
// instance did not act in it, or did not say it had. It deliberately does not
// unwrap to *Error: an unconfirmed 404 is not the row's absence, and a caller
// that read it as one would go on to create.
type SpaceError struct {
	Kind      SpaceErrorKind
	Instance  string
	SpaceID   string
	SpaceName string
	Method    string
	Path      string
	Status    int
	// Code is the response body's wire code, "" for none.
	Code string
	// Echo is the X-Ronja-Space the response carried, "" for none.
	Echo string
}

func (e *SpaceError) Error() string {
	space := "Space " + e.SpaceID
	if e.SpaceName != "" && e.SpaceName != e.SpaceID {
		space = fmt.Sprintf("Space %s (%s)", e.SpaceName, e.SpaceID)
	}
	switch e.Kind {
	case SpaceUnavailable:
		return fmt.Sprintf("the person this credential signs in as cannot enter %s, or it no longer exists (%s answered HTTP %d %s). Ask an Admin to include them on the Space's page, or sign in as someone who can (`ronja login`). Nothing further was sent",
			space, e.Instance, e.Status, e.Code)
	case SpaceNotSelectable:
		return fmt.Sprintf("this credential cannot act in %s: it is an organization API token, one from the API Tokens page not marked Personal token (%s answered HTTP %d %s). Sign in with `ronja login`, or use a personal access token. Nothing further was sent",
			space, e.Instance, e.Status, e.Code)
	}
	echoed := "with no " + HeaderSpace
	if e.Echo != "" {
		echoed = "naming Space " + e.Echo
	}
	return fmt.Sprintf("%s did not confirm it acted in %s: it runs a Ronja that does not honour the Space header, so this request may have acted in the default Space. Nothing further was sent (%s %s answered HTTP %d %s)",
		e.Instance, space, e.Method, e.Path, e.Status, echoed)
}

// SpaceLatch holds the first SpaceError of every client sharing it, and refuses
// each of their later requests with it before it is sent, so a failure some
// caller swallowed still stops the command. The zero value is ready; a nil
// latch holds nothing.
type SpaceLatch struct {
	mu  sync.Mutex
	err *SpaceError
}

// Held is the SpaceError the latch holds, nil for none.
func (l *SpaceLatch) Held() *SpaceError {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *SpaceLatch) hold(e *SpaceError) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = e
	}
}

// setSpace puts the client's Space on header, unless the latch already refuses
// it, and reports whether it did. A header the caller set (`ronja api -H`) is
// left as written and its answer is not checked.
func (c *Client) setSpace(header http.Header) (bool, error) {
	if c.Space == "" || header.Get(HeaderSpace) != "" {
		return false, nil
	}
	if held := c.SpaceLatch.Held(); held != nil {
		return false, held
	}
	header.Set(HeaderSpace, c.Space)
	return true, nil
}

// spaceAnswer reads the response to a request that carried the client's Space:
// nil when the instance acted in it or never reached a verdict, otherwise the
// SpaceError, which the latch then holds. code is the body's wire code, read
// by the caller.
//
// Only 401, 403, 429 and 5xx may lack the echo, being refusals before the Space
// was acted on or no answer at all. Every other status is the handler's, and
// the middleware echoes before the handler runs.
func (c *Client) spaceAnswer(method, path string, status int, header http.Header, code string) error {
	e := &SpaceError{Instance: c.BaseURL, SpaceID: c.Space, SpaceName: c.SpaceName,
		Method: method, Path: path, Status: status, Code: code, Echo: header.Get(HeaderSpace)}
	switch {
	case status == http.StatusConflict && code == SpaceUnavailableCode:
		e.Kind = SpaceUnavailable
	case status == http.StatusBadRequest && code == SpaceNotSelectableCode:
		e.Kind = SpaceNotSelectable
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusTooManyRequests, status >= 500:
		return nil
	case e.Echo == c.Space:
		return nil
	default:
		e.Kind = SpaceUnconfirmed
	}
	c.SpaceLatch.hold(e)
	return e
}

// readsSpaceCode reports whether a response's body must be read to judge its
// Space: a 400 or 409 that does not echo it may be one of the two refusals.
func (c *Client) readsSpaceCode(status int, header http.Header) bool {
	return (status == http.StatusBadRequest || status == http.StatusConflict) && header.Get(HeaderSpace) != c.Space
}
