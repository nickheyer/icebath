package selfupdate

import (
	"fmt"
	"os"
)

// The descriptor holding reap launcher exists only where the checkout lock is a descriptor, never on Windows
func reapStandIn([]string) int {
	fmt.Fprintln(os.Stderr, custodyUnavailable+" (Windows keeps update children in its job, not a descriptor custodian); the command was not run")
	return custodyRefusedExit
}

func reapCustodian(args []string) int { return reapStandIn(args) }
