package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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

// Trims an oversized error string of a JSON object result, the object re-encoded spaced in UTF-8 with its numbers exactly as written
func boundJSONErrorResult(env *Env, result string) string {
	if utf8.RuneCountInString(result) <= maxToolErrorChars || !strings.Contains(result, `"error"`) {
		return result
	}
	decoded, err := jsonutil.DecodeWith(result, jsonutil.BigInts())
	if err != nil {
		return result
	}
	payload, ok := decoded.(*jsonutil.Object)
	if !ok {
		return result
	}
	v, _ := payload.Get("error")
	errText, ok := v.(string)
	if !ok || utf8.RuneCountInString(errText) <= maxToolErrorChars {
		return result
	}
	payload.Set("error", boundModelErrorText(env, errText))
	out, err := jsonutil.Encode(payload, jsonutil.Spaced())
	if err != nil {
		return result
	}
	return out
}
