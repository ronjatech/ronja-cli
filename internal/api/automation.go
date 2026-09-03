package api

// The automation surface: read one, list a feature's, create, update, delete.
//
// One file rather than the read/write split workflow.go and table.go carry,
// because there is much less of it — an automation has no draft, no version
// history, no file sync and no build, so the whole loop is five calls.
//
// Same hand-mirror rule as everywhere else in this package: every json tag below
// is copied from the server struct named in its comment, and the fixture test in
// automation_test.go is what holds them in step. A tag that does not exist on
// the wire decodes to the zero value SILENTLY, which here would read as an
// automation with no schedule, no trigger, or — the one that costs money — no
// action at all.
//
// ⚠️ READ AND WRITE DISAGREE ABOUT TWO FIELD NAMES, and that is the single
// sharpest trap on this surface. The ROW emits `emailAllowedFromAddrs` and
// `watchedTableIDs`; the create/update BODIES read `emailAllowedFromAddresses`
// and `watchedTableIds`. The server now accepts BOTH spellings on input, but
// sending both of one field is a 400 — so the types below use exactly one
// vocabulary per direction: Automation carries the ROW's names, and the two
// input types carry the INPUT's. They are deliberately not shared structs.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Automation trigger kinds, mirrored from the TriggerKind* consts in
// backend/resource/rscheduledjob. The CLI branches on these by exact string, so
// they are a wire contract in the same sense the workflow lifecycles are.
const (
	TriggerKindCron    = "cron"
	TriggerKindEmail   = "email"
	TriggerKindWebhook = "webhook"
	TriggerKindTable   = "table"
	TriggerKindEvent   = "event"
	TriggerKindMailbox = "mailbox"
)

// Automation action kinds, mirrored from the ActionKind* consts in the same
// package. `agent` runs the inline prompt; `workflow` runs a workflow;
// `saved_agent` runs a saved Agent, optionally with a per-automation seed.
const (
	ActionKindAgent      = "agent"
	ActionKindWorkflow   = "workflow"
	ActionKindSavedAgent = "saved_agent"
)

// AutomationReference is one resource a reference-based run may reach, mirrored
// from rscheduledjob.AutomationActionRefResponse — which is also the shape the
// create/update bodies take (workspace.referenceInput), so this one type serves
// both directions.
//
// Kind is the server's reference vocabulary and is NOT the CLI's dependency
// vocabulary: a table is `model` here, where markers.KindTable is "table". The
// full set is model, workflow, note, secret, dataapp, mcpserver, feature,
// mailbox (rscheduledjob.ValidateReferenceKind).
type AutomationReference struct {
	ResourceID string `json:"resourceID"`
	Kind       string `json:"kind"`
}

// AutomationMailboxFilter is the optional match filter on a mailbox trigger,
// mirrored from workspace.mailboxFilterInput — the same shape the row's
// `mailboxFilter` jsonb emits, so it serves both directions.
//
// Semantics: AND across fields, OR within a list, case-folded, and the two
// sender lists are ORed with each other. An absent or all-empty filter fires on
// every new message, which is a supported state.
//
// Label is carried even though the server REFUSES any non-empty value, for the
// server's own stated reason: Ronja has no per-message label projection, so a
// stored label could only ever evaluate to "never matches" — an automation that
// looks healthy and cannot fire. A type that dropped the field would turn a
// declared label into a filter BROADER than the author asked for, silently.
// Carrying it is what makes the refusal reachable.
type AutomationMailboxFilter struct {
	FromDomains       []string `json:"fromDomains,omitempty"`
	FromAddrs         []string `json:"fromAddrs,omitempty"`
	SubjectContains   []string `json:"subjectContains,omitempty"`
	RequireAttachment bool     `json:"requireAttachment,omitempty"`
	Label             string   `json:"label,omitempty"`
}

