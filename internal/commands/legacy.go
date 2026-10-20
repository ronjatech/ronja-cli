package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// legacyStoreName is the pre-profiles credential file, deliberately not
// migrated. It may still hold a live token `ronja logout` cannot clear, so it is
// never left unmentioned.
const legacyStoreName = "hosts.json"

// warnLegacyStore is called only where credentials are looked at (profile
// listing, after login), not from every command, which would nag.
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
