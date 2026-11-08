package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Messaging platform a row source label names, false for the API transport, local labels and unknown labels
func messagingPlatform(ctx context.Context, label any) (Platform, bool) {
	platform, err := ParsePlatform(ctx, label)
	if err != nil || platform == PlatformLocal || platform == PlatformAPIServer {
		return "", false
	}
	return platform, true
}

// IsMessagingPlatformSource reports whether a row source label names a messaging platform, for the state wiring
func IsMessagingPlatformSource(source string) bool {
	_, ok := messagingPlatform(context.Background(), source)
	return ok
}

// Source from the row persisted origin, else from its columns, nil when the row has no chat id
func rowSource(ctx context.Context, row *pyjson.Object, platform Platform, profile string) *SessionSource {
	raw, _ := row.Get("origin_json")
	if text, ok := raw.(string); ok {
		if parsed, err := pyjson.Loads(text); err == nil {
			obj, isObject := parsed.(*pyjson.Object)
			if !isObject {
				panic(&PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(parsed) + "' object has no attribute 'get'"})
			}
			if source, err := SessionSourceFromDict(ctx, pyjson.Plain(obj).(map[string]any)); err == nil {
				return source
			}
		}
	}
	chatID, ok := row.Get("chat_id")
	if !ok || chatID == nil {
		return nil
	}
	chatType := "dm"
	if v, _ := row.Get("chat_type"); pyTruthy(v) {
		chatType = pyStrOf(v)
	}
	source := &SessionSource{Platform: platform, ChatID: pyStrOf(chatID), ChatType: chatType,
		UserID: rowOptStr(row, "user_id"), ThreadID: rowOptStr(row, "thread_id"), ChatName: rowOptStr(row, "display_name")}
	if profile != "" {
		source.Profile = &profile
	}
	return source
}

// Optional text column of a row, nil for a missing or null value
func rowOptStr(row *pyjson.Object, key string) *string {
	v, ok := row.Get(key)
	if !ok || v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	if text, ok := v.(string); ok {
		s = text
	}
	return &s
}

// Python type name of a decoded JSON value
func jsonValueTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case []any:
		return "list"
	case float64:
		return "float"
	}
	return "int"
}
