package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// automationFile is one .json file in an automation folder: the declaration a
// person commits, in the vocabulary the CREATE and UPDATE bodies read.
//
// A CURATED SCHEMA rather than the wire row, and the three reasons are each a
// silent failure:
//
//   - The row and the input DISAGREE about two field names. A row emits
//     `emailAllowedFromAddrs` and `watchedTableIDs`; the bodies read
//     `emailAllowedFromAddresses` and `watchedTableIds`. A file written in the
//     row's spelling would be accepted, ignored, and answer 200 — so this type
//     carries the INPUT names, and parseAutomationFile refuses the row's by name
//     rather than letting DisallowUnknownFields describe them as typos.
//   - The row carries STATE A FILE MUST NOT OWN: nextRunAt, disabledReason, the
//     minted email address and webhook tokens, and the run-health columns. None
//     of them is a decision anybody makes in a pull request.
//   - `private` is create-only server-side, and it stays out of the file for a
//     reason of its own: a shared folder pushing private:true makes the row
//     invisible to the next colleague who pushes, whose status then reports it
//     gone and whose push creates a duplicate.
//
// EVERY FIELD IS A POINTER, and that is the whole three-state model rather than
// a convenience:
//
//	absent  — this folder does not manage the field. A push never sends it and
//	          status never reports drift on it.
//	present — managed, INCLUDING an explicit "" or []. That is how a folder
//	          clears a description or removes the last watched table.
//
// `enabled` is the field the model was built for (see automationReEnableGuard),
// and generalising it to every field is what stops the other trap in the same
// family: a file that simply omits `model` silently clearing the model somebody
// set in the web app. It follows the manifest's own precedent — Parameters and
// ReportingTimezone are pointers there for exactly this reason.
//
// ⚠️ IT HOLDS ONE LEVEL DOWN TOO, and that is why `action` is
// automationActionFile rather than the wire type: a nested config that could
// only say "" would have made absent mean CLEARED for the four fields inside it,
// which is the same bug this model exists to remove, one object deeper. See
// automationActionConfigFile.
//
// ⚠️ The NAME is not here. It is the filename's stem (automationNameFor), so
// renaming the automation is renaming the file — and a `name` key is refused
// rather than ignored, because two sources for one value is a value nobody can
// predict.
type automationFile struct {
	Description *string `json:"description,omitempty"`
	// Prompt is the INLINE AGENT action's instructions. A saved-agent action's
	// per-run seed message is action.config.prompt, which is a different field
	// on a different object; the server reads them separately and so does this.
	//
	// It is REFUSED beside a workflow or saved_agent action, declared or on the
	// row — see refuseAutomationPromptFor.
	Prompt *string `json:"prompt,omitempty"`

	// TriggerKind is IMMUTABLE server-side — see refuseUnpatchableAutomation.
	TriggerKind *string `json:"triggerKind,omitempty"`

	CronExpr          *string `json:"cronExpr,omitempty"`
	Timezone          *string `json:"timezone,omitempty"`
	ReportingTimezone *string `json:"reportingTimezone,omitempty"`

	// Enabled absent is UNMANAGED, and that is the state `init` writes. See
	// automationReEnableGuard for the incident this leaves room for.
	Enabled *bool `json:"enabled,omitempty"`

	Model *string `json:"model,omitempty"`
	// ReferenceBased and EventName are CREATE-ONLY, like TriggerKind: the update
	// body carries neither. They are here because a create legitimately sets
	// them; a push that would CHANGE either is refused.
	ReferenceBased *bool                      `json:"referenceBased,omitempty"`
	References     *[]api.AutomationReference `json:"references,omitempty"`

	ApprovedTools      *[]string `json:"approvedTools,omitempty"`
	ApprovedToolGroups *[]string `json:"approvedToolGroups,omitempty"`

	EmailAllowedFromDomains *[]string `json:"emailAllowedFromDomains,omitempty"`
	// ⚠️ THE INPUT'S SPELLING. A read returns `emailAllowedFromAddrs`; see this
	// type's doc.
	EmailAllowedFromAddresses *[]string `json:"emailAllowedFromAddresses,omitempty"`
	// ⚠️ THE INPUT'S SPELLING, likewise: a read returns `watchedTableIDs`.
	WatchedTableIds *[]string `json:"watchedTableIds,omitempty"`

	EventName     *string                      `json:"eventName,omitempty"`
	MailboxID     *string                      `json:"mailboxID,omitempty"`
	MailboxFilter *api.AutomationMailboxFilter `json:"mailboxFilter,omitempty"`

	RateLimitPerMinute *int `json:"rateLimitPerMinute,omitempty"`
	RateLimitPerDay    *int `json:"rateLimitPerDay,omitempty"`

	Action *automationActionFile `json:"action,omitempty"`
}

// automationActionFile is the `action` object as a FILE declares it.
//
// A type of its own rather than api.AutomationActionInput because the wire type
// cannot say ABSENT: its config fields are plain strings and a plain map, so
// "unmanaged" and "cleared" are the same value on the way out.
//
// ⚠️ `kind` is REQUIRED whenever `action` is present — see
// checkAutomationVocabulary. It is the discriminator that decides which config
// fields the server reads at all, and an empty one is read as an inline agent
// action, so a file that omitted it would silently get an action it did not
// describe.
//
// ⚠️ `kind` is also ONE-WAY out of `agent`: an existing workflow or saved_agent
// action cannot be switched back — see refuseAutomationActionKindSwitch.
type automationActionFile struct {
	Kind   string                     `json:"kind"`
	Config automationActionConfigFile `json:"config"`
}

