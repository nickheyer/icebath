package urlutil

import (
	"errors"
	"fmt"
	"net/url"
)

// Error for a port number above 65535
var ErrPortRange = errors.New("port out of range 0-65535")

// Number of a decimal port text, an error when it is empty or not all ASCII digits and one wrapping ErrPortRange above 65535
func parsePort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty port")
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid port %q", s)
		}
		if n <= 65535 {
			n = n*10 + int(c-'0')
		}
	}
	if n > 65535 {
		return 0, fmt.Errorf("%w: %s", ErrPortRange, s)
	}
	return n, nil
}

// Port number of a parsed URL, false when it names none, an error wrapping ErrPortRange for a number past 65535
func Port(u *url.URL) (int, bool, error) {
	text := u.Port()
	if text == "" {
		return 0, false, nil
	}
	n, err := parsePort(text)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}
