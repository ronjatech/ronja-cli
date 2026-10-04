package api

import (
	"context"
	"net/url"
	"time"
)

// The token half of the client is two READS. The CLI never mints a Ronja token:
// POST /authentication/token is for a person's browser alone (the server
// refuses every token on it), and nothing here calls it.

// TokenLink is GET /authentication/token-link: the Account page's create-token
// form, pre-filled and stamped with the organization. It is a link, not a
// credential — a person opens it and decides.
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

// MyToken is one row of GET /authentication/token-mine.
//
// Deliberately WITHOUT the row's `token` field. The route blanks it anyway, but
// a field this struct does not declare is one the decoder drops on the floor,
// so no future server answer can carry a credential into anything a command
// prints.
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
