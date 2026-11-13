//go:build windows

package main

import "golang.org/x/sys/windows"

// Downloads known folder of the user, empty when the shell cannot resolve it
func downloadsDir() string {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		return ""
	}
	return dir
}
