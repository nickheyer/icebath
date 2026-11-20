//go:build windows

package gateway

import "os"

// Owner uid of a stat result, Windows homes keeping their own ACL report the effective uid
func statOwnerUID(os.FileInfo) int { return os.Geteuid() }
