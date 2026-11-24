package executil

import "errors"

// Error of Shell and ShellCommand when the platform names no command shell
var ErrNoShell = errors.New("no command shell found")
