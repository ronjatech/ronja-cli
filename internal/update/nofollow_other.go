//go:build !unix

package update

// openNoFollow is zero where the platform has no O_NOFOLLOW — Windows, where
// `ronja update` refuses before it ever opens anything, and where the file
// exists only so `go build` from a checkout still works. A zero flag adds
// nothing to the open, which is the honest answer: the protection is not
// available here, rather than silently believed to be.
const openNoFollow = 0
