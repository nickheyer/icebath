package model

import (
	"context"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const OfflineSummary = "icebath can't reach the model provider. You may be offline. Check your internet connection and try again."

var networkResolutionMarkers = []string{
	"temporary failure in name resolution",
	"name or service not known",
	"nodename nor servname provided, or not known",
	"getaddrinfo failed",
	"no address associated with hostname",
	"network is unreachable",
}

const xaiEntitlementHint = " — xAI rejected this OAuth account. NOTE: X Premium+ does NOT " +
	"include xAI API access — only standalone SuperGrok subscribers " +
	"can use this provider. Other possible causes: no Grok " +
	"subscription, your tier doesn't include this model, or your " +
	"quota is exhausted. Check https://grok.com/?_s=usage to see " +
	"which, or run `/model` to switch providers."

var errorDetailKeys = []string{"message", "detail", "error", "code", "type"}

var thinkingTimeoutTexts = []string{
	"broken pipe", "errno 32", "remote protocol", "connection reset", "connection lost",
	"peer closed", "server disconnected",
}

var (
	htmlTitle = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	htmlRayID = regexp.MustCompile(`Cloudflare Ray ID:\s*<strong[^>]*>([^<]+)</strong>`)
)

func xaiEntitlementText(lower string) bool {
	return strings.Contains(lower, "do not have an active grok subscription") ||
		(strings.Contains(lower, "out of available resources") && strings.Contains(lower, "grok")) ||
		(strings.Contains(lower, "does not have permission") && strings.Contains(lower, "grok"))
}

func EntitlementFailure(c ErrorContext, status int) bool {
	if status != 401 && status != 403 && status != 0 {
		return false
	}
	haystack := strings.ToLower(c.Message) + " " + strings.ToLower(c.Reason)
	if strings.TrimSpace(haystack) == "" {
		return false
	}
	if strings.Contains(haystack, "[wke=unauthenticated:") || strings.Contains(haystack, "oauth2 access token could not be validated") {
		return false
	}
	return xaiEntitlementText(haystack)
}

func DecorateXAIEntitlement(detail string) string {
	if detail == "" || !xaiEntitlementText(strings.ToLower(detail)) || strings.Contains(detail, "X Premium+ does NOT include") {
		return detail
	}
	return detail + xaiEntitlementHint
}

func DetailText(raw []byte) string {
	v, err := wire.ParseValue(raw)
	if err != nil {
		return string(raw)
	}
	return detailText(v)
}

func detailText(v wire.Value) string {
	switch v.Kind() {
	case wire.KindString:
		return v.Text()
	case wire.KindObject:
		for _, key := range errorDetailKeys {
			if s, ok := v.Get(key).Str(); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		for _, key := range errorDetailKeys {
			if nested := v.Get(key); nested.Exists() {
				if text := detailText(nested); text != "" {
					return text
				}
			}
		}
		text, _ := wire.FormatJSON(v.Raw(), wire.TextLayout{Spaced: true, Sorted: true, UTF8: true})
		return text
	case wire.KindArray:
		var parts []string
		for _, item := range v.Elems() {
			if text := detailText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "; ")
	case wire.KindNull, wire.KindNone:
		return ""
	}
	return v.Display()
}

func timeoutPhase(err error) string {
	var he *httpclient.Error
	if errors.As(err, &he) {
		if !he.Timeout() {
			return ""
		}
		switch he.Phase {
		case httpclient.PhaseConnect, httpclient.PhaseProxy, httpclient.PhaseTLS:
			return "connect"
		case httpclient.PhasePool, httpclient.PhaseWrite:
			return ""
		}
		return "read"
	}
	var ne net.Error
	if errors.Is(err, httpclient.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "read"
	}
	return ""
}

func TimeoutSummary(err error, provider string) (string, bool) {
	switch timeoutPhase(err) {
	case "connect":
		return "Connection to the provider timed out before it was established (check the base_url and network path)", true
	case "read":
		key := strings.TrimSpace(provider)
		if key == "" {
			key = "<provider>"
		}
		return "Request timed out waiting for the provider to respond (if the model needs longer, raise providers." +
			key + ".request_timeout_seconds in config.yaml)", true
	}
	return "", false
}

func IsStreamParseError(err error, mode wire.Mode) bool {
	var se *wire.StreamError
	return mode == wire.ModeAnthropic && errors.As(err, &se) && se.Code == wire.StreamNonJSON
}

func Summary(err error, provider string, mode wire.Mode) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if containsAny(strings.ToLower(e.Error()), networkResolutionMarkers) {
			return OfflineSummary
		}
	}
	if offlineFailure(err) {
		return OfflineSummary
	}
	if text, ok := TimeoutSummary(err, provider); ok {
		return text
	}
	raw := safeText(err)
	if IsStreamParseError(err, mode) {
		return "Malformed provider streaming response: " + truncateRunes(raw, 300)
	}
	var ae *wire.APIError
	var se *wire.StreamError
	isAPI := errors.As(err, &ae)
	prefix := ""
	if isAPI && ae.Status != 0 {
		prefix = "HTTP " + strconv.Itoa(ae.Status) + ": "
	}
	if strings.Contains(raw, "<!DOCTYPE") || strings.Contains(raw, "<html") {
		title := "HTML error page (title not found)"
		if m := htmlTitle.FindStringSubmatch(raw); m != nil {
			title = strings.TrimSpace(m[1])
		}
		var parts []string
		if prefix != "" {
			parts = append(parts, prefix[:len(prefix)-2])
		}
		parts = append(parts, title)
		if m := htmlRayID.FindStringSubmatch(raw); m != nil {
			parts = append(parts, "Ray "+strings.TrimSpace(m[1]))
		}
		return strings.Join(parts, " — ")
	}
	if isAPI && strings.HasPrefix(ae.Code(), "gemini_") {
		return wire.MaskCredentials(truncateRunes(ae.Message(), 1000))
	}
	var body wire.Value
	switch {
	case isAPI:
		body, _ = wire.ParseValue(ae.Body())
	case errors.As(err, &se):
		body = streamErrorBody(se)
	}
	if body.IsObject() {
		msg := body.Get("message")
		if obj := body.Get("error"); obj.IsObject() {
			msg = obj.Get("message")
		}
		if msg.Truthy() {
			return DecorateXAIEntitlement(prefix + truncateRunes(detailText(msg), 300))
		}
	}
	if isAPI && !ae.InStream {
		snippet := ""
		if s, ok := body.Str(); ok {
			snippet = strings.TrimSpace(s)
		} else if body.Exists() {
			snippet = strings.TrimSpace(string(ae.Body()))
		}
		if snippet != "" {
			return wire.MaskCredentials(prefix + truncateRunes(snippet, 300))
		}
	}
	return DecorateXAIEntitlement(prefix + truncateRunes(raw, 500))
}

