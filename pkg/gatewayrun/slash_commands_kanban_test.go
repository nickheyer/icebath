package gatewayrun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/kanban"
)

func TestSlashSubscriptionKeepsTheRoutedSourceOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(t.TempDir(), "kanban.db"))
	ctx := context.Background()
	r := allocGatewayRunner()
	r.kanbanNotifierProfile = "default"
	source := &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatID: "post", ChatType: "thread",
		ThreadID: kstr("post"), ScopeID: kstr("guild"), ParentChatID: kstr("parent"), Profile: kstr("yuki")}
	conn, err := kanban.Connect(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := kanban.CreateTask(ctx, conn, kanban.CreateTaskOptions{Title: "slash-created"})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := r.kanbanAutoSubscribe(ctx, &gateway.MessageEvent{Text: "/kanban create", Source: source}, task, nil)
	if err != nil || !ok {
		t.Fatalf("subscribe %v %v", ok, err)
	}
	conn, err = kanban.Connect(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	subs, err := kanban.ListNotifySubs(ctx, conn, &task, kanban.NotifyOwnerScope{})
	if err != nil || len(subs) == 0 {
		t.Fatalf("subs %v %v", subs, err)
	}
	sub := subs[0]
	if got := sub.GetOr("notifier_profile", nil); got != "yuki" {
		t.Fatalf("notifier_profile %v", got)
	}
	metadata, _ := sub.GetOr("delivery_metadata", nil).(map[string]any)
	if metadata == nil {
		if obj, isObj := sub.GetOr("delivery_metadata", nil).(interface{ GetOr(string, any) any }); isObj {
			metadata = map[string]any{"scope_id": obj.GetOr("scope_id", nil), "parent_chat_id": obj.GetOr("parent_chat_id", nil)}
		}
	}
	if metadata["scope_id"] != "guild" || metadata["parent_chat_id"] != "parent" {
		t.Fatalf("delivery_metadata %v", sub.GetOr("delivery_metadata", nil))
	}
}
