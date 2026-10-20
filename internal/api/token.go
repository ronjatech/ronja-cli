package api

import (
	"context"
	"net/url"
	"time"
)

// The CLI never mints a Ronja token: POST /authentication/token is for a
// person's browser alone (the server refuses every token on it).

// TokenLink is GET /authentication/token-link: the Account page's pre-filled
// create-token form. It is a link, not a credential — a person opens it and decides.
type TokenLink struct {
	URL string `json:"url"`
}

// GetTokenLink asks the server to build and validate the pre-filled link. An
// empty expires is a token that never expires.
func (c *Client) GetTokenLink(ctx context.Context, name string, scopes []string, expires string) (*TokenLink, error) {
	query := url.Values{}
	query.Set("name", name)
	query["scope"] = scopes
	if expires != "" {
		query.Set("expires", expires)
	}
	var out TokenLink
	if err := c.Do(ctx, "GET", "authentication/token-link?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MyToken is one row of GET /authentication/token-mine. It deliberately omits
// the row's `token` field, so no server answer can carry a credential into
// anything a command prints.
type MyToken struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	ScopeGrants map[string]string `json:"scopeGrants"`
	ExpiresAt   *time.Time        `json:"expiresAt"`
}

// ListMyTokens reads the caller's own personal access tokens.
func (c *Client) ListMyTokens(ctx context.Context) ([]MyToken, error) {
	var out []MyToken
	if err := c.Do(ctx, "GET", "authentication/token-mine", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
