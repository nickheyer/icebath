//go:build windows

package selfupdate

import "os"

// Windows has no blocking flock so the caller polls the nonblocking lock
func blockingFileLock(*os.File) (bool, error) { return false, nil }
