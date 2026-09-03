//go:build windows

package update

// dirOwnerUID has no answer on Windows: there is no uid, and os.Geteuid there
// returns -1, so the caller's sudo branch is unreachable anyway. The file
// exists so the package still compiles for anyone building the CLI for Windows
// from source — the release pipeline does not, and `ronja update` refuses
// there, but `go build` must not break.
func dirOwnerUID(string) (int, bool) { return 0, false }
