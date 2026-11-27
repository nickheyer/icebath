package credentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Failure of a sign in or of the renewal of its credential, saying whether only signing in again can fix it
type AuthError struct {
	// Kind of provider it is of
	Provider string
	// Stable name of the failure, such as invalid_grant or device_code_timeout
	Code    string
	Message string
	// Only signing in again helps
	Relogin bool
	// Wait the provider asked for before trying again, zero when it asked none
	RetryAfter time.Duration
	Err        error
}

func (e *AuthError) Error() string { return e.Message }

func (e *AuthError) Unwrap() error { return e.Err }

// Failure of the provider's sign in with its code, wrapping the cause when there is one
func Failed(provider, code, message string, relogin bool, cause error) *AuthError {
	return &AuthError{Provider: provider, Code: code, Message: message, Relogin: relogin, Err: cause}
}

// Code a sign in that waited out its device code fails with
const Expired = "device_code_timeout"

// Sign in the person stopped, or whose prompt closed
var ErrCancelled = errors.New("The sign-in was cancelled.")

// Whether the sign in ended because the person stopped it
func Cancelled(err error) bool {
	return errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}

// Whether a failure is one of reaching the provider: a refused or reset connection, a name that doesn't resolve, a timeout
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*AuthError](err); ok {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, code := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE, syscall.ETIMEDOUT} {
		if errors.Is(err, code) {
			return true
		}
	}
	_, network := errors.AsType[net.Error](err)
	_, address := errors.AsType[*url.Error](err)
	return network || address
}

// Lines a failed sign in prints: why in words the person can act on, then the details when it didn't stop on its own
func FailureLines(err error, host, retry string) []string {
	host, retry = strings.TrimSpace(host), strings.TrimSpace(retry)
	if host == "" {
		host = "the sign-in service"
	}
	if retry == "" {
		retry = "icebath auth add"
	}
	switch {
	case Cancelled(err):
		return []string{fmt.Sprintf("The sign-in was cancelled. Run `%s` when you want to try again.", retry)}
	case Unreachable(err):
		return []string{fmt.Sprintf("icebath could not reach %s, so the sign-in stopped. Check your internet connection or proxy, then run `%s` again.",
			host, retry), "  Details: " + err.Error()}
	}
	failure, ok := errors.AsType[*AuthError](err)
	switch {
	case ok && failure.Code == Expired:
		return []string{strings.TrimSpace(failure.Message) + fmt.Sprintf(" Run `%s` to get a new code.", retry)}
	case ok:
		return []string{"The sign-in failed. " + strings.TrimSpace(failure.Message),
			fmt.Sprintf("Run `%s` to try again, or `icebath providers` to pick another provider.", retry)}
	}
	return []string{fmt.Sprintf("The sign-in failed. Run `%s` to try again, or `icebath providers` to pick another provider.", retry),
		"  Details: " + err.Error()}
}

// Command that signs the provider in again
func Relogin(provider string) string {
	return "icebath auth add " + provider + " --type oauth"
}
