package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type msgSessionKey struct{}
type msgLoopKey struct{}

// Telegram adapter double recording sends and edits, edit mode none, editing, raising or failed
type msgAdapter struct {
	mu    sync.Mutex
	mode  string
	calls []string
	gate  chan struct{}
}

type msgTarget struct {
	chat    string
	adapter *msgAdapter
}

func (a *msgAdapter) send(chat, text string) map[string]any {
	if a.gate != nil {
		<-a.gate
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "send "+chat+" "+text)
	return map[string]any{"ok": true, "message_id": fmt.Sprintf("m%d", len(a.calls))}
}

func (a *msgAdapter) record() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, "|")
}

// Installs a live gateway whose session 42 resolves to the adapter, as the Python tests patch the runner
func installMessageGateway(t *testing.T, a *msgAdapter, resolveErr error) {
	t.Helper()
	previous := installedServices()
	InstallServices(&Services{
		GatewayLoop: func(ctx context.Context) (any, bool) {
			on, _ := ctx.Value(msgLoopKey{}).(bool)
			return "runner", on
		},
		BoundSessionKey: func(ctx context.Context) string { k, _ := ctx.Value(msgSessionKey{}).(string); return k },
		BoundRoute:      func(context.Context) any { return nil },
		ResolvePluginMessageTarget: func(_ context.Context, runner any, key string, _ any, pluginHome, pluginID string) (any, map[string]any, error) {
			if resolveErr != nil {
				return nil, nil, resolveErr
			}
			if runner != "runner" || pluginID != "progress" || pluginHome == "" || key != "agent:main:telegram:dm:42" {
				return nil, messageErr("no_conversation", "session '"+key+"' is not known to this gateway"), nil
			}
			return &msgTarget{chat: "42", adapter: a}, nil, nil
		},
		PostPluginMessage: func(_ context.Context, target any, text, _ string) map[string]any {
			tg := target.(*msgTarget)
			return tg.adapter.send(tg.chat, text)
		},
		EditPluginMessage: func(_ context.Context, target, id any, text, _ string) map[string]any {
			tg := target.(*msgTarget)
			fallback := func(editErr *string) map[string]any {
				out := tg.adapter.send(tg.chat, text)
				out["edited"], out["fallback"] = false, true
				if editErr != nil {
					out["edit_error"] = *editErr
				}
				return out
			}
			switch tg.adapter.mode {
			case "editing":
				tg.adapter.mu.Lock()
				tg.adapter.calls = append(tg.adapter.calls, fmt.Sprintf("edit %s %v %s", tg.chat, id, text))
				tg.adapter.mu.Unlock()
				return map[string]any{"ok": true, "message_id": id, "edited": true}
			case "raising":
				e := "malformed edit response"
				return fallback(&e)
			case "failed":
				e := ""
				return fallback(&e)
			}
			return fallback(nil)
		},
	})
	t.Cleanup(func() { InstallServices(previous) })
}

func progressContext(t *testing.T) *Context {
	return newContext(&Manifest{Name: "progress", Key: "progress", Source: "user"}, NewManager(t.TempDir()))
}

func inSession(key string) context.Context {
	return context.WithValue(context.Background(), msgSessionKey{}, key)
}

// test_plugin_post_message.py test_post_then_edit_targets_the_calling_session, all four adapters
func TestPostThenEditTargetsTheCallingSession(t *testing.T) {
	for _, mode := range []string{"editing", "none", "raising", "failed"} {
		t.Run(mode, func(t *testing.T) {
			a := &msgAdapter{mode: mode}
			installMessageGateway(t, a, nil)
			c := progressContext(t)
			handle := c.PostMessage(inSession("agent:main:telegram:dm:42"), "step 1/2")
			if !handle.OK() || handle.MessageID() != "m1" {
				t.Fatalf("post = %v", handle)
			}
			result := handle.Edit(inSession(""), "step 2/2")
			edited := mode == "editing"
			if result["ok"] != true || result["edited"] != edited {
				t.Fatalf("edit = %v", result)
			}
			if edited {
				if a.record() != "send 42 step 1/2|edit 42 m1 step 2/2" {
					t.Fatalf("calls %s", a.record())
				}
				return
			}
			if a.record() != "send 42 step 1/2|send 42 step 2/2" || result["fallback"] != true || handle.MessageID() != "m2" {
				t.Fatalf("calls %s result %v handle %v", a.record(), result, handle)
			}
		})
	}
}

