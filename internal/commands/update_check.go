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

// The daily "you are behind" notice (cli/README.md). It never touches stdout,
// never changes the exit code or fails a command, and never runs for a machine
// (no goroutine, request or file write at all).

// pendingUpdateCheck is a package var because the PreRun hook and run sit on
// opposite sides of cobra's Execute; newRootCmd clears it for each new tree.
var pendingUpdateCheck *updateCheck

// updateRanThisProcess backs up updateCheckSilentCommand: the notice must
// never appear under the line saying an update was just installed.
var updateRanThisProcess bool

var updateCheckNow = time.Now

// updateCheckFetchTimeout is a var only so timeout tests need not each cost a
// real second.
var updateCheckFetchTimeout = time.Second

const (
	// updateCheckInterval gates both fetching and notifying, on separate
	// timestamps so a cached answer still reaches the next human invocation.
	updateCheckInterval = 24 * time.Hour

	// updateCheckTimeoutBackdate makes a timeout retry hourly: a full day's
	// back-off would leave slow links permanently unaware.
	updateCheckTimeoutBackdate = 23 * time.Hour
)

// updateCheckDue treats a future timestamp as due: one left by a wrong clock
// would otherwise read as inside the window forever.
func updateCheckDue(last, now time.Time) bool {
	return now.Sub(last) >= updateCheckInterval || last.After(now)
}

// updateCheck's result is nil when no fetch ran.
type updateCheck struct {
	current string
	state   update.State
	result  chan updateFetchResult
}

type updateFetchResult struct {
	latest string
	err    error
}

// startUpdateCheck runs from PersistentPreRunE, the earliest point cobra has
// resolved the command and parsed --json; an argv scan reads `ronja --profile
// acme update` as command "acme".
func startUpdateCheck(ctx context.Context, cmd *cobra.Command) *updateCheck {
	current := resolveVersion()
	if !update.IsRelease(current) {
		return nil
	}
	// Both streams: stderr keeps the line out of `2>build.log`.
	if !isTerminal(os.Stdout) || !isTerminal(os.Stderr) {
		return nil
	}
	// Any non-empty CI counts, `CI=false` included (as gh does): every CI
	// provider spells the value differently.
	if os.Getenv("CI") != "" || os.Getenv("RONJA_NO_UPDATE_CHECK") != "" || flagJSON {
		return nil
	}
	if updateCheckSilentCommand(cmd) {
		return nil
	}

	// Unreadable state would mean a fetch on every command.
	state, err := update.LoadState()
	if err != nil {
		return nil
	}

	c := &updateCheck{current: current, state: state}
	now := updateCheckNow()
	if !updateCheckDue(state.CheckedAt, now) {
		// Cached, but the notice may still be due in finishUpdateCheck.
		return c
	}

	// Claim the check before fetching, and skip the fetch if the claim cannot
	// be written: an unwritable file then costs nothing per command, and two
	// shells racing make at most two fetches.
	state.CheckedAt = now
	if err := update.SaveState(state); err != nil {
		return nil
	}
	c.state = state

	// Buffered, so the goroutine never blocks on a receiver that went away.
	c.result = make(chan updateFetchResult, 1)
	go func() {
		fetchCtx, cancel := context.WithTimeout(ctx, updateCheckFetchTimeout)
		defer cancel()
		release, err := update.Latest(fetchCtx, releaseBaseURL, current)
		c.result <- updateFetchResult{latest: release.Version, err: err}
	}()
	return c
}

// updateCheckSilentCommand decides on the top-level command, not cmd.Name(),
// because `ronja completion bash` arrives here as "bash". Cobra does route help
// and completion through PersistentPreRun.
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
	// cobra's hidden completion drivers (__complete, __completeNoDesc).
	return strings.HasPrefix(name, "__")
}

// finishUpdateCheck is called from run after the command's output and any error
// line, so the notice is always last on stderr.
func finishUpdateCheck(c *updateCheck) {
	if c == nil || updateRanThisProcess {
		return
	}

	// What this run LEARNED, which the write-back depends on: an attempted
	// fetch that was refused or unparseable learned nothing.
	var (
		checkedAt time.Time // zero unless this run learned when the mirror was last asked
		latest    string    // empty unless the mirror answered with a tag
	)
	if c.result != nil {
		// Bounded by the fetch's own deadline.
		res := <-c.result
		switch {
		case res.err == nil && res.latest != "":
			checkedAt, latest = updateCheckNow(), res.latest
		case errors.Is(res.err, context.DeadlineExceeded), errors.Is(res.err, context.Canceled):
			// A timeout or Ctrl-C rewinds CheckedAt so the retry is hourly, and
			// learned no Latest.
			checkedAt = updateCheckNow().Add(-updateCheckTimeoutBackdate)
		default:
			// Nothing learned, nothing written: the claim startUpdateCheck saved
			// already holds off the mirror for a day, and a rewrite would rewind
			// what another shell recorded meanwhile.
		}
	}

	known := c.state.Latest
	if latest != "" {
		known = latest
	}
	newer, ok := update.Compare(c.current, known)
	if !(ok && newer) && checkedAt.IsZero() && latest == "" {
		return
	}

	// Reload before deciding and writing: another shell's check or update may
	// have recorded timestamps the NotifiedAt gate must see. Only the fields
	// this run learned go back, or what landed meanwhile is rewound.
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
	// Without a fetched tag, announce the file's current Latest: another shell
	// may have learned a newer one, and naming the stale tag would then silence
	// the real one for a day.
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
		// No notice either: an unrecordable NotifiedAt would repeat it on every
		// command.
		return
	}
	if notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
}

func updateInstallMethod() update.Method {
	if exe, err := currentExecutable(); err == nil && update.InstalledByHomebrew(exe) {
		return update.MethodHomebrew
	}
	if runtime.GOOS == "windows" {
		// `ronja update` refuses on Windows: no release asset, and no rename
		// over a running exe.
		return update.MethodGoInstall
	}
	return update.MethodSwap
}
