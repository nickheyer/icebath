//go:build !windows

package shell

// Words of a command line a user typed, POSIX shell rules on this platform
func SplitCommandLine(line string) ([]string, error) { return Split(line) }
