package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
func rowSource(ctx context.Context, row *jsonutil.Object, platform Platform, profile string) *SessionSource {
	raw, _ := row.Get("origin_json")
	if text, ok := raw.(string); ok {
		if parsed, err := jsonutil.Decode(text); err == nil {
			obj, isObject := parsed.(*jsonutil.Object)
			if !isObject {
				panic(&errorutil.Error{Kind: "AttributeError", Msg: "the stored session source is " + errchain.Kind(parsed) + ", expected a mapping"})
			}
			if source, err := SessionSourceFromDict(ctx, jsonutil.Plain(obj).(map[string]any)); err == nil {
				return source
			}
		}
	}
	chatID, ok := row.Get("chat_id")
	if !ok || chatID == nil {
		return nil
	}
	chatType := "dm"
	if v, _ := row.Get("chat_type"); anyutil.IsTrue(v) {
		chatType = anyutil.String(v)
	}
	source := &SessionSource{Platform: platform, ChatID: anyutil.String(chatID), ChatType: chatType,
		UserID: rowOptStr(row, "user_id"), ThreadID: rowOptStr(row, "thread_id"), ChatName: rowOptStr(row, "display_name")}
	if profile != "" {
		source.Profile = &profile
	}
	return source
}

// Optional text column of a row, nil for a missing or null value
func rowOptStr(row *jsonutil.Object, key string) *string {
	v, ok := row.Get(key)
	if !ok || v == nil {
		return nil
	}
	s := anyutil.String(v)
	if text, ok := v.(string); ok {
		s = text
	}
	return &s
}
