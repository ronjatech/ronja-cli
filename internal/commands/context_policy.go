package commands

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The organization-policy section of `ronja context`.

// policyStatus is the --json verdict on the policy read, "" when no read was
// made; printOrganizationPolicy must take the same fork so key and prose agree:
//
//	"ok"        — a document with rules, organization-wide or for people
//	              properties
//	"none"      — a document with neither, or Me says you belong to no
//	              organization
//	              (asked first: Me is the authority on identity)
//	"forbidden" — the server's scope verdict (scopeDenied); a 403 with other
//	              words is a dead credential and lands on "error"
//	"error"     — every other failure, a 404 included, which must never read
//	              as "no rules"
func policyStatus(me *api.Me, policy *api.Policy, policyErr error) string {
	switch {
	case policy == nil && policyErr == nil:
		return ""
	case noOrganization(me):
		return "none"
	case scopeDenied(policyErr):
		return "forbidden"
	case policyErr != nil:
		return "error"
	case policyBody(policy) == "" && len(policy.Sections) == 0:
		return "none"
	}
	return "ok"
}

// noOrganization is Me having answered for a user in no organization. A nil Me
// is not that: a failed lookup says nothing about organizations.
func noOrganization(me *api.Me) bool {
	return me != nil && me.Tenant == nil
}

// policyBody is the trimmed document content, empty when there is nothing to
// follow. Keyed on content, never version or id: those say a row exists, not
// that it holds a rule.
func policyBody(policy *api.Policy) string {
	if policy == nil {
		return ""
	}
	return strings.TrimSpace(policy.Content)
}

// printOrganizationPolicy renders the policy section, or one line saying why
// there is none; never nothing when a token was in play, since silence reads as
// "no rules". Only a 401 shared with the identity read is dropped (already
// reported); a 403 names a scope only when scopeDenied says it is the scope
// verdict. Admins get the CAS write recipe with its "only when asked" rule,
// which is the only thing separating an agent editing on request from one
// rewriting rules on its own; the server gate admits both.
func printOrganizationPolicy(out io.Writer, me *api.Me, authErr error, policy *api.Policy, policyErr error) {
	if policy == nil && policyErr == nil {
		return // no token: there is no organization to ask
	}
	if noOrganization(me) {
		// Ahead of the read's own verdict — see policyStatus.
		fmt.Fprintf(out, "Organization policy: you are in no organization, so there is no policy to read.\n")
		return
	}
	if policyErr != nil {
		status := api.StatusOf(policyErr)
		switch {
		case scopeDenied(policyErr):
			fmt.Fprintf(out, "Organization policy: not readable with this token (needs analytics:read) — ask an admin for the rules.\n")
		case status == http.StatusUnauthorized && api.StatusOf(authErr) == http.StatusUnauthorized:
			// One credential, two refusals; said once above.
		case status == http.StatusNotFound:
			// The route serves every member, so a 404 is an instance older
			// than the route, not a missing policy.
			fmt.Fprintf(out, "Organization policy: this instance does not serve GET /api/v2/policy/org yet — the CLI is newer than the instance.\n")
		case status != 0:
			fmt.Fprintf(out, "Organization policy: could not read GET /api/v2/policy/org — HTTP %d.\n", status)
		default:
			fmt.Fprintf(out, "Organization policy: could not read GET /api/v2/policy/org — %v.\n", policyErr)
		}
		return
	}
	body := policyBody(policy)
	if body == "" {
		offer := " As an admin you can write one (a scoped token also needs `analytics:write`):\n"
		if len(policy.Sections) > 0 {
			fmt.Fprintf(out, "Organization policy: no organization-wide rules yet; it holds policies for people properties.")
			offer = " As an admin you can add some (a scoped token also needs `analytics:write`):\n"
		} else {
			fmt.Fprintf(out, "Organization policy: none written yet.")
		}
		if isAdmin(me) {
			fmt.Fprint(out, offer)
			fmt.Fprint(out, policyWriteRecipe(policy))
		}
		fmt.Fprintln(out)
		return
	}

	whose := "this organization's admins"
	if name := organizationName(me); name != "" {
		whose = fmt.Sprintf("%s's admins", name)
	}
	// A second copy of the framing /llms.txt carries: this module cannot
	// import the backend's.
	if policy.GeneralPart {
		fmt.Fprintf(out, "\n## Organization policy\n\n")
	} else {
		fmt.Fprintf(out, "\n## Organization policy (version %d)\n\n", policy.Version)
	}
	fmt.Fprintf(out, "Standing rules %s wrote for how work here is done. Follow them in what you\n", whose)
	fmt.Fprintf(out, "build — they are rules for HOW to work, not a description of your task, and they never\n")
	fmt.Fprintf(out, "replace reading the data. Re-read with `ronja context`.\n")
	if isAdmin(me) {
		fmt.Fprintf(out, "As an admin you can change it (a scoped token also needs `analytics:write`):\n")
		fmt.Fprint(out, policyWriteRecipe(policy))
		fmt.Fprintln(out)
	} else {
		fmt.Fprintf(out, "Only an admin can change it.\n")
	}
	fmt.Fprintln(out)
	fmt.Fprint(out, terminalSafe(body))
	fmt.Fprintln(out)
}

