package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The device-authorization flow. Mirrors backend/api/v2/authentication/
// api_cli_auth.go — the poll error codes are a wire contract, so they are
// matched by exact string.
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
	// UserID and TenantID are unused by the CLI today. They are kept because
	// this struct is a faithful mirror of the server's response, and a mirror
	// that quietly omits fields is one nobody can check against the source.
	UserID   string `json:"userID"`
	TenantID string `json:"tenantID"`
	// ExpiresAt is RFC 3339. CLI-minted tokens are time-bounded, and a
	// credential that stops working with no warning is a bad surprise.
	ExpiresAt string `json:"expiresAt"`
}

// Me mirrors the /authentication/me response (MeResponse in
// backend/api/v2/authentication/api_login.go). Tenant and Role are absent for a
// user who has not selected an organization.
//
// This is a HAND-MIRROR of server types, so the field names have to come from
// the structs that are actually serialized, not from what a CLI would like to
// receive:
//
//   - User is a *rdb.User — backend/ronja/rdb/table_user.go is the source of
//     truth. It has first_name / last_name and NO "name" field.
//   - Role is a *rdb.Role — backend/ronja/rdb/table_roles.go — which is keyed
//     on name and has NO id column.
//
// Keeping this in step with those files is load-bearing (see CLAUDE.md): a
// field that does not exist on the wire decodes to the zero value silently, so
// the mistake shows up as a blank line of output rather than an error.
type Me struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		// Serialized from optional.V[string], so these are `null` rather than
		// absent when unset; encoding/json leaves the zero value in place.
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

// PollDevice waits for a human to approve the flow, then returns the minted
// token. It blocks until approval, denial, expiry, or ctx cancellation.
//
// The server's advertised interval is the starting cadence; a slow_down
// response backs it off, because the server enforces the throttle and an
// over-eager client would otherwise never make progress.
//
// TRANSIENT failures are tolerated rather than fatal, and the tolerance is
// measured in ELAPSED TIME (MaxPollFailureWindow), not in retries. A login has
// a ~10 minute budget during which a human is walking to a browser, and in that
// window a rolling deploy, a 429 from the global rate limiter or a dropped
// connection are all entirely ordinary. Aborting on the first one would burn a
// flow the user then has to restart for no reason — and a retry COUNT quietly
// made the real tolerance a function of the cadence, so it came out shorter
// than an ordinary deploy takes. The RFC-defined outcomes (access_denied,
// expired_token, invalid_grant) are still terminal — those are the server's
// considered answer, not a blip. The deadline remains the backstop so this
// always terminates.
func (c *Client) PollDevice(ctx context.Context, start *DeviceStart) (*DeviceToken, error) {
	base := time.Duration(start.Interval) * time.Second
	if base < c.MinPollInterval {
		base = c.MinPollInterval
	}
	interval := base
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	// Zero means "no run of failures in progress"; it is set on the first
	// transient failure and cleared by any clean answer.
	var failingSince time.Time

	for {
		// Poll FIRST, then sleep. The server's throttle passes any first poll
		// (last_polled_at IS NULL), so sleeping up front only added a fixed
		// several-second delay to every login — including one the user has
		// already approved by the time the CLI gets here.
		var out DeviceToken
		err := c.Do(ctx, "POST", "authentication/cli/token",
			map[string]string{"deviceCode": start.DeviceCode}, &out)
		if err == nil {
			return &out, nil
		}

		switch CodeOf(err) {
		case ErrAuthorizationPending:
			// Expected: the human has not decided yet.
			failingSince = time.Time{}
			interval = base
		case ErrSlowDown:
			// Capped, and reset on the next clean answer: an uncapped
			// monotonic climb would eventually poll less often than the flow
			// has life left.
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
			// Anything else is either a transport error (no *Error at all) or
			// a server-side status that says nothing about this flow. Retry a
			// bounded number of times, then give up with the real error so a
			// genuinely broken instance is not hidden behind a silent loop.
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
			// Back off on 429 specifically. The others say nothing about load,
			// so they resume the server's own advertised cadence; a 429 is the
			// global rate limiter, and answering it at the same rate is a
			// promise to keep causing the condition it is reporting.
			if StatusOf(err) == 429 {
				interval += c.RateLimitBackoff
				if interval > c.MaxPollInterval {
					interval = c.MaxPollInterval
				}
			} else {
				interval = base
			}
		}

		// Independent of the server's own expiry, so a flow cannot poll forever
		// if the server keeps answering "pending".
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

// isTransient reports whether a poll failure is worth retrying.
//
// A transport-level failure (no HTTP response at all) always is. Of the HTTP
// statuses, 429 comes from the server-wide rate limiter and 5xx from a pod
// going away mid-deploy — neither is a verdict on this device code. A 4xx
// other than 429 is the server rejecting the request itself, which retrying
// will not fix.
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

// Me identifies the caller. It is the CLI's login verification: the endpoint
// returns the user, and their tenant and role when one is selected.
//
// Note that /api/v2/authentication is admin-scoped, so a SCOPED token cannot
// call it — which is why `login` mints a full-access Personal Access
// Token. A PAT still never exceeds its user's live role.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.Do(ctx, "GET", "authentication/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
