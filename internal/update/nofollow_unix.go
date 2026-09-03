//go:build unix

package update

import "syscall"

// openNoFollow makes an open refuse a symlink at the final path element.
//
// The constraint is `unix` rather than `!windows` (which is what the
// dirOwnerUID pair next door uses) because O_NOFOLLOW is a POSIX flag: it is
// what every platform the CLI is built for has, and naming the platforms that
// have it is more honest than naming the one that does not.
const openNoFollow = syscall.O_NOFOLLOW
