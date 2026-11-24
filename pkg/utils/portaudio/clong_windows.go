//go:build windows

package portaudioutil

// C long and unsigned long, 32 bits on Windows
type (
	clong  = int32
	culong = uint32
)
