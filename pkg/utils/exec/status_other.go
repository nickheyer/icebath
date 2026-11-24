//go:build !unix

package executil

import (
	"os"
	"strconv"
)

// Exit code of a finished process, -1 for a nil state
func ExitCode(state *os.ProcessState) int { return state.ExitCode() }

func statusText(code int) string { return "exit status " + strconv.Itoa(code) }
