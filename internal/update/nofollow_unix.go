//go:build unix

package update

import "syscall"

// openNoFollow makes an open refuse a symlink at the final path element.
const openNoFollow = syscall.O_NOFOLLOW
