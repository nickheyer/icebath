package app

import "errors"

// signal.SIGTSTP does not exist on Windows, where reading it raises AttributeError
func suspendProcessGroup() error { return errors.New("module 'signal' has no attribute 'SIGTSTP'") }
