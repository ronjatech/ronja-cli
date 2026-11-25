package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ScratchAppLimitCode is the wire code on the 409 PUT /dataapp/scratch
// answers when the caller already holds the most test apps at once. Its body
// carries nextExpiresAt, when the next one is deleted.
const ScratchAppLimitCode = "scratch_app_limit"

// MaxScratchBody is the most PUT /dataapp/scratch reads: echangelist's
// MaxPreviewsJSON, the most a change list carries, since a test app that
// could never be proposed tests nothing.
const MaxScratchBody = 24 << 20

// ScratchApp is what one push of a folder as a test app made. Previous is
// "kept" when the sent id was rewritten, "gone" when a new one replaced it,
// and empty when no id was sent.
type ScratchApp struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	Previous  string    `json:"previous,omitempty"`
}

// PushScratchApp sends body, an already encoded ScratchBody, as this folder's
// test app.
func (c *Client) PushScratchApp(ctx context.Context, body []byte) (*ScratchApp, error) {
	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("Content-Type", "application/json")
	const path = "/api/v2/dataapp/scratch"
	resp, err := c.DoRaw(ctx, http.MethodPut, path, header, BytesBody(body), slowRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Status < 200 || resp.Status > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, errorFrom(resp.Status, raw)
	}
	var out ScratchApp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode the response to PUT %s: %w", c.BaseURL+path, err)
	}
	return &out, nil
}
