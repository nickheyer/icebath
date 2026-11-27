package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Credential that would go somewhere the transport policy forbids, never worth a retry
type DestinationError struct{ *CredentialError }

func (e *DestinationError) Unwrap() error { return e.CredentialError }

func destinationError(message string) *DestinationError {
	return &DestinationError{&CredentialError{Message: message}}
}

// Address for messages with any user info taken out
func DisplayURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		if head, tail, found := strings.Cut(raw, "//"); found {
			return head + "//" + tail[strings.LastIndexByte(tail, '@')+1:]
		}
		return raw[strings.LastIndexByte(raw, '@')+1:]
	}
	if parsed.User == nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

// The address when a credential may be sent to it: https, or plain http to this machine, with no user info, query or fragment
func checkedURL(raw, variable string) (string, error) {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Port() != "" {
		if port, perr := strconv.Atoi(parsed.Port()); perr != nil || port > 65535 {
			err = fmt.Errorf("Port out of range 0-65535")
		}
	}
	if err != nil {
		return "", destinationError(fmt.Sprintf("%s=%s is not a valid URL: %v", variable, strconv.Quote(DisplayURL(raw)), err))
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", destinationError(fmt.Sprintf("%s=%s has no host.", variable, strconv.Quote(DisplayURL(raw))))
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", destinationError(variable + " must be a plain origin and path (no user info, query or fragment); " +
			"Remote Config does not send credentials to an ambiguous destination.")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" || scheme == "http" && loopback(host) {
		return raw, nil
	}
	if scheme == "" {
		scheme = "no"
	}
	return "", destinationError(fmt.Sprintf("%s must be an https:// URL (plain http is accepted only for localhost, "+
		"127.0.0.1 or ::1); Remote Config does not send credentials to %s://%s.", variable, scheme, host))
}

// Whether plain http may carry a credential to the host: localhost or a loopback address, one with a zone left out, as a name under
// .localhost is only as safe as the resolver that maps it
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && !strings.Contains(host, "%") && ip.IsLoopback()
}

// Client that hands back a redirect instead of following it, so a credential never goes on to where it points
var credentialed = &http.Client{
	Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Body that ends the request's deadline once it is closed
type deadlineBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Sends a request carrying a credential within the time, refusing every redirect it is answered with
func send(req *http.Request, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	resp, err := credentialed.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		resp.Body.Close()
		cancel()
		return nil, destinationError(fmt.Sprintf("%s answered HTTP %d redirecting to %s; Remote Config never follows a redirect with credentials. "+
			"Point the URL at the final destination.", req.URL.String(), resp.StatusCode, strconv.Quote(DisplayURL(resp.Header.Get("Location")))))
	}
	resp.Body = &deadlineBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
