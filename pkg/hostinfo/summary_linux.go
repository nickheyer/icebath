package hostinfo

import (
	"os/user"
	"strings"
	"syscall"
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

// Kernel release from uname
func osRelease() string {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return ""
	}
	var b []byte
	for _, c := range uts.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
