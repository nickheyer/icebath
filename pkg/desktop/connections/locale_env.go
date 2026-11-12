//go:build !windows && !darwin

package connections

import "os"

// Preferred UI languages from the process environment
func preferredUILanguages() []string { return envPreferredLanguages(os.Getenv) }
