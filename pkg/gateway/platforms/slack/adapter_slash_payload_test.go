package slack

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSlashCommandTextMapsSubcommandsAndKeepsNativeSlashes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		command map[string]any
		want    string
	}{
		{"compact maps to compress", map[string]any{"text": "compact", "user_id": "U1", "channel_id": "C1"}, "/compress"},
		{"native slash keeps its argument", map[string]any{"command": "/reasoning", "text": "xhigh"}, "/reasoning xhigh"},
		{"legacy prefix still works", map[string]any{"command": "/icebath", "text": "btw run the tests"}, "/btw run the tests"},
		{"native slash keeps raw spacing", map[string]any{"command": "/btw", "text": "  two  spaces "}, "/btw   two  spaces "},
		{"native slash without text", map[string]any{"command": "/stop", "text": ""}, "/stop"},
		{"native slash name is stripped", map[string]any{"command": "//model "}, "/model"},
		{"legacy subcommand alone", map[string]any{"command": "/icebath", "text": "  stop  "}, "/stop"},
		{"free form text is a question", map[string]any{"command": "/icebath", "text": "  what broke today? "}, "what broke today?"},
		{"empty legacy text asks for help", map[string]any{"command": "/icebath", "text": "   "}, "/help"},
		{"missing command and text asks for help", map[string]any{}, "/help"},
		{"text is coerced to str", map[string]any{"command": "/retry", "text": 42}, "/retry 42"},
	} {
		got, err := SlashCommandText(ctx, tc.command)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v want %q", tc.name, got, err, tc.want)
		}
	}
	_, err := SlashCommandText(ctx, map[string]any{"command": 7})
	var payloadErr *SlashPayloadError
	if !errors.As(err, &payloadErr) || err.Error() != "'int' object has no attribute 'lstrip'" {
		t.Fatalf("non string command error = %v", err)
	}
}

func TestSlashCommandTextPanicsWithoutTheWiredSubcommandMap(t *testing.T) {
	wired := SubcommandMap
	SubcommandMap = nil
	defer func() {
		SubcommandMap = wired
		if r := recover(); r == nil || !strings.HasPrefix(r.(string), "slack.SubcommandMap unassigned") {
			t.Fatalf("recover = %v", r)
		}
	}()
	SlashCommandText(context.Background(), map[string]any{"command": "/icebath", "text": "btw"})
}

func TestSlashThreadIDPrefersThreadTsAcrossNestedPayloads(t *testing.T) {
	for _, tc := range []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"thread_ts": "1111111111.000001"}, "1111111111.000001"},
		{map[string]any{"message": map[string]any{"thread_ts": "2222222222.000001"}}, "2222222222.000001"},
		{map[string]any{"container": map[string]any{"thread_ts": "3333333333.000001"}}, "3333333333.000001"},
		{map[string]any{"message_ts": "4444444444.000001"}, "4444444444.000001"},
		{map[string]any{"container": map[string]any{"message_ts": "5555555555.000001"}}, "5555555555.000001"},
		{map[string]any{"message_ts": "fallback-message-ts", "message": map[string]any{"thread_ts": "parent-thread-ts"}}, "parent-thread-ts"},
	} {
		command := map[string]any{"command": "/reasoning", "text": "xhigh", "user_id": "U1", "channel_id": "C1"}
		for k, v := range tc.payload {
			command[k] = v
		}
		got := SlashThreadID(command)
		if got == nil || *got != tc.want {
			t.Errorf("%v: got %v want %q", tc.payload, got, tc.want)
		}
		text, err := SlashCommandText(context.Background(), command)
		if err != nil || text != "/reasoning xhigh" {
			t.Errorf("%v: text %q, %v", tc.payload, text, err)
		}
	}
	if got := SlashThreadID(map[string]any{"thread_ts": "", "message": "not a dict", "container": map[string]any{}}); got != nil {
		t.Fatalf("no anchor = %q", *got)
	}
	if got := SlashThreadID(map[string]any{"message_ts": 1700000000.5}); got == nil || *got != "1700000000.5" {
		t.Fatalf("numeric ts = %v", got)
	}
}
