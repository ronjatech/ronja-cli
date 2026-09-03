package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/update"
	"github.com/spf13/cobra"
)

// The daily "you are behind" notice.
//
// Nobody knows they are running an old CLI, because nothing tells them. This
// tells them, once a day, in one line on stderr — and it is bound by three
// rules that matter more than the feature does:
//
//  1. It never touches stdout. `eval "$(ronja env)"`, `ronja api --jq`, every
//     agent reading a command's output: one stray line there is a broken
//     script, not a cosmetic problem.
//  2. It never changes the exit code, and it cannot fail a command. Every path
//     in this file returns quietly; there is nothing here worth telling
//     somebody about.
//  3. It never runs for a machine. Not a terminal, CI set, --json asked for:
//     no goroutine, no request, no file write at all.
//
// It runs concurrently with the command (started from the root's
// PersistentPreRunE, joined in run) so that for anything talking to the Ronja
// server the fetch is already finished by the time the command is. The honest
// worst case is a command that finishes in well under a second being held for
// the remainder of that second, at most once per 24 h, when github.com is slow.

// pendingUpdateCheck is the one value passed from the PreRun hook to run. A
// package var because those two are on opposite sides of cobra's Execute and
// share nothing else; newRootCmd clears it, so a second tree in the same
// process (every test does this) never inherits the first one's check.
var pendingUpdateCheck *updateCheck

// updateRanThisProcess suppresses the notice when `ronja update` itself ran.
//
// Belt and braces: the command-name test in startUpdateCheck already covers it,
// and this covers the case where that test is bypassed some future way — the
// one thing the notice must never do is print "a newer release is available"
// underneath the line saying it was just installed.
var updateRanThisProcess bool

// updateCheckNow is time.Now, as a var so the 24-hour arithmetic is testable
// without a test that sleeps for a day.
var updateCheckNow = time.Now

// updateCheckFetchTimeout is the whole budget for the release lookup. A var,
// not a const, only so the timeout tests do not each cost a real second; the
// shipping value is the one the plan committed to and one test still exercises
// it end to end.
var updateCheckFetchTimeout = time.Second

const (
	// updateCheckInterval is both cadences: ask the mirror at most this often,
	// and tell the user at most this often. They are separate timestamps
	// because a cached answer must still notify the next human invocation —
	// otherwise a user whose check ran inside a CI-flagged shell would never
	// hear about it.
	updateCheckInterval = 24 * time.Hour

	// updateCheckTimeoutBackdate is how far CheckedAt is rewound after a
	// timeout, so the retry is hourly rather than daily.
	//
	// A ~400 ms-RTT link needs DNS, TCP, TLS and an HTTP round trip inside one
	// second, and will not always make it. Backing off a full day on that would
	// leave exactly the users on the worst connections permanently unaware,
	// which is the failure this feature exists to fix.
	updateCheckTimeoutBackdate = 23 * time.Hour
)

// updateCheckDue reports whether a recorded timestamp is old enough to open one
// of the two 24-hour gates — the mirror's and the notice's.
//
// A timestamp in the FUTURE is due. A clock that was wrong when the file was
// written, a config directory copied from a machine running ahead, a laptop
// corrected backwards: each leaves a CheckedAt or NotifiedAt that now.Sub reads
// as negative, which is inside the window and never leaves it. The check, or
// the notice, would then be off for as long as that file survives, with nothing
// anywhere saying why.
func updateCheckDue(last, now time.Time) bool {
	return now.Sub(last) >= updateCheckInterval || last.After(now)
}

// updateCheck is a check in flight: what the state file said when the command
// started, and the channel the fetch will answer on (nil when no fetch ran).
type updateCheck struct {
	current string
	state   update.State
	result  chan updateFetchResult
}

type updateFetchResult struct {
	latest string
	err    error
}