// AutomationActionConfig mirrors rscheduledjob.AutomationActionConfigResponse —
// the camelCase WIRE shape, not the snake_case storage one. Only the fields the
// kind in use populates are set.
type AutomationActionConfig struct {
	// Agent fields.
	Prompt             string                `json:"prompt,omitempty"`
	Model              *string               `json:"model,omitempty"`
	ReferenceBased     bool                  `json:"referenceBased,omitempty"`
	References         []AutomationReference `json:"references,omitempty"`
	ApprovedTools      []string              `json:"approvedTools,omitempty"`
	ApprovedToolGroups []string              `json:"approvedToolGroups,omitempty"`

	// Workflow fields.
	WorkflowID      string         `json:"workflowID,omitempty"`
	ParameterValues map[string]any `json:"parameterValues,omitempty"`

	// AgentID is the saved Agent a `saved_agent` action runs.
	AgentID string `json:"agentID,omitempty"`
}

// AutomationAction mirrors rscheduledjob.AutomationActionResponse.
//
// ⚠️ This is what GET :jobID gained on this branch. Before it the route returned
// the bare row and the only route carrying an action was LIST, so reading back
// the automation a folder had just written meant listing up to 200 rows for the
// whole organization and filtering client-side — and past that, silently not
// finding rows that exist. For a sync loop "I cannot find the row" is not a
// benign miss: it reads as gone, and the next push creates a second one.
type AutomationAction struct {
	ID             string                 `json:"id"`
	ScheduledJobID string                 `json:"scheduledJobID"`
	Ordinal        int                    `json:"ordinal"`
	Kind           string                 `json:"kind"`
	Config         AutomationActionConfig `json:"config"`
}

