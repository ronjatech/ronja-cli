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
)

// `ronja secret create --in-browser` is the browser-paste handshake: for a value
// that must not pass through whatever runs the command — an AI agent has no
// terminal to be prompted at, and its transcript is no place for the value.
// That is the whole guarantee: the value never passes through this process, its
// arguments, a shell history or a transcript. It is not a boundary against the
// caller: whoever can edit the secret or the feature's workflows — the same
// login included — can still use the value and change where it goes.
//
// The CLI reads NO value. It asks the server for a pending secret (POST
// /secret/pending), opens the page the SERVER names, and waits on GET
// /secret/:id until the person the secret belongs to has entered the value
// there and it turns ready. pending → ready is the whole signal: nothing else
// moves a pending secret to ready.
//
// It is an explicit choice, never a fallback. Nothing on stdin is refused with
// a pointer here rather than silently opening a browser, because an agent with
// an empty pipe has to hear that it supplied nothing.

// inBrowserPollInterval and inBrowserWaitLimit are package vars so the tests can
// collapse them. Fifteen minutes: longer than a sign-in, since the person may
// have to go and find the key; the secret stays pending past it either way.
var (
	inBrowserPollInterval = 3 * time.Second
	inBrowserWaitLimit    = 15 * time.Minute
)

// inBrowserOlderServer is the answer when the route does not exist (plan §5.3).
const inBrowserOlderServer = "this Ronja server can't take a value in the browser yet (the CLI is newer). Run in a terminal to be prompted, or pipe the value on stdin."

func runSecretInBrowser(ctx context.Context, client *api.Client, in api.CreatePendingSecretInput, fieldNames []string, noBrowser bool) error {
	out := os.Stderr

	started, err := client.CreatePendingSecret(ctx, in)
	if err != nil {
		return pendingCreateError(err, in.FeatureID)
	}
	sec := started.Secret
	if started.URL == "" {
		return fmt.Errorf("the server created pending secret %s but sent no link to it; finish it in Ronja", sec.ID)
	}

	if len(started.Existing) > 0 {
		fmt.Fprintln(out, "Already in Ronja for the same system (a notice — this creates its own secret anyway):")
		for _, e := range started.Existing {
			owner := ""
			if e.OwnedByYou {
				owner = ", yours"
			}
			fmt.Fprintf(out, "  %s  %s (%s, %s%s)\n", e.ID, e.Name, e.ProviderKey, e.Status, owner)
		}
		fmt.Fprintln(out)
	}
	if started.Reused {
		fmt.Fprintf(out, "Using the pending secret you started earlier: %s\n", sec.ID)
	}

	spawned := false
	if !noBrowser {
		spawned = openBrowser(started.URL) == nil
	}
	if spawned {
		fmt.Fprintln(out, "Opening the secret in Ronja so its value can be entered there.")
		fmt.Fprintf(out, "If nothing opened, open this link instead:\n  %s\n\n", started.URL)
	} else {
		fmt.Fprintf(out, "Open this link to enter the value in Ronja (the form to enter it shows only for you, signed in):\n  %s\n\n", started.URL)
	}
	fmt.Fprintln(out, "The value goes straight to Ronja; this terminal never sees it.")
	fmt.Fprintln(out, "Waiting for the value... (Ctrl-C to stop waiting; the secret stays pending)")

	if err := waitForValue(ctx, client, sec.ID, started.URL); err != nil {
		return err
	}
	return reportEnteredSecret(&sec, in, fieldNames)
}

