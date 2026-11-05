//go:build !linux && !darwin && !windows

package hostinfo

import (
	"os/user"
	"runtime"
	"strings"
)

// Login and full name from the user database
func account() (string, string) {
	u, err := user.Current()
	if err != nil {
		return "", ""
	}
	full, _, _ := strings.Cut(u.Name, ",")
	return u.Username, strings.TrimSpace(full)
}

func osLocale() string { return linuxLocale() }

func osRelease() string { return runtime.GOOS }
