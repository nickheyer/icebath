package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

var (
	ErrCancelled           = errors.New("login cancelled")
	ErrDeviceFlowCancelled = errors.New("device sign-in cancelled")
)

func IsRateLimited(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && !ae.Relogin && ae.Code == CodexRateLimitedCode
}

func PrimaryFailureWording(err error) (logPhrase, userPhrase string) {
	if IsRateLimited(err) {
		return "rate-limited (429)", "Primary provider quota exhausted"
	}
	return "auth failed", "Primary auth failed"
}

var genericEntitlementMessages = map[string]string{
	"subscription_required": "No active paid subscription found. Please purchase/activate a subscription, then retry.",
	"insufficient_credits":  "Subscription credits are exhausted. Top up/renew credits, then retry.",
}

var entitlementErrorCodes = []string{
	"subscription_required", "insufficient_credits",
	"subscription_expired", "no_usable_credits", "account_missing", "member_spend_cap_exceeded",
}

func (s *Store) FormatError(ctx context.Context, err error) string {
	var ae *Error
	if !errors.As(err, &ae) || IsRateLimited(err) {
		return err.Error()
	}
	if ae.Relogin {
		return fmt.Sprintf("%s Run `icebath %smodel` to re-authenticate.", ae.Error(), home.ProfileSelector(s.home))
	}
	if slices.Contains(entitlementErrorCodes, ae.Code) {
		if ae.Provider == ProviderPortal {
			if spec, ok := s.dir.Spec(s.bind(ctx), ProviderPortal); ok && spec.EntitlementMessage != nil {
				if message := spec.EntitlementMessage(ctx); message != "" {
					return message
				}
			}
			return fmt.Sprintf("%s Check credits or billing in Icebath Portal, then retry.", ae.Error())
		}
		if generic, ok := genericEntitlementMessages[ae.Code]; ok {
			return generic
		}
	}
	if ae.Code == "temporarily_unavailable" {
		return ae.Error() + " Please retry in a few seconds."
	}
	return ae.Error()
}

var deviceFlowErrorCopy = map[string]string{
	"expired_token": "The sign-in code expired before it was approved in the browser. Run `{retry}` to get a new code.",
	"access_denied": "Sign-in was declined in the browser. Run `{retry}` to try again, or `icebath model` to pick a " +
		"different provider.",
	"invalid_grant":  "The sign-in code was not accepted by the server. Run `{retry}` to get a new code.",
	"invalid_client": "The server did not recognize this copy of icebath. Run `icebath update`, then `{retry}` again.",
}

type DeviceCodeExpiredError struct {
	Message string
	Cause   error
}

func (e *DeviceCodeExpiredError) Error() string { return e.Message }

func (e *DeviceCodeExpiredError) Unwrap() error { return e.Cause }

type SignInError struct {
	Message   string
	OAuthCode string
}

func (e *SignInError) Error() string { return e.Message }

