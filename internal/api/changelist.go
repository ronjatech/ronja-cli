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
	// URL is the page the person approves it on, empty when the instance has
	// no frontend origin configured.
	URL string `json:"url"`
	// Result is the deploy job's report (echangelist.Result), absent until it
	// reports.
	Result json.RawMessage `json:"result"`
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
	// Plan is the echangelist.Plan JSON.
	Plan      json.RawMessage
	ServerURL string
	Stack     string
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
