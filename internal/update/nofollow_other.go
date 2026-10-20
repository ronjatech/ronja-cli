//go:build !unix

package update

// openNoFollow is zero where the platform has no O_NOFOLLOW (Windows, where
// `ronja update` refuses before opening anything); the file exists so the
// package still builds there.
const openNoFollow = 0
