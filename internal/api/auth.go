package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The device-flow poll error codes are a wire contract with
// backend/api/v2/authentication/api_cli_auth.go, matched by exact string.
const (
	ErrAuthorizationPending = "authorization_pending"
	ErrSlowDown             = "slow_down"
	ErrExpiredToken         = "expired_token"
	ErrAccessDenied         = "access_denied"
	ErrInvalidGrant         = "invalid_grant"
)

// ErrDeviceDenied and ErrDeviceExpired are the terminal outcomes a caller is
// expected to explain to a human, as opposed to genuine failures.
var (
	ErrDeviceDenied  = errors.New("the request was declined in the browser")
	ErrDeviceExpired = errors.New("the code expired before it was approved")
)

type DeviceStart struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type DeviceToken struct {
	Token     string `json:"token"`
	TokenID   string `json:"tokenID"`
	TokenName string `json:"tokenName"`
	// UserID and TenantID are unused, kept so this mirrors the server's
	// response field for field.
	UserID   string `json:"userID"`
	TenantID string `json:"tenantID"`
	// ExpiresAt is RFC 3339.
	ExpiresAt string `json:"expiresAt"`
}

// Me hand-mirrors MeResponse in backend/api/v2/authentication/api_login.go,
// whose User is an *rdb.User (first_name / last_name, no "name") and Role an
// *rdb.Role (no id). A field missing on the wire decodes silently to its zero
// value, so the names must come from those structs. Tenant and Role are nil
// before an organization is selected.
type Me struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		// `null` when unset (optional.V on the server).
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"user"`
	Tenant *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"tenant,omitempty"`
	Role *struct {
		Name           string `json:"name"`
		PrivilegeLevel int    `json:"privilegeLevel"`
	} `json:"role,omitempty"`
	IsRonjaStaff bool `json:"isRonjaStaff"`
}

// StartDevice opens a device-authorization flow. No authentication required.
func (c *Client) StartDevice(ctx context.Context, clientName string) (*DeviceStart, error) {
	var out DeviceStart
	err := c.Do(ctx, "POST", "authentication/cli/device",
		map[string]string{"clientName": clientName}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PollDevice blocks until the flow is approved, denied or expired, or ctx ends.
// Transient failures are tolerated for MaxPollFailureWindow of ELAPSED time, not
// a retry count, which would tie the tolerance to the cadence and come out
// shorter than a rolling deploy; the RFC outcomes stay terminal.
func (c *Client) PollDevice(ctx context.Context, start *DeviceStart) (*DeviceToken, error) {
	base := time.Duration(start.Interval) * time.Second
	if base < c.MinPollInterval {
		base = c.MinPollInterval
	}
	interval := base
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	// Zero: no run of failures in progress.
	var failingSince time.Time

	for {
		// Poll FIRST, then sleep: the server's throttle passes any first poll
		// (last_polled_at IS NULL).
		var out DeviceToken
		err := c.Do(ctx, "POST", "authentication/cli/token",
			map[string]string{"deviceCode": start.DeviceCode}, &out)
		if err == nil {
			return &out, nil
		}

		switch CodeOf(err) {
		case ErrAuthorizationPending:
			failingSince = time.Time{}
			interval = base
		case ErrSlowDown:
			// Capped, and reset on the next clean answer, so it never polls less
			// often than the flow has life left.
			failingSince = time.Time{}
			interval += c.PollBackoff
			if interval > c.MaxPollInterval {
				interval = c.MaxPollInterval
			}
		case ErrAccessDenied:
			return nil, ErrDeviceDenied
		case ErrExpiredToken:
			return nil, ErrDeviceExpired
		case ErrInvalidGrant:
			return nil, fmt.Errorf("this login was already completed; run login again")
		default:
			// A transient error is retried for MaxPollFailureWindow, then
			// surfaced so a broken instance is not hidden behind a silent loop.
			if !isTransient(err) {
				return nil, err
			}
			now := time.Now()
			if failingSince.IsZero() {
				failingSince = now
			}
			if failing := now.Sub(failingSince); failing > c.MaxPollFailureWindow {
				return nil, fmt.Errorf("gave up after %s of consecutive failures: %w",
					failing.Round(time.Second), err)
			}
			// Only a 429 (the global rate limiter) says anything about load, so
			// only it backs off.
			if StatusOf(err) == 429 {
				interval += c.RateLimitBackoff
				if interval > c.MaxPollInterval {
					interval = c.MaxPollInterval
				}
			} else {
				interval = base
			}
		}

		// Independent of the server's expiry, so a flow cannot poll forever on
		// "pending".
		if time.Now().After(deadline) {
			return nil, ErrDeviceExpired
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// isTransient: a transport failure, a 429 or a 5xx is no verdict on this device
// code; any other 4xx is, and retrying will not fix it.
func isTransient(err error) bool {
	status := StatusOf(err)
	switch {
	case status == 0:
		return true // no HTTP response: DNS, connection reset, timeout
	case status == 429:
		return true
	case status >= 500:
		return true
	default:
		return false
	}
}

// Me identifies the caller, the CLI's login verification. /api/v2/authentication
// is admin-scoped, so a scoped token without that scope fails here, which is why
// the device flow mints a full-access PAT; a PAT still never exceeds its user's
// live role.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.Do(ctx, "GET", "authentication/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