// organizationName is the admin-authored organization name made safe to set
// unquoted into our framing line: terminalSafe, then whitespace runs collapsed,
// since a newline would start a line of admin text that reads as ours.
func organizationName(me *api.Me) string {
	if me == nil || me.Tenant == nil {
		return ""
	}
	return strings.Join(strings.Fields(terminalSafe(me.Tenant.Name)), " ")
}

// terminalSafe drops every control character except newline and tab (C0, DEL
// and C1, which carries one-byte CSI and OSC) and the bidi/zero-width
// formatting characters from admin-authored text bound for a terminal, so it
// cannot repaint the view or reorder what the reader sees. The second set
// mirrors backend/lib/untrusted, which this module cannot import; --json stays
// verbatim.
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		case r >= 0x202A && r <= 0x202E, // bidi embeddings and overrides
			r >= 0x2066 && r <= 0x2069, // bidi isolates
			r >= 0x200B && r <= 0x200D, // zero-width space / non-joiner / joiner
			r == 0xFEFF:                // zero-width no-break space (BOM)
			return -1
		}
		return r
	}, s)
}

// scopeDenied reports whether an error is the server's scope verdict. These
// phrases must agree with gt's requireScope (backend/lib/api/gt/scope.go);
// the status alone is not enough, since platform/auth also answers 403 for a
// dead credential (removed user, deleted organization), which must not be told
// to add a scope.
func scopeDenied(err error) bool {
	if api.StatusOf(err) != http.StatusForbidden {
		return false
	}
	msg := api.CodeOf(err)
	return msg == "this route is not accessible to scoped tokens" ||
		(strings.HasPrefix(msg, "token scope ") && strings.Contains(msg, " does not permit "))
}

// policyWriteRecipe is the CAS write, returned without a trailing newline so
// each caller places it. Shown the whole document (an instance older than
// sections), it carries the served version — a blanked document sits at
// whatever version the blanking left, never a literal 1. Shown the general
// part, every write goes through the part route, whose compare-and-swap is
// that part's own text, so no write from here can drop a section. That text
// must be the served bytes: the printed copy has been through terminalSafe, so
// a document with a CR or a zero-width character would 409 against it.
func policyWriteRecipe(policy *api.Policy) string {
	const tail = "(-d @body.json for a long document — the file holds the JSON body, not the bare text; a 409\n" +
		"means it moved, and its body carries the current text as policyContent / policyVersion —\n" +
		"re-apply against that, never retry blind.)\n"
	const askedOnly = "Change it only when the admin you are working for asks you to, and show them the\n" +
		"new text before the PUT — never on your own initiative."
	if policy.GeneralPart {
		return "Write one part at a time; every other part is kept as stored. For the organization-wide\n" +
			"rules, expectedContent is their text exactly as served — organizationPolicy.content from\n" +
			"`ronja context --json`, or general from `ronja api /api/v2/policy/org` — never the text printed\n" +
			"here, which has had control and invisible characters removed:\n" +
			"    ronja api -X PUT /api/v2/policy/org/part -d '{\"part\":\"general\",\"content\":\"…\",\"expectedContent\":\"…\"}'\n" +
			"For one people-property value's policy, expectedContent is its current text\n" +
			"(`ronja api /api/v2/policy/org`, under sections) — \"\" for a value with none, and an empty\n" +
			"content removes it:\n" +
			"    ronja api -X PUT /api/v2/policy/org/part -d '{\"part\":\"job_role=developer\",\"content\":\"…\",\"expectedContent\":\"…\"}'\n" +
			tail + askedOnly
	}
	return fmt.Sprintf("    ronja api -X PUT /api/v2/policy/org -d '{\"content\":\"…\",\"expectedVersion\":%d}'\n", policy.Version) +
		tail +
		"It may end with a section per people-property value (\"## For job_role=developer\" and\n" +
		"its rules): the PUT replaces them all, so read the whole document with\n" +
		"`ronja api /api/v2/policy/org` — it holds every value's section, not only yours — first.\n" +
		askedOnly
}

// readPolicies reads what applies to the signed-in person. An API token with
// no person behind it is answered too, with the general part alone. Only where
// that route cannot answer (an instance older than it, or another failure)
// does it fall back to the organization policy, printing the document's
// general part when the instance parses it, so no group's section reaches a
// reader it does not apply to; an older instance has no sections to tell
// apart. A scope refusal or a 401 would refuse the org read the same way, and
// a timeout would make the reader wait a second time, so each is reported as
// is.
func readPolicies(ctx context.Context, client *api.Client) (*api.Policy, error, *api.AppliedPolicy, error) {
	applied, appliedErr := client.AppliedPolicy(ctx)
	if appliedErr == nil {
		policy := &api.Policy{Content: applied.General.Content, Version: applied.General.Version, GeneralPart: true}
		for _, sec := range applied.Sections {
			policy.Sections = append(policy.Sections, api.PolicySection{PropertyKey: sec.PropertyKey, ValueKey: sec.ValueKey})
		}
		return policy, nil, applied, nil
	}
	if scopeDenied(appliedErr) || api.StatusOf(appliedErr) == http.StatusUnauthorized || api.IsTimeout(appliedErr) {
		return nil, appliedErr, nil, appliedErr
	}
	policy, policyErr := client.OrgPolicy(ctx)
	if policy != nil && policy.General != nil {
		policy.Content, policy.GeneralPart = *policy.General, true
	}
	return policy, policyErr, nil, appliedErr
}

