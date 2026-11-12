package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Theme apply answers in milliseconds, a picker waits for a person
const (
	themeTimeoutS     = 15.0
	themePickTimeoutS = 300.0
)

// Server request sender and clock the picker bridge reads
var (
	themeSend      = tuigateway.Send
	themeMonotonic = pythread.Monotonic
)

// Result of a theme request no desktop window answered
func themeUnanswered() map[string]any {
	return map[string]any{"success": false, "error": "No Icebath Desktop window answered the theme request."}
}

// Renderer answer to a set_theme request, a dismissed picker told apart from an expired one
func themeRequest(ctx context.Context, sid string, payload map[string]any) any {
	if _, picker := payload["picker"]; !picker {
		timeout := themeTimeoutS
		answer := askRequest(ctx, "theme.apply", sid, copyDict(payload), &timeout)
		if answer == "" {
			return themeUnanswered()
		}
		decoded, err := pyjson.Loads(answer)
		if err != nil {
			panic(err)
		}
		return decoded
	}
	started := themeMonotonic()
	timeout := themePickTimeoutS
	result := sendResult(ctx, themeSend)("theme.pick", sid, objectOf(copyDict(payload)), &timeout, nil)
	if result == nil {
		if themeMonotonic()-started >= themePickTimeoutS {
			return map[string]any{"timed_out": true}
		}
		return map[string]any{"cancelled": true}
	}
	switch picked := result.GetOr("picked", nil).(type) {
	case *pyjson.Object:
		return map[string]any{"success": true, "picked": picked}
	case map[string]any:
		return map[string]any{"success": true, "picked": picked}
	}
	if said, isText := result.GetOr("said", nil).(string); isText && pystr.Strip(said) != "" {
		return map[string]any{"said": pystr.Strip(said)}
	}
	return map[string]any{"cancelled": true}
}
