package updaterecord

import "io"

// Points the obligation warnings at w and returns the call that restores the previous writer
func SetObligationStderr(w io.Writer) func() {
	saved := obligationStderr
	obligationStderr = w
	return func() { obligationStderr = saved }
}
