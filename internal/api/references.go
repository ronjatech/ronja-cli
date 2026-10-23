package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// The feature REFERENCE INVENTORY (GET /api/v2/feature/:id/references): every
// live root resource in a feature the caller can read, and every place each one
// references another resource — the server's own scan, the one a save binds
// with. `ronja import feature` rewrites references from it and from nothing
// else, so it never needs a scanner of its own.
//
// Every shape mirrors backend/engine/erefs/view.go on the branch that added the
// route. The position, direction and kind
// strings are a wire contract there ("values may be added, never renamed"), so
// this file branches on them and never on prose.

// Reference directions (erefs/vocab.go Dir*).
const (
	RefDirRead       = "read"
	RefDirWrite      = "write"
	RefDirReadWrite  = "read_write"
	RefDirRun        = "run"
	RefDirSideEffect = "side_effect"
)

// Reference positions (erefs/vocab.go Pos*).
const (
	RefPosRefMarker         = "ref_marker"
	RefPosOptionsQueryRef   = "options_query_ref"
	RefPosWriteMarker       = "write_marker"
	RefPosOutputLiteral     = "output_literal"
	RefPosSecretMarker      = "secret_marker"
	RefPosQuerySecretMarker = "query_secret_marker"
	RefPosAgentMarker       = "agent_marker"
	RefPosCodexMarker       = "codex_marker"
	RefPosMailboxMarker     = "mailbox_marker"
	RefPosWorkflowMarker    = "workflow_marker"
	RefPosModuleMarker      = "module_marker"
	RefPosFileMarker        = "file_marker"
	RefPosFolderLiteral     = "folder_literal"
	RefPosStructuredField   = "structured_field"
	RefPosMetricSource      = "metric_source"
	RefPosAppAccess         = "app_access"
	RefPosAppRefMarker      = "app_ref_marker"
	RefPosAppLiteral        = "app_literal"
	RefPosAutomationAction  = "automation_action"
	RefPosAutomationRef     = "automation_reference"
	RefPosAutomationTrigger = "automation_trigger"
	RefPosAgentReference    = "agent_reference"
	RefPosNoteMarker        = "note_marker"
)

// Resource and target kinds (erefs/vocab.go Kind*).
const (
	RefKindTable      = "table"
	RefKindMetric     = "metric"
	RefKindWorkflow   = "workflow"
	RefKindDataApp    = "dataapp"
	RefKindModule     = "module"
	RefKindAutomation = "automation"
	RefKindAgent      = "agent"
	RefKindNote       = "note"
	RefKindSecret     = "secret"
	RefKindCodex      = "codex"
	RefKindMCPServer  = "mcpserver"
	RefKindMailbox    = "mailbox"
	RefKindFeature    = "feature"
	RefKindFolder     = "folder"
)

// FeatureReferences is one page of the inventory (erefs.View).
type FeatureReferences struct {
	FeatureID    string              `json:"featureID"`
	FeatureName  string              `json:"featureName"`
	Resources    []ReferenceResource `json:"resources"`
	Next         string              `json:"next,omitempty"`
	OmittedKinds []OmittedRefKind    `json:"omittedKinds"`
}

// OmittedRefKind is a kind the call asked for and the token may not read
// (erefs.OmittedKind).
type OmittedRefKind struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Scope  string `json:"scope,omitempty"`
}

// ReferenceResource is one listed resource (erefs.ResourceView).
type ReferenceResource struct {
	Kind            string              `json:"kind"`
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	TableKind       string              `json:"tableKind,omitempty"`
	OwnerWorkflowID string              `json:"ownerWorkflowID,omitempty"`
	LogicalName     string              `json:"logicalName,omitempty"`
	References      []ReferencePosition `json:"references"`
}

// ReferencePosition is one reference (erefs.ReferenceView). When Readable is
// false the target row's facts — name, feature, table kind — are withheld, and
// the reference must be read as pointing OUTSIDE the feature.
type ReferencePosition struct {
	Position        string `json:"position"`
	Direction       string `json:"direction"`
	TargetKind      string `json:"targetKind"`
	TargetID        string `json:"targetID,omitempty"`
	TargetPath      string `json:"targetPath,omitempty"`
	Readable        bool   `json:"readable"`
	TargetName      string `json:"targetName,omitempty"`
	TargetFeatureID string `json:"targetFeatureID,omitempty"`
	TargetTableKind string `json:"targetTableKind,omitempty"`
	OwnOutput       bool   `json:"ownOutput,omitempty"`
	LogicalName     string `json:"logicalName,omitempty"`
	NameForm        bool   `json:"nameForm,omitempty"`
	Arg             string `json:"arg,omitempty"`
	Path            string `json:"path,omitempty"`
	Line            int    `json:"line,omitempty"`
	Field           string `json:"field,omitempty"`
	Call            string `json:"call,omitempty"`
	SecretField     string `json:"secretField,omitempty"`
}

// Writes reports whether the position may change its target (erefs
// Direction.Writes).
func (r ReferencePosition) Writes() bool {
	return r.Direction == RefDirWrite || r.Direction == RefDirReadWrite
}

// FeatureReferencesPage reads one page. after is the previous page's Next; limit
// 0 takes the server's default (500).
func (c *Client) FeatureReferencesPage(ctx context.Context, featureID, after string, limit int) (*FeatureReferences, error) {
	query := url.Values{}
	if after != "" {
		query.Set("after", after)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "feature/" + url.PathEscape(featureID) + "/references"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var out FeatureReferences
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// maxReferencePages bounds the walk, so a server that kept answering a cursor
// could not hold the command for ever. 500 pages of 500 resources is far past
// any feature the inventory can describe.
const maxReferencePages = 500

// FeatureReferences walks every page of a feature's inventory and returns the
// resources of all of them, with the first page's name and omitted kinds (every
// page carries the same list). The walk ends when `next` is absent — a page can
// be short, even empty, and still have a next one — never when a page is short.
func (c *Client) FeatureReferences(ctx context.Context, featureID string, limit int) (*FeatureReferences, error) {
	var out *FeatureReferences
	seen := map[string]bool{}
	after := ""
	for page := 0; ; page++ {
		if page >= maxReferencePages {
			return nil, fmt.Errorf("the reference inventory of %s did not end after %d pages", featureID, maxReferencePages)
		}
		got, err := c.FeatureReferencesPage(ctx, featureID, after, limit)
		if err != nil {
			return nil, err
		}
		next, page := got.Next, got.Resources
		if out == nil {
			out = got
			out.Resources = nil
		}
		// One resource is listed once: a page boundary that moved under a
		// concurrent write must not make one row two folders.
		for _, res := range page {
			key := res.Kind + "\x00" + res.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Resources = append(out.Resources, res)
		}
		if next == "" {
			out.Next = ""
			return out, nil
		}
		if next == after {
			return nil, fmt.Errorf("the reference inventory of %s returned the same cursor twice", featureID)
		}
		after = next
	}
}
