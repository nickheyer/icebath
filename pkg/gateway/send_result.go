package gateway

import (
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
)

// SendResult is the outcome of one adapter send or edit, gateway/platforms/base.py SendResult
type SendResult struct {
	Success                bool
	MessageID              any
	Error                  any
	RawResponse            any
	Retryable              bool
	RetryAfter             *float64
	ContinuationMessageIDs []any
	ErrorKind              *string
}

// SendErrorKinds is the platform neutral set of send failure kinds
var SendErrorKinds = map[string]bool{
	"too_long": true, "bad_format": true, "forbidden": true, "not_found": true,
	"rate_limited": true, "transient": true, "unknown": true,
}

var (
	chatLevelNotFoundSubstrings = []string{"chat not found"}
	subchatNotFoundSubstrings   = []string{
		"message to edit not found", "message to reply not found", "thread not found", "topic_deleted", "message_id_invalid",
	}
	retryableErrorPatterns = []string{
		"connecterror", "connectionerror", "connectionreset", "connectionrefused", "connecttimeout",
		"network", "broken pipe", "remotedisconnected", "eoferror",
	}
)

// Lowercased text of the error text, the error message and its Python class that both classifiers read
func sendErrorBlob(exc error, errorText string) string {
	var parts []string
	if errorText != "" {
		parts = append(parts, errorText)
	}
	if exc != nil {
		for _, p := range []string{exc.Error(), pyErrorKind(exc)} {
			if p != "" {
				parts = append(parts, p)
			}
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func anyIn(blob string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(blob, n) {
			return true
		}
	}
	return false
}

var sendErrorClassifiers = []struct {
	kind  string
	match func(string) bool
}{
	{"too_long", func(b string) bool { return anyIn(b, "message_too_long", "too long", "message is too long") }},
	{"bad_format", func(b string) bool {
		return anyIn(b, "can't parse entities", "cant parse entities", "can't find end", "unsupported start tag") ||
			(strings.Contains(b, "entity") && strings.Contains(b, "parse")) ||
			(strings.Contains(b, "bad request") && strings.Contains(b, "entit"))
	}},
	{"forbidden", func(b string) bool {
		return anyIn(b, "forbidden", "bot was blocked", "blocked by the user", "user is deactivated",
			"not enough rights", "have no rights", "not a member")
	}},
	{"not_found", func(b string) bool {
		return anyIn(b, chatLevelNotFoundSubstrings...) || anyIn(b, subchatNotFoundSubstrings...)
	}},
	{"rate_limited", func(b string) bool { return anyIn(b, "flood", "too many requests", "retry after", "rate limit") }},
	{"transient", func(b string) bool { return anyIn(b, retryableErrorPatterns...) || anyIn(b, "connecttimeout") }},
}

// Send failure kind of an error or error text, unknown when nothing matches
func ClassifySendError(exc error, errorText string) string {
	blob := sendErrorBlob(exc, errorText)
	for _, c := range sendErrorClassifiers {
		if c.match(blob) {
			return c.kind
		}
	}
	return "unknown"
}

// True when a not found failure means the whole chat is gone, a sub chat marker winning
func IsChatLevelNotFound(exc error, errorText string) bool {
	blob := sendErrorBlob(exc, errorText)
	return !anyIn(blob, subchatNotFoundSubstrings...) && anyIn(blob, chatLevelNotFoundSubstrings...)
}

// True when a split payload was partly delivered and its visible head must never be sent again
func IsPartialDelivery(result any) bool {
	raw, _ := sendResultAttr(result, "raw_response")
	m, ok := raw.(map[string]any)
	return ok && pyrepr.Truthy(m["partial_overflow"])
}

func snakeToCamelField(key string) string {
	var b strings.Builder
	for _, part := range strings.Split(key, "_") {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		if part == "ids" {
			b.WriteString("IDs")
			continue
		}
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// Attribute of a send result whether it is a result mapping or a result struct
func sendResultAttr(result any, name string) (any, bool) {
	switch m := result.(type) {
	case nil:
		return nil, false
	case map[string]any:
		v, ok := m[name]
		return v, ok
	}
	rv := reflect.ValueOf(result)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	f := rv.FieldByName(snakeToCamelField(name))
	if !f.IsValid() || !f.CanInterface() {
		return nil, false
	}
	v := f.Interface()
	if p, ok := v.(*string); ok {
		if p == nil {
			return nil, true
		}
		return *p, true
	}
	return v, true
}

// Fields of the result the shared metrics read, the raw response only when it is a dict
func (r SendResult) MetricsSendResult() *observability.SendResult {
	raw, _ := r.RawResponse.(map[string]any)
	out := &observability.SendResult{Success: r.Success, RawResponse: raw, Error: r.Error, Retryable: r.Retryable}
	if r.RetryAfter != nil {
		out.RetryAfter = *r.RetryAfter
	}
	if r.ErrorKind != nil {
		out.ErrorKind = *r.ErrorKind
	}
	return out
}
