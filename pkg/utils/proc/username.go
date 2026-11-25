package proc

import (
	"os/user"
	"strconv"
)

// Account name of a real uid like psutil, the decimal uid when the passwd database has no entry
func uidName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}
