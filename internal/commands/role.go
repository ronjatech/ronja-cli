package commands

import "github.com/ronjatech/ronja-cli/internal/api"

// adminPrivilegeLevel mirrors sherlock.USR_ADMIN. LOWER is more privileged, so
// the test is <=; backwards, it lets every read-only member past an admin gate.
const adminPrivilegeLevel = 10

// isAdmin reads Me's role the way sherlock.RoleAllowed does (a super-admin's 0
// clears it too). A nil Me (no credential, or a scoped token that cannot call
// /me) is not an admin: whatever the caller would offer on it is withheld.
func isAdmin(me *api.Me) bool {
	return me != nil && me.Role != nil && me.Role.PrivilegeLevel <= adminPrivilegeLevel
}
