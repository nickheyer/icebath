package hostinfo

import (
	"os"
	"strconv"
	"strings"
)

// Reports a login session with a runtime directory, the audit session id unset for daemons
func InteractiveSession() bool {
	session := strings.TrimSpace(readText("/proc/self/sessionid", 64))
	if session == "" || session == "4294967295" {
		return false
	}
	st, err := os.Stat("/run/user/" + strconv.Itoa(os.Getuid()))
	return err == nil && st.IsDir()
}
