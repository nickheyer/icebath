//go:build windows

package selfupdate

import "errors"

const noFollowFlag = 0

// Windows spawns and exits instead of replacing the image
func execSelf(string, []string) error { return errors.New("exec is not available on Windows") }
