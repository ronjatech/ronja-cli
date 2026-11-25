package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

// ChangeList is a change list (backend api/v2/changelist) as the CLI reads it:
// the answer to a proposal, and each read `ronja propose wait` makes, which
// never decodes the plan, since it holds each migration's SQL.
type ChangeList struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	Stack        *string    `json:"stack"`
	RequestedFor string     `json:"requestedFor"`
	Reason       string     `json:"reason"`
	ExpiresAt    time.Time  `json:"expiresAt"`
	DecidedAt    *time.Time `json:"decidedAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
	// SentAt is when the person was told of it; nil while it is unsent.
	SentAt *time.Time `json:"sentAt"`
	// URL is the page the person approves it on, empty when the instance has
	// no frontend origin configured.
	URL string `json:"url"`
	// Result is the deploy job's report (echangelist.Result), absent until it
	// reports.
	Result json.RawMessage `json:"result"`
	// Drafts are the apps the proposal stored a draft for, on the answer to a
	// proposal only; an older Ronja, which ignores the previews, sends none.
	Drafts []ChangeListDraft `json:"drafts,omitempty"`
	// Title is the assistant's, else one Ronja derives from the changes.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Changes are the rows the person reads, each under the key its note is
	// written with; absent when the answer leaves the plan out.
	Changes []ChangeListChange `json:"changes,omitempty"`
	// Skeleton is what `ronja propose describe` prints and saves back.
	Skeleton *ChangeListSkeleton `json:"skeleton,omitempty"`
	// DroppedNotes are the replaced list's notes whose change this list no
	// longer makes, on the answer to a proposal only.
	DroppedNotes []string `json:"droppedNotes,omitempty"`
	// Next is the step to take next, as the server words it.
	Next string `json:"next,omitempty"`
}

// ChangeListChange is one row of a change list (rchangelist.Change).
type ChangeListChange struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Action string `json:"action"`
	Title  string `json:"title"`
	Note   string `json:"note"`
}

// ChangeListSkeleton is a list's description with every key present, ""
// where nothing is written yet (rchangelist.Skeleton).
type ChangeListSkeleton struct {
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Notes       map[string]string `json:"notes"`
}

// Described is whether it has the title and the description a send needs.
func (s *ChangeListSkeleton) Described() bool {
	return s != nil && s.Title != "" && s.Description != ""
}

// ChangeListDraft is one app's draft: what it will look like once the list
// deploys, opened from the change list.
type ChangeListDraft struct {
	FolderID string `json:"folderID"`
	Title    string `json:"title"`
	URL      string `json:"url,omitempty"`
	// Refused is why the app has no draft, in place of URL.
	Refused string `json:"refused,omitempty"`
}

// maxChangeListRead bounds a list's read: an older Ronja ignores omit=plan,
// and its answer holds an 8 MiB plan beside a 16 MiB result.
const maxChangeListRead = 32 << 20

// ChangeList reads one change list, and Ronja's clock at the answer (its Date
// header, else this machine's). A connection lost mid-answer is a *url.Error,
// as one lost before it is; an answer that does not decode is a plain error,
// never an *Error.
func (c *Client) ChangeList(ctx context.Context, id string) (*ChangeList, time.Time, error) {
	path := "/api/v2/change-list/" + url.PathEscape(id) + "?omit=plan"
	resp, err := c.DoRaw(ctx, http.MethodGet, path, http.Header{"Accept": {"application/json"}}, nil, requestTimeout)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	now, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		now = time.Now()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxChangeListRead))
	if err != nil {
		return nil, now, &url.Error{Op: http.MethodGet, URL: c.BaseURL + path, Err: err}
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, now, errorFrom(resp.Status, raw)
	}
	var out ChangeList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, now, fmt.Errorf("decode change list %s: %w", id, err)
	}
	if out.ID == "" || out.Status == "" {
		return nil, now, fmt.Errorf("the answer for change list %s names no id or status", id)
	}
	if string(out.Result) == "null" {
		out.Result = nil
	}
	return &out, now, nil
}

// ChangeListProposal is the multipart body of POST /api/v2/change-list.
type ChangeListProposal struct {
	// Archive is the gzipped tar of the folder the plan was made on.
	Archive []byte
	// Previews is the gzipped JSON of each app the plan creates or updates,
	// as the push would write it; nil when there is none.
	Previews []byte
	// Plan is the echangelist.Plan JSON.
	Plan      json.RawMessage
	ServerURL string
	Stack     string
	// Unsent asks for the list unsent: nobody is told of it until it is
	// described and sent. An older Ronja ignores it and answers pending.
	Unsent bool
}

// maxChangeListAnswer bounds the read of the answer, which echoes the plan.
const maxChangeListAnswer = 16 << 20

// ProposeChangeList uploads a change list for the person this credential acts
// for to approve.
func (c *Client) ProposeChangeList(ctx context.Context, p ChangeListProposal) (*ChangeList, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	archive, err := w.CreateFormFile("archive", "copy.tar.gz")
	if err != nil {
		return nil, err
	}
	if _, err := archive.Write(p.Archive); err != nil {
		return nil, err
	}
	if len(p.Previews) > 0 {
		previews, err := w.CreateFormFile("previews", "previews.json.gz")
		if err != nil {
			return nil, err
		}
		if _, err := previews.Write(p.Previews); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct{ name, value string }{
		{"plan", string(p.Plan)},
		{"server_url", p.ServerURL},
		{"stack", p.Stack},
	} {
		if field.value == "" {
			continue
		}
		if err := w.WriteField(field.name, field.value); err != nil {
			return nil, err
		}
	}
	if p.Unsent {
		if err := w.WriteField("unsent", "true"); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	resp, err := c.DoRaw(ctx, http.MethodPost, "/api/v2/change-list",
		http.Header{"Content-Type": {w.FormDataContentType()}, "Accept": {"application/json"}},
		BytesBody(body.Bytes()), slowRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxChangeListAnswer))
	if err != nil {
		return nil, fmt.Errorf("read the change list's answer: %w", err)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, errorFrom(resp.Status, raw)
	}
	var out ChangeList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode the change list's answer: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("the server answered the proposal without a change list id")
	}
	return &out, nil
}

// DescribeChangeList saves description, a JSON object of title, description
// and notes, over what the unsent list says.
func (c *Client) DescribeChangeList(ctx context.Context, id string, description json.RawMessage) (*ChangeList, error) {
	var out ChangeList
	if err := c.Do(ctx, http.MethodPut, "change-list/"+url.PathEscape(id)+"/description", description, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendChangeList tells the list's person it waits for their approval.
func (c *Client) SendChangeList(ctx context.Context, id string) (*ChangeList, error) {
	var out ChangeList
	if err := c.Do(ctx, http.MethodPost, "change-list/"+url.PathEscape(id)+"/send", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RejectChangeList ends an unsent or a waiting list: the reject route, which
// discards an unsent one.
func (c *Client) RejectChangeList(ctx context.Context, id string) (*ChangeList, error) {
	var out ChangeList
	if err := c.Do(ctx, http.MethodPost, "change-list/"+url.PathEscape(id)+"/reject", map[string]string{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
