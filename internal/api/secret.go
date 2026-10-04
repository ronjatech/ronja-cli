package api

import (
	"context"
	"encoding/json"
	"net/url"
)

// CreateSecretInput is the POST /secret body (rsecret.CreateInput server-side).
//
// Credentials is the one field that carries a secret value. It is held as a
// RawMessage so the caller decides its exact bytes — a database blob read from a
// file is sent as the object it was, numbers and all — and it is never part of
// anything this package prints: the client has no logging, and an error carries
// only the server's answer.
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

// CreatedSecret is the metadata the create answers with. Deliberately narrow: the
// server never returns a value, and decoding only what is reported keeps it
// that way on this side too.
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

// OAuthInitiateInput is the POST /oauth/:service/initiate body.
//
// Scopes is a pointer for the server's reason: an ABSENT key replays the grant
// the connection already holds (plus the service's own scopes), while a present
// one REPLACES it with exactly the chosen set. nil is never sent.
type OAuthInitiateInput struct {
	FeatureID string    `json:"featureID,omitempty"`
	Scopes    *[]string `json:"scopes,omitempty"`
}

// OAuthInitiate is what initiate answers.
//
// AuthURL carries the one-shot `state`; it is not a Ronja credential, but it
// signs whoever completes it into THIS caller's secret, so a command shows it
// only on stderr and never in --json.
//
// RequestedScopes is nil on a server older than the field — a caller must then
// not judge "already connected" or a partial grant at all, since it cannot know
// what was asked for.
type OAuthInitiate struct {
	AuthURL         string    `json:"authURL"`
	SecretID        string    `json:"secretID"`
	RequestedScopes *[]string `json:"requestedScopes"`
	// URL is the secret's page in Ronja, where the person reads what is being
	// connected and presses Continue to reach the provider. Empty from a server
	// that predates it; the command then opens AuthURL as before.
	URL string `json:"url"`
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

// SecretStatus is the slice of GET /secret/:id a connect waits on. The route
// never returns a value; decoding only these fields keeps that true here too.
//
// GrantedScopes is written ONLY by the OAuth callback, so a change in it is a
// completed consent. updatedAt is deliberately absent: an ordinary background
// token refresh bumps it, and a wait keyed on it would report a sign-in that
// never happened.
type SecretStatus struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	FeatureID     *string   `json:"featureID"`
	GrantedScopes *[]string `json:"grantedScopes"`
}

// GetSecretStatus reads one secret's metadata.
func (c *Client) GetSecretStatus(ctx context.Context, id string) (*SecretStatus, error) {
	var out SecretStatus
	if err := c.Do(ctx, "GET", "secret/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IsTransient reports whether a poll failure is worth retrying — the same rule
// the login poll uses (a dropped connection, a 429, a 5xx), exported for the
// other handshakes' waits so there is one definition of "a blip".
func IsTransient(err error) bool { return isTransient(err) }

// CreatePendingSecretInput is the POST /secret/pending body
// (rsecret.CreatePendingInput server-side). It has no credentials field at all:
// the value is entered by a person on the secret's page in Ronja, and the
// caller never holds it.
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

// PendingSecret is what POST /secret/pending answers.
//
// URL is the secret's page, built and org-stamped by the server; a command
// opens it as given and never builds one. Reused is true when the caller's
// identical pending secret was returned rather than a new one created.
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
