package toolchain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

var (
	// Name the catalog holds no entry for
	ErrUnknown = errors.New("unknown toolchain entry")
	// Entry with no usable copy on this machine
	ErrNotInstalled = errors.New("not installed")
	// Entry upstream publishes no build of for this target
	ErrUnavailable = errors.New("no build for this target")
	// Install refused because installs on first use are switched off
	ErrLazyInstallsDisabled = errors.New("lazy installs are disabled")
	// Download that failed because this machine has no route to the host
	ErrOffline = errors.New("network unreachable")
	// Download the server answered with not found or gone
	ErrNotFound = errors.New("not found on the server")
)

// Remedy an install failure names when its cause carries none
const defaultRemedy = "retry, or run `icebath pm doctor`"

// Failed install of one entry with its cause and what the user can do about it
type InstallError struct {
	Name   string
	Cause  string
	Remedy string
	Err    error
}

// Entry, cause and remedy in the one line every surface prints
func (e *InstallError) Error() string {
	remedy := e.Remedy
	if remedy == "" {
		remedy = defaultRemedy
	}
	return e.Name + ": " + e.Cause + " — " + remedy
}

// Underlying failure
func (e *InstallError) Unwrap() error { return e.Err }

// Install failure of name wrapping err as its cause
func installFailure(name string, err error) error {
	var ie *InstallError
	var ue *UnavailableError
	if errors.As(err, &ie) || errors.As(err, &ue) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &InstallError{Name: name, Cause: "install failed: " + err.Error(), Err: err}
}

// Entry that has no build for a target, with the reason upstream ships none
type UnavailableError struct {
	Name   string
	Target Target
	Reason string
}

// Entry, target and reason, with no remedy to offer
func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s: unavailable on %s: %s — none", e.Name, e.Target, e.Reason)
}

// Matches ErrUnavailable
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Complete download whose bytes do not hash to the pinned digest
type HashError struct {
	URL  string
	Want string
	Got  string
}

// Address with the pinned and the computed digest
func (e *HashError) Error() string {
	return fmt.Sprintf("sha256 mismatch for %s: pinned %s, got %s", e.URL, e.Want, e.Got)
}

// Download larger than its pin or its limit allows
type SizeError struct {
	URL   string
	Size  int64
	Limit int64
	Exact bool
}

// Address with the size seen and the size allowed
func (e *SizeError) Error() string {
	if e.Exact {
		return fmt.Sprintf("download from %s is %d bytes, the pin records %d", e.URL, e.Size, e.Limit)
	}
	return fmt.Sprintf("download from %s is %d bytes, over the limit of %d", e.URL, e.Size, e.Limit)
}

// Release signature or checksum list that does not vouch for a pinned archive
type SignatureError struct {
	Name   string
	Reason string
}

// Entry and why its provenance was rejected
func (e *SignatureError) Error() string { return e.Name + ": " + e.Reason }

// Explicit override that names no program on this machine
type OverrideError struct {
	Name  string
	Value string
}

// Entry and the override value that could not be found
func (e *OverrideError) Error() string {
	return fmt.Sprintf("%s: override %q is not a program on this machine", e.Name, e.Value)
}

// Response status that ends a request, with the headers a retry reads
type statusError struct {
	url    string
	code   int
	status string
	header http.Header
}

// Status line as the server sent it
func (e *statusError) Error() string { return "HTTP " + e.status }

// Range or representation that changed, leaving partial bytes unusable
type rangeError struct{ msg string }

// Why the partial bytes cannot be reused
func (e *rangeError) Error() string { return e.msg }

// Network request that ran out of attempts, with the address and status it failed on
type TransportError struct {
	URL    string
	Status int
	Err    error
	edge   string
}

// Address, cause and any firewall explanation
func (e *TransportError) Error() string {
	reason := e.Err.Error()
	switch {
	case e.edge != "":
		reason += "; " + e.edge
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		reason += "; the host refused access"
	}
	return "download failed from " + e.URL + ": " + reason
}

// Underlying failure
func (e *TransportError) Unwrap() error { return e.Err }

// Matches ErrNotFound for a missing file and ErrOffline for an unreachable network
func (e *TransportError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound || e.Status == http.StatusGone
	case ErrOffline:
		return e.Status == 0 && offline(e.Err)
	}
	return false
}

// Reports whether a pinned mirror may serve the bytes this request failed to get
func (e *TransportError) fallbackAllowed() bool {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	var dns *net.DNSError
	return e.Status >= 500 || errors.As(e.Err, &dns) || isTransient(e.Err)
}

// Transport failure of url from the error that exhausted its attempts
func transportFailure(url string, err error, mirrorHost string) *TransportError {
	te := &TransportError{URL: url, Err: err}
	var st *statusError
	if errors.As(err, &st) {
		te.Status = st.code
		te.edge = edgeBlock(st, mirrorHost)
	}
	return te
}

// Explanation of a 403 or 429 a CDN firewall sent in place of the server, empty otherwise
func edgeBlock(st *statusError, mirrorHost string) string {
	if st.code != http.StatusForbidden && st.code != http.StatusTooManyRequests {
		return ""
	}
	host := hostOf(st.url)
	cdn := ""
	switch {
	case st.header.Get("x-vercel-mitigated") != "":
		cdn = "Vercel"
	case st.header.Get("cf-mitigated") != "":
		cdn = "Cloudflare"
	case host != "" && host == mirrorHost && strings.EqualFold(st.header.Get("server"), "cloudflare") && st.header.Get("cf-cache-status") == "":
		cdn = "Cloudflare"
	default:
		return ""
	}
	if host == "" {
		host = "the server"
	}
	return host + "'s CDN firewall (" + cdn + ") refused this network. Your install is fine; retry from another network"
}

// Reports whether err means the network itself is unreachable or the name does not resolve
func offline(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	for _, errno := range unreachableErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
