//go:build !windows

package portaudioutil

import "strings"

// Device name bytes that are not UTF-8, the invalid bytes replaced
func deviceName(raw string, _ int32) string { return strings.ToValidUTF8(raw, "�") }
