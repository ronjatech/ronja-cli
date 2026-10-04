package api

// A table's health checks: list, create and edit (silencing is an edit with
// {"enabled": false}). Mirrored by hand from
// backend/api/v2/feature/api_model_check_authoring.go (the write bodies and
// TableCheckWriteResult) and backend/resource/rtablecheck/model.go (Check).
//
// There is no delete, here or anywhere over HTTP: only the in-app agent's
// setTableChecks deletes a check. A pipeline push re-creates a check a chat
// deleted if the folder's file still declares it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Check kinds and severities, mirrored from rtablecheck.
const (
	TableCheckKindExpression = "expression"
	TableCheckKindFreshness  = "freshness"
)

// TableNotBuiltCode is the `code` on the 409 a check write gets when the table
// has never built, so the expression could not be run — mirrored from
// feature.tableNotBuiltCode. A pipeline defers the check until a publish builds
// the table, rather than reporting it refused.
const TableNotBuiltCode = "table_not_built"

// TableCheck is one check as rtablecheck.Check serves it, the fields the CLI
// reads.
type TableCheck struct {
	ID                      string `json:"id"`
	TableID                 string `json:"tableID"`
	Kind                    string `json:"kind"`
	Name                    string `json:"name"`
	Description             string `json:"description,omitempty"`
	Expression              string `json:"expression,omitempty"`
	ExpectedIntervalSeconds int    `json:"expectedIntervalSeconds,omitempty"`
	Severity                string `json:"severity"`
	Enabled                 bool   `json:"enabled"`
	LastStatus              string `json:"lastStatus,omitempty"`
	LastDetail              string `json:"lastDetail,omitempty"`
}

// TableCheckVerdict is the verdict a write produced when it ran the check.
type TableCheckVerdict struct {
	Status   string `json:"status"`
	Observed string `json:"observed,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// TableCheckWriteResult is the answer to both writes: the check as it now
// stands (flattened, as the server embeds it), plus what the call did.
type TableCheckWriteResult struct {
	TableCheck
	Updated bool               `json:"updated"`
	ReRan   bool               `json:"reRan"`
	Verdict *TableCheckVerdict `json:"verdict,omitempty"`
}

// CreateTableCheckInput is the POST body. The optional fields are pointers with
// omitempty, so an absent one is absent on the wire and takes the server's
// default — never an explicit empty value, which the server refuses.
type CreateTableCheckInput struct {
	Name                  string  `json:"name"`
	Description           *string `json:"description,omitempty"`
	Expression            *string `json:"expression,omitempty"`
	ExpectedIntervalHours *int    `json:"expectedIntervalHours,omitempty"`
	Severity              *string `json:"severity,omitempty"`
	// Enabled false creates the check silenced, in the same write. Omitted
	// (nil) is enabled.
	Enabled *bool `json:"enabled,omitempty"`
}

// UpdateTableCheckInput is the PUT body; absent means unchanged.
type UpdateTableCheckInput struct {
	Name                  *string `json:"name,omitempty"`
	Description           *string `json:"description,omitempty"`
	Expression            *string `json:"expression,omitempty"`
	ExpectedIntervalHours *int    `json:"expectedIntervalHours,omitempty"`
	Severity              *string `json:"severity,omitempty"`
	Enabled               *bool   `json:"enabled,omitempty"`
}

// ListTableChecks reads every check on a table, silenced ones included.
func (c *Client) ListTableChecks(ctx context.Context, tableID string) ([]TableCheck, error) {
	var out []TableCheck
	if err := c.Do(ctx, "GET", "feature/model/"+url.PathEscape(tableID)+"/checks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateTableCheck adds a check. The server RUNS it before storing it, so this
// is a slow call: a 400 means it could not be evaluated and was NOT saved.
func (c *Client) CreateTableCheck(ctx context.Context, tableID string, in CreateTableCheckInput) (*TableCheckWriteResult, error) {
	var out TableCheckWriteResult
	if err := c.doSlow(ctx, "POST", "feature/model/"+url.PathEscape(tableID)+"/checks", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateTableCheck edits a check. A changed expression or interval is run first.
func (c *Client) UpdateTableCheck(ctx context.Context, tableID, checkID string, in UpdateTableCheckInput) (*TableCheckWriteResult, error) {
	var out TableCheckWriteResult
	if err := c.doSlow(ctx, "PUT", "feature/model/"+url.PathEscape(tableID)+"/checks/"+url.PathEscape(checkID), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrorCodeOf reads the `code` discriminator a coded error carries beside its
// `error` message ({"error": "...", "code": "table_not_built"}), or "".
func ErrorCodeOf(err error) string {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil {
		return ""
	}
	return body.Code
}

// ErrorMessageOf is the server's own sentence from a JSON error body, falling
// back to the error's rendering.
func ErrorMessageOf(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(apiErr.Body), &body) == nil && body.Error != "" {
			return body.Error
		}
	}
	return err.Error()
}

// IsUnmatchedRoute reports a server that does not have the route at all: a 405,
// or a 404 whose body is NOT JSON — the router's plain-text breadcrumb for an
// unmatched /api path (backend/ronja/server.go apiNotFoundBody). A JSON 404 is a
// handler's answer about the resource (a table the caller cannot read), and is
// deliberately not this.
func IsUnmatchedRoute(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case http.StatusMethodNotAllowed:
		return true
	case http.StatusNotFound:
		body := strings.TrimSpace(apiErr.Body)
		return !json.Valid([]byte(body)) || !strings.HasPrefix(body, "{")
	}
	return false
}
