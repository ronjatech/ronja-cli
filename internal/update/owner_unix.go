//go:build !windows

package update

import (
	"os"
	"syscall"
)

// dirOwnerUID reports the uid owning path. The second result is false when the
// answer is unavailable — the path does not exist, or the platform does not
// carry a uid — and a caller must then assume nothing.
func dirOwnerUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
