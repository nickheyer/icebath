package selfupdate

import (
	"fmt"
	"os"
	"strconv"
)

var custodySubreaper = func() bool { return false }

// Child stand-ins the Windows suite re-executes this binary as
func cuChild(mode string, args []string) int {
	if mode == "built" {
		fmt.Println("built")
		code, _ := strconv.Atoi(args[0])
		return code
	}
	fmt.Fprintln(os.Stderr, "unknown selfupdate test child mode "+mode)
	return 2
}