// test_plugin_post_message.py test_no_bound_session_is_refused_without_sending
func TestNoBoundSessionIsRefusedWithoutSending(t *testing.T) {
	a := &msgAdapter{mode: "editing"}
	installMessageGateway(t, a, nil)
	handle := progressContext(t).PostMessage(context.Background(), "from a timer")
	edit := handle.Edit(context.Background(), "still nothing")
	if handle.OK() || handle.ErrorCode() != "no_conversation" || edit["ok"] != false || edit["detail"] != noSessionDetail || a.record() != "" {
		t.Fatalf("handle %v edit %v calls %s", handle, edit, a.record())
	}
	if got := handle.String(); got != "<PluginMessage ok=False message_id=None error='no_conversation'>" {
		t.Fatalf("repr %s", got)
	}
}

// Unknown session, no gateway, the gateway loop itself and a raising resolve each refuse with a result
func TestPostMessageRefusals(t *testing.T) {
	a := &msgAdapter{mode: "editing"}
	installMessageGateway(t, a, nil)
	c := progressContext(t)
	handle := c.PostMessage(inSession("agent:main:telegram:dm:7"), "x")
	if handle.ErrorCode() != "no_conversation" || !strings.Contains(handle.Result()["detail"].(string), "not known") {
		t.Fatalf("unknown session %v", handle.Result())
	}
	if edit := handle.Edit(context.Background(), "y"); edit["error"] != "no_conversation" || !strings.Contains(edit["detail"].(string), "not known") {
		t.Fatalf("refused edit %v", edit)
	}
	onLoop := context.WithValue(inSession("agent:main:telegram:dm:42"), msgLoopKey{}, true)
	if h := c.PostMessage(onLoop, "x"); h.ErrorCode() != "on_event_loop" {
		t.Fatalf("on loop %v", h.Result())
	}
	installMessageGateway(t, a, errors.New("session store unreadable"))
	if h := c.PostMessage(inSession("agent:main:telegram:dm:42"), "x"); h.ErrorCode() != "send_failed" || h.Result()["detail"] != "session store unreadable" {
		t.Fatalf("raising resolve %v", h.Result())
	}
	InstallServices(&Services{})
	if h := c.PostMessage(inSession("agent:main:telegram:dm:42"), "x"); h.ErrorCode() != "no_conversation" {
		t.Fatalf("no binder %v", h.Result())
	}
	InstallServices(&Services{BoundSessionKey: func(context.Context) string { return "k" }})
	if h := c.PostMessage(context.Background(), "x"); h.ErrorCode() != "gateway_unavailable" {
		t.Fatalf("no gateway %v", h.Result())
	}
	if a.record() != "" {
		t.Fatalf("calls %s", a.record())
	}
}

// A post slower than the wait stays pending, lands on the handle, and the next edit waits behind it
func TestSlowPostStaysPendingAndOrdersTheEdit(t *testing.T) {
	previous := pluginMessageTimeout
	pluginMessageTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pluginMessageTimeout = previous })
	a := &msgAdapter{mode: "editing", gate: make(chan struct{})}
	installMessageGateway(t, a, nil)
	handle := progressContext(t).PostMessage(inSession("agent:main:telegram:dm:42"), "slow")
	if handle.ErrorCode() != "timeout" || !handle.Pending() || !strings.Contains(handle.Result()["detail"].(string), "within 0s") {
		t.Fatalf("timed out post %v pending %v", handle.Result(), handle.Pending())
	}
	if edit := handle.Edit(context.Background(), "next"); edit["error"] != "timeout" || edit["detail"] != "the previous operation on this message has not finished" {
		t.Fatalf("edit behind pending %v", edit)
	}
	close(a.gate)
	pluginMessageTimeout = 5 * time.Second
	edit := handle.Edit(context.Background(), "next")
	if edit["ok"] != true || edit["edited"] != true || handle.MessageID() != "m1" || handle.Pending() {
		t.Fatalf("edit after landing %v handle %v", edit, handle)
	}
	if a.record() != "send 42 slow|edit 42 m1 next" {
		t.Fatalf("calls %s", a.record())
	}
}

// Async forms on the gateway loop post and edit directly, the sync edit refusing there
func TestAsyncPostAndEditOnTheGatewayLoop(t *testing.T) {
	a := &msgAdapter{mode: "none"}
	installMessageGateway(t, a, nil)
	onLoop := context.WithValue(inSession("agent:main:telegram:dm:42"), msgLoopKey{}, true)
	handle, err := progressContext(t).APostMessage(onLoop, "a")
	if err != nil || !handle.OK() || handle.Pending() {
		t.Fatalf("apost %v %v", handle, err)
	}
	if edit := handle.Edit(onLoop, "b"); edit["error"] != "on_event_loop" {
		t.Fatalf("sync edit on loop %v", edit)
	}
	edit, err := handle.AEdit(onLoop, "b")
	if err != nil || edit["fallback"] != true || handle.MessageID() != "m2" {
		t.Fatalf("aedit %v %v", edit, err)
	}
	if got := handle.String(); got != "<PluginMessage ok=True message_id='m2' error=None>" {
		t.Fatalf("repr %s", got)
	}
}
