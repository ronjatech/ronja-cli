package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// legacyStoreName is the credential file this CLI used before profiles.
//
// Deliberately NOT migrated. That was a decision, not an oversight: the CLI is
// not in production, so its installed base is Ronja engineers, and a one-time
// converter is code that exists forever to serve a population that re-runs
// `ronja login` in thirty seconds.
//
// What DOES deserve saying out loud is the part a migration would have quietly
// handled: the abandoned file still holds a live, full-access token with up to
// 90 days left on it, and this CLI can no longer see it. `ronja logout` will
// not clear it. Left unmentioned it sits on disk indefinitely — a credential
// nothing manages and nobody remembers.
//
// So: no migration, but never silence.
const legacyStoreName = "hosts.json"

// warnLegacyStore reports an abandoned pre-profiles credential file, if one is
// there.
//
// Called from the two places someone actually looks at their credentials —
// `profile list`, and the tail of a successful login — rather than from every
// command, which would nag. Best-effort: a store we cannot stat is not worth an
// error, since nothing here is load-bearing.
func warnLegacyStore() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, legacyStoreName)
	if _, err := os.Stat(path); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"\n  Note: %s is left over from an older CLI and is no longer read.\n"+
			"  The access token in it is still LIVE — `ronja logout` cannot clear it.\n"+
			"  Revoke it under Account -> Access tokens, then delete the file.\n",
		path)
}
