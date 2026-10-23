package api

import "context"

// Policy mirrors the fields of the organization policy document (PolicyView in
// backend/api/v2/policy/views.go) that `context` renders; a field named here
// that the server does not serialize decodes to its zero value in silence.
type Policy struct {
	Content string `json:"content"`
	// Version is the whole document's compare-and-swap token, the
	// expectedVersion a `ronja api -X PUT /api/v2/policy/org` of its Content
	// must carry.
	Version int `json:"version"`
	// UpdatedAt is absent for a document nobody has written yet: the read does
	// not create a row.
	UpdatedAt string `json:"updatedAt,omitempty"`
	// General is the document before its first people-property section
	// ("## For job_role=ceo"). Absent from an instance older than sections.
	General *string `json:"general,omitempty"`
	// Sections are the document's people-property sections; `context` reads
	// only whether there are any.
	Sections []PolicySection `json:"sections,omitempty"`
	// GeneralPart is set by `context`, never served: Content holds the general
	// part, verbatim, which is the expectedContent of its part write. Version is
	// the whole document's and is not offered: a whole-document PUT of this
	// content at it would drop every section.
	GeneralPart bool `json:"-"`
}

// PolicySection is one people-property section of the organization policy.
type PolicySection struct {
	PropertyKey string `json:"propertyKey"`
	ValueKey    string `json:"valueKey"`
}

// AppliedPolicy mirrors GET /api/v2/policy/applied (AppliedView): what Ronja
// is told about how to work with the signed-in person.
type AppliedPolicy struct {
	General  AppliedDocument  `json:"general"`
	Sections []AppliedSection `json:"sections"`
	// Own is the person's own policy, served only when the caller is that
	// person and has written one; an API token has no person, so never gets it.
	Own *AppliedDocument `json:"own,omitempty"`
}

// AppliedDocument is one document's text and version. General's version is
// the whole organization policy's, so it is never paired with General's
// content in a PUT.
type AppliedDocument struct {
	Content string `json:"content"`
	Version int    `json:"version"`
}

// AppliedSection is one organization-policy section for a value the person
// holds.
type AppliedSection struct {
	PropertyKey   string `json:"propertyKey"`
	ValueKey      string `json:"valueKey"`
	PropertyLabel string `json:"propertyLabel"`
	ValueLabel    string `json:"valueLabel"`
	Content       string `json:"content"`
	Because       string `json:"because"`
}

// OrgPolicy reads the organization's policy. Every member clears the route's
// role floor, so a 403 means a scoped token lacking analytics:read; unlike Me
// it is not admin-scoped, which is why `context` reads it even when Me fails.
func (c *Client) OrgPolicy(ctx context.Context) (*Policy, error) {
	var out Policy
	if err := c.Do(ctx, "GET", "policy/org", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AppliedPolicy reads what applies to the signed-in person. A role-bound API
// token has no person behind it and is answered with the general part alone;
// an instance older than the route answers 404, where `context` falls back to
// OrgPolicy.
func (c *Client) AppliedPolicy(ctx context.Context) (*AppliedPolicy, error) {
	var out AppliedPolicy
	if err := c.Do(ctx, "GET", "policy/applied", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