// Automation is a HAND-MIRROR of the subset of
// rscheduledjob.ScheduledJobWithActionResponse the CLI reads — the DTO create,
// update and GET :jobID all return, which is what lets a caller read a row and
// write it back without a second shape to learn.
//
// The DTO embeds rdb.ScheduledJob, so the row's own fields sit at the top level
// of the JSON object and are flattened here. Mirroring decisions, the same ones
// Workflow's doc records:
//
//   - The server's optional.V[T] marshals as the bare value or JSON `null`, and
//     unmarshalling `null` into a non-pointer is a documented no-op — so the
//     optional STRINGS are plain strings ("" is exactly what "unset" means for a
//     workspace or a mailbox) and the optional INTS are pointers, because
//     "inherit the organization default" and "an explicit 0" are different
//     answers a folder may mean.
//   - This is a SUBSET. Left out on purpose: nextRunAt / lastRunAt and the run
//     health columns (state a file must not own), emailAddressToken and
//     webhookToken (server-minted capabilities), the soft-delete and move
//     bookkeeping, and the audit stamps. A mirrored field nobody reads is a
//     field nobody keeps in step.
type Automation struct {
	ID          string `json:"id"`
	FeatureID   string `json:"featureID"`
	WorkspaceID string `json:"workspaceID"`

	Name        string `json:"name"`
	Description string `json:"description"`
	// Prompt is the inline-agent action's instructions. Empty on the workflow
	// and saved_agent kinds, which carry a sentinel server-side.
	Prompt string `json:"prompt"`

	// TriggerKind is IMMUTABLE. UpdateAutomationInput carries no field for it,
	// so AutomationPatch has none either — a folder that changed it is refused
	// by the CLI with the reason rather than having the field silently ignored,
	// and the remedy is the web app: DELETE is a 30-day soft delete the CLI
	// cannot empty (purge is human-only), so "delete and push again" is not an
	// executable answer.
	TriggerKind string `json:"triggerKind"`

	CronExpr string `json:"cronExpr"`
	Timezone string `json:"timezone"`
	// ReportingTimezone is the zone the run's queries bucket dates in, ORTHOGONAL
	// to Timezone above (trigger-WHEN vs report-IN) and accepted on every trigger
	// kind. "" means the row declares none and falls back to the organization
	// default — not the same as the literal "UTC" a reset writes.
	ReportingTimezone string `json:"reportingTimezone"`

	Enabled bool `json:"enabled"`
	// DisabledReason is why a disabled row is disabled: "" (nobody said), "user"
	// (a human paused it), "mailbox_disconnected", or one of the
	// marked-for-deletion family.
	//
	// ⚠️ Load-bearing for the re-enable gate, and asymmetric:
	// CallerMayClearDisabledReason admits "", "user" and
	// "mailbox_disconnected" and REFUSES the deletion family with an input
	// error. So a folder declaring enabled:true against a paused row is
	// ACCEPTED — silently reversing an incident response — while the same
	// folder against a row in a workspace pending deletion fails every push
	// with a message about something else entirely.
	DisabledReason string `json:"disabledReason"`

	EmailAllowedFromDomains []string `json:"emailAllowedFromDomains"`
	// ⚠️ THE ROW'S SPELLING. The create/update bodies read this field as
	// `emailAllowedFromAddresses`; see this file's header.
	EmailAllowedFromAddrs []string `json:"emailAllowedFromAddrs"`
	// ⚠️ THE ROW'S SPELLING, likewise: the bodies read `watchedTableIds`.
	WatchedTableIDs []string `json:"watchedTableIDs"`

	EventName     string                  `json:"eventName"`
	MailboxID     string                  `json:"mailboxID"`
	MailboxFilter AutomationMailboxFilter `json:"mailboxFilter"`

	RateLimitPerMinute *int `json:"rateLimitPerMinute"`
	RateLimitPerDay    *int `json:"rateLimitPerDay"`

	ReferenceBased     bool     `json:"referenceBased"`
	ApprovedTools      []string `json:"approvedTools"`
	ApprovedToolGroups []string `json:"approvedToolGroups"`
	Model              string   `json:"model"`

	// Private is the create-time visibility flag, READ ONLY here on purpose.
	// The update route refuses a value that CHANGES it (a matching echo is
	// accepted and does nothing), so AutomationPatch carries no such field —
	// a client that never sends one cannot get that refusal wrong.
	Private bool `json:"private"`
	// FeatureScope is the joined feature's scope (private|workspace|
	// organization), stamped at read time — the field to branch on for privacy
	// rather than Private above.
	FeatureScope string `json:"featureScope"`

	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is the row's last write, and it is this loop's ONLY drift
	// anchor. See Stamp for how it is spelled, and
	// wfdir.LockAutomation.UpdatedAt for why there is nothing else to anchor on
	// and what is refused with it.
	UpdatedAt time.Time `json:"updatedAt"`

	// Action is what runs when the trigger fires. It carries the reference set
	// too (inside Config), so a route that returns the action returns the
	// references — there is no second call to make.
	Action *AutomationAction `json:"action"`

	// EmailAddress / WebhookURL are the resolved per-automation receiver for an
	// email or webhook trigger, computed by the handler rather than stored.
	// Empty for the other kinds, and empty when the deployment has no ingest
	// domain or base URL configured — which is the ordinary state of a dev box,
	// so absence is never an error.
	EmailAddress string `json:"emailAddress"`
	WebhookURL   string `json:"webhookURL"`
	// WebhookHasSigningSecret reports whether HMAC verification is on. Only the
	// BOOLEAN is readable afterwards.
	WebhookHasSigningSecret bool `json:"webhookHasSigningSecret"`
	// WebhookSigningSecret is the real HMAC key, revealed ONCE in a create or
	// rotate response and never again — the get and list paths never set it. A
	// caller that does not store it here cannot get it back, and clearing a
	// secret is a human-only route, so a folder declares signing on or off at
	// create and can never re-assert it.
	WebhookSigningSecret string `json:"webhookSigningSecret"`

	// Warnings are NON-BLOCKING advisories about a write that SUCCEEDED — today
	// the two mailbox-trigger ones. A property of the WRITE, never of the row,
	// so they are absent on get and list and must be reported at the moment they
	// arrive or not at all.
	Warnings []string `json:"warnings"`
}

// Normalize replaces nil slices with empty ones.
//
// The server omits several of these when empty, so "no watched tables" and
// "field absent" arrive identically — and code that compares a folder's
// declaration against the row should not have to care which.
func (a *Automation) Normalize() {
	if a == nil {
		return
	}
	for _, slot := range []*[]string{
		&a.EmailAllowedFromDomains, &a.EmailAllowedFromAddrs, &a.WatchedTableIDs,
		&a.ApprovedTools, &a.ApprovedToolGroups, &a.Warnings,
	} {
		if *slot == nil {
			*slot = []string{}
		}
	}
	if a.Action != nil && a.Action.Config.References == nil {
		a.Action.Config.References = []AutomationReference{}
	}
}

