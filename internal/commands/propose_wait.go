package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// The wait's clocks; variables so the tests can shorten them.
var (
	proposeWaitFastPoll   = 10 * time.Second
	proposeWaitFastWindow = 10 * time.Minute
	proposeWaitSlowPoll   = 60 * time.Second
	// proposeWaitGrace is how long past its expiry, or past the stuck limit, a
	// list may stay open before the wait stops trusting Ronja to close it.
	proposeWaitGrace = 5 * time.Minute
	// proposeWaitReadFailures is how long an unbroken run of unanswered reads
	// may last, counted from its first.
	proposeWaitReadFailures = 5 * time.Minute
)

// Change list statuses (backend engine/echangelist).
const (
	changeListUnsent   = "unsent"
	changeListPending  = "pending"
	changeListApplying = "applying"
	changeListApplied  = "applied"
	changeListFailed   = "failed"
	changeListRejected = "rejected"
	changeListExpired  = "expired"
)

func newProposeWaitCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a proposed change list to be decided and deployed, and say what happened",
		Long: `Wait for a change list ` + "`ronja propose`" + ` uploaded to end, and print what
happened. It only reads the list; it never approves, rejects or deploys.

  ronja propose wait changelist-…                 # in the background: until the list ends
  ronja propose wait changelist-… --timeout 9m    # in the foreground, under a tool time limit

It ends when the list does, and exits 0 only when it deployed:

  deployed   what went live                                            exit 0
  failed     the reason, how far the deploy got, each refusal, and     exit 1
             any folder sent for review instead ("sent for review,
             not live")
  rejected   the note; nothing was deployed                            exit 1
  expired    not approved within a day, or replaced by a newer list    exit 1

A list not sent yet ends the wait at once, exit 1, with the next step.

Without --timeout it waits until the list ends, which Ronja guarantees within
a day of sending, plus the deploy. At --timeout it says "still waiting for approval" or "still deploying"
and exits 1; run it again to keep waiting. Run in the background, it wakes the
agent that started it when the list ends.

It checks every 10 seconds for the first 10 minutes, then every minute while
the list waits for approval, and every 10 seconds while it deploys. Each check
reads the list's status and outcome, never its SQL. A read Ronja does not
answer is tried again for 5 minutes; Ctrl-C stops the wait and leaves the list
as it is.

--json writes {id, status, reason, result, expiresAt, decidedAt, finishedAt, next},
where result is the deploy's own report; keys with no value are left out.`,
		Args: changeListID("wait", 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout < 0 {
				return fmt.Errorf("--timeout %s is negative; 0 waits until the list ends", timeout)
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			return runProposeWait(cmd.Context(), api.New(resolved.URL, resolved.Token), args[0], timeout)
		},
	}
	durationFlag(cmd, &timeout, "timeout", 0, "stop waiting after this long (default 0: until the list ends)")
	return cmd
}

