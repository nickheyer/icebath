package gateway

import (
	"context"
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestDestinationRetainsOnlyFieldsConsumedBySender(t *testing.T) {
	cases := []struct {
		target string
		fields map[string]any
	}{
		{"log", map[string]any{}},
		{"github_comment", map[string]any{"repo": "owned/repo", "pr_number": "7"}},
		{"telegram", map[string]any{"chat_id": "7", "thread_id": "8", "message_thread_id": "9"}},
	}
	for _, c := range cases {
		extra := jsonutil.NewObject()
		for key, value := range c.fields {
			extra.Set(key, value)
		}
		extra.Set("token", "private-secret")
		extra.Set("unused", "private-payload")
		original := jsonutil.ObjectOf("deliver", c.target, "deliver_extra", extra)
		got, err := webhookValidateDestination(context.Background(), original)
		if err != nil {
			t.Fatalf("%s: %v", c.target, err)
		}
		want := map[string]any{"deliver": c.target, "deliver_extra": c.fields}
		if !anyutil.Equal(got, want) {
			t.Fatalf("%s: %v", c.target, jsonutil.Plain(got))
		}
		if token, _ := extra.Get("token"); token != "private-secret" {
			t.Fatalf("%s: original extra changed %v", c.target, token)
		}
	}
}
