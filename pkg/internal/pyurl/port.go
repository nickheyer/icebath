package pyurl

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// ErrPortOutOfRange is the ValueError SplitResult.port raises past 65535
var ErrPortOutOfRange = errors.New("Port out of range 0-65535")

// Port of the netloc like SplitResult.port, absent when empty, the ValueError when not an ASCII number in range
func (u URL) Port() (int, bool, error) {
	hostinfo := u.Netloc[strings.LastIndex(u.Netloc, "@")+1:]
	var port string
	if _, bracketed, open := strings.Cut(hostinfo, "["); open {
		_, rest, _ := strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
	} else {
		_, port, _ = strings.Cut(hostinfo, ":")
	}
	if port == "" {
		return 0, false, nil
	}
	n := 0
	for i := 0; i < len(port); i++ {
		c := port[i]
		if c < '0' || c > '9' {
			return 0, false, errors.New("Port could not be cast to integer value as " + pyrepr.StrRepr(port))
		}
		if n <= 65535 {
			n = n*10 + int(c-'0')
		}
	}
	if n > 65535 {
		return 0, false, ErrPortOutOfRange
	}
	return n, true, nil
}
