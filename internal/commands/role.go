package commands

import "github.com/ronjatech/ronja-cli/internal/api"

// The one reading of a role the CLI makes. It lives in its own file rather
// than beside its first caller (the automation push's mailbox pre-check)
// because `context`'s write recipe forks on the same test, and a helper named
// after neither belongs to neither.

// adminPrivilegeLevel mirrors sherlock.USR_ADMIN. LOWER is more privileged, so
// the test is <=, and getting that backwards would let every read-only member
// past a gate meant for admins.
const adminPrivilegeLevel = 10

// isAdmin reads Me's role the way sherlock.RoleAllowed does: privilege levels
// count DOWN, so an admin is at or below adminPrivilegeLevel and a
// super-admin's 0 clears it too. A nil Me (no credential, or a scoped token
// that cannot call /me) is not an admin — whatever the caller was about to
// offer on the strength of it is withheld, never guessed. ONE spelling of the
// comparison, here beside the constant it reads, because the direction is the
// whole bug: the automation push's mailbox pre-check and `context`'s write recipe
// both fork on it.
func isAdmin(me *api.Me) bool {
	return me != nil && me.Role != nil && me.Role.PrivilegeLevel <= adminPrivilegeLevel
}
