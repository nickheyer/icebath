package connections

import "strings"

var linuxPasswordStores = map[string]bool{"gnome-libsecret": true, "kwallet": true, "kwallet5": true, "kwallet6": true, "basic": true}

// LinuxPasswordStore is the --password-store backend to launch with and the warning for a value it ignored
type LinuxPasswordStore struct {
	Store   string
	Warning string
}

// ResolveLinuxPasswordStore validates ICEBATH_DESKTOP_PASSWORD_STORE on Linux, empty everywhere else
func ResolveLinuxPasswordStore(getenv func(string) string, platform string) LinuxPasswordStore {
	requested := strings.TrimSpace(getenv("ICEBATH_DESKTOP_PASSWORD_STORE"))
	if platform != "linux" || requested == "" {
		return LinuxPasswordStore{}
	}
	if !linuxPasswordStores[requested] {
		return LinuxPasswordStore{Warning: "ignoring unknown ICEBATH_DESKTOP_PASSWORD_STORE value: " + requested}
	}
	return LinuxPasswordStore{Store: requested}
}
