package remote

import (
	"context"
	"errors"
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
			err = errors.New("the port must be from 0 to 65535")
		}
	}
	if err != nil {
		return "", destinationError(fmt.Sprintf("%s is set to %s, which is not a valid address: %v.", variable, strconv.Quote(DisplayURL(raw)), err))
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", destinationError(fmt.Sprintf("%s is set to %s, which names no server.", variable, strconv.Quote(DisplayURL(raw))))
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", destinationError(variable + " must be an address with no user name, no ? and no # part. icebath does not send its sign-in to an " +
			"address that can be read two ways.")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" || scheme == "http" && loopback(host) {
		return raw, nil
	}
	if scheme == "" {
		scheme = "no"
	}
	return "", destinationError(fmt.Sprintf("%s must start with https://. Plain http is taken only for localhost, 127.0.0.1 and ::1, so icebath does "+
		"not send its sign-in to %s://%s.", variable, scheme, host))
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
		return nil, destinationError(fmt.Sprintf("%s answered HTTP %d and sent icebath on to %s. icebath does not follow a redirect with its sign-in, so "+
			"set the address to where it ends up.", req.URL.String(), resp.StatusCode, strconv.Quote(DisplayURL(resp.Header.Get("Location")))))
	}
	resp.Body = &deadlineBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