type proposeWaitReport struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	DecidedAt  *time.Time      `json:"decidedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Next       string          `json:"next,omitempty"`
}

func runProposeWait(ctx context.Context, client *api.Client, id string, timeout time.Duration) error {
	start := time.Now()
	var deadline time.Time
	// readCtx bounds each read by --timeout; ctx alone tells Ctrl-C apart from
	// the deadline.
	readCtx := ctx
	if timeout > 0 {
		deadline = start.Add(timeout)
		var cancel context.CancelFunc
		readCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	again := fmt.Sprintf("run `%s` again", carryCredential("ronja propose wait "+id))
	approver := ""
	var last *api.ChangeList
	var lastErr error
	var failingSince time.Time
	timedOut := func() bool { return !deadline.IsZero() && !time.Now().Before(deadline) }
	timeoutEnding := func() error {
		switch {
		case last == nil:
			return fmt.Errorf("change list %s could not be read within --timeout %s: %w — the list is unaffected; %s", id, timeout, lastErr, again)
		case last.Status == changeListApplying:
			return endWait(last, fmt.Sprintf("Change list %s is still deploying after %s. To hear when it ends, run: %s", id, timeout, carryCredential("ronja propose wait "+id)))
		default:
			return endWait(last, fmt.Sprintf("Change list %s is still waiting for approval after %s. To keep waiting, run: %s", id, timeout, carryCredential("ronja propose wait "+id)))
		}
	}
	for {
		list, now, err := client.ChangeList(readCtx, id)
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("stopped waiting for change list %s; the list is unaffected: %s to pick it up", id, again)
		case err == nil:
			failingSince = time.Time{}
		case api.StatusOf(err) != 0 && !api.Unanswered(err):
			return fmt.Errorf("change list %s: %w", id, err)
		case api.Unanswered(err) || api.IsTimeout(err):
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) >= proposeWaitReadFailures {
				return fmt.Errorf("Ronja has not answered for change list %s since %s: %w — the list is unaffected; %s",
					id, failingSince.Format(time.Kitchen), err, again)
			}
		default:
			return fmt.Errorf("%w — `ronja api /api/v2/change-list/%s` shows the answer as Ronja sent it", err, id)
		}

		if list != nil {
			if last == nil || last.Status != list.Status {
				if approver == "" && list.Status == changeListPending {
					approver = waitApprover(readCtx, client, list)
				}
				waitProgress(list, approver)
			}
			last = list
			switch list.Status {
			case changeListUnsent:
				sentence := fmt.Sprintf("Change list %s is not sent yet: nobody has been asked to approve it, so there is nothing to wait for.", id)
				if list.Next != "" {
					sentence += " Next: " + carryCredential(list.Next)
				}
				return endWait(list, sentence)
			case changeListApplied, changeListFailed, changeListRejected, changeListExpired:
				return finishWait(list)
			case changeListPending:
				if !list.ExpiresAt.IsZero() && now.After(list.ExpiresAt.Add(proposeWaitGrace)) {
					return endWait(list, fmt.Sprintf("Change list %s expired at %s, and Ronja has not closed this list; it will not deploy, since an expired list cannot be approved. Propose again.",
						id, waitTime(list.ExpiresAt)))
				}
			case changeListApplying:
				if list.DecidedAt != nil && now.After(list.DecidedAt.Add(changeListStuckAfter+proposeWaitGrace)) {
					return endWait(list, fmt.Sprintf("Change list %s has been deploying since %s, past Ronja's %s limit, and Ronja has not closed this list; it cannot say what landed. Check what is live before proposing again.",
						id, waitTime(*list.DecidedAt), changeListStuckAfter))
				}
			default:
				return fmt.Errorf("change list %s is %q, which this ronja does not know — `ronja api /api/v2/change-list/%s` shows it, and `ronja update` may know it", id, list.Status, id)
			}
		}

		lastErr = err
		if timedOut() {
			return timeoutEnding()
		}

		interval := proposeWaitSlowPoll
		if time.Since(start) < proposeWaitFastWindow || (last != nil && last.Status == changeListApplying) {
			interval = proposeWaitFastPoll
		}
		if !deadline.IsZero() {
			interval = min(interval, time.Until(deadline))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stopped waiting for change list %s; the list is unaffected: %s to pick it up", id, again)
		case <-timer.C:
		}
		// A read started at the deadline is cut off before it is answered.
		if timedOut() {
			return timeoutEnding()
		}
	}
}

// waitApprover names the person a pending list waits for when it is the
// caller; an admin waiting on someone else's list gets no name.
func waitApprover(ctx context.Context, client *api.Client, list *api.ChangeList) string {
	me, err := client.Me(ctx)
	if err != nil || me.User.ID != list.RequestedFor {
		return "the person it was proposed for"
	}
	return personName(me)
}

func waitProgress(list *api.ChangeList, approver string) {
	switch list.Status {
	case changeListPending:
		fmt.Fprintf(os.Stderr, "Waiting for %s to approve change list %s (expires %s)…\n", approver, list.ID, waitTime(list.ExpiresAt))
	case changeListApplying:
		fmt.Fprintln(os.Stderr, "Approved; deploying…")
	}
}

func waitTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04 MST") }

func waitReport(list *api.ChangeList) proposeWaitReport {
	return proposeWaitReport{ID: list.ID, Status: list.Status, Reason: list.Reason, Result: list.Result,
		ExpiresAt: list.ExpiresAt, DecidedAt: list.DecidedAt, FinishedAt: list.FinishedAt, Next: list.Next}
}

// endWait ends a wait on a list that is still open: the sentence on stdout,
// or with --json the list on stdout and the sentence on stderr; exit 1.
func endWait(list *api.ChangeList, sentence string) error {
	if flagJSON {
		fmt.Fprintln(os.Stderr, sentence)
		if err := emitJSON(waitReport(list)); err != nil {
			return err
		}
		return errAlreadyReported
	}
	fmt.Fprintln(os.Stdout, sentence)
	return errAlreadyReported
}

// finishWait prints a list that ended; only a deployed one exits 0.
func finishWait(list *api.ChangeList) error {
	if flagJSON {
		if err := emitJSON(waitReport(list)); err != nil {
			return err
		}
	} else {
		printWaitOutcome(os.Stdout, list)
	}
	if list.Status == changeListApplied {
		return nil
	}
	return errAlreadyReported
}

// waitResult is the part of echangelist.Result the outcome prints.
type waitResult struct {
	Reason   string `json:"reason"`
	Stage    string `json:"stage"`
	Refusals []struct {
		Folder string `json:"folder"`
		Path   string `json:"path"`
		Detail string `json:"detail"`
	} `json:"refusals"`
	Migrations []struct {
		Dir        string          `json:"dir"`
		DatabaseID string          `json:"databaseID"`
		Report     json.RawMessage `json:"report"`
		Error      string          `json:"error"`
	} `json:"migrations"`
	// Sync is `ronja sync apply --json`'s report.
	Sync json.RawMessage `json:"sync"`
}

func printWaitOutcome(out io.Writer, list *api.ChangeList) {
	id := list.ID
	switch list.Status {
	case changeListRejected:
		fmt.Fprintf(out, "Change list %s was rejected by the person it was proposed for or an admin; nothing was deployed.\n", id)
		if list.Reason != "" {
			fmt.Fprintf(out, "  Their note: %s\n", terminalSafe(list.Reason))
		} else {
			fmt.Fprintln(out, "  They left no note.")
		}
		return
	case changeListExpired:
		fmt.Fprintf(out, "Change list %s expired: %s. Nothing was deployed.\n", id, orElse(list.Reason, "Ronja gave no reason"))
		if m := replacedBy.FindStringSubmatch(list.Reason); m != nil {
			fmt.Fprintf(out, "To hear what becomes of the newer list, run: %s\n", carryCredential("ronja propose wait "+m[1]))
		}
		return
	}

	if list.Status == changeListApplied {
		fmt.Fprintf(out, "Change list %s deployed.\n", id)
	} else {
		fmt.Fprintf(out, "Change list %s failed to deploy: %s.\n", id, orElse(list.Reason, "Ronja gave no reason"))
	}
	if len(list.Result) == 0 {
		if list.Status == changeListFailed {
			fmt.Fprintln(out, "  The deploy never reported back; Ronja cannot say what landed.")
			fmt.Fprintln(out, "Check what is live, then propose again: what already deployed plans as unchanged.")
		}
		return
	}
	var res waitResult
	if err := json.Unmarshal(list.Result, &res); err != nil {
		fmt.Fprintf(out, "  Its report could not be read (%v); `ronja api /api/v2/change-list/%s` shows it.\n", err, id)
		return
	}
	if list.Status == changeListFailed {
		switch res.Stage {
		case "before_write":
			fmt.Fprintln(out, "  Where it stopped: before the deploy wrote anything; nothing was written.")
		case "migrations":
			fmt.Fprintln(out, "  Where it stopped: a migration failed, so no folder deployed.")
		case "sync":
			fmt.Fprintln(out, "  Where it stopped: every migration applied; folders partly deployed.")
		}
		for _, r := range res.Refusals {
			name := r.Folder
			if r.Path != "" {
				name += "/" + r.Path
			}
			fmt.Fprintf(out, "  Refused: %s: %s\n", name, r.Detail)
		}
	}
	for _, m := range res.Migrations {
		fmt.Fprintf(out, "  %s (database %s): %s\n", m.Dir, m.DatabaseID, migrationRunLine(m.Report, m.Error))
	}
	lines, reviewOnly := syncFolderLines(res.Sync)
	for _, line := range lines {
		fmt.Fprintf(out, "  %s\n", line)
	}
	switch {
	case list.Status != changeListFailed:
	case reviewOnly && len(res.Refusals) == 0:
		fmt.Fprintln(out, "Every folder that did not go live was sent for review: an admin or a maintainer of its feature decides it in Ronja, and once it is approved it is live, with nothing to propose again. Tell the person; proposing again sends the same review.")
	default:
		fmt.Fprintln(out, "Fix what it names, then propose again: what already deployed plans as unchanged.")
	}
}

// migrationRunLine is one database's line: `ronja db migrate push --json`'s
// count of applied migrations, or its error.
func migrationRunLine(report json.RawMessage, failure string) string {
	if failure != "" {
		return "failed: " + failure
	}
	var r api.MigrationSetResult
	if len(report) == 0 || json.Unmarshal(report, &r) != nil {
		return "applied (no report)"
	}
	n := 0
	for _, m := range r.Results {
		if m.Outcome == api.MigrationApplied {
			n++
		}
	}
	return fmt.Sprintf("%d %s applied", n, plural(n, "migration"))
}

// syncFolderLines is each folder of a `ronja sync apply --json` report. A
// review request is not a deploy, whatever the list's status says.
// reviewOnly is that at least one folder was sent for review and every folder
// that did not apply was; a report that cannot be read in full is never it.
func syncFolderLines(raw json.RawMessage) (lines []string, reviewOnly bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var report struct {
		Truncated bool `json:"truncated"`
		Folders   []struct {
			Path    string `json:"path"`
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
			Detail  string `json:"detail"`
		} `json:"folders"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Truncated {
		return []string{"the folders' report was too large to keep"}, false
	}
	lines = make([]string, 0, len(report.Folders))
	reviews, unfinished, unknown := 0, 0, false
	for _, f := range report.Folders {
		if f.Verdict != verdictApplied {
			unfinished++
		}
		if f.Verdict == verdictUnknown {
			unknown = true
		}
		var line string
		switch {
		case f.Reason == syncReasonSubmittedForReview:
			reviews++
			line = f.Path + ": sent for review, not live"
		case f.Verdict == verdictApplied:
			line = f.Path + ": deployed"
		case f.Reason != "":
			line = fmt.Sprintf("%s: %s (%s)", f.Path, f.Verdict, f.Reason)
		default:
			line = f.Path + ": " + f.Verdict
		}
		if f.Detail != "" && f.Verdict != verdictApplied {
			line += ": " + f.Detail
		}
		lines = append(lines, line)
	}
	return lines, !unknown && reviews > 0 && reviews == unfinished
}

func orElse(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
