package pm

import "os"

// Windows has no whole filesystem flush, so the selection record's atomic write is the only ordering
var filesystemSync func() error

// Processor architecture Windows reports for the machine, the value Python platform machine returns
func platformMachine() string {
	if wow := os.Getenv("PROCESSOR_ARCHITEW6432"); wow != "" {
		return wow
	}
	return os.Getenv("PROCESSOR_ARCHITECTURE")
}