// automationActionConfigFile is action.config with the same three-state model
// the top level has, and it is load-bearing rather than symmetric.
//
// ⚠️ THE SERVER REPLACES AN ACTION'S CONFIG WHOLESALE. writeActionTx DELETEs the
// job's action row and INSERTs a new one from the config the body carried, so a
// push that sends `action` at all decides the value of every field inside it —
// not only the one it meant to change. Value-typed, a file managing just
// `workflowID` would send an empty `parameterValues` beside the new id and wipe
// the values somebody had set in the web app: not listed in `changed`, and not
// caught by the post-write verification, which was reading the same absent field
// as unmanaged.
//
// So absent means UNTOUCHED here in the only way it can against a wholesale
// replace: automationActionFile.wire carries the ROW's current value forward
// into every field the file does not manage. Every one of the four is readable
// back off the row (AutomationActionConfigResponse carries workflowID,
// parameterValues, agentID and prompt), so there is no field this has to refuse
// rather than carry.
type automationActionConfigFile struct {
	WorkflowID      *string         `json:"workflowID,omitempty"`
	ParameterValues *map[string]any `json:"parameterValues,omitempty"`
	AgentID         *string         `json:"agentID,omitempty"`
	// Prompt is the per-automation seed message for a saved_agent action, not
	// the inline agent's instructions — those are the file's top-level `prompt`.
	Prompt *string `json:"prompt,omitempty"`
}

// wire builds the action body the server will store, filling every field this
// file does not manage from the row it is replacing.
//
// `current` is nil on a create, where there is nothing to carry forward and an
// unmanaged field is simply not set. It is also ignored when the KIND changes:
// an action of a different kind is a different shape, and the old kind's fields
// are not values the new one could keep.
func (a *automationActionFile) wire(current *api.AutomationAction) *api.AutomationActionInput {
	if a == nil {
		return nil
	}
	var carry api.AutomationActionConfig
	if current != nil && current.Kind == a.Kind {
		carry = current.Config
	}
	out := &api.AutomationActionInput{
		Kind: a.Kind,
		Config: api.AutomationActionConfigInput{
			WorkflowID:      strOrCarried(a.Config.WorkflowID, carry.WorkflowID),
			AgentID:         strOrCarried(a.Config.AgentID, carry.AgentID),
			Prompt:          strOrCarried(a.Config.Prompt, carry.Prompt),
			ParameterValues: carry.ParameterValues,
		},
	}
	if a.Config.ParameterValues != nil {
		out.Config.ParameterValues = *a.Config.ParameterValues
	}
	return out
}

// strOrCarried answers the declared value, or the row's when the file does not
// manage the field.
func strOrCarried(declared *string, carried string) string {
	if declared == nil {
		return carried
	}
	return *declared
}

// The keys a file may NOT carry, each with the reason, so the refusal is about
// this folder's model rather than about JSON.
//
// Spelled out rather than left to DisallowUnknownFields because every one of
// them is a key somebody writes ON PURPOSE — three of them are what a GET
// response actually contains, so "unknown field" would read as a bug in the CLI.
var automationRefusedKeys = map[string]string{
	"name":                  "the automation's name is the FILE's name — rename the file",
	"id":                    "the row's id is recorded per stack in " + wfdir.LockName + ", not in the file",
	"featureID":             "the feature is the stack's, in " + wfdir.ManifestName,
	"private":               "visibility is fixed when an automation is created and the update route refuses a change; a shared folder declaring it would also hide the row from the next colleague who pushes, whose push then creates a duplicate",
	"emailAllowedFromAddrs": "that is the spelling a READ returns; the write vocabulary is \"emailAllowedFromAddresses\"",
	"watchedTableIDs":       "that is the spelling a READ returns; the write vocabulary is \"watchedTableIds\"",
	"nextRunAt":             "that is state the server computes, not a declaration",
	"lastRunAt":             "that is state the server computes, not a declaration",
	"disabledReason":        "that is state the server computes; declare \"enabled\" instead",
	"emailAddress":          "an email trigger's address is minted by the server and cannot be declared",
	"webhookURL":            "a webhook's URL is minted by the server and cannot be declared",
	"webhookToken":          "a webhook's token is minted by the server and cannot be declared",
	"webhookSigningSecret":  "a signing secret is minted ONCE at create and is never readable again, so a committed file cannot re-assert it — turn signing on in the web app",
	"updatedAt":             "the drift anchor is recorded per stack in " + wfdir.LockName + ", not in the file",
	"createdAt":             "that is state the server computes, not a declaration",
}

// automationMarkerPattern finds a `{{ … }}` marker anywhere in a file.
//
// Deliberately WIDER than markers.Scan, which matches only the families the
// server actually parses. The form a reader reaches for here is
// `{{ note('policy') }}`, and there is no note family on either side — so a
// scan that only knew the real families would let exactly the commonest mistake
// through, as a literal string in a `resourceID` field.
var automationMarkerPattern = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

// parseAutomationFile decodes one file and refuses everything a folder must not
// commit, before any of it can reach a request.
//
// The order is deliberate: markers first (a file full of them is a file written
// against the wrong model, and every later message would be noise), then the
// refused keys by name, then the strict decode, then the vocabulary checks.
func parseAutomationFile(path, body string) (*automationFile, error) {
	if err := refuseAutomationMarkers(path, body); err != nil {
		return nil, err
	}
	// The refused keys are read off a LOOSE decode, so the message can name the
	// key and its reason rather than being whatever DisallowUnknownFields says
	// about an unexpected field. A body that will not decode at all falls through
	// to the strict pass below, which produces the better parse error.
	var loose map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &loose) == nil {
		for _, key := range sortedKeys(loose) {
			if reason, refused := automationRefusedKeys[key]; refused {
				return nil, fmt.Errorf("%q is not a key an automation file may carry: %s", key, reason)
			}
		}
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	// Unknown keys are an ERROR rather than being dropped. A dropped key is the
	// exact failure this whole loop exists to remove: the file says one thing,
	// the row keeps another, and nothing anywhere reports a difference.
	dec.DisallowUnknownFields()
	var file automationFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("this file is not a valid automation declaration: %w", err)
	}
	if err := checkAutomationVocabulary(&file); err != nil {
		return nil, err
	}
	if err := refuseZeroRateLimit("rateLimitPerMinute", file.RateLimitPerMinute); err != nil {
		return nil, err
	}
	if err := refuseZeroRateLimit("rateLimitPerDay", file.RateLimitPerDay); err != nil {
		return nil, err
	}
	return &file, nil
}

