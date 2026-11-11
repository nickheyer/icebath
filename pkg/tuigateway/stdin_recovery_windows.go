//go:build windows

package tuigateway

// fcntl is absent here so every empty read is a genuine close
func stdinRecoverable() bool { return false }

// No O_NONBLOCK flag exists on this platform
func stdinNonblock() bool { return false }

// Flag report without fcntl
func stdinFlagParts() []string { return []string{"O_NONBLOCK=n/a (no fcntl)"} }

// fd 0 is never read as a socket here
func stdinRcvTimeo() (int, bool) { return 0, false }

// Nothing to restore without fcntl
func restoreStdinBlocking() {}
