package cron

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
)

const (
	// BlockedConfigMarker prefixes a preflight refusal that is delivered as a failure notice
	BlockedConfigMarker = "[blocked_config]"
	// BlockedConfigSilentMarker prefixes a preflight refusal that is recorded without a notice
	BlockedConfigSilentMarker = "[blocked_config:silent]"
)

var (
	dnsFailureNeedles       = []string{"nodename nor servname", "name or service not known"}
	transientOSErrorNeedles = append(append([]string{}, dnsFailureNeedles...), "temporary failure in name resolution", "network is unreachable")
	transientHTTPNeedles    = append(append([]string{}, transientOSErrorNeedles...), "failed to resolve", "connection refused", "timed out", "timeout")
)

// Message contains any needle
func containsAny(msg string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// Errno is one of the transient connection errnos of the platform
func isTransientErrno(errno syscall.Errno) bool {
	for _, e := range transientErrnos {
		if errno == e {
			return true
		}
	}
	return false
}

// Error is an operating system level failure the way Python OSError subclasses are
func isOSLevelError(err error) bool {
	switch err.(type) {
	case *net.OpError, *net.DNSError, *net.AddrError, *os.SyscallError, *os.PathError, *os.LinkError, syscall.Errno:
		return true
	}
	return false
}

// Primary provider resolution failed for a transient network reason somewhere in the cause chain
func isTransientProviderResolveError(err error) bool {
	for cur, depth := err, 0; cur != nil && depth < maxErrorChain; cur, depth = errors.Unwrap(cur), depth+1 {
		msg := strings.ToLower(cur.Error())
		if netErr, ok := cur.(net.Error); ok && netErr.Timeout() {
			return true
		}
		if op, ok := cur.(*net.OpError); ok && op.Op == "dial" {
			return true
		}
		if _, ok := cur.(*url.Error); ok && containsAny(msg, transientHTTPNeedles) {
			return true
		}
		if isOSLevelError(cur) {
			if dnsErr, ok := cur.(*net.DNSError); ok {
				if dnsErr.IsNotFound || dnsErr.IsTemporary || dnsErr.Err == "server misbehaving" {
					return true
				}
			} else if errno, ok := cur.(syscall.Errno); ok && isTransientErrno(errno) {
				return true
			}
			if containsAny(msg, transientOSErrorNeedles) {
				return true
			}
		}
		if containsAny(msg, dnsFailureNeedles) {
			return true
		}
	}
	return false
}