// refuseZeroRateLimit refuses a rate limit declared as 0, which is the one value
// on this surface that CANNOT CONVERGE — and which fails differently on each of
// the two write paths, so neither server answer would tell the reader what is
// wrong with the file.
//
// On an UPDATE the server reads an explicit 0 as "reset to the organization
// default" and stores NULL, so the row reads back with no limit at all. The
// delta then compares 0 against nil for ever: `status` reports drift, and every
// push re-sends the field, succeeds, and prints its own "the row still differs"
// note — the permanent drift this loop exists to remove, arrived at through a
// value that looks like a setting. On a CREATE it is worse than non-convergent:
// ValidateRateLimit refuses `v <= 0` outright, so the folder is unpushable with
// a 400 the file's schema gave no way to anticipate.
//
// Omitting the key is the spelling that means what a 0 looks like it means, so
// the message names it: an absent field is one this folder does not manage, and
// the organization's own default applies.
//
// Negative values are NOT refused here. The server rejects them on both paths
// with the same message, so its refusal is already the right one.
func refuseZeroRateLimit(field string, v *int) error {
	if v == nil || *v != 0 {
		return nil
	}
	return fmt.Errorf("%q is 0, and no automation can ever match that: on an update Ronja reads 0 as \"use the organization's default\" and stores no limit, so this file would report a difference on every push for ever, and on a create it is refused outright as not a positive integer. Leave %q out of the file to let the organization's default apply, or give it a positive number", field, field)
}

// refuseAutomationMarkers refuses a `{{ … }}` marker found anywhere in the file.
//
// ⚠️ It must not be sent literally, and that is the whole reason this is a
// refusal rather than a pass-through. An automation's references are a
// STRUCTURED FIELD the CLI resolves before the request is built — nothing on the
// server parses a marker out of a scheduled_jobs row — so a marker that reached
// the wire would be stored verbatim as a resource id, resolve to nothing, and
// fail at the first unattended run. The push looks fine, the status looks fine,
// and the automation is broken.
//
// It covers the WHOLE document, prompts included, and that over-reach is
// deliberate: a marker in an automation's prompt is inert text too, so a reader
// who wrote one believing it bound a reference is wrong in the same way and is
// better off told.
//
// ⚠️ The over-reach has a real cost and there is NO escape hatch, so the refusal
// says so: a prompt that legitimately contains `{{ … }}` — a templated example
// the agent is meant to reproduce, a mustache snippet — makes the file
// unpushable, and the advice the refusal gives ("write the alias as the field's
// own value") does not apply to it. Nothing here can tell the two apart, and
// guessing in favour of the rarer one would let the common mistake through
// silently; the answer for that prompt is to keep it in a note the automation
// references, or to write it in the web app.
func refuseAutomationMarkers(path, body string) error {
	var found []string
	seen := map[string]bool{}
	walkJSONStrings(body, func(where, value string) {
		for _, marker := range automationMarkerPattern.FindAllString(value, -1) {
			line := fmt.Sprintf("%s: %s", where, strings.TrimSpace(marker))
			if seen[line] {
				continue
			}
			seen[line] = true
			found = append(found, line)
		}
	})
	if len(found) == 0 {
		// Belt and braces for a marker the walk cannot reach — inside a key, or
		// in a file that did not decode. Reported without a field, since there is
		// none to name.
		if automationMarkerPattern.MatchString(body) && !json.Valid([]byte(body)) {
			return fmt.Errorf("this file carries a %s marker.\n  An automation's references are resolved FIELD BY FIELD before the request is built — nothing on the server reads a marker out of an automation — so one sent literally would be stored as a resource id, resolve to nothing, and fail at the first run.\n  Write the alias as the field's own value instead: \"resourceID\": \"policy\"\n  %s", "`{{ … }}`", automationMarkerLimitation)
		}
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("this file carries markers, which an automation file cannot use:\n    %s\n  An automation's references are a structured field the CLI resolves before the request is built — nothing on the server reads a marker out of an automation — so a marker sent literally would be stored as a resource id, resolve to nothing, and fail at the first run.\n  Write the alias as the field's own value: \"resourceID\": \"policy\", \"workflowID\": \"nightly\"\n  %s",
		strings.Join(found, "\n    "), automationMarkerLimitation)
}

// automationMarkerLimitation names what this refusal cannot tell apart, because
// the reader whose prompt legitimately holds a `{{ … }}` is otherwise sent to
// fix a field they never wrote.
const automationMarkerLimitation = "⚠️ This covers the whole file, prompts included, and there is no way to exempt one: a prompt that really does contain `{{ … }}` — a template the agent is meant to reproduce — cannot be pushed from this folder. Keep that prompt in a note the automation references, or write it in the web app."

// walkJSONStrings visits every string VALUE in a JSON document, with the dotted
// path it sits at.
//
// It walks the decoded document rather than the raw bytes so a refusal can name
// the field — `references[2].resourceID` sends a reader to one line, and "this
// file contains a marker" sends them reading the whole thing. A body that does
// not decode visits nothing; its caller has a better error to give.
func walkJSONStrings(body string, visit func(where, value string)) {
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return
	}
	var walk func(where string, node any)
	walk = func(where string, node any) {
		switch v := node.(type) {
		case string:
			visit(where, v)
		case []any:
			for i, item := range v {
				walk(fmt.Sprintf("%s[%d]", where, i), item)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := key
				if where != "" {
					child = where + "." + key
				}
				walk(child, v[key])
			}
		}
	}
	walk("", doc)
}

