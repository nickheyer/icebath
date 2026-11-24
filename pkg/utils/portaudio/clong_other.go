//go:build !windows

package portaudioutil

// C long and unsigned long, the width of a pointer outside Windows
type (
	clong  = int
	culong = uint
)
