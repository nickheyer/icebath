//go:build unix

package home

import "github.com/nickheyer/icebath/internal/hostinfo"

// Home directory the system account database reports through every name service, of uid when name is empty
func accountHome(name string, uid int) (string, bool) {
	var dir string
	var err error
	if name != "" {
		dir, err = hostinfo.AccountHome(name)
	} else {
		dir, err = hostinfo.AccountHomeByID(uid)
	}
	return dir, err == nil && dir != ""
}
