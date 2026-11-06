package provider

import (
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// CoerceLLMMessage pulls the message out of a response or returns a bare message, nil for an empty choices list
func CoerceLLMMessage(response any) any {
	if response == nil {
		return nil
	}
	if _, ok := response.(string); ok {
		return response
	}
	var choices any
	if m, ok := response.(map[string]any); ok {
		raw, has := m["choices"]
		if !has {
			return response
		}
		choices = raw
	} else {
		choices = MessageField(response, "choices")
		if !auxTruthy(choices) {
			return response
		}
	}
	v := reflect.ValueOf(choices)
	if !auxTruthy(choices) || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) || v.Len() == 0 {
		return nil
	}
	return MessageField(v.Index(0).Interface(), "message")
}

// MessageField reads a field from a dict shaped message or the matching attribute of a struct, nil when absent
func MessageField(message any, field string) any {
	switch m := message.(type) {
	case nil:
		return nil
	case map[string]any:
		return m[field]
	case map[string]string:
		if s, ok := m[field]; ok {
			return s
		}
		return nil
	}
	v := reflect.ValueOf(message)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == field || strings.EqualFold(f.Name, strings.ReplaceAll(field, "_", "")) {
			return v.Field(i).Interface()
		}
	}
	return nil
}

// Closed reasoning block pattern per think tag name, the agent runtime helpers reasoning block patterns
var auxReasoningBlockPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(ThinkTagNames))
	for _, n := range ThinkTagNames {
		q := regexp.QuoteMeta(n)
		out = append(out, regexp.MustCompile(`(?is)<`+q+`>.*?</`+q+`>`))
	}
	return out
}()

// ExtractContentOrReasoning is the response content without think blocks, else its reasoning fields bounded when the cap is not negative
func ExtractContentOrReasoning(response any, maxReasoningChars int) string {
	msg := CoerceLLMMessage(response)
	if msg == nil {
		return ""
	}
	if s, ok := msg.(string); ok {
		return strings.TrimSpace(s)
	}
	raw, ok := MessageField(msg, "content").(string)
	if !ok {
		if v := MessageField(msg, "content"); pyrepr.Truthy(v) {
			raw = pyrepr.Str(v)
		}
	}
	if content := strings.TrimSpace(raw); content != "" {
		cleaned := content
		for _, re := range auxReasoningBlockPatterns {
			cleaned = re.ReplaceAllString(cleaned, "")
		}
		if cleaned = strings.TrimSpace(cleaned); cleaned != "" {
			return cleaned
		}
	}
	var parts []string
	seen := func(s string) bool { return slices.Contains(parts, s) }
	for _, field := range []string{"reasoning", "reasoning_content"} {
		if val, ok := MessageField(msg, field).(string); ok && strings.TrimSpace(val) != "" && !seen(val) {
			parts = append(parts, strings.TrimSpace(val))
		}
	}
	if details, ok := MessageField(msg, "reasoning_details").([]any); ok {
		for _, d := range details {
			detail, ok := d.(map[string]any)
			if !ok {
				continue
			}
			summary := detail["summary"]
			if !pyrepr.Truthy(summary) {
				summary = detail["content"]
			}
			if !pyrepr.Truthy(summary) {
				summary = detail["text"]
			}
			if !pyrepr.Truthy(summary) {
				continue
			}
			if s, isStr := summary.(string); isStr {
				if !seen(s) {
					parts = append(parts, strings.TrimSpace(s))
				}
			} else if str := pyrepr.Str(summary); !seen(str) {
				parts = append(parts, str)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	text := strings.Join(parts, "\n\n")
	if runes := []rune(text); maxReasoningChars >= 0 && len(runes) > maxReasoningChars {
		slog.Warn(fmt.Sprintf("fell back to reasoning fields (%d chars); truncating to %d", len(runes), maxReasoningChars))
		return string(runes[:maxReasoningChars])
	}
	return text
}