// checkAutomationVocabulary refuses a trigger kind, action kind or reference
// kind this build does not know.
//
// Locally, before the request, because all three are closed vocabularies the
// server would refuse anyway — and the server's refusal arrives after the push
// has already created or updated the automations that sorted before this one.
func checkAutomationVocabulary(file *automationFile) error {
	if file.TriggerKind != nil && !validAutomationTrigger(*file.TriggerKind) {
		return fmt.Errorf("%q is not a trigger kind — the kinds are %s",
			*file.TriggerKind, strings.Join(automationTriggerKinds, ", "))
	}
	if file.Action != nil {
		// An action with NO kind is refused BY NAME rather than sent. The server
		// reads a missing kind as an inline `agent` action — the back-compat
		// shape — so `{"action": {"config": {"workflowID": "…"}}}` is accepted,
		// answered 200, and stores an agent action whose config was ignored. That
		// is this folder's own §1.1 class of bug, and the file can be refused for
		// it without a credential.
		if strings.TrimSpace(file.Action.Kind) == "" {
			return fmt.Errorf("\"action\" carries no \"kind\" — the kind decides which config fields the server reads at all, and an action sent without one is stored as an inline agent action whose config is ignored. The kinds are %s",
				strings.Join(automationActionKinds, ", "))
		}
		if !validAutomationAction(file.Action.Kind) {
			return fmt.Errorf("%q is not an action kind — the kinds are %s",
				file.Action.Kind, strings.Join(automationActionKinds, ", "))
		}
		// The DECLARED half of the references rule (refuseAutomationReferencesFor),
		// earned here without a credential because the file's own `kind` answers
		// it. The half a file cannot answer — references beside NO `action` at
		// all, against a row that already holds one of these kinds — is asked of
		// the row in refuseUnpatchableAutomation.
		if err := refuseAutomationReferencesFor(file.Action.Kind, file.References); err != nil {
			return err
		}
		// The DECLARED half of the prompt rule (refuseAutomationPromptFor), same
		// split as the references rule above: the file's own `kind` answers it
		// here without a credential, and the half a file cannot answer — a
		// `prompt` beside NO `action` at all, against a row that already holds
		// one of these kinds — is asked of the row in
		// refuseUnpatchableAutomation.
		if err := refuseAutomationPromptFor(file.Action.Kind, file.Prompt); err != nil {
			return err
		}
	}
	if file.References != nil {
		for i, ref := range *file.References {
			if _, known := automationReferenceKinds[ref.Kind]; !known {
				return fmt.Errorf("references[%d].kind is %q, which is not a reference kind — the kinds are %s",
					i, ref.Kind, strings.Join(sortedKeys(automationReferenceKinds), ", "))
			}
			if strings.TrimSpace(ref.ResourceID) == "" {
				return fmt.Errorf("references[%d] names a %s and carries no resourceID", i, ref.Kind)
			}
		}
	}
	return nil
}

// refuseAutomationReferencesFor is THE rule about references and action kinds,
// written once and asked from two places: the parse check, against the kind the
// FILE declares, and refuseUnpatchableAutomation, against the kind the push
// would EFFECTIVELY leave behind.
//
// ⚠️ ONLY AN INLINE AGENT ACTION CARRIES REFERENCES, and the two other kinds
// fail differently — which is why the workflow one is refused by this CLI rather
// than left to the server.
//
// A saved_agent action carrying references earns a 400 ("automation access does
// not apply to a saved_agent action"), so the server already says no. A WORKFLOW
// action carrying them is ACCEPTED and stores NONE: the write paths put
// references on a branch a workflow action never reaches, SetWorkflowAction
// DELETEs the job's reference rows on top, and the read decodes the config off
// the stored action JSON, which has nowhere to keep them. So the push reports
// success, the row reads back with none, and every later status reports drift
// the author cannot edit away while every later push re-sends the same
// references for ever. That is the permanent false red this loop exists to
// remove.
//
// An EMPTY declaration is ALLOWED, deliberately. The server's own gate is
// len(refs) > 0, an empty list survives the drop unchanged, and it compares
// clean against a row that has none — so refusing it would refuse a file that
// pushes green today, the same over-reach refuseUnpatchableAutomation avoids by
// not refusing a declaration the row already agrees with.
//
// An UNKNOWN or empty kind answers nil rather than guessing. On a create with no
// action the server stores an inline agent action, which holds references
// perfectly well; refusing there would refuse the ordinary shape.
func refuseAutomationReferencesFor(kind string, refs *[]api.AutomationReference) error {
	if refs == nil || len(*refs) == 0 {
		return nil
	}
	switch kind {
	case api.ActionKindSavedAgent:
		return fmt.Errorf("\"references\" cannot be declared beside a saved_agent action: automation access does not apply to one — the agent runs under its OWN references, and the server refuses this with a 400. Declare them on the saved Agent itself in the web app, or drop the action and let this be an inline \"agent\" automation")
	case api.ActionKindWorkflow:
		return fmt.Errorf("\"references\" cannot be declared beside a workflow action: the server stores NONE — writing a workflow action deletes the automation's reference rows — so this would push green, read back empty, and report drift no edit could ever close. A workflow runs under the resources its own script markers bind; references belong on an inline \"agent\" action")
	}
	return nil
}

// refuseAutomationPromptFor is THE rule about a top-level `prompt` and action
// kinds, written once and asked from two places — the parse check, against the
// kind the FILE declares, and refuseUnpatchableAutomation, against the kind the
// push would EFFECTIVELY leave behind. Exactly the two-half shape
// refuseAutomationReferencesFor has, and for the same reason: without the row
// side, `{"prompt": "…"}` with no `action` block pushed against a row that
// already holds a workflow or saved_agent action pushes GREEN and then reports
// the same drift for ever — the very permanent false red this rule exists to
// remove.
//
// A top-level `prompt` beside a workflow or saved_agent action is a value that
// CANNOT CONVERGE. The server pins those kinds' parent prompt to a sentinel
// no-op: create stamps it over whatever the body sent, and update drops the
// prompt patch entirely (actionLocked). So the push reports success, the row
// reads back the sentinel, `status` reports the same drift for ever and CI
// exits 1 while describing a change that already landed.
//
// An UNKNOWN or empty kind answers nil rather than guessing, like the
// references rule: on a create with no action the server stores an inline agent
// action, whose prompt is exactly this field, and refusing there would refuse
// the ordinary shape.
func refuseAutomationPromptFor(kind string, prompt *string) error {
	if prompt == nil {
		return nil
	}
	switch kind {
	case api.ActionKindWorkflow, api.ActionKindSavedAgent:
		return fmt.Errorf("\"prompt\" is not a key a %s automation may carry: the server pins that kind's prompt to a sentinel — it stamps the sentinel at create and drops the prompt patch on every update — so the declared value can never converge and `status` would report the same drift for ever. Remove the key; a saved_agent action's per-run seed message is \"action.config.prompt\", a different field on a different object that the server does read", kind)
	}
	return nil
}

