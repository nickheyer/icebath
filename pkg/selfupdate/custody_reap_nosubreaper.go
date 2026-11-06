//go:build !linux && !windows

package selfupdate

// Platforms without a child subreaper record the descendant tree from ps instead
var custodySubreaper = func() bool { return false }
