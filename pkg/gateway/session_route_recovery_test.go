package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestMessagingPlatformAndRowSource(t *testing.T) {
	ctx := context.Background()
	for label, want := range map[any]Platform{"telegram": PlatformTelegram, "local": "", "api_server": "", "cli": "", "tui": "", nil: ""} {
		got, ok := messagingPlatform(ctx, label)
		if got != want || ok != (want != "") {
			t.Fatalf("%v: %q %v", label, got, ok)
		}
	}
	row := func(pairs ...any) *pyjson.Object {
		o := pyjson.NewObject()
		for i := 0; i < len(pairs); i += 2 {
			o.Set(pairs[i].(string), pairs[i+1])
		}
		return o
	}
	origin := `{"platform": "telegram", "chat_id": "-100", "chat_type": "group", "thread_id": "600", "user_id": "111"}`
	if s := rowSource(ctx, row("origin_json", origin, "chat_id", "ignored"), PlatformTelegram, ""); s == nil || s.ChatID != "-100" || *s.ThreadID != "600" || s.Profile != nil {
		t.Fatalf("origin source %+v", s)
	}
	cols := rowSource(ctx, row("origin_json", nil, "chat_id", int64(-1001), "user_id", "111", "thread_id", "600", "display_name", "Forum"), PlatformTelegram, "work")
	if cols == nil || cols.ChatID != "-1001" || cols.ChatType != "dm" || *cols.UserID != "111" || *cols.ChatName != "Forum" || *cols.Profile != "work" {
		t.Fatalf("column source %+v", cols)
	}
	if s := rowSource(ctx, row("origin_json", "{not json", "chat_id", "7", "chat_type", "channel"), PlatformDiscord, ""); s == nil || s.ChatType != "channel" || s.UserID != nil {
		t.Fatalf("bad json falls to columns %+v", s)
	}
	if s := rowSource(ctx, row("origin_json", `{"platform": "nosuch", "chat_id": "1"}`, "chat_id", nil), PlatformTelegram, ""); s != nil {
		t.Fatalf("no chat id %+v", s)
	}
	defer func() {
		if err, ok := recover().(*PyError); !ok || err.Kind != "AttributeError" || err.Msg != "'list' object has no attribute 'get'" {
			t.Fatalf("recovered %v", err)
		}
	}()
	rowSource(ctx, row("origin_json", "[1]", "chat_id", "1"), PlatformTelegram, "")
}

func TestIsMessagingPlatformSourceExcludesLocalAPIAndUnknownLabels(t *testing.T) {
	for label, want := range map[string]bool{"telegram": true, "discord": true, "local": false, "api_server": false, "nope": false, "": false} {
		if got := IsMessagingPlatformSource(label); got != want {
			t.Errorf("%q: got %v want %v", label, got, want)
		}
	}
}
