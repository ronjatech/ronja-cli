//go:build windows

package update

// dirOwnerUID has no answer on Windows (no uid, and os.Geteuid returns -1); the
// file exists so the package still builds there from source.
func dirOwnerUID(string) (int, bool) { return 0, false }
