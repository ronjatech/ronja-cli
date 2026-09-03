package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Same discipline as workflow_test.go: the fixture is TAG VERIFICATION against
// rscheduledjob.ScheduledJobWithActionResponse and the rdb.ScheduledJob it
// embeds, because a name that does not match decodes to the zero value in
// silence — and here that silence reads as an automation with no schedule, no
// watched tables, or no action at all.
//
// The two ROW spellings are the point of half of it: the row emits
// `emailAllowedFromAddrs` and `watchedTableIDs`, while the create/update bodies
// read `emailAllowedFromAddresses` and `watchedTableIds`. A mirror that used the
// input names on the read type would decode both to empty and report a
// configured allowlist as unconfigured.
const automationFixture = `{
  "id": "sj-abc",
  "tenantID": "t-1",
  "workspaceID": null,
  "featureID": "collection-1",
  "name": "Nightly rebuild",
  "description": "Rebuilds the order tables",
  "prompt": "do the thing",
  "cronExpr": "0 2 * * *",
  "timezone": "Europe/Stockholm",
  "reportingTimezone": "Europe/Stockholm",
  "enabled": false,
  "disabledReason": "user",
  "triggerKind": "table",
  "emailAddressToken": null,
  "emailAllowedFromDomains": ["acme.com"],
  "emailAllowedFromAddrs": ["ops@acme.com"],
  "webhookToken": null,
  "watchedTableIDs": ["table-1", "table-2"],
  "eventName": "invoice.settled",
  "mailboxID": "mailbox-3",
  "mailboxFilter": {"fromDomains": ["acme.com"], "fromAddrs": [],
                    "subjectContains": ["invoice"], "requireAttachment": true, "label": ""},
  "mailboxArmedAt": null,
  "rateLimitPerMinute": 5,
  "rateLimitPerDay": null,
  "referenceBased": true,
  "private": true,
  "featureScope": "workspace",
  "approvedTools": ["queryTable"],
  "approvedToolGroups": ["read"],
  "model": "standard",
  "lastRunAt": null,
  "nextRunAt": null,
  "tags": [],
  "createdBy": "user-1",
  "updatedBy": "user-1",
  "createdAt": "2026-08-01T08:00:00Z",
  "updatedAt": "2026-09-02T09:15:00Z",
  "action": {
    "id": "sja-1",
    "scheduledJobID": "sj-abc",
    "ordinal": 0,
    "kind": "saved_agent",
    "config": {
      "prompt": "seed",
      "model": "standard",
      "referenceBased": true,
      "references": [
        {"resourceID": "note-policy", "kind": "note"},
        {"resourceID": "table-1", "kind": "model"}
      ],
      "approvedTools": ["queryTable"],
      "approvedToolGroups": ["read"],
      "agentID": "agent-9"
    }
  },
  "emailAddress": "acme+tok@ingest.example.test",
  "webhookURL": "https://api.example.test/api/v2/automations/webhook/tok",
  "webhookHasSigningSecret": true,
  "warnings": ["the bound Agent cannot reach mailbox-3"]
}`

