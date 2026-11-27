//go:build !windows

package portaudio

import "strings"

// C long and unsigned long, pointer wide outside Windows
type (
	clong  = int
	culong = uint
)

// Name bytes that are not UTF-8, the invalid bytes replaced
func deviceName(raw string, _ int32) string { return strings.ToValidUTF8(raw, "�") }
