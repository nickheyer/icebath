package remote

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Credential bound for a destination the transport policy forbids, never retryable
type CredentialDestinationError struct{ *PlaneCredentialError }

func (e *CredentialDestinationError) Unwrap() error { return e.PlaneCredentialError }

func newCredentialDestinationError(message string) *CredentialDestinationError {
	return &CredentialDestinationError{&PlaneCredentialError{Message: message}}
}

// Python urlsplit fields with the raw netloc kept for hostname, port and userinfo reads
type splitURL struct {
	scheme, netloc, path, query, fragment string
}

// Splits like urllib.parse.urlsplit, the Python ValueError text for a bad IPv6 netloc
func urlsplit(raw string) (splitURL, error) {
	url := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	var p splitURL
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIILetter(url[0]) {
		ok := true
		for _, c := range []byte(url[:i]) {
			if !isASCIILetter(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
				ok = false
				break
			}
		}
		if ok {
			p.scheme, url = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		end := len(url)
		for _, c := range "/?#" {
			if j := strings.IndexRune(url[2:], c); j >= 0 && j+2 < end {
				end = j + 2
			}
		}
		p.netloc, url = url[2:end], url[end:]
		if strings.Contains(p.netloc, "[") != strings.Contains(p.netloc, "]") {
			return p, fmt.Errorf("Invalid IPv6 URL")
		}
		if strings.Contains(p.netloc, "[") {
			bracketed := strings.SplitN(strings.SplitN(p.netloc, "[", 2)[1], "]", 2)[0]
			if !strings.HasPrefix(bracketed, "v") && net.ParseIP(strings.SplitN(bracketed, "%", 2)[0]) == nil {
				return p, fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", pyrepr.StrRepr(bracketed))
			}
		}
	}
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url, p.fragment = url[:i], url[i+1:]
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url, p.query = url[:i], url[i+1:]
	}
	p.path = url
	return p, nil
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Username and password presence of the netloc userinfo
func (p splitURL) userinfo() (hasUser, hasPassword bool) {
	i := strings.LastIndexByte(p.netloc, '@')
	if i < 0 {
		return false, false
	}
	return true, strings.Contains(p.netloc[:i], ":")
}

// Raw host and port text after any userinfo, brackets dropped
func (p splitURL) hostinfo() (host, port string) {
	info := p.netloc[strings.LastIndexByte(p.netloc, '@')+1:]
	if _, bracketed, ok := strings.Cut(info, "["); ok {
		host, rest, _ := strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
		return host, port
	}
	host, port, _ = strings.Cut(info, ":")
	return host, port
}

// Lowercased host with any zone id kept as written
func (p splitURL) hostname() string {
	host, _ := p.hostinfo()
	name, zone, found := strings.Cut(host, "%")
	if found {
		return pystr.Lower(name) + "%" + zone
	}
	return pystr.Lower(name)
}

// Port as an int, -1 when absent, the Python ValueError text when malformed
func (p splitURL) port() (int, error) {
	_, raw := p.hostinfo()
	if raw == "" {
		return -1, nil
	}
	for _, c := range raw {
		if !(c >= '0' && c <= '9') {
			return 0, fmt.Errorf("Port could not be cast to integer value as %s", pyrepr.StrRepr(raw))
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n > 65535 {
		return 0, fmt.Errorf("Port out of range 0-65535")
	}
	return n, nil
}

// Joins the parts back like urllib.parse.urlunsplit
func (p splitURL) unsplit() string {
	url := p.path
	if p.netloc != "" {
		if url != "" && url[0] != '/' {
			url = "/" + url
		}
		url = "//" + p.netloc + url
	} else if strings.HasPrefix(url, "//") {
		url = "//" + url
	}
	if p.scheme != "" {
		url = p.scheme + ":" + url
	}
	if p.query != "" {
		url += "?" + p.query
	}
	if p.fragment != "" {
		url += "#" + p.fragment
	}
	return url
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && !strings.Contains(host, "%") && ip.IsLoopback()
}

// URL for messages with any user info dropped, an unparsable one cut at the last at sign
func DisplayURL(url string) string {
	p, err := urlsplit(url)
	if err == nil {
		var port int
		if port, err = p.port(); err == nil {
			hasUser, _ := p.userinfo()
			if !hasUser {
				return url
			}
			host := p.hostname()
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			if port >= 0 {
				host += ":" + strconv.Itoa(port)
			}
			p.netloc = host
			return p.unsplit()
		}
	}
	head, tail, found := strings.Cut(url, "//")
	if found {
		return head + "//" + tail[strings.LastIndexByte(tail, '@')+1:]
	}
	return url[strings.LastIndexByte(url, '@')+1:]
}

// URL itself when a credential may be sent to it, else a CredentialDestinationError
func CheckedURL(url, envName string) (string, error) {
	p, err := urlsplit(url)
	if err == nil {
		_, err = p.port()
	}
	if err != nil {
		return "", newCredentialDestinationError(fmt.Sprintf("%s=%s is not a valid URL: %v", envName, pyrepr.StrRepr(DisplayURL(url)), err))
	}
	host := p.hostname()
	if host == "" {
		return "", newCredentialDestinationError(fmt.Sprintf("%s=%s has no host.", envName, pyrepr.StrRepr(DisplayURL(url))))
	}
	if hasUser, _ := p.userinfo(); hasUser || p.query != "" || p.fragment != "" {
		return "", newCredentialDestinationError(envName + " must be a plain origin and path (no user info, query or fragment); " +
			"Remote Config does not send credentials to an ambiguous destination.")
	}
	if p.scheme == "https" || (p.scheme == "http" && isLoopback(host)) {
		return url, nil
	}
	scheme := p.scheme
	if scheme == "" {
		scheme = "no"
	}
	return "", newCredentialDestinationError(fmt.Sprintf("%s must be an https:// URL (plain http is accepted only for localhost, "+
		"127.0.0.1 or ::1); Remote Config does not send credentials to %s://%s.", envName, scheme, host))
}

var credentialedClient = &http.Client{
	Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Sends a credentialed request, refusing every redirect answer instead of following it
func openCredentialed(req *http.Request, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := contextWithTimeout(req, timeout)
	resp, err := credentialedClient.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		resp.Body.Close()
		cancel()
		return nil, newCredentialDestinationError(fmt.Sprintf("%s answered HTTP %d redirecting to %s; "+
			"Remote Config never follows a redirect with credentials. Point the URL at the final destination.",
			req.URL.String(), resp.StatusCode, pyrepr.StrRepr(DisplayURL(resp.Header.Get("Location")))))
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