func MaskCredential(c auth.Credential) string {
	if c.Tokens != nil {
		return "<entra-id-bearer>"
	}
	key := c.Key.Reveal()
	switch n := len([]rune(key)); {
	case key == "":
		return ""
	case n <= 12:
		return "***"
	}
	runes := []rune(key)
	return string(runes[:8]) + "..." + string(runes[len(runes)-4:])
}

func CleanMessage(text string) string {
	if text == "" {
		return "Unknown error"
	}
	if strings.HasPrefix(strings.TrimSpace(text), "<!DOCTYPE html") || strings.Contains(text, "<html") {
		return "Service temporarily unavailable (HTML error page returned)"
	}
	cleaned := strings.Join(strings.Fields(text), " ")
	if len([]rune(cleaned)) > 150 {
		cleaned = truncateRunes(cleaned, 150) + "..."
	}
	return cleaned
}

func IsThinkingTimeout(e *Error, model string) bool {
	if e == nil || e.Class != ClassTimeout {
		return false
	}
	if _, ok := ReasoningStaleFloor(model); !ok {
		return false
	}
	if e.Err == nil {
		return false
	}
	return containsAny(strings.ToLower(e.Err.Error()), thinkingTimeoutTexts) || connectionDropped(e.Err) || brokenPipe(e.Err)
}

func ThinkingTimeoutGuidance(provider, model, label, homeDir string) (string, error) {
	shown, err := home.Display(homeDir)
	if err != nil {
		return "", err
	}
	if label == "" {
		label = model
	}
	return label + " was thinking for so long that the connection timed out before it wrote anything " +
		"(common for reasoning models behind cloud gateways such as NVIDIA NIM, OpenAI, Anthropic, " +
		"DeepSeek). Easiest fixes: `/reasoning low`, or switch to a faster model with /model. " +
		"Advanced: set `providers." + provider + ".models." + model + ".stale_timeout_seconds: 900` in " +
		"`" + shown + "/config.yaml` to allow a longer wait.", nil
}
