//go:build windows

package tuigateway

// fcntl is absent here so every empty read is a genuine close
func stdinRecoverable() bool { return false }

// No O_NONBLOCK flag exists on this platform
func stdinNonblock() bool { return false }

// Flag report without fcntl
func stdinFlagParts() []string { return []string{"O_NONBLOCK=n/a (no fcntl)"} }

// fd 0 is no socket handle here so every socket option read fails
func stdinSockopt(func(fd int) (int, error)) (int, bool) { return 0, false }

// Integer read of SO_RCVTIMEO, false since fd 0 is never a socket here
func stdinRcvTimeo() (int, bool) { return stdinSockopt(nil) }

// Nothing to restore without fcntl
func restoreStdinBlocking() {}
