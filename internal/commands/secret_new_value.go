package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// newValueCheckWait is how long the command waits, after the value changed,
// for the server's on-save check to stamp its verdict. A package var so the
// tests can collapse it.
var newValueCheckWait = 15 * time.Second

const sameValueNote = "saving the value it already has doesn't count as a new one"

const newValueOlderServer = "this Ronja server can't take a new value for an existing secret yet (the CLI is newer). Ask Ronja in a chat to replace it."

func newSecretNewValueCmd() *cobra.Command {
	var noBrowser bool

	cmd := &cobra.Command{
		Use:   "new-value <secret-id>",
		Short: "Enter a new value for an API key or database login, in Ronja",
		Long: `Enter a new value for an API key or database login, in Ronja.

Opens the secret's form for a new value in your browser. You type the value
there and it goes straight to Ronja; this command never sees it. It waits (up
to 15 minutes) for the value to be saved, then prints what Ronja found when she
tried it:

  ronja secret new-value <secret-id>

The secret's owner, the owner of the private feature it is in, an Admin, a
maintainer of its feature, or in a project's feature a member with Full access
can enter a new value. Anyone else gets the sentence saying who can.

--no-browser prints the form's link instead of opening it. Nothing is read
from a prompt, a pipe or a file. Never prints a value; --json prints the
outcome as metadata.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return errors.New("name the secret, by its id")
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			return runSecretNewValue(cmd.Context(), newClient(resolved.URL, resolved.Token), id, noBrowser)
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening it")
	return cmd
}

// runSecretNewValue reads no value from a prompt, pipe or file; it only opens
// the server's form.
func runSecretNewValue(ctx context.Context, client *api.Client, id string, noBrowser bool) error {
	out := os.Stderr

	before, err := client.GetSecretStatus(ctx, id)
	if err != nil {
		if api.StatusOf(err) == http.StatusNotFound {
			return fmt.Errorf("secret %s was not found — check the ID, and that you can see it", id)
		}
		return fmt.Errorf("read secret %s: %w", id, err)
	}
	switch nv := before.NewValue; {
	case nv == nil:
		return errors.New(newValueOlderServer)
	case nv.Refusal != "":
		return errors.New(nv.Refusal)
	case nv.URL == "":
		return fmt.Errorf("the server sent no link to the form for a new value for secret %s; enter it on the secret's page in Ronja", id)
	}
	link := before.NewValue.URL

	spawned := false
	if !noBrowser {
		spawned = openBrowser(link) == nil
	}
	if spawned {
		fmt.Fprintln(out, "Opening the form for a new value in Ronja.")
		fmt.Fprintf(out, "If nothing opened, open this link instead:\n  %s\n\n", link)
	} else {
		fmt.Fprintf(out, "Open this link to enter the new value in Ronja (signed in):\n  %s\n\n", link)
	}
	fmt.Fprintln(out, "The value goes straight to Ronja; this terminal never sees it.")
	fmt.Fprintf(out, "Waiting for the new value; %s. (Ctrl-C to stop waiting; the secret keeps its current value)\n", sameValueNote)

	after, err := pollSecret(ctx, client, id, inBrowserWaitLimit, func(s *api.SecretStatus) bool {
		return valueChangedSince(before.ValueChangedAt, s.ValueChangedAt)
	})
	switch {
	case errors.Is(err, errStoppedWaiting):
		return fmt.Errorf("stopped waiting; secret %s keeps its current value unless one is saved at %s", id, link)
	case errors.Is(err, errWaitedTooLong):
		return fmt.Errorf("still waiting for a new value after %s. Secret %s keeps its current value (%s); enter one at %s or re-run",
			humanDuration(inBrowserWaitLimit), id, sameValueNote, link)
	case err != nil:
		return err
	}

	if !checkedSinceChange(after) {
		checked, err := pollSecret(ctx, client, id, newValueCheckWait, checkedSinceChange)
		switch {
		case err == nil:
			after = checked
		case errors.Is(err, errWaitedTooLong):
		case errors.Is(err, errStoppedWaiting):
			return fmt.Errorf("stopped waiting; the new value for secret %s is saved, and its health section shows Ronja's check when she has made it", id)
		default:
			return fmt.Errorf("the new value for secret %s is saved; reading Ronja's check failed: %w", id, err)
		}
	}
	return reportNewValue(id, after)
}

// valueChangedSince: an absent first reading means the value was never
// replaced, so any reading at all is a change.
func valueChangedSince(first, now *time.Time) bool {
	if now == nil {
		return false
	}
	return first == nil || now.After(*first)
}

// checkedSinceChange is true for the on-save check's verdict on the new value:
// a probe, or a login secret's login (refresh). A verdict stamped before the
// replacement describes the old value, and an observed one is real traffic,
// which can have used it.
func checkedSinceChange(s *api.SecretStatus) bool {
	onSaveCheck := s.LastCheckSource == "probe" || s.LastCheckSource == "refresh"
	return onSaveCheck && s.LastCheckStatus != "" && s.LastCheckedAt != nil && s.ValueChangedAt != nil &&
		!s.LastCheckedAt.Before(*s.ValueChangedAt)
}

// reportNewValue prints the line the secret's page shows after a save. A
// refusal also fails the command: the old value is gone and the stored one is
// known not to work, so a script must not carry on as if it did.
func reportNewValue(id string, s *api.SecretStatus) error {
	status, detail := "", ""
	if checkedSinceChange(s) {
		status, detail = s.LastCheckStatus, strings.TrimSpace(s.LastCheckError)
	}
	if flagJSON {
		out := map[string]any{
			"id":             s.ID,
			"name":           s.Name,
			"valueChangedAt": s.ValueChangedAt,
			"checkStatus":    nil,
			"checkError":     nil,
		}
		if status != "" {
			out["checkStatus"] = status
		}
		if detail != "" {
			out["checkError"] = detail
		}
		if err := emitJSON(out); err != nil {
			return err
		}
		return refusedNewValue(id, status)
	}

	var line string
	switch status {
	case "ok":
		line = "Saved. Ronja tried the new value and it works."
	case "auth_failed":
		line = "Saved, but the service refused the new value."
		if detail != "" {
			line = "Saved, but the service refused the new value: " + detail
		}
	case "error":
		line = "Saved. Ronja couldn't check it."
		if detail != "" {
			line = "Saved. Ronja couldn't check it: " + detail
		}
	default:
		line = "Saved. Ronja hasn't checked the new value yet; the health section shows it when she has."
	}
	fmt.Fprintln(os.Stdout, line)
	return refusedNewValue(id, status)
}

func refusedNewValue(id, status string) error {
	if status != "auth_failed" {
		return nil
	}
	return fmt.Errorf("the new value for secret %s is saved, but the service refused it; the old value is gone, so enter a working one with `ronja secret new-value %s`", id, id)
}
