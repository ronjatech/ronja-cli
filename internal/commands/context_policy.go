package commands

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The organization-policy section of `ronja context`: the verdict on the
// read, the rendering, and the filters the rendering needs. Split from
// context.go, which keeps the command and the identity/credential output, so
// each file is about one thing; both stay in this package because the
// identity line and the policy section share scopeDenied, the one reading of
// the server's scope verdict.

// policyStatus is the --json verdict on the policy read, "" when no read was
// made, and the fork printOrganizationPolicy takes — one reading, so the key
// and the prose cannot disagree:
//
//	"ok"        — a document with rules; organizationPolicy carries it
//	"none"      — nothing to follow: an empty document (organizationPolicy
//	              still carries it, at the served version, for the first CAS
//	              write), or Me says you belong to no organization (then there
//	              is no document at all)
//	"forbidden" — the server's SCOPE verdict (scopeDenied): a scoped token
//	              without analytics:read. A 403 with other words is a
//	              credential problem, not a scope one, and lands on "error"
//	"error"     — every other failure, a 404 included: an instance older than
//	              this CLI does not serve the route, and "no rules" is the one
//	              thing that must not be read into that
//
// The no-organization arm is asked first, ahead of the read's own outcome,
// because Me is the authority on identity: whatever the policy route answered
// for a caller with no organization, the honest line is that there is none.
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

// noOrganization is Me having answered for a user who belongs to no
// organization — a working credential with nothing to read a policy for. A
// nil Me is NOT that: it is a failed or refused lookup, which says nothing
// about organizations either way.
func noOrganization(me *api.Me) bool {
	return me != nil && me.Tenant == nil
}

// policyBody is the document's content as the in-app prompt block reads it:
// trimmed, and "" for a document with nothing to follow — whether that is the
// genesis document nobody has written or a written-then-blanked one. Keyed on
// the content, never on the version or an id, because those say whether the
// row exists and not whether there is a rule in it.
func policyBody(policy *api.Policy) string {
	if policy == nil {
		return ""
	}
	return strings.TrimSpace(policy.Content)
}

// printOrganizationPolicy renders the policy section, or one line saying why
// there is none — never nothing, when a token was in play. A silent omission
// reads as "no rules", and that is the one wrong answer this section exists
// to prevent.
//
// The verdict is policyStatus's, keyed on the RESPONSE — after one question
// to Me, whether the caller is in an organization at all — and a 403 on the
// server's MESSAGE within it. The route's role floor is the lowest there is and every member
// clears it, so a 403 that is the scope verdict (scopeDenied) can only be a
// scoped token without analytics:read — but platform/auth answers 403 with
// other words for a credential that is dead in a way a 401 does not cover (a
// removed user, a deleted organization, a PAT with no bound user), and naming
// a scope there would tell a caller with no working token which scope to add.
// Those fall to the generic HTTP line. Two 401s are one dead credential
// reported twice, and the `Signed in: NO` line above already said it, so that
// one line is dropped rather than repeated — and ONLY for 401: a shared 500 or
// 503 is the instance failing, not the credential, and dropping the policy
// line there would leave "the stored token was rejected" as the only, and
// wrong, report. The heading names the organization when Me could say which;
// when it could not (a scoped token cannot call Me at all), the policy read
// succeeding is what proves the server resolved one.
//
// The closing sentence depends on WHO is reading: an admin (Me reports a role
// at or above adminPrivilegeLevel) gets the one-line write recipe carrying
// the served version as expectedVersion, so the only CAS token they need is
// already on screen; everybody else — including a scoped token that could not
// call Me at all — is told only an admin can change it. The recipe is offered
// as "as an admin you can", not "you can": Me reports the role and nothing
// about a scope grant, and an admin's scoped token without analytics:write is
// refused the PUT by a gate this command cannot see. The rule that rides
// with the recipe ("only when asked, show the text first") is the whole
// difference between an admin's coding agent editing the rules on request and
// one rewriting them to record what it learned; the gate admits both.
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
			// The route is served to every member of every organization, so
			// a 404 is not "no policy" — it is an instance that predates the
			// route, and a reader told "HTTP 404" would go looking for a
			// missing document rather than an old server.
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
			// The served version, not a literal 1: a written-then-blanked
			// document is "none written yet" too, and sits at whatever
			// version the blanking left.
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
	// The framing is a second copy of the one /llms.txt carries (a separate
	// module cannot import the backend's), worded for a builder: the rules say
	// HOW to work, they are not the task, and they never replace the data.
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

