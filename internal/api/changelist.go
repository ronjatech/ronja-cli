package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// ChangeList is the part of a change list (backend api/v2/changelist) the
// CLI reads back after proposing one.
type ChangeList struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Stack     *string   `json:"stack"`
	ExpiresAt time.Time `json:"expiresAt"`
	// URL is the page the person approves it on, empty when the instance has
	// no frontend origin configured.
	URL string `json:"url"`
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
