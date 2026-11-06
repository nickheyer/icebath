package pyhttpx

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strings"
	"syscall"
)

// str of the httpx exception a failed request raises, timeouts and redirects first then the socket error
func ErrorText(err error) string {
	var te *TimeoutError
	if errors.As(err, &te) {
		return te.Error()
	}
	if errors.Is(err, ErrTooManyRedirects) {
		return ErrTooManyRedirects.Error()
	}
	return OSErrorText(err)
}

// str of the OSError a socket call raises, the errno form when the cause carries one
func OSErrorText(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch runtime.GOOS {
		case "windows":
			return "[Errno 11001] getaddrinfo failed"
		case "darwin", "ios", "freebsd", "netbsd", "openbsd", "dragonfly":
			return "[Errno 8] nodename nor servname provided, or not known"
		}
		if dns.IsTemporary || dns.IsTimeout {
			return "[Errno -3] Temporary failure in name resolution"
		}
		return "[Errno -2] Name or service not known"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if runtime.GOOS == "windows" {
			return fmt.Sprintf("[WinError %d] %s", uintptr(errno), strings.TrimRight(errno.Error(), " \t\r\n."))
		}
		msg := errno.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return fmt.Sprintf("[Errno %d] %s", uintptr(errno), msg)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// UnsupportedProtocol text httpx raises before sending, empty when the scheme is http or https
func ProtocolError(rawURL string) string {
	scheme := ""
	if u, err := url.Parse(rawURL); err == nil {
		scheme = strings.ToLower(u.Scheme)
	}
	switch scheme {
	case "http", "https":
		return ""
	case "":
		return "Request URL is missing an 'http://' or 'https://' protocol."
	}
	return "Request URL has an unsupported protocol '" + scheme + "://'."
}
