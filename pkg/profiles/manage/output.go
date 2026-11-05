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
