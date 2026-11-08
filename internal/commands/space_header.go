package commands

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The refusals of a stack whose Space the CLI cannot act in at all. Each is a
// machine contract: add, never rename.
const (
	// syncReasonSpaceHeaderUnsupported — a response to a request naming the
	// stack's Space did not echo it: the instance does not honour the Space
	// header, and may have acted in its default Space.
	syncReasonSpaceHeaderUnsupported = "space_header_unsupported"
	// syncReasonSpaceUnavailable — the signed-in person cannot enter the
	// stack's Space, or it no longer exists (the server's 409).
	syncReasonSpaceUnavailable = "space_unavailable"
	// syncReasonSpaceNotSelectable — the credential is one that may not choose
	// a Space, such as an organization API token (the server's 400).
	syncReasonSpaceNotSelectable = "space_not_selectable"
)

var spaceEntryReasons = map[api.SpaceErrorKind]string{
	api.SpaceUnconfirmed:   syncReasonSpaceHeaderUnsupported,
	api.SpaceUnavailable:   syncReasonSpaceUnavailable,
	api.SpaceNotSelectable: syncReasonSpaceNotSelectable,
}

// spaceEntry is a refusal of a stack whose Space the CLI cannot act in, as a
// report line carries it. Nothing further was sent for the stack's Space.
type spaceEntry struct {
	Reason string
	Detail string
}

// asSpaceEntry reads an api.SpaceError anywhere in err's chain; the process
// exits 2 on one (exitCodeFor).
func asSpaceEntry(err error) (*spaceEntry, bool) {
	var e *api.SpaceError
	if !errors.As(err, &e) {
		return nil, false
	}
	return &spaceEntry{Reason: spaceEntryReasons[e.Kind], Detail: e.Error()}, true
}

// stackClient is the client for one stack, built through newClient (the
// tests' transport seam): a stack that records a Space sends it on every
// request and has every answer confirm it, sharing one latch per (instance,
// organization, Space). With no token there is no one to act in the Space,
// and the request fails as the server's sign-in refusal.
func stackClient(resolved *config.Resolved, key wfdir.InstanceKey, stack wfdir.Stack) *api.Client {
	c := newClient(resolved.URL, resolved.Token)
	if stack.SpaceID != "" && resolved.Token != "" {
		c.Space, c.SpaceName = stack.SpaceID, stack.Space
		c.SpaceLatch = spaceLatchFor(key, stack.SpaceID)
	}
	return c
}

// client is the client for the folder's selected stack.
func (f *folder) client() *api.Client {
	return stackClient(f.Resolved, f.Key, f.Manifest.Stacks[f.Stack])
}

type spaceLatchKey struct {
	url, tenantID, spaceID string
}

// spaceLatches is one latch per (instance, organization, Space) for the
// process; forgetSpaceLatches clears it per command tree.
var spaceLatches struct {
	sync.Mutex
	held map[spaceLatchKey]*api.SpaceLatch
}

func spaceLatchFor(key wfdir.InstanceKey, spaceID string) *api.SpaceLatch {
	k := spaceLatchKey{url: key.URL, tenantID: key.TenantID, spaceID: spaceID}
	spaceLatches.Lock()
	defer spaceLatches.Unlock()
	if spaceLatches.held == nil {
		spaceLatches.held = map[spaceLatchKey]*api.SpaceLatch{}
	}
	if spaceLatches.held[k] == nil {
		spaceLatches.held[k] = &api.SpaceLatch{}
	}
	return spaceLatches.held[k]
}

func forgetSpaceLatches() {
	spaceLatches.Lock()
	spaceLatches.held = nil
	spaceLatches.Unlock()
}

// heldSpaceRefusals is every refusal the process's latches hold, in key order.
func heldSpaceRefusals() []*api.SpaceError {
	spaceLatches.Lock()
	defer spaceLatches.Unlock()
	keys := make([]spaceLatchKey, 0, len(spaceLatches.held))
	for k := range spaceLatches.held {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b spaceLatchKey) int {
		return cmp.Or(cmp.Compare(a.url, b.url), cmp.Compare(a.tenantID, b.tenantID), cmp.Compare(a.spaceID, b.spaceID))
	})
	var out []*api.SpaceError
	for _, k := range keys {
		if e := spaceLatches.held[k].Held(); e != nil {
			out = append(out, e)
		}
	}
	return out
}

// heldSpaceRefusal is the refusal held by the latch of f's stack's Space, nil
// for none, no folder, or a stack that records no Space.
func (f *folder) heldSpaceRefusal() *api.SpaceError {
	if f == nil {
		return nil
	}
	spaceID := f.stackSpaceID()
	if spaceID == "" {
		return nil
	}
	spaceLatches.Lock()
	l := spaceLatches.held[spaceLatchKey{url: f.Key.URL, tenantID: f.Key.TenantID, spaceID: spaceID}]
	spaceLatches.Unlock()
	return l.Held()
}

