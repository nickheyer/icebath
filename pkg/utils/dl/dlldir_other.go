//go:build !windows

package dlutil

// Nothing to add outside Windows, the returned function does nothing
func AddDLLDirectory(dir string) (func(), error) { return func() {}, nil }
