package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type raisingDeliveryAdapter struct {
	message   string
	failTimes int
	calls     []string
}

func (a *raisingDeliveryAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (SendResult, error) {
	a.calls = append(a.calls, chatID)
	if len(a.calls) <= a.failTimes {
		return SendResult{}, errors.New(a.message)
	}
	return SendResult{Success: true}, nil
}

func TestDeadTargetMarkIsDeadClearRoundtrip(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	reg := NewDeadTargetRegistry(context.Background(), "")
	if reg.IsDead("telegram", "123") || !reg.MarkDead("telegram", "123", "forbidden") || !reg.IsDead("telegram", "123") {
		t.Fatal("mark")
	}
	if reg.MarkDead("telegram", "123", "forbidden") {
		t.Fatal("second mark must report already present")
	}
	if !reg.Clear("telegram", "123") || reg.IsDead("telegram", "123") {
		t.Fatal("clear")
	}
}

func TestDeadTargetPersistsAcrossInstances(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	NewDeadTargetRegistry(ctx, "").MarkDead("telegram", "999", "deleted group")
	if !NewDeadTargetRegistry(ctx, "").IsDead("telegram", "999") {
		t.Fatal("a new instance must read the on disk store")
	}
}

func TestForbiddenMarksTargetDeadThenShortCircuits(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	adapter := &raisingDeliveryAdapter{message: "Forbidden: the group chat was deleted", failTimes: 99}
	router := NewDeliveryRouterIn(ctx, deliveryConfig(), map[string]any{"telegram": adapter}, nil)
	target := ParseDeliveryTarget(ctx, "telegram:42", nil)
	first := router.Deliver(ctx, "hi", []DeliveryTarget{target}, "", "", nil)
	if first["telegram:42"]["success"] != false || !router.DeadTargets.IsDead("telegram", "42") || !reflect.DeepEqual(adapter.calls, []string{"42"}) {
		t.Fatalf("first %v %v", first, adapter.calls)
	}
	second := router.Deliver(ctx, "hi again", []DeliveryTarget{target}, "", "", nil)
	if second["telegram:42"]["skipped"] != "dead_target" || second["telegram:42"]["success"] != false || !reflect.DeepEqual(adapter.calls, []string{"42"}) {
		t.Fatalf("second %v %v", second, adapter.calls)
	}
}

func TestSharedDeadRegistryIsUsedWhenInjected(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	shared := NewDeadTargetRegistry(ctx, "")
	shared.MarkDead("telegram", "500", "pre-existing")
	adapter := &raisingDeliveryAdapter{}
	router := NewDeliveryRouterIn(ctx, deliveryConfig(), map[string]any{"telegram": adapter}, shared)
	res := router.Deliver(ctx, "hi", []DeliveryTarget{ParseDeliveryTarget(ctx, "telegram:500", nil)}, "", "", nil)
	if res["telegram:500"]["skipped"] != "dead_target" || len(adapter.calls) != 0 {
		t.Fatalf("%v %v", res, adapter.calls)
	}
}

func TestNotFoundBlastRadius(t *testing.T) {
	for _, message := range []string{"Bad Request: message thread not found", "Bad Request: TOPIC_DELETED", "Bad Request: message to edit not found",
		"Bad Request: message to reply not found", "Bad Request: MESSAGE_ID_INVALID"} {
		if IsChatLevelNotFound(nil, message) {
			t.Fatalf("%s is not chat level", message)
		}
	}
	if IsChatLevelNotFound(nil, "chat not found; message thread not found") {
		t.Fatal("a sub chat marker must win when both are present")
	}
}
