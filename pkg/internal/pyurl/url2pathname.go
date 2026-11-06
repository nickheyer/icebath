package pyurl

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// URLError is urllib.error.URLError, its reason wrapped in the urlopen error brackets
type URLError struct{ Reason string }

func (e *URLError) Error() string { return "<urlopen error " + e.Reason + ">" }

// Python exception class name the error stands for
func (e *URLError) PyTypeName() string { return "URLError" }

// Local path of a file URL like urllib.request.url2pathname, a WTF-8 str decoded with the fs error handler
func URL2Pathname(url string, requireScheme, resolveHost bool) (string, error) {
	return url2pathname(url, requireScheme, resolveHost, runtime.GOOS == "windows")
}

func url2pathname(url string, requireScheme, resolveHost, windows bool) (string, error) {
	if !requireScheme {
		url = "file:" + url
	}
	parts, err := Split(url)
	if err != nil {
		return "", err
	}
	if parts.Scheme != "file" {
		return "", &URLError{Reason: "URL is missing a 'file:' scheme"}
	}
	authority, path := parts.Netloc, parts.Path
	if windows {
		if charAt(authority, 1) == ":" {
			path = authority + path
		} else if local, err := isLocalAuthority(authority, resolveHost); err != nil {
			return "", err
		} else if !local {
			path = "//" + authority + path
		} else if strings.HasPrefix(path, "///") {
			path = path[1:]
		} else {
			if third := charAt(path, 2); strings.HasPrefix(path, "/") && (third == ":" || third == "|") {
				path = path[1:]
			}
			if charAt(path, 1) == "|" {
				first := charAt(path, 0)
				path = first + ":" + path[len(first)+1:]
			}
		}
		return unquoteDecode(strings.ReplaceAll(path, "/", `\`), surrogatePassDecode)
	}
	if local, err := isLocalAuthority(authority, resolveHost); err != nil {
		return "", err
	} else if !local {
		return "", &URLError{Reason: "file:// scheme is supported only on localhost"}
	}
	return unquoteDecode(path, func(b []byte) (string, error) { return pystr.SurrogateEscapeDecode(b), nil })
}

// Code point n of a WTF-8 str as a substring, empty past the end like a one character slice
func charAt(s string, n int) string {
	for i := 0; i < len(s); n-- {
		_, size := pystr.DecodeRune(s[i:])
		if n == 0 {
			return s[i : i+size]
		}
		i += size
	}
	return ""
}

// Bytes decoded as utf-8 with surrogatepass, encoded surrogates kept and any other bad byte raising
func surrogatePassDecode(b []byte) (string, error) {
	if pos, ok := pystr.ValidIndex(b); !ok {
		return "", pystr.UTF8DecodeError(b, pos)
	}
	return string(b), nil
}

// Percent decodes like urllib.parse.unquote, each ASCII run decoded by the given codec and other text kept
func unquoteDecode(s string, decode func([]byte) (string, error)) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		j := i
		if s[i] >= utf8.RuneSelf {
			for j < len(s) && s[j] >= utf8.RuneSelf {
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		var run []byte
		for ; j < len(s) && s[j] < utf8.RuneSelf; j++ {
			if s[j] == '%' && j+2 < len(s) && isHex(s[j+1]) && isHex(s[j+2]) {
				run = append(run, unhex(s[j+1])<<4|unhex(s[j+2]))
				j += 2
				continue
			}
			run = append(run, s[j])
		}
		text, err := decode(run)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
		i = j
	}
	return out.String(), nil
}

// Reports an empty, localhost or own hostname authority, resolving it against the host addresses on request
func isLocalAuthority(authority string, resolve bool) (bool, error) {
	if authority == "" || authority == "localhost" {
		return true, nil
	}
	if hostname, err := os.Hostname(); err == nil && authority == hostname {
		return true, nil
	}
	if !resolve {
		return false, nil
	}
	address, err := gethostbyname(authority)
	if err != nil {
		return false, nil
	}
	names, err := fileHandlerNames()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if name == address {
			return true, nil
		}
	}
	return false, nil
}

// IPv4 addresses a name resolves to, the socket.gethostbyname_ex address list
func gethostbynameEx(name string) ([]string, error) {
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out, nil
}

// First IPv4 address a name resolves to, the socket.gethostbyname result
func gethostbyname(name string) (string, error) {
	addresses, err := gethostbynameEx(name)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return addresses[0], nil
}

var fileHandler struct {
	sync.Mutex
	names []string
}

// Addresses of localhost and this host, cached for the process like FileHandler.names
func fileHandlerNames() ([]string, error) {
	fileHandler.Lock()
	defer fileHandler.Unlock()
	if fileHandler.names != nil {
		return fileHandler.names, nil
	}
	local, err := gethostbynameEx("localhost")
	if err == nil {
		hostname, herr := os.Hostname()
		if herr != nil {
			return nil, herr
		}
		var own []string
		if own, err = gethostbynameEx(hostname); err == nil {
			fileHandler.names = append(local, own...)
			return fileHandler.names, nil
		}
	}
	address, err := gethostbyname("localhost")
	if err != nil {
		return nil, err
	}
	fileHandler.names = []string{address}
	return fileHandler.names, nil
}