func TestGetAutomationDecodesEveryMirroredField(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/automations/sj-abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}
		w.Write([]byte(automationFixture))
	})

	a, err := client.GetAutomation(context.Background(), "sj-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"ID", a.ID, "sj-abc"},
		{"FeatureID", a.FeatureID, "collection-1"},
		{"Name", a.Name, "Nightly rebuild"},
		{"Description", a.Description, "Rebuilds the order tables"},
		{"Prompt", a.Prompt, "do the thing"},
		{"CronExpr", a.CronExpr, "0 2 * * *"},
		{"Timezone", a.Timezone, "Europe/Stockholm"},
		{"ReportingTimezone", a.ReportingTimezone, "Europe/Stockholm"},
		{"Enabled", a.Enabled, false},
		{"DisabledReason", a.DisabledReason, "user"},
		{"TriggerKind", a.TriggerKind, TriggerKindTable},
		{"EventName", a.EventName, "invoice.settled"},
		{"MailboxID", a.MailboxID, "mailbox-3"},
		{"ReferenceBased", a.ReferenceBased, true},
		{"Private", a.Private, true},
		{"FeatureScope", a.FeatureScope, "workspace"},
		{"Model", a.Model, "standard"},
		{"EmailAddress", a.EmailAddress, "acme+tok@ingest.example.test"},
		{"WebhookURL", a.WebhookURL, "https://api.example.test/api/v2/automations/webhook/tok"},
		{"WebhookHasSigningSecret", a.WebhookHasSigningSecret, true},
		// The ROW's spellings. Empty here means the mirror used the INPUT names.
		{"len(EmailAllowedFromAddrs)", len(a.EmailAllowedFromAddrs), 1},
		{"EmailAllowedFromAddrs[0]", a.EmailAllowedFromAddrs[0], "ops@acme.com"},
		{"len(WatchedTableIDs)", len(a.WatchedTableIDs), 2},
		{"WatchedTableIDs[0]", a.WatchedTableIDs[0], "table-1"},
		{"EmailAllowedFromDomains[0]", a.EmailAllowedFromDomains[0], "acme.com"},
		{"ApprovedTools[0]", a.ApprovedTools[0], "queryTable"},
		{"ApprovedToolGroups[0]", a.ApprovedToolGroups[0], "read"},
		{"MailboxFilter.SubjectContains[0]", a.MailboxFilter.SubjectContains[0], "invoice"},
		{"MailboxFilter.RequireAttachment", a.MailboxFilter.RequireAttachment, true},
		{"Warnings[0]", a.Warnings[0], "the bound Agent cannot reach mailbox-3"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	// The action is the whole reason this route is usable — before this branch
	// GET returned the bare row and only LIST carried one.
	if a.Action == nil {
		t.Fatal("no action decoded — a folder cannot be read back without it")
	}
	if a.Action.Kind != ActionKindSavedAgent || a.Action.Config.AgentID != "agent-9" {
		t.Errorf("action = %+v", a.Action)
	}
	if a.Action.Config.Model == nil || *a.Action.Config.Model != "standard" {
		t.Errorf("action config model = %v", a.Action.Config.Model)
	}
	// References ride INSIDE the action's config, so a route that returns the
	// action returns them — there is no second call to make.
	if len(a.Action.Config.References) != 2 ||
		a.Action.Config.References[0] != (AutomationReference{ResourceID: "note-policy", Kind: "note"}) {
		t.Errorf("references = %+v", a.Action.Config.References)
	}

	// optional.V[int]: an explicit value is a pointer, `null` stays nil — the
	// difference between a declared limit and "inherit the organization's".
	if a.RateLimitPerMinute == nil || *a.RateLimitPerMinute != 5 {
		t.Errorf("RateLimitPerMinute = %v", a.RateLimitPerMinute)
	}
	if a.RateLimitPerDay != nil {
		t.Errorf("a null rate limit must stay nil, got %v", *a.RateLimitPerDay)
	}
	// optional.V[string] marshals `null` when unset, and unmarshalling null into
	// a string is a documented no-op.
	if a.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want empty", a.WorkspaceID)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Errorf("timestamps did not decode: created=%v updated=%v", a.CreatedAt, a.UpdatedAt)
	}
	// The get path never reveals the HMAC secret.
	if a.WebhookSigningSecret != "" {
		t.Errorf("a read exposed the signing secret: %q", a.WebhookSigningSecret)
	}
}

// TestAutomationStampIsTheLockAnchor: the value has to be one spelling, because
// it is compared against ITSELF across runs — and a ZERO time must answer "",
// which disarms the guard, rather than a well-formed year-1 anchor that claims
// agreement and can never match again.
func TestAutomationStampIsTheLockAnchor(t *testing.T) {
	a := &Automation{UpdatedAt: time.Date(2026, 9, 2, 9, 15, 0, 0, time.UTC)}
	if got := a.Stamp(); got != "2026-09-02T09:15:00Z" {
		t.Errorf("Stamp = %q", got)
	}
	// A non-UTC reading of the same instant must produce the same string, or two
	// clients would report drift on a row neither of them touched.
	elsewhere := &Automation{UpdatedAt: a.UpdatedAt.In(time.FixedZone("x", 3600))}
	if elsewhere.Stamp() != a.Stamp() {
		t.Errorf("the zone changed the anchor: %q vs %q", elsewhere.Stamp(), a.Stamp())
	}
	if got := (&Automation{}).Stamp(); got != "" {
		t.Errorf("a zero time must disarm the guard, got %q", got)
	}
	if got := (*Automation)(nil).Stamp(); got != "" {
		t.Errorf("a nil automation must disarm the guard, got %q", got)
	}
}

// ── The list, and the cliff it refuses to walk off ──────────────────────────