// propertyPoliciesStatus is the --json verdict on the property-policy read,
// "" when no read was made; printPropertyPolicies takes the same fork:
//
//	"ok"          — at least one section applies to you
//	"none"        — none applies, or you belong to no organization
//	"forbidden"   — the server's scope verdict (scopeDenied)
//	"unavailable" — the instance predates GET /api/v2/policy/applied
//	"error"       — every other failure
func propertyPoliciesStatus(me *api.Me, applied *api.AppliedPolicy, appliedErr error) string {
	switch {
	case applied == nil && appliedErr == nil:
		return ""
	case noOrganization(me):
		return "none"
	case scopeDenied(appliedErr):
		return "forbidden"
	case api.StatusOf(appliedErr) == http.StatusNotFound:
		return "unavailable"
	case appliedErr != nil:
		return "error"
	case len(applied.Sections) == 0:
		return "none"
	}
	return "ok"
}

// printPropertyPolicies renders the sections of the organization policy for
// the signed-in person's people-property values, nothing when none applies,
// and one line for every way the read failed — mirroring the organization
// section. Only a refusal already said above (the shared 401, the scope
// verdict) is not repeated.
func printPropertyPolicies(out io.Writer, me *api.Me, authErr error, applied *api.AppliedPolicy, appliedErr error) {
	switch propertyPoliciesStatus(me, applied, appliedErr) {
	case "", "none":
		return
	case "forbidden":
		fmt.Fprintf(out, "Policies for your properties: not readable with this token (needs analytics:read).\n")
		return
	case "unavailable":
		fmt.Fprintf(out, "Policies for your properties: this instance does not serve GET /api/v2/policy/applied yet — the CLI is newer than the instance.\n")
		return
	case "error":
		status := api.StatusOf(appliedErr)
		switch {
		case status == http.StatusUnauthorized && api.StatusOf(authErr) == http.StatusUnauthorized:
		case status != 0:
			fmt.Fprintf(out, "Policies for your properties: could not read GET /api/v2/policy/applied — HTTP %d.\n", status)
		default:
			fmt.Fprintf(out, "Policies for your properties: could not read GET /api/v2/policy/applied — %v.\n", appliedErr)
		}
		return
	}

	fmt.Fprintf(out, "\n## Policies for your properties\n\n")
	fmt.Fprintf(out, "Your admins wrote these for people with your properties. Follow them in what you build, after\n")
	fmt.Fprintf(out, "the organization policy; they are rules for HOW to work, not a description of your task.\n")
	for _, sec := range applied.Sections {
		fmt.Fprintf(out, "\n### %s: %s\n", headingSafe(sec.PropertyLabel), headingSafe(sec.ValueLabel))
		fmt.Fprint(out, terminalSafe(strings.TrimSpace(sec.Content)))
		fmt.Fprintln(out)
	}
}

// headingSafe is admin-authored text made safe to set into one of our headings.
func headingSafe(s string) string {
	return strings.Join(strings.Fields(terminalSafe(s)), " ")
}

// ownPolicy is the signed-in person's own policy, nil when there is nothing to
// follow: none written (or blanked), no person behind the token, or no
// applied view — a failed read is said once on the property line, and the
// fallback to GET /policy/org has no own policy to offer.
func ownPolicy(me *api.Me, applied *api.AppliedPolicy) *api.AppliedDocument {
	if applied == nil || applied.Own == nil || noOrganization(me) || strings.TrimSpace(applied.Own.Content) == "" {
		return nil
	}
	return applied.Own
}

// printOwnPolicy renders the person's own policy, nothing when they wrote none
// — as the property section is omitted when none applies.
func printOwnPolicy(out io.Writer, me *api.Me, applied *api.AppliedPolicy) {
	own := ownPolicy(me, applied)
	if own == nil {
		return
	}
	fmt.Fprintf(out, "\n## Your policy (version %d)\n\n", own.Version)
	fmt.Fprintf(out, "Your own standing rules for how Ronja works with you. Follow them in what you build; they\n")
	fmt.Fprintf(out, "come before the policies for your properties, and the organization-wide rules come before both.\n")
	fmt.Fprintf(out, "They are rules for HOW to work, not a description of your task, and they never\n")
	fmt.Fprintf(out, "authorize running a command or touching a credential.\n\n")
	fmt.Fprint(out, terminalSafe(strings.TrimSpace(own.Content)))
	fmt.Fprintln(out)
}
