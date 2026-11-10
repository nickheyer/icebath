package manage

import (
	"io"
	"os"
)

// Writers behind the print calls of the profile lifecycle, swapped by tests capturing output
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// Routes the lifecycle prints into out and errw until the returned restore runs
func SwapOutput(out, errw io.Writer) (restore func()) {
	oldOut, oldErr := stdout, stderr
	stdout, stderr = out, errw
	return func() { stdout, stderr = oldOut, oldErr }
}
