package provider

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

	"github.com/nickheyer/icebath/pkg/cliui"
)

// DeviceFlowErrorCopy maps RFC 8628 device flow error codes onto plain copy that names the retry command
var DeviceFlowErrorCopy = map[string]string{
	"expired_token":  "The sign-in code expired before it was approved in the browser. Run `{retry}` to get a new code.",
	"access_denied":  "Sign-in was declined in the browser. Run `{retry}` to try again, or `icebath model` to pick a different provider.",
	"invalid_grant":  "The sign-in code was not accepted by the server. Run `{retry}` to get a new code.",
	"invalid_client": "The server did not recognize this copy of Icebath. Run `icebath update`, then `{retry}` again.",
}

// DeviceCodeExpiredCode is the code a device code poller carries when the code ran out
const DeviceCodeExpiredCode = "device_code_timeout"

// Codes the device code pollers raise when the code runs out
var deviceCodeExpiredCodes = map[string]bool{DeviceCodeExpiredCode: true, "timeout": true}

// DeviceCodeExpiredError is a device code that ran out before the browser approved it
type DeviceCodeExpiredError struct {
	Message string
	Cause   error
}

func (e *DeviceCodeExpiredError) Error() string { return e.Message }

func (e *DeviceCodeExpiredError) Unwrap() error { return e.Cause }

// Code is the stable device code timeout code every provider shares
func (e *DeviceCodeExpiredError) Code() string { return DeviceCodeExpiredCode }

// SignInCopyError is an error whose text is already user copy with a lead line and a details line
type SignInCopyError struct {
	Message        string
	OAuthErrorCode string
}

func (e *SignInCopyError) Error() string { return e.Message }

// SignInCancelledError is an interactive login the user interrupted or ended at EOF
type SignInCancelledError struct {
	ExitCode int
	HasCode  bool
}

func (e *SignInCancelledError) Error() string { return "Login cancelled." }

// IsNetworkError is true for connection, DNS, and timeout failures from the transport
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var copyErr *SignInCopyError
	var expired *DeviceCodeExpiredError
	if errors.As(err, &copyErr) || errors.As(err, &expired) {
		return false
	}
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	var urlErr *url.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &netErr) || errors.As(err, &urlErr) || errors.As(err, &opErr) || errors.As(err, &dnsErr)
}

// IsDeviceCodeExpired is true when a device code sign in ended because its code ran out
func IsDeviceCodeExpired(err error) bool {
	if err == nil {
		return false
	}
	var expired *DeviceCodeExpiredError
	if errors.As(err, &expired) {
		return true
	}
	var authErr *AuthError
	if errors.As(err, &authErr) && deviceCodeExpiredCodes[authErr.Code] {
		return true
	}
	var copyErr *SignInCopyError
	return errors.As(err, &copyErr) && copyErr.OAuthErrorCode == "expired_token"
}

// IsCancelled is true for an interrupted prompt, a closed stdin, or a clean exit
func IsCancelled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
		return true
	}
	var cancelled *SignInCancelledError
	if errors.As(err, &cancelled) {
		return !cancelled.HasCode || cancelled.ExitCode == 130 || cancelled.ExitCode == 0
	}
	var flowCancelled *DeviceFlowCancelledError
	return errors.As(err, &flowCancelled)
}

// DeviceFlowError builds the user facing error for an OAuth device flow error code
func DeviceFlowError(code, description, retryCommand string) *SignInCopyError {
	if retryCommand == "" {
		retryCommand = "icebath portal"
	}
	lead := strings.ReplaceAll(DeviceFlowErrorCopy[code], "{retry}", retryCommand)
	if lead == "" {
		reason := description
		if reason == "" {
			reason = "the server rejected the request"
		}
		lead = fmt.Sprintf("Sign-in did not complete: %s. Run `%s` to try again.", reason, retryCommand)
	}
	details := description
	if code != "" {
		details = code + ": " + description
	}
	message := lead
	if details != "" {
		message = lead + "\n  Details: " + details
	}
	return &SignInCopyError{Message: message, OAuthErrorCode: code}
}

func signInDetailsLine(err error) string {
	text := strings.TrimSpace(err.Error())
	if text == "" {
		text = fmt.Sprintf("%T", err)
	}
	return "  Details: " + text
}

type signInCopyRule struct {
	pred func(error) bool
	copy string
}

func classifySignInError(err error, rules []signInCopyRule, other string) string {
	for _, rule := range rules {
		if rule.pred(err) {
			return rule.copy
		}
	}
	return other
}

// SignInFailureLines are the lines to print when a device code or browser sign in fails for a non timeout reason
func SignInFailureLines(err error, serviceHost, retryCommand string) []string {
	if serviceHost == "" {
		serviceHost = "portal.nousresearch.com"
	}
	if retryCommand == "" {
		retryCommand = "icebath portal"
	}
	noteSignInFailure(err)
	var copyErr *SignInCopyError
	if errors.As(err, &copyErr) {
		return strings.Split(copyErr.Message, "\n")
	}
	rules := []signInCopyRule{
		{IsCancelled, "Sign-in was cancelled. Run `{retry}` when you want to try again."},
		{IsNetworkError, "Could not sign in: Icebath could not reach {host}. Check your internet connection or proxy, then run `{retry}` again."},
	}
	lead := classifySignInError(err, rules, "Could not sign in. Run `{retry}` to try again, or `icebath model` to pick a different provider.")
	lead = strings.ReplaceAll(strings.ReplaceAll(lead, "{host}", serviceHost), "{retry}", retryCommand)
	lines := []string{lead}
	if !IsCancelled(err) {
		lines = append(lines, signInDetailsLine(err))
	}
	return lines
}

// ProviderSetupFailureLines are the lines to print when the setup wizard's provider step fails
func ProviderSetupFailureLines(err error, retryCommand string) []string {
	if retryCommand == "" {
		retryCommand = "icebath model"
	}
	nothingSaved := fmt.Sprintf("Your provider settings were not changed. Continue the wizard now and run `%s` afterwards to try again.", retryCommand)
	var copyErr *SignInCopyError
	if errors.As(err, &copyErr) {
		parts := strings.Split(copyErr.Message, "\n")
		lead := parts[0]
		if lead != "" {
			lead = strings.ToLower(lead[:1]) + lead[1:]
		}
		out := []string{"Could not finish connecting a provider: " + lead, nothingSaved}
		return append(out, parts[1:]...)
	}
	rules := []signInCopyRule{
		{IsCancelled, "sign-in was cancelled"},
		{IsNetworkError, "no internet connection, or the provider could not be reached"},
	}
	reason := classifySignInError(err, rules, "something went wrong while talking to the provider")
	lines := []string{fmt.Sprintf("Could not finish connecting a provider (%s).", reason), nothingSaved}
	if !IsCancelled(err) {
		lines = append(lines, signInDetailsLine(err))
	}
	return lines
}
