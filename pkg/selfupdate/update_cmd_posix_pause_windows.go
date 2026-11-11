package selfupdate

import "syscall"

// Windows has no session to start, the replayed gateway inherits the default process attributes
func posixPauseDetachedAttr() *syscall.SysProcAttr { return nil }
