package api

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
)

// CreateSecretInput is the POST /secret body (rsecret.CreateInput server-side).
// Credentials, the one field carrying a secret value, is a RawMessage so a blob
// read from a file is sent byte for byte; nothing in this package prints it, and
// an error carries only the server's answer.
type CreateSecretInput struct {
	FeatureID   string          `json:"featureID"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	SecretType  string          `json:"secretType,omitempty"`
	Credentials json.RawMessage `json:"credentials"`
	AllowedURLs []string        `json:"allowedURLs,omitempty"`
	FieldSchema json.RawMessage `json:"fieldSchema,omitempty"`
	AgentFields []string        `json:"agentFields,omitempty"`
	Dialect     string          `json:"dialect,omitempty"`
}

// CreatedSecret is the metadata the create answers with. Deliberately narrow:
// decoding no value field keeps a secret value out of this side too.
type CreatedSecret struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	SecretType string  `json:"secretType"`
	FeatureID  *string `json:"featureID"`
}

// CreateSecret stores a new secret with its value in one request.
func (c *Client) CreateSecret(ctx context.Context, in CreateSecretInput) (*CreatedSecret, error) {
	var out CreatedSecret
	if err := c.Do(ctx, "POST", "secret", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OAuthInitiateInput is the POST /oauth/:service/initiate body. Scopes is a
// pointer because an ABSENT key replays the connection's existing grant, while a
// present one REPLACES it with exactly the chosen set.
type OAuthInitiateInput struct {
	FeatureID string    `json:"featureID,omitempty"`
	Scopes    *[]string `json:"scopes,omitempty"`
	// Link prepares the connection and answers a link anyone may finish the
	// sign-in from, instead of starting one.
	Link bool `json:"link,omitempty"`
	// ExpectedCompany is a pointer because a present blank is the server's 400,
	// where an absent one prepares a link for any company.
	ExpectedCompany *string `json:"expectedCompany,omitempty"`
	Replace         bool    `json:"replace,omitempty"`
}

// OAuthInitiate is what initiate answers. AuthURL carries the one-shot `state`
// and signs whoever completes it into THIS caller's secret, so a command shows it
// only on stderr, never in --json. A nil RequestedScopes (an older server) means
// a caller must not judge "already connected" or a partial grant at all.
type OAuthInitiate struct {
	AuthURL         string    `json:"authURL"`
	SecretID        string    `json:"secretID"`
	RequestedScopes *[]string `json:"requestedScopes"`
	// URL is the secret's page in Ronja, where the person presses Continue to
	// reach the provider; empty from an older server, and the command then opens AuthURL.
	// With Link it is the prepared link instead, a bearer credential.
	URL string `json:"url"`
	// ExpiresAt is set only on a prepared link.
	ExpiresAt *time.Time `json:"expiresAt"`
}

// InitiateOAuth starts a Ronja-managed OAuth connection for one registry
// service, creating the caller's pending provider secret when there is none.
func (c *Client) InitiateOAuth(ctx context.Context, service string, in OAuthInitiateInput) (*OAuthInitiate, error) {
	var out OAuthInitiate
	if err := c.Do(ctx, "POST", "oauth/"+url.PathEscape(service)+"/initiate", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SecretStatus is the slice of GET /secret/:id the CLI's waits read.
// GrantedScopes is written ONLY by the OAuth callback, so a change in it is a
// completed consent; ValueChangedAt moves only when a stored value is replaced.
// updatedAt is deliberately absent because a background token refresh or a
// login bumps it, and a wait keyed on it would report something that never happened.
type SecretStatus struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	FeatureID     *string   `json:"featureID"`
	GrantedScopes *[]string `json:"grantedScopes"`
	// NewValue is nil from a server older than the new-value form.
	NewValue        *SecretNewValue `json:"newValue"`
	ValueChangedAt  *time.Time      `json:"valueChangedAt"`
	LastCheckedAt   *time.Time      `json:"lastCheckedAt"`
	LastCheckStatus string          `json:"lastCheckStatus"`
	LastCheckError  string          `json:"lastCheckError"`
	LastCheckSource string          `json:"lastCheckSource"`
}

// SecretNewValue holds either the link to the form for a new value, built and
// org-stamped by the server, or the sentence saying why the caller may not
// enter one.
type SecretNewValue struct {
	URL     string `json:"url"`
	Refusal string `json:"refusal"`
}

// GetSecretStatus reads one secret's metadata.
func (c *Client) GetSecretStatus(ctx context.Context, id string) (*SecretStatus, error) {
	var out SecretStatus
	if err := c.Do(ctx, "GET", "secret/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IsTransient reports whether a poll failure is worth retrying (a dropped
// connection, a 429, a 5xx): the login poll's rule, exported so every wait shares it.
func IsTransient(err error) bool { return isTransient(err) }

// CreatePendingSecretInput is the POST /secret/pending body
// (rsecret.CreatePendingInput server-side). It has no credentials field: a person
// enters the value on the secret's page in Ronja, and the caller never holds it.
type CreatePendingSecretInput struct {
	FeatureID   string          `json:"featureID"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	SecretType  string          `json:"secretType,omitempty"`
	AllowedURLs []string        `json:"allowedURLs,omitempty"`
	FieldSchema json.RawMessage `json:"fieldSchema,omitempty"`
	AgentFields []string        `json:"agentFields,omitempty"`
	Dialect     string          `json:"dialect,omitempty"`
}

// PendingSecret is what POST /secret/pending answers. URL is built and
// org-stamped by the server, and a command opens it as given, never building one.
// Reused is true when the caller's identical pending secret was returned.
type PendingSecret struct {
	Secret   CreatedSecret    `json:"secret"`
	URL      string           `json:"url"`
	Reused   bool             `json:"reused"`
	Existing []ExistingSecret `json:"existing"`
}

// ExistingSecret is one secret the caller can already reach that points at the
// same system — a notice, never a refusal.
type ExistingSecret struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SecretType  string `json:"secretType"`
	Status      string `json:"status"`
	ProviderKey string `json:"providerKey"`
	OwnedByYou  bool   `json:"ownedByYou"`
}

// CreatePendingSecret starts a secret whose value a person enters in Ronja.
func (c *Client) CreatePendingSecret(ctx context.Context, in CreatePendingSecretInput) (*PendingSecret, error) {
	var out PendingSecret
	if err := c.Do(ctx, "POST", "secret/pending", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
