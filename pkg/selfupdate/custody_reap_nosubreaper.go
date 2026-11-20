//go:build !linux && !windows && !icebath_core && !icebath_noextras

package selfupdate

// Platforms without a child subreaper record the descendant tree from ps instead
var custodySubreaper = func() bool { return false }
