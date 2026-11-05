//go:build windows

package config

import "os"

type fileOwnerID struct{}

// Windows files carry no POSIX owner
func fileOwnerOf(os.FileInfo) fileOwnerID { return fileOwnerID{} }

// Windows files carry no POSIX owner to restore
func restoreFileOwner(string, fileOwnerID) {}

// Windows synthesizes mode bits so a new file keeps the mode the temp file got
func defaultNewFileMode() (os.FileMode, bool) { return 0, false }