// refuseAutomationActionKindSwitch refuses a file that would switch an action
// back to an inline `agent` one, which the update route CANNOT DO.
//
// ⚠️ THE ROUTE WRITES A CHILD ACTION ROW FOR TWO KINDS AND NO MORE. PUT
// :jobID branches on `isWorkflowAction` / `isSavedAgentAction` and calls
// SetWorkflowAction or SetSavedAgentAction; there is no SetAgentAction anywhere
// in the backend to call for the third. With kind "agent" both booleans are
// false, nothing rewrites the child row, and the row keeps the kind it had —
// while rscheduledjob.Update's own agent re-derive is itself gated on the
// CURRENT kind already being agent, so it does not stand in for the missing
// write either.
//
// So the push answers 200, the row reads back `workflow`, automationDelta marks
// `action.kind` changed on the next status, the next push sends the same patch,
// and nothing the author can write in the file ever closes it. That is the same
// permanent-drift class as the two references doors above, by a different field.
//
// ONLY THAT ONE DIRECTION IS REFUSED, and deliberately:
//
//   - agent → agent is no change at all, and refusing it would refuse every
//     folder that simply records the inline action it created.
//   - workflow → saved_agent and saved_agent → workflow BOTH WORK: each has its
//     own setter, and the handler reaches it from whichever kind the row holds.
//     Refusing them would be the over-reach this loop is careful to avoid.
//   - agent → workflow and agent → saved_agent work for the same reason. It is
//     the way BACK that has no route.
//   - An ABSENT `action` is UNMANAGED — the push sends none and the row keeps
//     its kind — so there is no change to refuse.
//   - A CREATE has no row, and the create route stores whatever kind it is
//     given. Nothing to compare against.
//
// It never fires beside the references refusal: that one needs an EFFECTIVE
// kind of workflow or saved_agent, and this one needs the file to declare
// `agent`, which makes the effective kind agent. A file declaring an agent
// action AND references against a workflow row gets this refusal alone — which
// is the right one, since the action kind is why the references would be lost.
func refuseAutomationActionKindSwitch(file *automationFile, row *api.Automation) error {
	if file.Action == nil || file.Action.Kind != api.ActionKindAgent {
		return nil
	}
	if row.Action == nil {
		return nil
	}
	switch row.Action.Kind {
	case api.ActionKindWorkflow, api.ActionKindSavedAgent:
		return fmt.Errorf("\"action\" declares kind %q and this automation holds a %s action: the update route writes a child action row only for a workflow or a saved_agent action, and there is no route back to an inline agent one — so the push would answer 200, leave the %s action in place, and report action.kind as drift for ever. Change the action in the web app, or declare the %s action the row actually has",
			api.ActionKindAgent, row.Action.Kind, row.Action.Kind, row.Action.Kind)
	}
	return nil
}

// effectiveAutomationActionKind names the action kind a push would LEAVE
// BEHIND: the file's when it declares one, and otherwise the row's.
//
// The fall-through is the whole point. An absent `action` is UNMANAGED, not
// "no action" — the push sends none and the row keeps the kind it has — so the
// file alone cannot say which kind its references would land beside. Only the
// row can.
func effectiveAutomationActionKind(file *automationFile, row *api.Automation) string {
	if file.Action != nil {
		return file.Action.Kind
	}
	if row != nil && row.Action != nil {
		return row.Action.Kind
	}
	return ""
}

var automationTriggerKinds = []string{
	api.TriggerKindCron, api.TriggerKindEmail, api.TriggerKindWebhook,
	api.TriggerKindTable, api.TriggerKindEvent, api.TriggerKindMailbox,
}

var automationActionKinds = []string{
	api.ActionKindAgent, api.ActionKindWorkflow, api.ActionKindSavedAgent,
}

func validAutomationTrigger(kind string) bool {
	for _, known := range automationTriggerKinds {
		if kind == known {
			return true
		}
	}
	return false
}