func TestListAutomationsAsksForOneFeatureAndOnePage(t *testing.T) {
	var gotQuery string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/automations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"token":"200","total":2,"result":[
			{"id":"sj-1","name":"a","featureID":"collection-1"},
			{"id":"sj-2","name":"b","featureID":"collection-1"}]}`))
	})

	rows, err := client.ListAutomations(context.Background(), "collection-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(gotQuery, "featureID=collection-1") {
		t.Errorf("query = %q — the listing must be scoped to the folder's feature", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=200") {
		t.Errorf("query = %q — asking for less than the server's maximum shrinks the page for nothing", gotQuery)
	}
	if len(rows) != 2 || rows[0].ID != "sj-1" {
		t.Fatalf("rows = %+v", rows)
	}
	// Normalize ran: an omitted list and an empty one must be the same thing to
	// a caller comparing a folder's declaration against the row.
	if rows[0].WatchedTableIDs == nil || rows[0].ApprovedTools == nil {
		t.Errorf("nil slices survived Normalize: %+v", rows[0])
	}
}

// TestListAutomationsRefusesATruncatedPage is the load-bearing one.
//
// The server's page token encodes an OFFSET, so paging while rows are created or
// deleted skips one — and a row a sync loop cannot see reads as deleted, which
// the next push acts on by creating a duplicate beside the live one. A short
// list is the same failure with no error attached. So: refuse, and name the
// cliff.
func TestListAutomationsRefusesATruncatedPage(t *testing.T) {
	calls := 0
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"token":"200","total":250,"result":[{"id":"sj-1"},{"id":"sj-2"}]}`))
	})

	rows, err := client.ListAutomations(context.Background(), "collection-1")
	if err == nil {
		t.Fatalf("a truncated listing must be refused, got %d rows", len(rows))
	}
	if rows != nil {
		t.Errorf("a refusal must return no rows — a caller reaching for them is reading a short list: %+v", rows)
	}
	var truncated *ErrAutomationListTruncated
	if !errors.As(err, &truncated) {
		t.Fatalf("err = %v, want ErrAutomationListTruncated so a caller can score it `unknown`", err)
	}
	if truncated.Total != 250 || truncated.Returned != 2 || truncated.FeatureID != "collection-1" {
		t.Errorf("truncation report = %+v", truncated)
	}
	if !strings.Contains(err.Error(), "250") {
		t.Errorf("the message must name what it could not see: %v", err)
	}
	if calls != 1 {
		t.Errorf("the client made %d requests — it must refuse rather than page", calls)
	}
}

