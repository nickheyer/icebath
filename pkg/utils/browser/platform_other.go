//go:build !unix && !windows

package browserutil

import "errors"

// No platform opener on this system, only BROWSER entries
func platformLaunchers() []Launcher { return nil }

// No operating system URL handler on this system
func systemOpen(string) error { return errors.ErrUnsupported }