// Stamp renders UpdatedAt as the drift anchor a folder's lock file records.
//
// A method rather than a caller-side Format for two reasons. The anchor is
// compared against itself across runs, so every writer has to spell it
// identically or the guard reports drift on a row nobody touched. And a ZERO
// time answers "" — the value that DISARMS the guard — rather than
// "0001-01-01T00:00:00Z", which is a perfectly well-formed anchor claiming
// agreement with a moment two thousand years ago and would never match again.
func (a *Automation) Stamp() string {
	if a == nil || a.UpdatedAt.IsZero() {
		return ""
	}
	return a.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

// AutomationActionInput is the `action` object on the create and update bodies,
// mirroring workspace.actionInput / actionConfigInput.
//
// A separate type from AutomationAction because it is a different shape: the
// wire config a client SENDS carries only the workflow and saved-agent fields
// (an inline agent action is built from the top-level fields for back-compat, so
// an agent action's config is ignored), and none of the response's ids.
type AutomationActionInput struct {
	Kind   string                      `json:"kind"`
	Config AutomationActionConfigInput `json:"config"`
}

// AutomationActionConfigInput is action.config as the server READS it.
type AutomationActionConfigInput struct {
	WorkflowID      string         `json:"workflowID,omitempty"`
	ParameterValues map[string]any `json:"parameterValues,omitempty"`
	AgentID         string         `json:"agentID,omitempty"`
	// Prompt is the per-automation seed message for a saved_agent action, not
	// the inline agent's instructions — those are the body's top-level `prompt`.
	Prompt string `json:"prompt,omitempty"`
}

// CreateAutomationInput is the body of POST /automations, mirroring the subset
// of workspace.CreateAutomationInput the CLI writes.
//
// It uses the INPUT vocabulary for the two mismatched fields
// (emailAllowedFromAddresses, watchedTableIds) and sends exactly one spelling of
// each — sending both is a 400.
//
// ⚠️ There is deliberately NO `private`. Visibility is create-only server-side
// and it stays out of the file schema entirely: a shared folder pushing
// private:true would make the row invisible to the next colleague who pushes,
// whose status then reports it gone and whose push creates a duplicate.
//
// The trigger-conditional fields are omitempty because the server REJECTS a
// field belonging to a different trigger kind rather than ignoring it, so a body
// must carry only what the chosen kind uses.
type CreateAutomationInput struct {
	FeatureID string `json:"featureID"`

	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt,omitempty"`

	// TriggerKind defaults to cron server-side when absent.
	TriggerKind string `json:"triggerKind,omitempty"`

	CronExpr          string `json:"cronExpr,omitempty"`
	Timezone          string `json:"timezone,omitempty"`
	ReportingTimezone string `json:"reportingTimezone,omitempty"`

	EmailAllowedFromDomains   []string `json:"emailAllowedFromDomains,omitempty"`
	EmailAllowedFromAddresses []string `json:"emailAllowedFromAddresses,omitempty"`
	WatchedTableIds           []string `json:"watchedTableIds,omitempty"`

	EventName     string                   `json:"eventName,omitempty"`
	MailboxID     string                   `json:"mailboxID,omitempty"`
	MailboxFilter *AutomationMailboxFilter `json:"mailboxFilter,omitempty"`

	// WebhookSigningSecret asks the server to mint an HMAC key, returned ONCE in
	// the response. Create-only: there is no way to re-assert it later, and
	// clearing one is a human-only route.
	WebhookSigningSecret *bool `json:"webhookSigningSecret,omitempty"`

	RateLimitPerMinute *int `json:"rateLimitPerMinute,omitempty"`
	RateLimitPerDay    *int `json:"rateLimitPerDay,omitempty"`

	// Enabled is a POINTER because absent is a legitimate third state: a folder
	// that does not declare `enabled` is not managing it, which is what stops a
	// hard-coded true from reversing a human pause on some later push.
	Enabled *bool `json:"enabled,omitempty"`

	Model              string                `json:"model,omitempty"`
	ReferenceBased     *bool                 `json:"referenceBased,omitempty"`
	References         []AutomationReference `json:"references,omitempty"`
	ApprovedTools      []string              `json:"approvedTools,omitempty"`
	ApprovedToolGroups []string              `json:"approvedToolGroups,omitempty"`

	Action *AutomationActionInput `json:"action,omitempty"`
}

// AutomationPatch is the body of PUT /automations/:jobID, mirroring the subset
// of workspace.UpdateAutomationInput the CLI writes.
//
// Every field is a POINTER because every field of the server's body is an
// optional.V: an absent key means "unchanged", while an explicit value —
// INCLUDING an empty list — REPLACES what is there. Both are things a folder
// legitimately means, and a plain slice with omitempty collapses them into the
// first, making it impossible to clear the last watched table.
//
// ⚠️ A POINTER TO A NIL SLICE IS NOT AN EMPTY LIST. `&([]string)(nil)` marshals
// as `null`, and an explicit null reads on the server as absent — "unchanged" —
// which is the opposite of what a caller writing `&refs` after refs came back
// empty means. Clearing a list takes a pointer to an EMPTY, NON-NIL slice, so
// anything building one of these fields from a value that may be nil has to
// normalise it first. The types cannot enforce that; this sentence is the only
// thing standing between a folder that deletes its last reference and a push
// that silently keeps it.
//
// Two fields are absent on purpose, and both absences are the safety property:
//
//   - `private` — the server accepts an echoed value and refuses a changed one.
//     A client that never sends it cannot get that wrong, and the field has no
//     place in a committed file (see CreateAutomationInput).
//   - `triggerKind` — the server's body has none, so a folder that changed it
//     must be REFUSED with the reason rather than have the field quietly
//     dropped. Having no field here is what makes the silent version
//     unrepresentable.
type AutomationPatch struct {
	Name              *string `json:"name,omitempty"`
	Description       *string `json:"description,omitempty"`
	Prompt            *string `json:"prompt,omitempty"`
	CronExpr          *string `json:"cronExpr,omitempty"`
	Timezone          *string `json:"timezone,omitempty"`
	ReportingTimezone *string `json:"reportingTimezone,omitempty"`

	Enabled *bool   `json:"enabled,omitempty"`
	Model   *string `json:"model,omitempty"`

	References         *[]AutomationReference `json:"references,omitempty"`
	ApprovedTools      *[]string              `json:"approvedTools,omitempty"`
	ApprovedToolGroups *[]string              `json:"approvedToolGroups,omitempty"`

	EmailAllowedFromDomains   *[]string `json:"emailAllowedFromDomains,omitempty"`
	EmailAllowedFromAddresses *[]string `json:"emailAllowedFromAddresses,omitempty"`
	WatchedTableIds           *[]string `json:"watchedTableIds,omitempty"`

	MailboxID     *string                  `json:"mailboxID,omitempty"`
	MailboxFilter *AutomationMailboxFilter `json:"mailboxFilter,omitempty"`

	RateLimitPerMinute *int `json:"rateLimitPerMinute,omitempty"`
	RateLimitPerDay    *int `json:"rateLimitPerDay,omitempty"`

	Action *AutomationActionInput `json:"action,omitempty"`
}

// Empty reports a patch that would change nothing, so a caller can skip the
// round trip rather than send `{}`.
//
// ⚠️ Worth more here than on DataAppPatch: `Update` and the reference / action
// writes are SEPARATE server-side calls, so every needless PUT is another chance
// to half-apply — the new schedule with the old references, indistinguishable
// after the fact from somebody editing references in the web app.
func (p AutomationPatch) Empty() bool {
	return p.Name == nil && p.Description == nil && p.Prompt == nil &&
		p.CronExpr == nil && p.Timezone == nil && p.ReportingTimezone == nil &&
		p.Enabled == nil && p.Model == nil &&
		p.References == nil && p.ApprovedTools == nil && p.ApprovedToolGroups == nil &&
		p.EmailAllowedFromDomains == nil && p.EmailAllowedFromAddresses == nil &&
		p.WatchedTableIds == nil &&
		p.MailboxID == nil && p.MailboxFilter == nil &&
		p.RateLimitPerMinute == nil && p.RateLimitPerDay == nil &&
		p.Action == nil
}

// automationPath is the single-row route, escaped once so a caller cannot forget.
func automationPath(id string) string { return "automations/" + url.PathEscape(id) }

// GetAutomation reads one automation's full configuration, action and reference
// set included.
//
// 404s for an id the caller cannot reach: the read gate does not distinguish
// absent from invisible, so a caller must not report "deleted" on a 404 — it
// means "not yours to see", which for a shared folder is a colleague's private
// feature as often as it is a missing row.
func (c *Client) GetAutomation(ctx context.Context, id string) (*Automation, error) {
	var out Automation
	if err := c.Do(ctx, "GET", automationPath(id), nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// automationListResponse mirrors
// model.PaginationResponse[rscheduledjob.ScheduledJobViewResponse].
//
// Token is decoded but NEVER acted on — see ListAutomations.
type automationListResponse struct {
	Token  string        `json:"token"`
	Total  uint64        `json:"total"`
	Result []*Automation `json:"result"`
}

// AutomationListLimit is what ListAutomations asks for: the server's own maximum.
//
// The server SILENTLY falls back to 100 for any value outside 1..200, so asking
// for more is not an error — it is just not honoured, and the caller would then
// be reasoning about a page it did not request.
const AutomationListLimit = 200

// ErrAutomationListTruncated is what ListAutomations returns when the feature
// holds more automations than one page carries.
//
// A REFUSAL rather than a short list, and rather than paging.
// model.QueryOptions.Token encodes an OFFSET, so walking pages while rows are
// created or deleted shifts a row across a boundary and skips it — which for a
// sync loop is not a benign miss: a row it cannot see reads as deleted, and the
// next push creates a duplicate beside the live one. That is the exact
// false-green this loop exists to remove, and paging would re-introduce it
// through the mechanism meant to fix it. An honest "could not tell" beats a
// paginated wrong answer.
type ErrAutomationListTruncated struct {
	FeatureID string
	// Total is what the server says exists; Returned is what one page carried.
	Total    uint64
	Returned int
	// Unfiltered records that the page came back holding rows from OTHER
	// features, which only an instance predating the `featureID` parameter does
	// — so Total is the organization's count and not this feature's. Without it
	// the message tells the reader to split a feature that may hold three
	// automations, and no --force gets them past it.
	Unfiltered bool
}

func (e *ErrAutomationListTruncated) Error() string {
	if e.Unfiltered {
		return fmt.Sprintf("this Ronja does not scope the automations listing to one feature — it is older than this CLI — so it answered with %d automations from across the organization and one page carries at most %d. Nothing here can tell which of %s's automations are missing. Upgrade Ronja, or manage these automations in Ronja's web app",
			e.Total, AutomationListLimit, e.FeatureID)
	}
	return fmt.Sprintf("%s holds %d automations and one page carries at most %d — this listing saw %d of them, so nothing here can tell which are missing. Split the feature, or manage these automations in Ronja. (If this Ronja is older than this CLI it does not scope the listing to one feature at all, and that count is the whole organization's.)",
		e.FeatureID, e.Total, AutomationListLimit, e.Returned)
}

// ListAutomations reads every automation belonging to one feature.
//
// featureID is REQUIRED, and the requirement is the point. The route without it
// lists everything the caller can see across the organization, which for a
// folder bound to one feature is both far more than it asked for and — past the
// page cap — silently less than it needs. `featureID` was added to this route on
// this branch precisely so a folder can ask its own question; a feature's
// automations fit one page, and Total is how the caller learns when they do not.
//
// A feature the caller cannot read is a 404, not a 403, matching the sibling
// trash route: a forbidden would confirm the feature exists.
//
// Returns ErrAutomationListTruncated rather than a short list when Total exceeds
// the rows returned. Read that type before making it non-fatal.
func (c *Client) ListAutomations(ctx context.Context, featureID string) ([]*Automation, error) {
	query := url.Values{}
	query.Set("featureID", featureID)
	query.Set("limit", strconv.Itoa(AutomationListLimit))

	var out automationListResponse
	if err := c.Do(ctx, "GET", "automations?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	// ⚠️ THE FILTER IS VERIFIED, NOT TRUSTED. This CLI is `go install`-ed
	// independently of the deployment it points at, and `featureID` was added to
	// this route on the same branch as this file — so an older backend takes the
	// parameter, ignores it, and answers with every automation the caller can
	// see. Total is then the ORGANIZATION-wide count, which for an organization
	// under the page cap equals what came back: the truncation refusal above
	// passes, and the caller is handed the whole organization dressed as one
	// feature's set. Phase B's orphan detection reads exactly this list and acts
	// on "in the feature, not in the folder" by deleting.
	//
	// DROPPED rather than refused, which is the one place this file prefers
	// degrading to failing. A row whose featureID is not the one asked for
	// cannot belong to this feature under any server behaviour, so removing it
	// leaves the exactly-correct set — the same answer a filtering server gives
	// — where a refusal would make the loop unusable against a backend that is
	// merely older. The case where dropping could hide something is a listing
	// that did not fit one page, and that is already a refusal.
	//
	// ⚠️ THE DROP IS COUNTED, AND THE TRUNCATION VERDICT IS TAKEN AFTER IT. A row
	// from another feature can only come from an instance that ignored the
	// parameter, which is also the instance whose Total is organization-wide — so
	// deciding truncation first told an organization over the page cap that its
	// three-automation feature had to be split, and named no way out.
	kept := out.Result[:0]
	unfiltered := false
	for _, a := range out.Result {
		if a.FeatureID != featureID {
			unfiltered = true
			continue
		}
		a.Normalize()
		kept = append(kept, a)
	}
	if out.Total > uint64(len(out.Result)) {
		// Still a refusal in BOTH cases: an unfiltered listing that did not fit
		// one page is a prefix of the organization, so rows of this feature may
		// be missing from it and a row this loop cannot see reads as deleted.
		// Only the message differs, and only because the reader's fix does.
		return nil, &ErrAutomationListTruncated{
			FeatureID: featureID, Total: out.Total, Returned: len(out.Result),
			Unfiltered: unfiltered,
		}
	}
	return kept, nil
}

// CreateAutomation creates one automation and returns the row it made.
//
// The response carries the action and, for a webhook trigger that asked for one,
// the HMAC signing secret in its ONLY appearance anywhere — a caller that
// discards this response cannot get it back.
func (c *Client) CreateAutomation(ctx context.Context, in CreateAutomationInput) (*Automation, error) {
	var out Automation
	if err := c.Do(ctx, "POST", "automations", in, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// UpdateAutomation patches one automation and returns the row as it now stands.
//
// ⚠️ NOT TRANSACTIONAL server-side: the row update and the reference / action
// writes are separate calls, so a failure at the second leaves the new schedule
// with the OLD references — a state indistinguishable, after the fact, from
// somebody editing references in the web app. Nothing here can fix that; a
// caller must report a failure at the moment it happens, while it is still
// attributable, rather than retrying quietly.
func (c *Client) UpdateAutomation(ctx context.Context, id string, patch AutomationPatch) (*Automation, error) {
	var out Automation
	if err := c.Do(ctx, "PUT", automationPath(id), patch, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// DeleteAutomation soft-deletes one automation, recording the reason on the
// audit entry.
//
// ⚠️ SOFT, and the softness is a caller's problem rather than a detail. The row
// stops firing and leaves normal listings, but sits in a 30-day trash this CLI
// cannot empty — purge is a human-only route — and it is AUTO-PAUSED there, so
// a restore brings back a disabled row. Deleting is therefore not a way to
// recreate an automation differently: a caller that "deletes and pushes again"
// to change something unpatchable leaves one trashed row per attempt.
func (c *Client) DeleteAutomation(ctx context.Context, id, reason string) error {
	body := struct {
		Reason string `json:"reason"`
	}{Reason: reason}
	return c.Do(ctx, "DELETE", automationPath(id), body, nil)
}
