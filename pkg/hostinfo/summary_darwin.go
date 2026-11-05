package hostinfo

import (
	"os/user"
	"strings"

	"golang.org/x/sys/unix"
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

func osLocale() string { return darwinPreferredLanguage() }

// The macOS product version, else the Darwin kernel release
func osRelease() string {
	if v, err := unix.Sysctl("kern.osproductversion"); err == nil && v != "" {
		return v
	}
	v, _ := unix.Sysctl("kern.osrelease")
	return v
}
