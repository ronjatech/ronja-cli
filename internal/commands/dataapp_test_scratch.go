package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A test app: the folder as its push would write it, saved by PUT
// /dataapp/scratch in the person's private area, rendered like any app and
// deleted a day after its last push. The folder's binding, app and draft are
// never touched; only .ronja/scratch.json remembers which test app is this
// folder's.

// scratchBody is api/v2/dataapp.ScratchBody.
type scratchBody struct {
	ID             string             `json:"id,omitempty"`
	Title          string             `json:"title"`
	Description    *string            `json:"description,omitempty"`
	ServerDataOnly *bool              `json:"serverDataOnly,omitempty"`
	Access         *api.DataAppAccess `json:"access,omitempty"`
	Files          []appPreviewFile   `json:"files"`
}

// appTestScratch is the --json report's scratch block.
type appTestScratch struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func scratchResult(s *api.ScratchApp) *appTestScratch {
	if s == nil {
		return nil
	}
	return &appTestScratch{ID: s.ID, URL: s.URL, ExpiresAt: s.ExpiresAt}
}

// pushScratchApp sends f as its test app and says where it is.
func pushScratchApp(ctx context.Context, client *api.Client, f *folder) (*api.ScratchApp, error) {
	p, err := buildAppPreview(f, "", true)
	if err != nil {
		return nil, err
	}
	if !f.Manifest.ManagesAccess() {
		row, err := boundAppRow(ctx, client, f)
		if err != nil {
			return nil, err
		}
		if row != nil {
			p.Access = &row.DataAppAccess
		}
	}
	if err := gateAppAccess(ctx, client, f, *p.Access); err != nil {
		return nil, err
	}
	key, err := scratchKey(ctx, f)
	if err != nil {
		return nil, err
	}
	// Two concurrent first runs in one folder make two test apps; the last recorded wins.
	in := scratchBody{
		ID: wfdir.LoadScratchApp(f.Root, key), Title: p.Title, Description: p.Description,
		ServerDataOnly: p.ServerDataOnly, Access: p.Access, Files: p.Files,
	}
	var body bytes.Buffer
	// Unescaped, as `ronja propose` sends the same app, so the two measure
	// one size against the one cap.
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		return nil, err
	}
	if body.Len() > api.MaxScratchBody {
		return nil, errors.New("this app is larger than 24 MiB, the most a change list can carry; it could never be proposed")
	}

	app, err := client.PushScratchApp(ctx, body.Bytes())
	switch status := api.StatusOf(err); {
	case err == nil:
	// An older server routes the path to PUT :id, which answers an unknown id
	// 404 and a body over its own cap 413.
	case status == http.StatusNotFound, status == http.StatusRequestEntityTooLarge:
		return nil, fmt.Errorf("%s (an older Ronja answers this too: if %s is one, it does not support test apps yet)",
			api.ErrorMessageOf(err), f.Resolved.URL)
	case api.WireCodeOf(err) == api.ScratchAppLimitCode:
		return nil, scratchLimitRefusal(err)
	default:
		return nil, err
	}

	if err := wfdir.RecordScratchApp(f.Root, key, app.ID); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not remember this test app (%v); the next run makes a new one.\n", err)
	}
	printScratchApp(f, app)
	return app, nil
}

// boundAppRow is the row whose access a test app of a bound folder takes when
// that access lives in Ronja rather than in ronja.json: the person's draft when
// there is one, else the live app. nil for an unbound folder.
func boundAppRow(ctx context.Context, client *api.Client, f *folder) (*api.DataApp, error) {
	if id, err := f.ResourceID(); err != nil || id == "" {
		return nil, err
	}
	target, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	return target.FilesRow(), nil
}

// scratchKey is the instance and organization the test app is made in. An
// unbound folder has no organization of its own, so the credential's is
// asked: an id is never sent to an organization it was not made in.
func scratchKey(ctx context.Context, f *folder) (wfdir.InstanceKey, error) {
	if f.Key.Known() {
		return f.Key, nil
	}
	if err := ensureTenant(ctx, f.Resolved); err != nil {
		return wfdir.InstanceKey{}, err
	}
	return wfdir.InstanceKey{URL: f.Resolved.URL, TenantID: f.Resolved.TenantID}, nil
}

// scratchLimitRefusal is the server's own sentence, which names when the next
// test app is deleted; --json also gets it, with nextExpiresAt, on stdout.
func scratchLimitRefusal(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	var body struct {
		Error         string `json:"error"`
		Code          string `json:"code"`
		NextExpiresAt string `json:"nextExpiresAt,omitempty"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil || body.Error == "" {
		return err
	}
	if !flagJSON {
		return errors.New(body.Error)
	}
	if err := emitJSON(body); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "error:", body.Error)
	return errAlreadyReported
}

func printScratchApp(f *folder, app *api.ScratchApp) {
	w := os.Stderr
	fmt.Fprintf(w, "Test app:  %s\n", app.URL)
	fmt.Fprintf(w, "           Only you and admins can open it. Deleted %s unless you run `ronja app test` in this folder again.\n",
		app.ExpiresAt.Local().Format("2006-01-02 15:04"))
	if app.Previous == "gone" {
		fmt.Fprintf(w, "           Your earlier test app is gone or in another Space; this is a new link.\n")
	}
	if id, _ := f.ResourceID(); id == "" {
		fmt.Fprintf(w, "           No app was made. To make one, run `ronja app push`.\n")
	}
}
