package memory

import (
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Marker returned when the redactor itself fails so raw text never leaves the process
const RedactionUnavailable = "[redaction-unavailable]"

// Every built in and plugin secret prefix pattern the matcher runs in registration order, the corpus egress callers test against
func PrefixPatterns() []string { return tools.RedactPrefixPatterns() }

// Leading literal characters of a prefix pattern, the text a synthetic token starts with
func LiteralPrefix(pattern string) string { return tools.RedactLiteralPrefix(pattern) }

var displayControlRe = regexp.MustCompile("[\x00-\x1f\x7f\u0080-\u009f\u200b-\u200f\u202a-\u202e\u2060-\u2064]")

// Secret masked for display, keeping head and tail once the value clears the floor
func MaskSecret(value string, head, tail, floor int, placeholder, empty string) string {
	if value != "" {
		value = displayControlRe.ReplaceAllString(value, "")
	}
	if value == "" {
		return empty
	}
	runes := []rune(value)
	if len(runes) < floor {
		return placeholder
	}
	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}

// Options of one redaction pass
type RedactOptions struct {
	CodeFile             bool
	FileRead             bool
	SecretFile           bool
	RedactURLCredentials bool
}

// Text with every credential shape masked, the force path every egress boundary runs
func RedactSensitiveText(text string, opts RedactOptions) string {
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, CodeFile: opts.CodeFile,
		FileRead: opts.FileRead, SecretFile: opts.SecretFile, URLCredentials: opts.RedactURLCredentials})
}

// Text scrubbed for a remote reader with the bearer sweep applied, never raw on failure
func RedactForEgress(text string) string { return tools.RedactForEgress(nil, text) }

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_\-./+]{16,}`)

// Values scrubbed for a provider, every token the scrub masked anywhere masked everywhere
func RedactForProvider(values ...any) []any {
	learned := map[string]bool{}
	scrub := func(text string) string {
		out := RedactForEgress(text)
		if out != text {
			after := map[string]bool{}
			for _, t := range tokenRe.FindAllString(out, -1) {
				after[t] = true
			}
			for _, t := range tokenRe.FindAllString(text, -1) {
				if !after[t] {
					learned[t] = true
				}
			}
		}
		return out
	}
	scrubbed := make([]any, len(values))
	for i, v := range values {
		scrubbed[i] = walkStrings(v, scrub)
	}
	if len(learned) == 0 {
		return scrubbed
	}
	tokens := make([]string, 0, len(learned))
	for t := range learned {
		tokens = append(tokens, t)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if len(tokens[i]) != len(tokens[j]) {
			return len(tokens[i]) > len(tokens[j])
		}
		return tokens[i] < tokens[j]
	})
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = regexp.QuoteMeta(t)
	}
	pattern := regexp.MustCompile(strings.Join(quoted, "|"))
	for i, v := range scrubbed {
		scrubbed[i] = walkStrings(v, func(t string) string {
			if pattern.MatchString(t) {
				return pattern.ReplaceAllString(t, "[redacted]")
			}
			return t
		})
	}
	return scrubbed
}

// Copy of a value with every string except data URLs passed through fix
func walkStrings(value any, fix func(string) string) any {
	switch v := value.(type) {
	case string:
		if v == "" || strings.HasPrefix(v, "data:") {
			return v
		}
		return fix(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = walkStrings(item, fix)
		}
		return out
	case *pyjson.Object:
		if v == nil {
			return value
		}
		out := pyjson.NewObject()
		for _, k := range v.Keys {
			out.Set(k, walkStrings(v.Vals[k], fix))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = walkStrings(item, fix)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = walkStrings(item, fix).(string)
		}
		return out
	}
	return value
}

// Scrubbed string for a provider hand off
func redactString(text string) string {
	return RedactForProvider(text)[0].(string)
}
