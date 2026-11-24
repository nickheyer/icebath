package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

const maxLoggedErrorChars = 8192

// Caps an error body at 2048 code points for model context while the debug log keeps 8192
func boundModelErrorText(env *Env, text string) string {
	n := utf8.RuneCountInString(text)
	if n <= maxToolErrorChars {
		return text
	}
	envLog(env).Debug(fmt.Sprintf("tool error body truncated for context (%d chars): %s", n, textutil.First(text, maxLoggedErrorChars)))
	return textutil.First(text, maxToolErrorChars) + toolErrorTruncationMarker
}

// Trims an oversized error string of a JSON object result, re-encoded with json.dumps defaults and ensure_ascii off
func boundJSONErrorResult(env *Env, result string) string {
	if utf8.RuneCountInString(result) <= maxToolErrorChars || !strings.Contains(result, `"error"`) {
		return result
	}
	decoded, err := pyjson.Loads(result)
	if err != nil {
		return result
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return result
	}
	v, _ := payload.Get("error")
	errText, ok := v.(string)
	if !ok || utf8.RuneCountInString(errText) <= maxToolErrorChars {
		return result
	}
	payload.Set("error", boundModelErrorText(env, errText))
	out, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false))
	if err != nil {
		return result
	}
	return out
}