// startUpdateCheck decides whether to check at all, and starts the fetch if so.
//
// Called from the root's PersistentPreRunE, which is the earliest point at
// which cobra has resolved WHICH command is running and parsed --json. Doing it
// from an argument scan instead was the first design and is wrong: `ronja
// --profile acme update` reads "acme" as the command, and the notice then lands
// directly under `Updated ronja ...` naming the version that was just replaced.
func startUpdateCheck(ctx context.Context, cmd *cobra.Command) *updateCheck {
	current := resolveVersion()
	// A build that is not a release cannot be compared against one. `dev`,
	// Go's `(devel)` and the pseudo-version of a `go install ...@<commit>` all
	// land here, and telling a developer running their own build that they are
	// behind would be both wrong and unfixable by the remedy offered.
	if !update.IsRelease(current) {
		return nil
	}
	// BOTH streams, not just stdout: stderr being a terminal is what keeps the
	// line out of `2>build.log`, and the stdout half covers every pipe, agent
	// and `eval "$(...)"` case.
	if !isTerminal(os.Stdout) || !isTerminal(os.Stderr) {
		return nil
	}
	// Any non-empty CI counts, `CI=false` included — which gh does too. Nobody
	// sets CI=false on purpose on a machine they are watching, and the cost of
	// reading it as "in CI" is one notice not printed; the cost of parsing it is
	// a rule about a variable's VALUE that every CI provider spells differently.
	if os.Getenv("CI") != "" || os.Getenv("RONJA_NO_UPDATE_CHECK") != "" || flagJSON {
		return nil
	}
	if updateCheckSilentCommand(cmd) {
		return nil
	}

	// A state file that cannot be READ is as disqualifying as one that cannot
	// be written: without it there is no way to know whether today's check has
	// already happened, so every command would fetch.
	state, err := update.LoadState()
	if err != nil {
		return nil
	}

	c := &updateCheck{current: current, state: state}
	now := updateCheckNow()
	if !updateCheckDue(state.CheckedAt, now) {
		// Cached: no request today. The notice may still be due — see
		// finishUpdateCheck — which is what makes a check performed in a
		// non-interactive shell still reach the user afterwards.
		return c
	}

	// Claim the check BEFORE the fetch, and refuse to fetch if the claim could
	// not be written. Two things fall out of that order. A state file that is
	// unreadable, root-owned or on a read-only directory costs nothing rather
	// than a 1 s stall and a github.com request on every single command; and
	// two shells starting at once both read stale, one writes first and the
	// other reads fresh — at worst two fetches, never a storm.
	state.CheckedAt = now
	if err := update.SaveState(state); err != nil {
		return nil
	}
	c.state = state

	// Buffered, so the goroutine can never block on a receiver that went away.
	c.result = make(chan updateFetchResult, 1)
	go func() {
		// Derived from the command's own context, so Ctrl-C ends this too.
		fetchCtx, cancel := context.WithTimeout(ctx, updateCheckFetchTimeout)
		defer cancel()
		release, err := update.Latest(fetchCtx, releaseBaseURL, current)
		c.result <- updateFetchResult{latest: release.Version, err: err}
	}()
	return c
}

// updateCheckSilentCommand reports the commands that get no notice regardless.
//
// Decided on the TOP-LEVEL command — the child of the root — rather than on
// cmd.Name(), because `ronja completion bash` arrives here as "bash".
//
//   - update prints its own report, and a nag under "Updated ronja ..." would
//     be naming the version it had just replaced.
//   - env's stdout is fed to `eval`; the TTY rule already covers that, and the
//     brief asked for it explicitly anyway.
//   - help and completion are output somebody reads or pipes as a whole. The
//     plan assumed cobra did not route these through PersistentPreRun; it does,
//     so they are excluded here instead of for free.
func updateCheckSilentCommand(cmd *cobra.Command) bool {
	top := cmd
	for top.Parent() != nil && top.Parent().Parent() != nil {
		top = top.Parent()
	}
	name := top.Name()
	switch name {
	case "update", "env", "help", "completion":
		return true
	}
	// cobra's hidden completion drivers (__complete, __completeNoDesc) are read
	// by a shell, not a person.
	return strings.HasPrefix(name, "__")
}

