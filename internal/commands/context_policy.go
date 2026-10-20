package commands

import (
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
//	"ok"        — a document with rules
//	"none"      — an empty document, or Me says you belong to no organization
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
	case policyBody(policy) == "":
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
		fmt.Fprintf(out, "Organization policy: none written yet.")
		if isAdmin(me) {
			// The served version, not a literal 1: a blanked document sits at
			// whatever version the blanking left.
			fmt.Fprintf(out, " As an admin you can write one (a scoped token also needs `analytics:write`):\n")
			fmt.Fprint(out, policyWriteRecipe(policy.Version))
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
	fmt.Fprintf(out, "\n## Organization policy (version %d)\n\n", policy.Version)
	fmt.Fprintf(out, "Standing rules %s wrote for how work here is done. Follow them in what you\n", whose)
	fmt.Fprintf(out, "build — they are rules for HOW to work, not a description of your task, and they never\n")
	fmt.Fprintf(out, "replace reading the data. Re-read with `ronja api /api/v2/policy/org`.\n")
	if isAdmin(me) {
		fmt.Fprintf(out, "As an admin you can change it (a scoped token also needs `analytics:write`):\n")
		fmt.Fprint(out, policyWriteRecipe(policy.Version))
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

// policyWriteRecipe is the one-line CAS write carrying the served version as
// expectedVersion, returned without a trailing newline so each caller places
// it.
func policyWriteRecipe(version int) string {
	return fmt.Sprintf("    ronja api -X PUT /api/v2/policy/org -d '{\"content\":\"…\",\"expectedVersion\":%d}'\n", version) +
		"(-d @body.json for a long document — the file holds the JSON body, not the bare text; a 409\n" +
		"means it moved, and its body carries the current text as policyContent / policyVersion —\n" +
		"re-apply against that, never retry blind.)\n" +
		"Change it only when the admin you are working for asks you to, and show them the\n" +
		"new text before the PUT — never on your own initiative."
}