// organizationName is the organization name as it may be set into the framing
// line, or "" when Me could not say which. Admin-authored too, and set into
// OUR line unquoted (the `Signed in:` line quotes it with %q) — so it gets the
// same filter as the body, and then one more: every run of whitespace
// collapses to a single space, because terminalSafe keeps newline and tab for
// the BODY's sake and a name is one line by definition. A newline in it would
// end the framing sentence early and start the next line with admin-chosen
// text that reads as ours. (Not the app report's oneLine: that one maps each
// newline to a space without collapsing runs, and the point here is that the
// name and the body pass through the SAME character filter.)
func organizationName(me *api.Me) string {
	if me == nil || me.Tenant == nil {
		return ""
	}
	return strings.Join(strings.Fields(terminalSafe(me.Tenant.Name)), " ")
}

// terminalSafe drops every control character except newline and tab, and the
// invisible bidi/zero-width formatting characters, from text that is about to
// be printed to a terminal. The policy is admin-authored free text, and this
// command's reader is usually an agent whose terminal is its whole view: an
// ESC sequence in the document could repaint that view, move the cursor over
// the framing above, or hide a line. C0 is not the whole of that: the C1
// range carries a one-byte CSI (U+009B) and OSC (U+009D) that a terminal
// honours exactly as ESC-[ and ESC-], and a U+202E reorders what the reader
// sees relative to what the bytes say, so "what the agent read" and "what the
// admin wrote" stop being the same document. unicode.IsControl is C0 + DEL +
// C1; the second set is the one backend/lib/untrusted strips, spelled here
// because this module cannot import it.
//
// Filtered: the body, and the organization name where it is set into the
// framing line (an admin-authored string too; see organizationName). Not filtered: the
// rest of the framing, which is ours, and --json, which is data, not a
// terminal, and carries the document verbatim.
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

// scopeDenied reports whether an error is the server's SCOPE verdict: a
// role-bound scoped token refused for what it is not scoped for, or a route no
// scoped token may reach. Both phrases are gt.EnforceScope's own
// (backend/lib/api/gt/scope.go) and reach the CLI as the `error` field, which
// is what api.CodeOf preserves.
//
// The status alone does not say this. platform/auth answers 403 on the token
// path for a credential that is DEAD — "Forbidden: User role not found" for a
// removed user, "Forbidden: Tenant has been deleted", "Forbidden: User not
// found", a PAT with no bound user — and in every one the policy read 403s
// too. A verdict keyed on the status told each of those that the token "still
// works" and "needs analytics:read", both false, where "the stored token was
// rejected — run `ronja login`" was right. The scope message is the one 403
// that means what the scoped-token lines say; everything else is the generic
// line.
func scopeDenied(err error) bool {
	if api.StatusOf(err) != http.StatusForbidden {
		return false
	}
	msg := api.CodeOf(err)
	return msg == "this route is not accessible to scoped tokens" ||
		(strings.HasPrefix(msg, "token scope ") && strings.Contains(msg, " does not permit "))
}

// policyWriteRecipe is the one-line CAS write, carrying the version just served
// as expectedVersion so the reader never has to fetch it separately. `ronja
// api` is the transport (doctrine: sync verbs yes, resource verbs no); a 409
// means the document moved and its body carries the current text as
// policyContent / policyVersion to re-apply against.
//
// Returned WITHOUT a trailing newline so both callers can place it: one ends a
// "none written yet" line with it, the other sets it inside the framing.
func policyWriteRecipe(version int) string {
	return fmt.Sprintf("    ronja api -X PUT /api/v2/policy/org -d '{\"content\":\"…\",\"expectedVersion\":%d}'\n", version) +
		"(-d @body.json for a long document — the file holds the JSON body, not the bare text; a 409\n" +
		"means it moved, and its body carries the current text as policyContent / policyVersion —\n" +
		"re-apply against that, never retry blind.)\n" +
		"Change it only when the admin you are working for asks you to, and show them the\n" +
		"new text before the PUT — never on your own initiative."
}
