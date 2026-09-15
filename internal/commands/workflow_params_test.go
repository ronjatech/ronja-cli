package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// `--param body=` sends an empty string, which the server refuses on a required
// parameter without a default. Catching it here costs no round trip, and the
// CLI must never accept what the server refuses.
func TestParseParams_BlankRequiredIsRefused(t *testing.T) {
	required := []api.WorkflowParameter{{Name: "body", Type: "string", Required: true}}
	for _, spec := range []string{"body=", "body=  "} {
		_, err := parseParams([]string{spec}, required)
		if err == nil || !strings.Contains(err.Error(), `"body"`) || !strings.Contains(err.Error(), "was passed empty") {
			t.Fatalf("--param %s on a required parameter: want an error naming body, got %v", spec, err)
		}
	}

	// A parameter left out entirely keeps the missing wording.
	if _, err := parseParams(nil, required); err == nil || !strings.Contains(err.Error(), "has no value and no default") {
		t.Fatalf("no --param on a required parameter: want a has-no-value error, got %v", err)
	}

	optional := []api.WorkflowParameter{{Name: "body", Type: "string"}}
	if _, err := parseParams([]string{"body="}, optional); err != nil {
		t.Fatalf("--param body= on an optional parameter must be accepted, got %v", err)
	}

	defaulted := []api.WorkflowParameter{{Name: "body", Type: "string", Required: true, DefaultValue: "x"}}
	if _, err := parseParams([]string{"body="}, defaulted); err != nil {
		t.Fatalf("--param body= on a defaulted parameter must be accepted, got %v", err)
	}

	// A static select's blank is refused as a non-option, as the server does —
	// not as missing, which would suggest a value "" can never be.
	staticSelect := []api.WorkflowParameter{{Name: "body", Type: "select", Required: true, Options: []string{"Q1", "Q2"}}}
	if _, err := parseParams([]string{"body="}, staticSelect); err == nil || !strings.Contains(err.Error(), "is not one of its options") {
		t.Fatalf("--param body= on a static select without a blank option: want a not-an-option error, got %v", err)
	}

	// Options mean nothing on a non-select (the server ignores them), so they
	// must not exempt a blank from the required check.
	stringWithOptions := []api.WorkflowParameter{{Name: "body", Type: "string", Required: true, Options: []string{"a", "b"}}}
	for _, spec := range []string{"body=", "body=  "} {
		if _, err := parseParams([]string{spec}, stringWithOptions); err == nil || !strings.Contains(err.Error(), "was passed empty") {
			t.Fatalf("--param %s on a required string declaring options: want a passed-empty error, got %v", spec, err)
		}
	}

	withBlankOption := []api.WorkflowParameter{{Name: "body", Type: "select", Required: true, Options: []string{"", "Q1"}}}
	if _, err := parseParams([]string{"body="}, withBlankOption); err != nil {
		t.Fatalf("a declared blank option must be accepted, got %v", err)
	}
}
