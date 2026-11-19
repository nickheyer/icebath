package botdesktop

import "os"

// Refuses the install claim, the screen runs on Linux hosts only
func flockAvailable() error { return errUnsupported }

// Refuses the same way for a caller that skipped the availability check
func flockExclusiveNonBlocking(f *os.File) error { return errUnsupported }