func validAutomationAction(kind string) bool {
	for _, known := range automationActionKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// automationReferenceKinds maps the SERVER's reference vocabulary onto the CLI's
// dependency kinds, which are two different contracts that only mostly coincide.
//
// The one that catches people: a table is `model` here and `table` in
// ronja.json, because the server's reference kinds mirror models_v2's own name.
//
// An empty value means the kind has NO alias vocabulary — `dataapp`,
// `mcpserver` and `feature` are reference kinds the alias layer cannot express,
// so a folder could only name one by committing a raw id. resolveAutomationRefs
// REFUSES those rather than committing one silently: an id belongs to exactly
// one organization, and a folder carrying one deploys green to the second stack
// and grants an id nobody there has.
var automationReferenceKinds = map[string]string{
	"model":     markers.KindTable,
	"workflow":  markers.KindWorkflow,
	"note":      markers.KindNote,
	"secret":    markers.KindSecret,
	"mailbox":   markers.KindMailbox,
	"dataapp":   "",
	"mcpserver": "",
	"feature":   "",
}

// sortedKeys orders any string-keyed map, so a report built from one reads the
// same way twice. Generic rather than one helper per value type: its callers
// hold raw JSON, errors and strings against the same file paths, and a per-type
// copy is how two of them would come to sort differently.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// automationRefField is one id-bearing field of an automation file: where it is
// written, which dependency kind its value names, the value as committed, and
// how to write the resolved id back.
//
// A LIST OF FIELDS rather than a textual pass over the document, and §3.5b of
// the plan settles why: markers.Rewrite substitutes by FAMILY, and the kind this
// loop most needs — `note` — has no family. Supporting both mechanisms over one
// document would be a fork with no precedence rule that worked for four kinds
// and silently failed for the fifth. Field-wise also gives the refusal a place
// to point at, which a textual pass cannot.
type automationRefField struct {
	Where string
	Kind  string
	Value string
	set   func(string)
}

// refFields enumerates every field of this file whose value names a row.
//
// The set is CLOSED and finite, which is what makes field-wise resolution
// possible at all: action.config.workflowID, action.config.agentID,
// references[].resourceID, mailboxID and watchedTableIds. A field added to the
// schema that carries an id and is not listed here would be committed as a raw
// id with nothing reporting it — so this function and automationFile are edited
// together.
func (a *automationFile) refFields() []automationRefField {
	var out []automationRefField
	if a.Action != nil {
		if id := a.Action.Config.WorkflowID; id != nil && *id != "" {
			out = append(out, automationRefField{
				Where: "action.config.workflowID", Kind: markers.KindWorkflow,
				Value: *id,
				set:   func(resolved string) { *id = resolved },
			})
		}
		if id := a.Action.Config.AgentID; id != nil && *id != "" {
			out = append(out, automationRefField{
				Where: "action.config.agentID", Kind: markers.KindAgent,
				Value: *id,
				set:   func(resolved string) { *id = resolved },
			})
		}
	}
	if a.MailboxID != nil && *a.MailboxID != "" {
		out = append(out, automationRefField{
			Where: "mailboxID", Kind: markers.KindMailbox, Value: *a.MailboxID,
			set: func(id string) { a.MailboxID = &id },
		})
	}
	if a.WatchedTableIds != nil {
		list := *a.WatchedTableIds
		for i := range list {
			i := i
			out = append(out, automationRefField{
				Where: fmt.Sprintf("watchedTableIds[%d]", i), Kind: markers.KindTable,
				Value: list[i], set: func(id string) { list[i] = id },
			})
		}
	}
	if a.References != nil {
		list := *a.References
		for i := range list {
			i := i
			out = append(out, automationRefField{
				Where: fmt.Sprintf("references[%d].resourceID", i),
				// The SERVER's kind, translated. An unmapped kind answers "" and
				// is refused by the resolver rather than resolved against a
				// dependency vocabulary that has no word for it.
				Kind:  automationReferenceKinds[list[i].Kind],
				Value: list[i].ResourceID,
				set:   func(id string) { list[i].ResourceID = id },
			})
		}
	}
	return out
}

// resolveAutomationRefs replaces this file's aliases with the ids the selected
// stack binds them to, IN PLACE, and refuses every value it cannot resolve.
//
// Three outcomes per field, and the middle one is the whole alias layer:
//
//   - already an id of that kind — left exactly as found, the same rule
//     markers.IsResourceID gives every other loop.
//   - a name this stack binds — substituted.
//   - anything else — REFUSED, naming the field. A name sent literally is stored
//     as a resource id, resolves to nothing, and fails at the first run; and for
//     a kind the alias layer has no word for, the only way to make the push
//     succeed would be committing one organization's id into a shared folder.
func resolveAutomationRefs(file *automationFile, codec aliasCodec, stack string) error {
	var refusals []string
	for _, field := range file.refFields() {
		switch {
		case field.Kind == "":
			refusals = append(refusals, fmt.Sprintf(
				"%s: this reference kind has no alias, so the only way to name it here is one organization's raw id — which deploys green to a second stack and grants an id nobody there has. Manage this automation in the web app until the alias layer covers it",
				field.Where))
		case markers.IsResourceID(field.Kind, field.Value):
			// A literal id. Accepted, as everywhere else; checkAliases refuses it
			// separately when it is an id some alias is already bound to.
		default:
			id, bound := codec.resolve(field.Kind, field.Value)
			if !bound {
				refusals = append(refusals, fmt.Sprintf(
					"%s: alias %q has no bind in %s — declare it under \"dependencies\" as a %s and bind it (`ronja bind`), or write the %s's id",
					field.Where, field.Value, describeStackFor(stack), field.Kind, field.Kind))
				continue
			}
			field.set(id)
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	sort.Strings(refusals)
	return fmt.Errorf("%s", strings.Join(refusals, "\n    "))
}

// describeStackFor names the stack a bind would live under, or the unnamed
// legacy entry, so a refusal reads correctly for both manifest shapes.
func describeStackFor(stack string) string {
	if stack == "" {
		return "the unnamed \"instances\" entry"
	}
	return fmt.Sprintf("stack %q", stack)
}

// refuseUnpatchableAutomation refuses a file that would CHANGE a field the
// update body cannot carry.
//
// ⚠️ THREE fields, not one. UpdateAutomationInput carries no `triggerKind`, no
// `referenceBased` and no `eventName` — verified against the struct, not
// assumed — so a push that sent the change would get a 200 for a write that did
// not happen. That is the §1.1 class of bug this loop exists to remove, and it
// is why api.AutomationPatch has no field for any of the three: having nowhere
// to put the value is what makes the silent version unrepresentable.
//
// The remedy named is the WEB APP, and never "delete it and push again":
// DELETE is a 30-day soft delete that auto-pauses the row and leaves it in a
// trash this CLI cannot empty (purge is human-only), so each attempt would leave
// one more trashed automation behind and the restored row would then meet the
// re-enable guard.
//
// A file that declares a value the row already has is NOT a change and is not
// refused — that is the ordinary state of a folder that recorded what it created.
//
// ⚠️ A FOURTH refusal rides here, and it is the same class rather than the same
// field: references the EFFECTIVE action cannot hold. The update body carries
// them happily and the server takes them — a file that declares `references` and
// no `action` reaches the reference branch precisely because it declared no
// action — but a workflow or saved_agent row reads its config back off the
// stored action JSON, which has nowhere to keep them. So the write answers 200,
// stores nothing readable, and the folder reports drift no edit can ever close:
// the §1.1 shape this function exists for.
//
// It lives HERE, in the one row-aware refusal both `push` and `status` run,
// rather than in a second function each surface would have to remember to call —
// a report that disagreed with the push it describes is this loop's own worst
// failure. It is a BACKSTOP, never a replacement: checkAutomationVocabulary
// refuses the declared half before any network call, which is strictly better.
//
// ⚠️ AND A FIFTH: an action kind that cannot be switched BACK. See
// refuseAutomationActionKindSwitch.
func refuseUnpatchableAutomation(file *automationFile, row *api.Automation) error {
	if err := refuseAutomationReferencesFor(effectiveAutomationActionKind(file, row), file.References); err != nil {
		return err
	}
	if err := refuseAutomationPromptFor(effectiveAutomationActionKind(file, row), file.Prompt); err != nil {
		return err
	}
	if err := refuseAutomationActionKindSwitch(file, row); err != nil {
		return err
	}

	var refusals []string
	if file.TriggerKind != nil && *file.TriggerKind != row.TriggerKind {
		refusals = append(refusals, fmt.Sprintf(
			"\"triggerKind\" declares %q and this automation is a %q trigger", *file.TriggerKind, row.TriggerKind))
	}
	if file.ReferenceBased != nil && *file.ReferenceBased != row.ReferenceBased {
		refusals = append(refusals, fmt.Sprintf(
			"\"referenceBased\" declares %v and the row is %v", *file.ReferenceBased, row.ReferenceBased))
	}
	if file.EventName != nil && *file.EventName != row.EventName {
		refusals = append(refusals, fmt.Sprintf(
			"\"eventName\" declares %q and the row subscribes to %q", *file.EventName, row.EventName))
	}
	if len(refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s.\n    The update route carries none of these fields, so sending the change would answer 200 for a write that never happened. Change it in the web app — deleting and re-pushing is not an answer either: a delete is a 30-day soft delete this CLI cannot empty, so every attempt would leave another paused row behind",
		strings.Join(refusals, ", and "))
}

// automationCreateInput builds the create body for a file with no row behind it.
//
// It sends only what the file DECLARES, which matters more here than it looks:
// the server REJECTS a field belonging to a different trigger kind rather than
// ignoring it, so a body that filled in every key would be refused for carrying
// `cronExpr` on an email trigger.
func automationCreateInput(featureID, name string, file *automationFile) api.CreateAutomationInput {
	in := api.CreateAutomationInput{FeatureID: featureID, Name: name}
	if file.Description != nil {
		in.Description = *file.Description
	}
	if file.Prompt != nil {
		in.Prompt = *file.Prompt
	}
	if file.TriggerKind != nil {
		in.TriggerKind = *file.TriggerKind
	}
	if file.CronExpr != nil {
		in.CronExpr = *file.CronExpr
	}
	if file.Timezone != nil {
		in.Timezone = *file.Timezone
	}
	if file.ReportingTimezone != nil {
		in.ReportingTimezone = *file.ReportingTimezone
	}
	if file.EmailAllowedFromDomains != nil {
		in.EmailAllowedFromDomains = nonNilStrings(*file.EmailAllowedFromDomains)
	}
	if file.EmailAllowedFromAddresses != nil {
		in.EmailAllowedFromAddresses = nonNilStrings(*file.EmailAllowedFromAddresses)
	}
	if file.WatchedTableIds != nil {
		in.WatchedTableIds = nonNilStrings(*file.WatchedTableIds)
	}
	if file.EventName != nil {
		in.EventName = *file.EventName
	}
	if file.MailboxID != nil {
		in.MailboxID = *file.MailboxID
	}
	if file.MailboxFilter != nil {
		in.MailboxFilter = file.MailboxFilter
	}
	in.RateLimitPerMinute = file.RateLimitPerMinute
	in.RateLimitPerDay = file.RateLimitPerDay
	in.Enabled = file.Enabled
	if file.Model != nil {
		in.Model = *file.Model
	}
	in.ReferenceBased = file.ReferenceBased
	if file.References != nil {
		in.References = nonNilRefs(*file.References)
	}
	if file.ApprovedTools != nil {
		in.ApprovedTools = nonNilStrings(*file.ApprovedTools)
	}
	if file.ApprovedToolGroups != nil {
		in.ApprovedToolGroups = nonNilStrings(*file.ApprovedToolGroups)
	}
	// Nothing to carry forward on a create: there is no row yet, so an unmanaged
	// config field is simply not set rather than being read off one.
	in.Action = file.Action.wire(nil)
	return in
}

// automationDelta compares one file's DECLARED fields against the row and
// answers both halves at once: which fields differ, and the patch that would
// make them match.
//
// ONE function for both because the report and the write must never disagree.
// Two of them is how a status comes to say "nothing to push" about a push that
// then writes something, or the reverse — and this loop's whole claim is that
// its report is what its push will do.
//
// Fields the file does not declare are not compared and not patched: absent is
// unmanaged (see automationFile).
//
// ⚠️ String LISTS compare as SETS. Nothing in any of these lists carries meaning
// in its order, and the server is free to return one in whatever order it stored
// it — so an ordered comparison would report permanent drift on a folder whose
// declaration is already correct, with nothing the author could edit to fix it.
func automationDelta(name string, file *automationFile, row *api.Automation) ([]string, api.AutomationPatch) {
	var changed []string
	var patch api.AutomationPatch
	mark := func(field string) { changed = append(changed, field) }

	// The name is ALWAYS declared — it is the filename — so a renamed file is a
	// renamed automation. (It is also a new binding key, which is why a rename
	// shows up as an orphan plus a create rather than a rename; see
	// automationOrphans.)
	if name != row.Name {
		mark("name")
		patch.Name = &name
	}
	diffString := func(field string, declared *string, current string, into **string) {
		if declared == nil || *declared == current {
			return
		}
		mark(field)
		value := *declared
		*into = &value
	}
	diffString("description", file.Description, row.Description, &patch.Description)
	diffString("prompt", file.Prompt, row.Prompt, &patch.Prompt)
	diffString("cronExpr", file.CronExpr, row.CronExpr, &patch.CronExpr)
	diffString("timezone", file.Timezone, row.Timezone, &patch.Timezone)
	diffString("reportingTimezone", file.ReportingTimezone, row.ReportingTimezone, &patch.ReportingTimezone)
	diffString("model", file.Model, row.Model, &patch.Model)
	diffString("mailboxID", file.MailboxID, row.MailboxID, &patch.MailboxID)

	if file.Enabled != nil && *file.Enabled != row.Enabled {
		mark("enabled")
		value := *file.Enabled
		patch.Enabled = &value
	}

	diffList := func(field string, declared *[]string, current []string, into **[]string) {
		if declared == nil || sameStringSet(*declared, current) {
			return
		}
		mark(field)
		// ⚠️ nonNilStrings, not the slice as decoded. A POINTER TO A NIL SLICE
		// marshals as `null`, and the server reads an explicit null as "absent" —
		// so clearing the last entry of a list would send "leave it alone".
		value := nonNilStrings(*declared)
		*into = &value
	}
	diffList("approvedTools", file.ApprovedTools, row.ApprovedTools, &patch.ApprovedTools)
	diffList("approvedToolGroups", file.ApprovedToolGroups, row.ApprovedToolGroups, &patch.ApprovedToolGroups)
	diffList("emailAllowedFromDomains", file.EmailAllowedFromDomains, row.EmailAllowedFromDomains, &patch.EmailAllowedFromDomains)
	// ⚠️ The spelling swap: the file's `emailAllowedFromAddresses` is compared
	// against the ROW's `emailAllowedFromAddrs`. Comparing like-named fields is
	// exactly the bug §1.1.3 describes, and it would report every email trigger
	// as permanently drifted.
	diffList("emailAllowedFromAddresses", file.EmailAllowedFromAddresses, row.EmailAllowedFromAddrs, &patch.EmailAllowedFromAddresses)
	diffList("watchedTableIds", file.WatchedTableIds, row.WatchedTableIDs, &patch.WatchedTableIds)

	if file.References != nil {
		current := []api.AutomationReference{}
		if row.Action != nil {
			current = row.Action.Config.References
		}
		if !sameReferenceSet(*file.References, current) {
			mark("references")
			value := nonNilRefs(*file.References)
			patch.References = &value
		}
	}
	if file.MailboxFilter != nil && !sameMailboxFilter(*file.MailboxFilter, row.MailboxFilter) {
		mark("mailboxFilter")
		value := *file.MailboxFilter
		patch.MailboxFilter = &value
	}
	if !sameIntPtr(file.RateLimitPerMinute, row.RateLimitPerMinute) {
		mark("rateLimitPerMinute")
		patch.RateLimitPerMinute = file.RateLimitPerMinute
	}
	if !sameIntPtr(file.RateLimitPerDay, row.RateLimitPerDay) {
		mark("rateLimitPerDay")
		patch.RateLimitPerDay = file.RateLimitPerDay
	}
	if fields := automationActionChanges(file.Action, row.Action); len(fields) > 0 {
		changed = append(changed, fields...)
		// ⚠️ Built AGAINST THE ROW, never sent as declared. The action's config is
		// replaced wholesale server-side, so the body has to carry the row's own
		// value for every config field this file does not manage — otherwise
		// changing the workflow clears the parameter values beside it, silently
		// and without appearing in `changed`. See automationActionConfigFile.
		patch.Action = file.Action.wire(row.Action)
	}
	sort.Strings(changed)
	return changed, patch
}

// automationActionChanges names the action fields a push would change.
//
// Only the config fields the DECLARED kind uses are compared, because the
// response's config is not the input's: an inline `agent` action's config is
// ignored on the way in (it is built from the body's top-level fields) and
// populated on the way out, so comparing `action.config.prompt` for an agent
// action would report a difference that no patch could ever close.
//
// An UNDECLARED config field is not compared, on the same rule the top level
// keeps — and it is only honest because wire() then carries the row's value
// forward. Comparing it here while clearing it there is exactly the pair that
// made an unmanaged field vanish without appearing in `changed`.
//
// The kind is always declared (checkAutomationVocabulary refuses an action
// without one), so there is no fall-through to the row's kind: comparing a
// workflow file's config against a saved-agent row would answer about fields
// neither of them has.
func automationActionChanges(declared *automationActionFile, current *api.AutomationAction) []string {
	if declared == nil {
		return nil
	}
	if current == nil {
		return []string{"action"}
	}
	var out []string
	if declared.Kind != current.Kind {
		out = append(out, "action.kind")
	}
	switch declared.Kind {
	case api.ActionKindWorkflow:
		if id := declared.Config.WorkflowID; id != nil && *id != current.Config.WorkflowID {
			out = append(out, "action.config.workflowID")
		}
		if pv := declared.Config.ParameterValues; pv != nil &&
			!sameParameterValues(*pv, current.Config.ParameterValues) {
			out = append(out, "action.config.parameterValues")
		}
	case api.ActionKindSavedAgent:
		if id := declared.Config.AgentID; id != nil && *id != current.Config.AgentID {
			out = append(out, "action.config.agentID")
		}
		if prompt := declared.Config.Prompt; prompt != nil && *prompt != current.Config.Prompt {
			out = append(out, "action.config.prompt")
		}
	}
	return out
}

// sameParameterValues compares two parameter maps, treating an absent map and an
// empty one as the same answer.
//
// ⚠️ Not tidiness: the response omits an empty `parameterValues` entirely, so a
// file declaring `{}` against a row that has none would report drift the push
// could never close — permanent drift with nothing the author could edit to fix
// it, which is what sameStringSet exists to prevent one level up.
func sameParameterValues(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// automationMailboxAuthority reports which part of a file needs the mailbox
// privileges, or "" when none does.
//
// A mailbox trigger and a mailbox-kind reference both require USR_ADMIN AND an
// admin-scoped token, while this loop's own scope is `automation` — so the CLI
// says so before sending rather than letting the push half-apply a folder and
// then earn a 403 on the fourth file.
func automationMailboxAuthority(file *automationFile) string {
	if file.TriggerKind != nil && *file.TriggerKind == api.TriggerKindMailbox {
		return "a mailbox trigger"
	}
	if file.MailboxID != nil && *file.MailboxID != "" {
		return "a mailbox trigger"
	}
	if file.References != nil {
		for _, ref := range *file.References {
			if ref.Kind == "mailbox" {
				return "a mailbox reference"
			}
		}
	}
	return ""
}

// The comparison helpers. Each is here rather than inline because automationDelta
// is the one place the report and the write agree, and a comparison written twice
// is the place they would stop.

func sameStringSet(a, b []string) bool {
	left := append([]string{}, a...)
	right := append([]string{}, b...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func sameReferenceSet(a, b []api.AutomationReference) bool {
	key := func(refs []api.AutomationReference) []string {
		out := make([]string, 0, len(refs))
		for _, ref := range refs {
			out = append(out, ref.Kind+"\x00"+ref.ResourceID)
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

func sameMailboxFilter(a, b api.AutomationMailboxFilter) bool {
	return sameStringSet(a.FromDomains, b.FromDomains) &&
		sameStringSet(a.FromAddrs, b.FromAddrs) &&
		sameStringSet(a.SubjectContains, b.SubjectContains) &&
		a.RequireAttachment == b.RequireAttachment &&
		a.Label == b.Label
}

func sameIntPtr(a, b *int) bool {
	// An absent declaration is UNMANAGED, so it matches whatever the row holds.
	if a == nil {
		return true
	}
	return b != nil && *a == *b
}

// nonNilStrings turns a possibly-nil slice into an empty one.
//
// ⚠️ Load-bearing on every patch field, not tidiness: `&([]string)(nil)`
// marshals as `null`, and an explicit null reads on the server as "absent" —
// which is the opposite of what a caller clearing the last entry of a list means.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilRefs(in []api.AutomationReference) []api.AutomationReference {
	if in == nil {
		return []api.AutomationReference{}
	}
	return in
}