func IsNetworkError(err error) bool {
	var sign *SignInError
	var expired *DeviceCodeExpiredError
	if err == nil || errors.As(err, &sign) || errors.As(err, &expired) {
		return false
	}
	var he *httpclient.Error
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &he) || errors.As(err, &ne) || errors.As(err, &ue) ||
		errors.Is(err, httpclient.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

func IsDeviceCodeExpired(err error) bool {
	var expired *DeviceCodeExpiredError
	if errors.As(err, &expired) {
		return true
	}
	var ae *Error
	if errors.As(err, &ae) && (ae.Code == "device_code_timeout" || ae.Code == "timeout") {
		return true
	}
	var flow *SignInFlowError
	if errors.As(err, &flow) && flow.OAuthCode == "expired_token" {
		return true
	}
	var sign *SignInError
	return errors.As(err, &sign) && sign.OAuthCode == "expired_token"
}

func IsCancelled(err error) bool {
	return errors.Is(err, ErrCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}

func DeviceFlowError(code, description, retryCommand string) *SignInError {
	if retryCommand == "" {
		retryCommand = "icebath portal"
	}
	lead := strings.ReplaceAll(deviceFlowErrorCopy[code], "{retry}", retryCommand)
	if lead == "" {
		shown := description
		if shown == "" {
			shown = "the server rejected the request"
		}
		lead = fmt.Sprintf("Sign-in did not complete: %s. Run `%s` to try again.", shown, retryCommand)
	}
	details := description
	if code != "" {
		details = code + ": " + description
	}
	message := lead
	if details != "" {
		message = lead + "\n  Details: " + details
	}
	return &SignInError{Message: message, OAuthCode: code}
}

func detailsLine(err error) string {
	text := strings.TrimSpace(err.Error())
	if text == "" {
		text = fmt.Sprintf("%T", err)
	}
	return "  Details: " + text
}

func SignInFailureLines(err error, serviceHost, retryCommand string) []string {
	if serviceHost == "" {
		serviceHost = "portal.icebath.app"
	}
	if retryCommand == "" {
		retryCommand = "icebath portal"
	}
	var sign *SignInError
	if errors.As(err, &sign) {
		return strings.Split(sign.Error(), "\n")
	}
	lead := "Could not sign in. Run `{retry}` to try again, or `icebath model` to pick a different provider."
	switch {
	case IsCancelled(err):
		lead = "Sign-in was cancelled. Run `{retry}` when you want to try again."
	case IsNetworkError(err):
		lead = "Could not sign in: icebath could not reach {host}. Check your internet connection or proxy, " +
			"then run `{retry}` again."
	}
	lines := []string{strings.NewReplacer("{host}", serviceHost, "{retry}", retryCommand).Replace(lead)}
	if !IsCancelled(err) {
		lines = append(lines, detailsLine(err))
	}
	return lines
}

func lowerFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToLower(r)) + s[i+len(string(r)):]
	}
	return s
}

func ProviderSetupFailureLines(err error, retryCommand string) []string {
	if retryCommand == "" {
		retryCommand = "icebath model"
	}
	nothingSaved := "Your provider settings were not changed. Continue the wizard now and run " +
		"`" + retryCommand + "` afterwards to try again."
	var sign *SignInError
	if errors.As(err, &sign) {
		parts := strings.Split(sign.Error(), "\n")
		lines := []string{"Could not finish connecting a provider: " + lowerFirst(parts[0]), nothingSaved}
		return append(lines, parts[1:]...)
	}
	reason := "something went wrong while talking to the provider"
	switch {
	case IsCancelled(err):
		reason = "sign-in was cancelled"
	case IsNetworkError(err):
		reason = "no internet connection, or the provider could not be reached"
	}
	lines := []string{fmt.Sprintf("Could not finish connecting a provider (%s).", reason), nothingSaved}
	if !IsCancelled(err) {
		lines = append(lines, detailsLine(err))
	}
	return lines
}

func (s *Store) UnknownProviderHint(ctx context.Context, provider string) string {
	ctx = s.bind(ctx)
	if moved := s.dir.InstallHint(ctx, provider); moved != "" {
		return moved
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "opencode-free", "free", "opencode_free":
		return "OpenCode discontinued anonymous free-tier access outside its own client " +
			"(relay 403s FreeTierError), so the keyless 'opencode-free' provider was removed. " +
			"Switch to 'opencode-zen' (pay-as-you-go, OPENCODE_ZEN_API_KEY) or 'opencode-go' " +
			"($10/mo subscription, OPENCODE_GO_API_KEY) via 'icebath model'."
	}
	issues, err := s.cfg.Validate(ctx)
	if err != nil || len(issues) == 0 {
		return ""
	}
	lines := []string{"Config issue detected — run 'icebath doctor' for full diagnostics:"}
	for _, issue := range issues {
		level := "WARNING"
		if issue.Severity == config.SeverityError {
			level = "ERROR"
		}
		lines = append(lines, fmt.Sprintf("  [%s] %s", level, issue.Message))
		if first, _, _ := strings.Cut(issue.Hint, "\n"); issue.Hint != "" && first != "" {
			lines = append(lines, "    → "+first)
		}
	}
	return strings.Join(lines, "\n")
}