// spaceRefusal is how f's line reports a Space refusal: through the tree
// step f is in (spaceStep.refusal), or, outside one, err's own.
func (f *folder) spaceRefusal(err error) (*spaceEntry, bool) {
	return f.spaceStep.refusal(err)
}

// spaceAttribution names, for one tree command, the folder whose step met
// each Space refusal. A latch keeps its first refusal for the rest of the
// process, so a refusal held when a folder's step begins was met by an
// earlier step, and that folder reads "not attempted" naming the one that met
// it rather than a second copy of the refusal.
type spaceAttribution struct {
	metBy map[*api.SpaceError]string
}

func newSpaceAttribution() *spaceAttribution {
	return &spaceAttribution{metBy: map[*api.SpaceError]string{}}
}

// spaceStep is one step of one folder: what the latches held as it began,
// and the folder, once open.
type spaceStep struct {
	spaces   *spaceAttribution
	path     string
	before   map[*api.SpaceError]bool
	f        *folder
	reported bool
}

func (a *spaceAttribution) begin(path string, f *folder) *spaceStep {
	before := map[*api.SpaceError]bool{}
	for _, e := range heldSpaceRefusals() {
		before[e] = true
	}
	return &spaceStep{spaces: a, path: path, before: before, f: f}
}

// opened is the folder the step opened, whose latch it now watches.
func (s *spaceStep) opened(f *folder) {
	s.f, f.spaceStep = f, s
}

// refusal is the Space refusal err is or, when err is another error or nil,
// the one the folder's latch came to hold during the step, which the step
// met whatever it went on to say. Another error keeps its own text when the
// latch already held its refusal as the step began.
func (s *spaceStep) refusal(err error) (*spaceEntry, bool) {
	if s == nil {
		return asSpaceEntry(err)
	}
	var e *api.SpaceError
	if !errors.As(err, &e) {
		if e = s.unreported(); e == nil {
			return nil, false
		}
	}
	return s.entry(e), true
}

// entry is the line of a folder whose step ended on e: e itself on the
// folder that met it, and "not attempted" naming that folder on one whose
// step began with e held.
func (s *spaceStep) entry(e *api.SpaceError) *spaceEntry {
	s.reported = true
	reason := spaceEntryReasons[e.Kind]
	if s.before[e] {
		return &spaceEntry{Reason: reason, Detail: notAttemptedInSpace(e, s.spaces.metBy[e], false)}
	}
	if _, met := s.spaces.metBy[e]; !met {
		s.spaces.metBy[e] = s.path
	}
	return &spaceEntry{Reason: reason, Detail: e.Error()}
}

// unreported is the refusal the folder's latch came to hold during the step,
// when no line of the step reported one yet.
func (s *spaceStep) unreported() *api.SpaceError {
	if s.reported {
		return nil
	}
	if e := s.f.heldSpaceRefusal(); e != nil && !s.before[e] {
		return e
	}
	return nil
}

// notAttemptedInSpace is the detail of a folder not attempted because its
// stack's Space was refused, naming the folder that met the refusal (or, met
// outside any folder's step, the request).
func notAttemptedInSpace(held *api.SpaceError, met string, created bool) string {
	if met == "" {
		met = held.Method + " " + held.Path
	}
	what := "got no confirmation of the Space"
	if held.Kind != api.SpaceUnconfirmed {
		what = "was refused the Space"
	}
	attempt := "not attempted"
	if created {
		attempt = "not attempted after its row was created"
	}
	return fmt.Sprintf("%s: %s %s, and nothing further was sent", attempt, met, what)
}

// withPrintedSpaceRefusals is a tree command's summary error carrying, in
// its chain and not its text, each held Space refusal some folder's detail
// already prints, so run does not print it again; one no detail prints (met
// outside any folder, or swallowed) stays out, for run to print. nil stays
// nil.
func withPrintedSpaceRefusals(err error, details []string) error {
	if err == nil {
		return nil
	}
	var printed []error
	for _, e := range heldSpaceRefusals() {
		if slices.ContainsFunc(details, func(d string) bool { return strings.Contains(d, e.Error()) }) {
			printed = append(printed, e)
		}
	}
	if len(printed) == 0 {
		return err
	}
	return &printedSpaceRefusals{summary: err, printed: printed}
}

type printedSpaceRefusals struct {
	summary error
	printed []error
}

func (e *printedSpaceRefusals) Error() string { return e.summary.Error() }

// Unwrap leads with the summary, so its exit code is the one exitCodeFor
// finds.
func (e *printedSpaceRefusals) Unwrap() []error {
	return append([]error{e.summary}, e.printed...)
}
