package telegram

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const toastLimit = 200

// Error text with secrets redacted, the class name in angle brackets when the text is empty
func redactTelegramErrorText(err error) (out string) {
	if err == nil {
		return ""
	}
	text := err.Error()
	if text == "" {
		return "<" + className(err) + ">"
	}
	defer func() {
		if recover() != nil {
			out = "<telegram error redacted>"
		}
	}()
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
}

// Deadline expiry of a bounded Telegram call, the builtin TimeoutError hermes raises
type deadlineTimeoutError struct{ msg string }

func (e *deadlineTimeoutError) Error() string      { return e.msg }
func (e *deadlineTimeoutError) PyTypeName() string { return "TimeoutError" }

// Runs fn under a wall clock deadline that abandons it on expiry and runs the cleanup detached
func awaitWithThreadDeadline(ctx context.Context, fn func(ctx context.Context) (any, error), timeout float64,
	onAbandon func(ctx context.Context) error, label string, dumpOnBlockedLoop bool) (any, error) {
	d, bounded := provider.ClampTimeout(timeout)
	result, err := provider.RunBoundedAsync(ctx, fn, d, bounded, label, onAbandon, dumpOnBlockedLoop, nil)
	if err != nil {
		return nil, err
	}
	if result.TimedOut {
		return nil, &deadlineTimeoutError{msg: fmt.Sprintf("timed out after %.0fs (%s)", timeout, label)}
	}
	return result.Value, nil
}

// First limit code points of text, the Python prefix slice of a string
func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// Refusal sentence answered to every unauthorized button tap in the active language
func unauthorizedToast(ctx context.Context) string {
	return clipRunes(gateway.UnauthorizedActionNoticeFor(ctx, gateway.PlatformTelegram), toastLimit)
}

// Translated callback answer text cut at the Telegram toast cap
func toast(ctx context.Context, key string, kwargs map[string]any) string {
	return clipRunes(i18n.T(ctx, key, "", kwargs), toastLimit)
}

// Fail closed send result for a flood wait over the inline cap
func floodCapResult(wait float64) gateway.SendResult {
	w := wait
	return gateway.SendResult{Success: false, Error: "flood_control:" + pyrepr.Str(wait), RetryAfter: &w}
}
