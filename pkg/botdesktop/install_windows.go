package botdesktop

import (
	"errors"
	"os"
)

// Error of importing fcntl, the module Windows lacks
func flockAvailable() error { return errors.New("No module named 'fcntl'") }

// Never reached on Windows, the fcntl import error ends the claim first
func flockExclusiveNonBlocking(f *os.File) error { return flockAvailable() }