// finishUpdateCheck joins the fetch, records what it learned and prints the
// notice if one is due.
//
// Called from run AFTER the command has finished writing and after any error
// line, so the notice is always the last thing on stderr rather than something
// that interleaved with a report.
func finishUpdateCheck(c *updateCheck) {
	if c == nil || updateRanThisProcess {
		return
	}

	// What this run LEARNED — which is not what it read at PreRun, and not even
	// "whether it asked". c.result != nil means a fetch was ATTEMPTED; a
	// refused connection, a 500 and an unparseable body all come back through
	// that channel having learned nothing about the mirror at all. Keeping the
	// two apart is what the write-back below depends on.
	var (
		checkedAt time.Time // zero unless this run learned when the mirror was last asked
		latest    string    // empty unless the mirror answered with a tag
	)
	if c.result != nil {
		// Bounded by the fetch's own deadline, so this waits at most the
		// remainder of the one-second budget.
		res := <-c.result
		switch {
		case res.err == nil && res.latest != "":
			checkedAt, latest = updateCheckNow(), res.latest
		case errors.Is(res.err, context.DeadlineExceeded), errors.Is(res.err, context.Canceled):
			// Not "we asked today". A slow link that cannot finish inside a
			// second would otherwise be silent forever; rewinding CheckedAt
			// turns the retry hourly. It IS something this run learned — that
			// the mirror is out of reach inside a second — so it is written,
			// but on its own: a timeout learned no Latest either.
			//
			// Ctrl-C belongs here rather than below for the same reason: a
			// check the user interrupted learned nothing about the mirror, and
			// charging it a full day's silence would make a habit of
			// interrupting slow commands quietly fatal to the notice.
			checkedAt = updateCheckNow().Add(-updateCheckTimeoutBackdate)
		default:
			// Refused, non-200, unparseable: nothing learned, so nothing is
			// written back. The CheckedAt = now that startUpdateCheck put on
			// disk BEFORE the fetch already stands on its own and leaves the
			// mirror alone for a day; re-writing this run's copy of it, and of
			// a Latest the fetch never confirmed, would only rewind whatever
			// another shell recorded in the meantime.
		}
	}

	// The newest release this run knows of: what the cache held when the
	// command started, replaced only by an answer that actually arrived.
	known := c.state.Latest
	if latest != "" {
		known = latest
	}
	newer, ok := update.Compare(c.current, known)
	if !(ok && newer) && checkedAt.IsZero() && latest == "" {
		// Nothing to say and nothing to record.
		return
	}

	// Reload before DECIDING and before writing, rather than working from the
	// copy read at PreRun. `ronja update` may have run in another shell while
	// this command did, or a second check may have finished; either records
	// timestamps of its own, and both the NotifiedAt gate below and the write
	// have to see them — a stale NotifiedAt tells a user twice what another
	// shell already told them once.
	//
	// Only the fields THIS run learned go back. A cached run learned neither
	// CheckedAt nor Latest, a failed fetch learned neither, and a timeout
	// learned only CheckedAt; assigning any of the others back would rewind
	// what landed in between to what was on disk when the command started.
	fresh, err := update.LoadState()
	if err != nil {
		return
	}
	if !checkedAt.IsZero() {
		fresh.CheckedAt = checkedAt
	}
	if latest != "" {
		fresh.Latest = latest
	}
	// A run that did not fetch has no tag of its own, so the version it
	// ANNOUNCES comes from the file as it stands NOW rather than from the copy
	// read at PreRun: another shell's check — or its `ronja update` — may have
	// learned a newer one while this command ran, and naming the older cached
	// tag would tell the user to move to a release that is no longer the
	// newest, then record NotifiedAt and stay quiet about the real one for a
	// day.
	if latest == "" && fresh.Latest != "" {
		known = fresh.Latest
		newer, ok = update.Compare(c.current, known)
	}

	notice := ""
	if ok && newer && updateCheckDue(fresh.NotifiedAt, updateCheckNow()) {
		notice = update.Notice(c.current, known, updateInstallMethod())
		fresh.NotifiedAt = updateCheckNow()
	}
	if notice == "" && checkedAt.IsZero() && latest == "" {
		return
	}
	if err := update.SaveState(fresh); err != nil {
		// Silent, and deliberately without the notice: a NotifiedAt that cannot
		// be recorded means the same line on every command until the file
		// becomes writable, which is nagging rather than telling.
		return
	}
	if notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
}

// updateInstallMethod picks the remedy the notice offers, from how this binary
// got onto the machine.
func updateInstallMethod() update.Method {
	if exe, err := currentExecutable(); err == nil && update.InstalledByHomebrew(exe) {
		return update.MethodHomebrew
	}
	if runtime.GOOS == "windows" {
		// `ronja update` refuses on Windows (no release asset, and no
		// rename-over-a-running-exe), so offering it would be offering a
		// command that answers with a refusal. `go install` works there.
		return update.MethodGoInstall
	}
	return update.MethodSwap
}