// A page that carries everything the server counted is not truncated, including
// the empty case: a feature with no automations is a real answer, not a cliff.
func TestListAutomationsAcceptsACompletePage(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"","total":0,"result":[]}`))
	})
	rows, err := client.ListAutomations(context.Background(), "collection-1")
	if err != nil {
		t.Fatalf("an empty feature must not be an error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

// TestListAutomationsDropsWhatTheServerDidNotFilter is the guard against the
// deployment this CLI cannot see.
//
// `ronja` is `go install`-ed independently of the instance it points at. Against
// a backend older than the `featureID` parameter the filter is accepted and
// ignored: the response is every automation the caller can see, `total` is the
// organization-wide count, and for an organization under the page cap that count
// EQUALS what came back — so the truncation refusal passes and the whole
// organization arrives looking exactly like one feature's set. Phase B's orphan
// detection reads this list and deletes what the folder does not have.
func TestListAutomationsDropsWhatTheServerDidNotFilter(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// An unfiltered answer: three rows, two of them somebody else's feature,
		// with a total that matches the page so nothing else refuses it.
		w.Write([]byte(`{"token":"","total":3,"result":[
			{"id":"sj-1","name":"ours","featureID":"collection-1"},
			{"id":"sj-2","name":"theirs","featureID":"collection-2"},
			{"id":"sj-3","name":"legacy"}]}`))
	})

	rows, err := client.ListAutomations(context.Background(), "collection-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "sj-1" {
		t.Fatalf("rows = %+v — a row this feature does not own must not be reported as its own", rows)
	}
	// A legacy row with no featureID at all is the same case: it cannot belong
	// to the feature that was asked for, whatever the server meant by returning
	// it. And Normalize still ran on what survived.
	if rows[0].ApprovedTools == nil {
		t.Errorf("nil slices survived Normalize: %+v", rows[0])
	}
}

// A feature the caller cannot read is a 404, matching the sibling trash route so
// the two cannot disagree — a 403 would confirm the feature exists. The status
// has to reach the caller, which is what api.Error carries.
func TestListAutomationsSurfacesTheStatus(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	})
	_, err := client.ListAutomations(context.Background(), "collection-gone")
	if got := StatusOf(err); got != 404 {
		t.Errorf("StatusOf = %d, want 404 — a caller cannot tell 404 from 400 without it", got)
	}
}

// ── The write bodies ────────────────────────────────────────────────────────

// TestCreateAutomationSendsOneSpellingAndNoPrivate is the shape check that
// matters more than any field it asserts.
//
// Sending BOTH spellings of one aliased field is a 400, so the body must carry
// exactly one — and `private` must not appear at all: the update route refuses a
// changed value, and a folder that could declare visibility would make the row
// invisible to the next colleague who pushes, whose status then reports it gone
// and whose push creates a duplicate.
func TestCreateAutomationSendsOneSpellingAndNoPrivate(t *testing.T) {
	var body map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v2/automations" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body: %v", err)
		}
		w.Write([]byte(`{"id":"sj-new","webhookSigningSecret":"shhh"}`))
	})

	enabled := false
	created, err := client.CreateAutomation(context.Background(), CreateAutomationInput{
		FeatureID:                 "collection-1",
		Name:                      "Nightly rebuild",
		TriggerKind:               TriggerKindTable,
		WatchedTableIds:           []string{"table-1"},
		EmailAllowedFromAddresses: []string{"ops@acme.com"},
		Enabled:                   &enabled,
		References:                []AutomationReference{{ResourceID: "note-policy", Kind: "note"}},
		Action:                    &AutomationActionInput{Kind: ActionKindSavedAgent, Config: AutomationActionConfigInput{AgentID: "agent-9"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, sent := body["private"]; sent {
		t.Error("the create body carries `private` — visibility must stay out of the file schema entirely")
	}
	for canonical, alias := range map[string]string{
		"watchedTableIds":           "watchedTableIDs",
		"emailAllowedFromAddresses": "emailAllowedFromAddrs",
	} {
		if _, ok := body[canonical]; !ok {
			t.Errorf("the body does not carry %q", canonical)
		}
		if _, ok := body[alias]; ok {
			t.Errorf("the body carries both %q and %q — sending both spellings is a 400", canonical, alias)
		}
	}
	// A false `enabled` has to survive: it is a pointer precisely so "declared
	// off" is distinguishable from "not managed", and omitempty on a bare bool
	// would have collapsed the two.
	if got, ok := body["enabled"]; !ok || got != false {
		t.Errorf("enabled = %v (present=%v), want an explicit false", got, ok)
	}
	// The one-time reveal is decoded, because it appears nowhere else ever.
	if created.WebhookSigningSecret != "shhh" {
		t.Errorf("the create response's signing secret was lost: %+v", created)
	}
}

// TestAutomationPatchCanClearAListAndSaysNothingOtherwise: every field is a
// pointer so an explicit empty list REPLACES and an absent one leaves the value
// alone. A plain slice with omitempty collapses the two, which makes it
// impossible to remove the last watched table.
func TestAutomationPatchCanClearAListAndSaysNothingOtherwise(t *testing.T) {
	var body map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/v2/automations/sj-abc" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body: %v", err)
		}
		w.Write([]byte(`{"id":"sj-abc"}`))
	})

	empty := []string{}
	if _, err := client.UpdateAutomation(context.Background(), "sj-abc", AutomationPatch{
		WatchedTableIds: &empty,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, ok := body["watchedTableIds"]; !ok {
		t.Error("an explicitly emptied list was dropped from the body — the last watched table could never be removed")
	} else if list, isList := got.([]any); !isList || len(list) != 0 {
		t.Errorf("watchedTableIds = %v, want []", got)
	}
	if len(body) != 1 {
		t.Errorf("the patch sent fields nobody set: %v", body)
	}
	// The two absences that are safety properties rather than omissions.
	for _, absent := range []string{"private", "triggerKind"} {
		if _, sent := body[absent]; sent {
			t.Errorf("the patch body carries %q", absent)
		}
	}
}

// TestAutomationPatchEmpty: Update and the reference/action writes are separate
// server-side calls, so a needless PUT is another chance to half-apply.
func TestAutomationPatchEmpty(t *testing.T) {
	if !(AutomationPatch{}).Empty() {
		t.Error("a zero patch must report empty")
	}
	name := "x"
	if (AutomationPatch{Name: &name}).Empty() {
		t.Error("a patch with a field set must not report empty")
	}
	empty := []string{}
	if (AutomationPatch{ApprovedTools: &empty}).Empty() {
		t.Error("an explicit clear is a change, not an empty patch")
	}
}

func TestDeleteAutomationSendsTheReason(t *testing.T) {
	var body map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/v2/automations/sj-abc" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"scheduledJobID":"sj-abc","markedForDeletion":true}`))
	})

	if err := client.DeleteAutomation(context.Background(), "sj-abc", "removed from the folder"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if body["reason"] != "removed from the folder" {
		t.Errorf("the audit reason did not reach the server: %v", body)
	}
}

// An id with a slash or a space in it must not be able to reach a different
// route — the same escaping every other single-row call in this package does.
func TestAutomationPathEscapes(t *testing.T) {
	if got := automationPath("sj-a/b"); got != "automations/sj-a%2Fb" {
		t.Errorf("automationPath = %q", got)
	}
}
