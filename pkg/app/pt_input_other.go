//go:build !unix

package app

import "os"

// Reports whether the file has bytes ready to read right now, never on platforms without poll
func fileReadable(*os.File) bool { return false }