// pendingCreateError turns the pending route's refusals into what a person can
// act on. A 409 (a pending secret of that name already exists with different
// settings) and a 400 are the server's own sentences, which name the field.
func pendingCreateError(err error, featureID string) error {
	switch status := api.StatusOf(err); {
	case status == http.StatusNotFound && api.CodeOf(err) == "":
		// Not the API's {"error": …} shape: no route answered at all.
		return errors.New(inBrowserOlderServer)
	case status == http.StatusNotFound:
		return fmt.Errorf("feature %s was not found — check the ID, and that a private feature is yours", featureID)
	case status == http.StatusMethodNotAllowed:
		return errors.New(inBrowserOlderServer)
	case status == http.StatusForbidden && (api.WireCodeOf(err) == api.InsufficientScopeCode || scopeDenied(err)):
		return errors.New("this login's token cannot write secrets (needs secrets:write)")
	case status == http.StatusForbidden && api.CodeOf(err) == "forbidden":
		return errors.New("you cannot create a secret in this feature: it needs the User role, and on a shared feature an Admin or a maintainer of it")
	case status == http.StatusConflict, status == http.StatusBadRequest, status == http.StatusForbidden:
		if msg := api.CodeOf(err); msg != "" {
			return errors.New(msg)
		}
	}
	return err
}

// waitForValue polls the secret until it is ready, the wait runs out, the
// secret disappears, or the person presses Ctrl-C. Transient failures get the
// login poll's window, as every handshake's wait does.
func waitForValue(ctx context.Context, client *api.Client, id, link string) error {
	deadline := time.Now().Add(inBrowserWaitLimit)
	interval := inBrowserPollInterval
	var failingSince time.Time
	stopped := func() error {
		return fmt.Errorf("stopped waiting; secret %s stays pending — the value can still be entered at %s", id, link)
	}

	for {
		select {
		case <-ctx.Done():
			return stopped()
		case <-time.After(interval):
		}

		now, err := client.GetSecretStatus(ctx, id)
		switch {
		case err == nil:
			failingSince = time.Time{}
			interval = inBrowserPollInterval
			if now.Status == "ready" {
				return nil
			}
		case ctx.Err() != nil:
			return stopped()
		case api.StatusOf(err) == http.StatusNotFound:
			return fmt.Errorf("the secret was deleted while waiting (%s)", id)
		case api.IsTransient(err):
			t := time.Now()
			if failingSince.IsZero() {
				failingSince = t
			}
			if failing := t.Sub(failingSince); failing > client.MaxPollFailureWindow {
				return fmt.Errorf("gave up after %s of consecutive failures: %w", failing.Round(time.Second), err)
			}
			if api.StatusOf(err) == http.StatusTooManyRequests {
				interval += client.RateLimitBackoff
			}
		default:
			return fmt.Errorf("read secret %s: %w", id, err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("still waiting for the value after %s. Secret %s stays pending; finish it at %s or re-run with the same arguments",
				humanDuration(inBrowserWaitLimit), id, link)
		}
	}
}

// reportEnteredSecret prints the result line in the terminal path's shape. The
// lengths the terminal path prints are unknown here — the value never passed
// through this process — so it says where the value was entered instead.
func reportEnteredSecret(sec *api.CreatedSecret, in api.CreatePendingSecretInput, fieldNames []string) error {
	featureID := in.FeatureID
	if sec.FeatureID != nil && *sec.FeatureID != "" {
		featureID = *sec.FeatureID
	}
	secretType := sec.SecretType
	if secretType == "" {
		secretType = in.SecretType
	}
	if flagJSON {
		out := map[string]any{
			"id":             sec.ID,
			"name":           sec.Name,
			"secretType":     secretType,
			"featureID":      featureID,
			"enteredInRonja": true,
		}
		if in.Dialect != "" {
			out["dialect"] = in.Dialect
		} else {
			out["fields"] = fieldNames
		}
		return emitJSON(out)
	}

	var what string
	if in.Dialect != "" {
		what = in.Dialect + " login entered in Ronja"
	} else {
		noun, verb := "field", "value"
		if len(fieldNames) != 1 {
			noun, verb = "fields", "values"
		}
		what = fmt.Sprintf("%d %s: %s — %s entered in Ronja", len(fieldNames), noun, strings.Join(fieldNames, ", "), verb)
	}
	fmt.Fprintf(os.Stdout, "%s  %s (%s, feature %s, %s)\n", sec.ID, sec.Name, secretType, featureID, what)
	return nil
}
