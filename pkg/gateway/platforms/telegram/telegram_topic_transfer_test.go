package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type fakeTransferHost struct {
	fn         TopicTransferFn
	authorized *bool
	authArgs   []string
}

func (h *fakeTransferHost) TopicTransferFn() TopicTransferFn { return h.fn }

func (h *fakeTransferHost) IsSenderAuthorized(userID string, chatType, chatID *string, _ gateway.AuthorizationOpts) *bool {
	h.authArgs = append(h.authArgs, userID, *chatType, *chatID)
	return h.authorized
}

func rootDMEvent(text string) *gateway.MessageEvent {
	user, msgID := "208214988", "4242"
	event := gateway.NewMessageEvent(text)
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "208214988", ChatType: "dm", UserID: &user, MessageID: &msgID}
	return event
}

func transferTo(threadID string, created bool, calls *int) TopicTransferFn {
	return func(context.Context, *gateway.SessionSource) (string, bool, bool, error) {
		*calls++
		return threadID, created, true, nil
	}
}

func TestRootDMTopicTransferMovesOntoTheHookTopic(t *testing.T) {
	calls := 0
	host := &fakeTransferHost{fn: transferTo("777", true, &calls)}
	event := rootDMEvent("plan the migration")
	ApplyRootDMTopicTransfer(context.Background(), host, event)
	if calls != 1 || *event.Source.ThreadID != "777" || event.Source.MessageID != nil || event.ReplyAnchorOverride == nil || *event.ReplyAnchorOverride != "" {
		t.Fatalf("created transfer = %+v override %v", event.Source, event.ReplyAnchorOverride)
	}
	if len(host.authArgs) != 3 || host.authArgs[0] != "208214988" || host.authArgs[1] != "dm" || host.authArgs[2] != "208214988" {
		t.Fatalf("auth args = %v", host.authArgs)
	}
	host.fn = transferTo("222", false, &calls)
	event = rootDMEvent("follow up")
	original := event.Source
	ApplyRootDMTopicTransfer(context.Background(), host, event)
	if *event.Source.ThreadID != "222" || event.Source.MessageID == nil || *event.Source.MessageID != "4242" || event.ReplyAnchorOverride != nil {
		t.Fatalf("lobby recovery = %+v", event.Source)
	}
	if original.ThreadID != nil || event.Source == original {
		t.Fatalf("source was edited in place")
	}
}

func TestRootDMTopicTransferLeavesIneligibleEventsAlone(t *testing.T) {
	calls := 0
	no := false
	cases := []struct {
		name  string
		host  *fakeTransferHost
		event func() *gateway.MessageEvent
	}{
		{"no hook", &fakeTransferHost{}, func() *gateway.MessageEvent { return rootDMEvent("hi") }},
		{"group", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent {
			e := rootDMEvent("hi")
			e.Source.ChatType = "group"
			return e
		}},
		{"no source", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent { return gateway.NewMessageEvent("hi") }},
		{"command", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent { return rootDMEvent("/new") }},
		{"plaintext restart", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent { return rootDMEvent("restart gateway") }},
		{"internal", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent {
			e := rootDMEvent("wake")
			e.Internal = true
			return e
		}},
		{"routed", &fakeTransferHost{fn: transferTo("1", true, &calls)}, func() *gateway.MessageEvent {
			e := rootDMEvent("hi")
			e.Metadata["gateway_session_key"] = " agent:main "
			return e
		}},
		{"unauthorized", &fakeTransferHost{fn: transferTo("1", true, &calls), authorized: &no}, func() *gateway.MessageEvent { return rootDMEvent("hi") }},
	}
	for _, c := range cases {
		event := c.event()
		before := event.Source
		ApplyRootDMTopicTransfer(context.Background(), c.host, event)
		if event.Source != before || event.ReplyAnchorOverride != nil {
			t.Fatalf("%s moved the event", c.name)
		}
	}
	if calls != 0 {
		t.Fatalf("hook ran %d times for ineligible events", calls)
	}
	restart := rootDMEvent("restart gateway")
	restart.AllowGatewayControl = false
	ApplyRootDMTopicTransfer(context.Background(), &fakeTransferHost{fn: transferTo("9", false, &calls)}, restart)
	if calls != 1 || *restart.Source.ThreadID != "9" {
		t.Fatalf("uncoerced restart text should transfer, calls %d", calls)
	}
}

func TestRootDMTopicTransferProceedsOnBlankRouteAndSwallowsHookFailures(t *testing.T) {
	calls := 0
	event := rootDMEvent("hi")
	event.Metadata["gateway_session_key"] = "   "
	ApplyRootDMTopicTransfer(context.Background(), &fakeTransferHost{fn: transferTo("5", false, &calls)}, event)
	if calls != 1 || *event.Source.ThreadID != "5" {
		t.Fatalf("blank route key blocked the transfer")
	}
	for _, fn := range []TopicTransferFn{
		func(context.Context, *gateway.SessionSource) (string, bool, bool, error) {
			return "", false, false, errors.New("boom")
		},
		func(context.Context, *gateway.SessionSource) (string, bool, bool, error) {
			return "", false, false, nil
		},
	} {
		event := rootDMEvent("hi")
		before := event.Source
		ApplyRootDMTopicTransfer(context.Background(), &fakeTransferHost{fn: fn}, event)
		if event.Source != before {
			t.Fatalf("failed or declined hook moved the event")
		}
	}
}
