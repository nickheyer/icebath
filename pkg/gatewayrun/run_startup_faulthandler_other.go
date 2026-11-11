//go:build !unix

package gatewayrun

import "os"

// No stack dump signal exists off POSIX
var stackDumpSignal os.Signal
