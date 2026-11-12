package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

const describeResult = `{"session_create":{"sources":["tui"],"parameters":["source","request_id"]}}`

// Local grant client started and subscribed, its first socket open
func startLocal(t *testing.T, boot *fakeBootstrap) (*gatewayClient, *fakeDialer, *fakeClock, *recorder, *fakeSocket) {
	t.Helper()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "")
	t.Setenv("ICEBATH_TUI_SIDECAR_URL", "")
	c, d, k := newTestClient(t, boot)
	rec := &recorder{}
	c.start()
	c.attach(rec.send)
	eventually(t, "the first socket", func() bool { return d.count() == 1 })
	s := d.at(0)
	s.open()
	return c, d, k, rec, s
}

// Answers the socket's runtime.describe with the JSON result
func describe(t *testing.T, s *fakeSocket, result string) {
	t.Helper()
	eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
	var body any
	_ = json.Unmarshal([]byte(result), &body)
	s.answer("runtime.describe", map[string]any{"result": body})
}

func TestGatewayClientWebsocketAttachMode(t *testing.T) {
	t.Run("publishes canonical readiness once after discovery and arms the negotiated heartbeat", func(t *testing.T) {
		c, _, k, rec, s := startLocal(t, nil)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{"heartbeat":true}}}`)
		eventually(t, "the capability advertisement", func() bool { return s.last("client.capabilities") != nil && s.last("runtime.describe") != nil })
		if len(rec.events("gateway.ready")) != 0 {
			t.Fatal("ready before discovery")
		}
		methods := s.methods()
		slices.Sort(methods)
		if !reflect.DeepEqual(methods, []string{"client.capabilities", "runtime.describe"}) {
			t.Fatalf("sent %v", methods)
		}
		describe(t, s, describeResult)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		k.advance(heartbeatInterval)
		if frames := s.sentFrames(); frames[len(frames)-1]["method"] != "gateway.ping" {
			t.Fatalf("last frame %v", frames[len(frames)-1])
		}
		c.kill("test")
		if n := k.pending(); n != 0 {
			t.Fatalf("timers left %d", n)
		}
	})
	t.Run("/logs carries the attach lifecycle like main's sidecar spawn line (row 24)", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "")
		c, d, _ := newTestClient(t, nil)
		c.bootstrap = func(context.Context, bool, bool) (localGrant, error) {
			return localGrant{URL: "ws://127.0.0.1:9/api/ws?token=secret", ProfileID: "/home/u/.icebath/profiles/work", InstanceID: "owner-1"}, nil
		}
		c.start()
		eventually(t, "the dial", func() bool { return d.count() == 1 })
		tail := c.logTail(20)
		if !strings.HasPrefix(tail, "[lifecycle] attached shared gateway ") && !strings.Contains(tail, "\n[lifecycle] attached shared gateway ") {
			t.Fatalf("tail %q", tail)
		}
		if !strings.Contains(tail, "profile=/home/u/.icebath/profiles/work") || !strings.Contains(tail, "instance=owner-1") || strings.Contains(tail, "secret") {
			t.Fatalf("tail %q", tail)
		}
		crash, err := os.ReadFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "logs", "tui_gateway_crash.log"))
		if err != nil || !strings.Contains(string(crash), "[tui-parent] ") || !strings.Contains(string(crash), "attached shared gateway start") {
			t.Fatalf("crash log %q %v", crash, err)
		}
	})
	t.Run("boots the owner's profile skin: runtime.describe.skin rides the client-local ready (row 3)", func(t *testing.T) {
		_, _, _, rec, s := startLocal(t, nil)
		describe(t, s, `{"session_create":{"sources":["tui"],"parameters":["source","request_id"]},"skin":{"name":"ares","colors":{"banner_title":"#C7A96B"},"branding":{"prompt_symbol":"⚔"}}}`)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		skin := rec.events("gateway.ready")[0].Payload["skin"]
		want := map[string]any{"name": "ares", "colors": map[string]any{"banner_title": "#C7A96B"}, "branding": map[string]any{"prompt_symbol": "⚔"}}
		if !reflect.DeepEqual(skin, want) {
			t.Fatalf("skin %#v", skin)
		}
	})
	t.Run("remembers clarify-batch-v1 from runtime.describe and forgets it on reconnect to an owner without it", func(t *testing.T) {
		c, d, k, rec, first := startLocal(t, nil)
		describe(t, first, `{"session_create":{"sources":["tui"],"parameters":["source","request_id"]},"capabilities":["clarify-batch-v1"]}`)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		if !c.clarifyBatch() {
			t.Fatal("capability not remembered")
		}
		first.closeWith(1000)
		eventually(t, "the reconnect timer", func() bool { return k.dueIn(reconnectBase) })
		k.advance(reconnectMax)
		eventually(t, "the second socket", func() bool { return d.count() == 2 })
		second := d.at(1)
		second.open()
		eventually(t, "the second describe", func() bool { return second.last("runtime.describe") != nil })
		if c.clarifyBatch() {
			t.Fatal("previous owner's capabilities outlived it")
		}
		describe(t, second, describeResult)
		eventually(t, "second readiness", func() bool { return len(rec.events("gateway.ready")) == 2 })
		if c.clarifyBatch() {
			t.Fatal("capability without advertisement")
		}
	})
	t.Run("treats a failed runtime.describe as a transport failure: redial, re-describe, then ready (B2-21)", func(t *testing.T) {
		c, d, k, rec, s := startLocal(t, nil)
		eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
		s.answer("runtime.describe", map[string]any{"error": map[string]any{"code": -32000, "message": "owner busy"}})
		eventually(t, "the closed socket", func() bool { return s.isClosed() && len(rec.events("gateway.reconnecting")) == 1 })
		created := goRequest(c, "session.create", map[string]any{"model": "m"})
		eventually(t, "the redial", func() bool { return d.count() == 2 })
		next := d.at(1)
		next.open()
		eventually(t, "the next describe", func() bool { return next.last("runtime.describe") != nil })
		for _, frame := range next.sentFrames() {
			if frame["method"] == "runtime.describe" {
				raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": map[string]any{
					"session_create": map[string]any{"sources": []string{"tui"}, "parameters": []string{"source", "request_id", "model"}}}})
				next.message(string(raw))
			}
		}
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		eventually(t, "the create", func() bool { return next.last("session.create") != nil })
		params := next.last("session.create")["params"].(map[string]any)
		if params["source"] != "tui" || params["model"] != "m" {
			t.Fatalf("create params %v", params)
		}
		next.answer("session.create", map[string]any{"result": map[string]any{"session_id": "fresh", "info": map[string]any{}}})
		value, err := created.wait(t)
		if err != nil || value.(map[string]any)["session_id"] != "fresh" {
			t.Fatalf("created %v %v", value, err)
		}
		if ev := rec.events("gateway.start_timeout"); len(ev) != 1 || ev[0].Payload["python"] != "runtime.describe" || ev[0].Payload["stderr_tail"] != "JsonRpcGatewayError: owner busy" {
			t.Fatalf("start timeout %#v", ev)
		}
		_ = k
	})
	t.Run("keeps discovery-only retries alive and delivers readiness to the mounted subscriber", func(t *testing.T) {
		boot := &fakeBootstrap{outcomes: []error{nil, errors.New("owner stopped"), errors.New("owner stopped"), nil}}
		_, d, k, rec, s := startLocal(t, boot)
		eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
		s.closeWith(1006)
		for _, delay := range []time.Duration{reconnectBase, 2 * reconnectBase, 4 * reconnectBase} {
			eventually(t, "the reconnect timer", func() bool { return k.dueIn(delay) })
			k.advance(delay)
		}
		eventually(t, "the redial", func() bool { return d.count() == 2 })
		calls := boot.callList()
		starts := make([]bool, len(calls))
		for i, call := range calls {
			starts[i] = call[0]
		}
		if !reflect.DeepEqual(starts, []bool{true, false, false, false}) {
			t.Fatalf("bootstrap calls %v", calls)
		}
		last := d.at(-1)
		last.open()
		describe(t, last, `{"session_create":{"sources":["tui"],"parameters":[]}}`)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
	})
	t.Run("pins the launcher tool-progress mode (icebath --tui -v) on the created session before returning it", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_TOOL_PROGRESS", "verbose")
		c, _, _, _, s := startLocal(t, nil)
		describe(t, s, describeResult)
		created := goRequest(c, "session.create", map[string]any{})
		eventually(t, "the create", func() bool { return s.last("session.create") != nil })
		s.answer("session.create", map[string]any{"result": map[string]any{"session_id": "fresh", "info": map[string]any{}}})
		eventually(t, "the pin", func() bool { return s.last("config.set") != nil })
		if created.settled() {
			t.Fatal("create settled before its pin")
		}
		pin := s.last("config.set")["params"].(map[string]any)
		if pin["key"] != "verbose" || pin["session_id"] != "fresh" || pin["value"] != "verbose" {
			t.Fatalf("pin %v", pin)
		}
		s.answer("config.set", map[string]any{"result": map[string]any{"key": "verbose", "value": "verbose", "scope": "session"}})
		value, err := created.wait(t)
		if err != nil || value.(map[string]any)["session_id"] != "fresh" {
			t.Fatalf("created %v %v", value, err)
		}
	})
	t.Run("retries a create whose launch pin failed after the ACK under the same request_id (one session, pin reapplied)", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_TOOL_PROGRESS", "verbose")
		c, _, _, _, s := startLocal(t, nil)
		minted := map[string]string{}
		var order []string
		ack := func() {
			requestID := s.last("session.create")["params"].(map[string]any)["request_id"].(string)
			if _, has := minted[requestID]; !has {
				minted[requestID] = "orphan-" + string(rune('0'+len(minted)+1))
				order = append(order, minted[requestID])
			}
			s.answer("session.create", map[string]any{"result": map[string]any{"session_id": minted[requestID], "info": map[string]any{}}})
		}
		describe(t, s, `{"session_create":{"sources":["tui"],"parameters":["source","request_id","model"]}}`)
		first := goRequest(c, "session.create", map[string]any{"request_id": "attempt-1", "model": "m"})
		eventually(t, "the first create", func() bool { return s.last("session.create") != nil })
		ack()
		eventually(t, "the first pin", func() bool { return s.last("config.set") != nil })
		s.answer("config.set", map[string]any{"error": map[string]any{"code": -32000, "message": "config refused"}})
		if _, err := first.wait(t); err == nil || err.Error() != "config refused" {
			t.Fatalf("first %v", err)
		}
		retry := goRequest(c, "session.create", map[string]any{"request_id": "attempt-2", "model": "m"})
		eventually(t, "the retried create", func() bool {
			return len(slices.DeleteFunc(s.methods(), func(m string) bool { return m != "session.create" })) == 2
		})
		ack()
		eventually(t, "the second pin", func() bool {
			return len(slices.DeleteFunc(s.methods(), func(m string) bool { return m != "config.set" })) == 2
		})
		pin := s.last("config.set")["params"].(map[string]any)
		if pin["key"] != "verbose" || pin["session_id"] != "orphan-1" || pin["value"] != "verbose" {
			t.Fatalf("pin %v", pin)
		}
		s.answer("config.set", map[string]any{"result": map[string]any{"key": "verbose", "value": "verbose", "scope": "session"}})
		value, err := retry.wait(t)
		if err != nil || value.(map[string]any)["session_id"] != "orphan-1" || !reflect.DeepEqual(order, []string{"orphan-1"}) {
			t.Fatalf("retry %v %v minted %v", value, err, order)
		}
	})
	t.Run("re-ensures a crashed owner on a bounded number of reconnects, then stays discovery-only", func(t *testing.T) {
		boot := &fakeBootstrap{outcomes: []error{nil, errors.New("gateway absent: not ready")}}
		_, _, k, _, s := startLocal(t, boot)
		s.closeWith(1006)
		for i, delay := range []time.Duration{1, 2, 4, 8, 16} {
			eventually(t, "the reconnect timer", func() bool { return k.dueIn(delay * reconnectBase) })
			k.advance(delay * reconnectBase)
			eventually(t, "the bootstrap", func() bool { return len(boot.callList()) == i+2 })
		}
		want := [][2]bool{{true, false}, {false, true}, {false, true}, {false, true}, {false, false}, {false, false}}
		if got := boot.callList(); !reflect.DeepEqual(got, want) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("negotiates the canonical wire on an explicit attach whose listener advertises a session authority", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		rec := &recorder{}
		c.attach(rec.send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{"skin":{"name":"mono"},"session_authority":true}}}`)
		eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
		if len(rec.events("gateway.ready")) != 0 {
			t.Fatal("ready before discovery")
		}
		describe(t, s, `{"session_create":{"sources":["tui"],"parameters":["source","request_id","model"]}}`)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		if skin := rec.events("gateway.ready")[0].Payload["skin"]; !reflect.DeepEqual(skin, map[string]any{"name": "mono"}) {
			t.Fatalf("skin %v", skin)
		}
		c.mu.Lock()
		canonical := c.isCanonical
		c.mu.Unlock()
		if !canonical {
			t.Fatal("not canonical")
		}
		created := goRequest(c, "session.create", map[string]any{"cols": 80, "model": "m"})
		eventually(t, "the create", func() bool { return s.last("session.create") != nil })
		create := s.last("session.create")
		params := create["params"].(map[string]any)
		if _, has := params["cols"]; has || params["source"] != "tui" || params["model"] != "m" {
			t.Fatalf("params %v", params)
		}
		s.answer("session.create", map[string]any{"result": map[string]any{"session_id": "fresh", "info": map[string]any{}}})
		if value, err := created.wait(t); err != nil || value.(map[string]any)["session_id"] != "fresh" {
			t.Fatalf("created %v %v", value, err)
		}
	})
	t.Run("waits for websocket open and resolves RPC requests", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws?token=abc")
		c, d, _ := newTestClient(t, nil)
		c.start()
		eventually(t, "the socket", func() bool { return d.count() == 1 })
		s := d.at(0)
		req := goRequest(c, "session.create", map[string]any{"cols": 80})
		time.Sleep(10 * time.Millisecond)
		if len(s.sentRaw()) != 0 {
			t.Fatal("sent before open")
		}
		s.open()
		eventually(t, "the request", func() bool { return len(s.sentRaw()) == 1 })
		if s.sentFrames()[0]["method"] != "session.create" {
			t.Fatalf("frame %v", s.sentFrames()[0])
		}
		s.answer("session.create", map[string]any{"result": map[string]any{"ok": true}})
		if value, err := req.wait(t); err != nil || !reflect.DeepEqual(value, map[string]any{"ok": true}) {
			t.Fatalf("value %v %v", value, err)
		}
	})
	t.Run("drains buffered events on a later tick, not synchronously inside drain()", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{}}}`)
		eventually(t, "the buffered ready", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.backlog) == 1 })
		rec := &recorder{}
		order := []string{}
		var mu = &rec.mu
		rec.hook = func(m tea.Msg) {
			if ev, isEvent := m.(gatewayEventMsg); isEvent {
				mu.Lock()
				order = append(order, "event:"+ev.Type)
				mu.Unlock()
			}
		}
		c.attach(rec.send)
		mu.Lock()
		order = append(order, "after-drain")
		mu.Unlock()
		eventually(t, "the replay", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 })
		if !reflect.DeepEqual(order, []string{"after-drain", "event:gateway.ready"}) {
			t.Fatalf("order %v", order)
		}
	})
	t.Run("preserves FIFO order when a live event arrives before the deferred flush", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{}}}`)
		eventually(t, "the buffered ready", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.backlog) == 1 })
		rec := &recorder{}
		c.attach(rec.send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"session.info","payload":{}}}`)
		eventually(t, "both events", func() bool { return len(rec.events("")) == 2 })
		evs := rec.events("")
		if evs[0].Type != "gateway.ready" || evs[1].Type != "session.info" {
			t.Fatalf("order %v %v", evs[0].Type, evs[1].Type)
		}
	})
	t.Run("mirrors event frames to sidecar websocket when configured", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws?token=abc")
		t.Setenv("ICEBATH_TUI_SIDECAR_URL", "ws://gateway.test/api/pub?token=abc&channel=demo")
		c, d, _ := newTestClient(t, nil)
		rec := &recorder{}
		c.start()
		eventually(t, "the socket", func() bool { return d.count() == 1 })
		d.at(0).open()
		eventually(t, "the sidecar", func() bool { return d.count() == 2 })
		sidecar := d.at(1)
		sidecar.open()
		eventually(t, "the sidecar link", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sidecar != nil })
		c.attach(rec.send)
		frame := `{"jsonrpc":"2.0","method":"event","params":{"type":"tool.start","payload":{"tool_id":"t1"}}}`
		d.at(0).message(frame)
		eventually(t, "the event", func() bool { return len(rec.events("tool.start")) == 1 })
		if !slices.Contains(sidecar.sentRaw(), frame) {
			t.Fatalf("mirrored %q", sidecar.sentRaw())
		}
	})
	t.Run("publishes local dashboard-control events to the sidecar websocket", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws?token=abc")
		t.Setenv("ICEBATH_TUI_SIDECAR_URL", "ws://gateway.test/api/pub?token=abc&channel=demo")
		c, d, _ := newTestClient(t, nil)
		rec := &recorder{}
		c.start()
		eventually(t, "the socket", func() bool { return d.count() == 1 })
		d.at(0).open()
		eventually(t, "the sidecar", func() bool { return d.count() == 2 })
		sidecar := d.at(1)
		sidecar.open()
		eventually(t, "the sidecar link", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sidecar != nil })
		c.attach(rec.send)
		c.publishLocalEvent(gatewayEventMsg{Type: "dashboard.new_session_requested", SessionID: "sid-old", Payload: map[string]any{"reason": "idle_exit_hotkey"}})
		eventually(t, "the event", func() bool { return len(rec.events("dashboard.new_session_requested")) == 1 })
		sent := sidecar.sentFrames()
		want := map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
			"payload": map[string]any{"reason": "idle_exit_hotkey"}, "session_id": "sid-old", "type": "dashboard.new_session_requested"}}
		if len(sent) == 0 || !reflect.DeepEqual(sent[len(sent)-1], want) {
			t.Fatalf("sidecar %v", sent)
		}
	})
	t.Run("emits exit when attached websocket closes", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		rec := &recorder{}
		c.attach(rec.send)
		s.closeWith(1011)
		eventually(t, "the exit", func() bool { return len(rec.exits()) == 1 })
		if !reflect.DeepEqual(rec.exits(), []int{1011}) {
			t.Fatalf("exits %v", rec.exits())
		}
	})
	t.Run("rejects pending RPCs when the attached socket closes", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		req := goRequest(c, "session.create", map[string]any{})
		eventually(t, "the request", func() bool { return len(s.sentRaw()) > 0 })
		s.closeWith(1011)
		if _, err := req.wait(t); err == nil || err.Error() != tr("libText.gateway.websocketClosedWithCode", 1011) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("rejects pending RPCs when kill() closes the attached websocket", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		req := goRequest(c, "session.create", map[string]any{})
		eventually(t, "the request", func() bool { return len(s.sentRaw()) > 0 })
		c.kill("test.shutdown")
		if _, err := req.wait(t); err == nil || err.Error() != tr("libText.gateway.closed") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("reattaches when ICEBATH_TUI_GATEWAY_URL rotates between requests", func(t *testing.T) {
		c, d, _, first := openExplicit(t, "ws://gateway-old.test/api/ws?token=abc")
		stale := goRequest(c, "session.create", map[string]any{})
		eventually(t, "the stale request", func() bool { return len(first.sentRaw()) > 0 })
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway-new.test/api/ws?token=xyz")
		next := goRequest(c, "session.create", map[string]any{})
		if _, err := stale.wait(t); err == nil || err.Error() != tr("libText.gateway.attachUrlChanged") {
			t.Fatalf("stale %v", err)
		}
		eventually(t, "the second socket", func() bool { return d.count() == 2 })
		second := d.at(1)
		if !strings.Contains(second.url, "gateway-new.test") {
			t.Fatalf("url %s", second.url)
		}
		second.open()
		eventually(t, "the next request", func() bool { return len(second.sentRaw()) > 0 })
		second.answer("session.create", map[string]any{"result": map[string]any{"ok": true}})
		if value, err := next.wait(t); err != nil || !reflect.DeepEqual(value, map[string]any{"ok": true}) {
			t.Fatalf("next %v %v", value, err)
		}
	})
	t.Run("surfaces JSON-RPC error code and data to callers (shared error mapping)", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		req := goRequest(c, "projects.create", map[string]any{})
		eventually(t, "the request", func() bool { return len(s.sentRaw()) > 0 })
		s.answer("projects.create", map[string]any{"error": map[string]any{"code": -32601, "data": map[string]any{"method": "projects.create"}, "message": "unknown method: projects.create"}})
		_, err := req.wait(t)
		var refusal *rpcError
		if !errors.As(err, &refusal) || refusal.Code != -32601 || refusal.Message != "unknown method: projects.create" || refusal.Data["method"] != "projects.create" {
			t.Fatalf("err %#v", err)
		}
	})
	t.Run("dials the attach URL verbatim, the WebSocket every Go build carries", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws?token=hunter2&channel=secret")
		c, d, _ := newTestClient(t, nil)
		c.start()
		eventually(t, "the socket", func() bool { return d.count() == 1 })
		if d.at(0).url != "ws://gateway.test/api/ws?token=hunter2&channel=secret" {
			t.Fatalf("url %s", d.at(0).url)
		}
	})
	t.Run("redacts attach URL secrets when the WebSocket constructor throws", func(t *testing.T) {
		secretURL := "ws://gateway.test:99999/api/ws?token=hunter2&channel=secret"
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", secretURL)
		c, d, _ := newTestClient(t, nil)
		c.start()
		c.attach((&recorder{}).send)
		tail := c.logTail(20)
		if d.count() != 0 || strings.Contains(tail, "hunter2") || strings.Contains(tail, "channel=secret") || strings.Contains(tail, secretURL) || !strings.Contains(tail, "ws://gateway.test:99999/api/ws?***") {
			t.Fatalf("tail %q", tail)
		}
	})
	t.Run("redacts sidecar URL secrets when the WebSocket constructor throws", func(t *testing.T) {
		sidecarURL := "ws://gateway.test:99999/api/pub?token=hunter2&channel=secret"
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://gateway.test/api/ws?token=abc")
		t.Setenv("ICEBATH_TUI_SIDECAR_URL", sidecarURL)
		c, d, _ := newTestClient(t, nil)
		c.start()
		eventually(t, "the socket", func() bool { return d.count() == 1 })
		d.at(0).open()
		eventually(t, "the sidecar failure", func() bool { return strings.Contains(c.logTail(20), "[sidecar] failed to connect") })
		tail := c.logTail(20)
		if strings.Contains(tail, "hunter2") || strings.Contains(tail, "channel=secret") || strings.Contains(tail, sidecarURL) || !strings.Contains(tail, "ws://gateway.test:99999/api/pub?***") {
			t.Fatalf("tail %q", tail)
		}
	})
	t.Run("redacts user-info credentials even on URLs the WHATWG parser rejects", func(t *testing.T) {
		fixture := "ws://alice:hunter2@gateway.test:99999/api/ws?token=secret"
		if _, ok := parseWHATWG(fixture); ok {
			t.Fatal("fixture parsed")
		}
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", fixture)
		c, _, _ := newTestClient(t, nil)
		c.start()
		c.attach((&recorder{}).send)
		tail := c.logTail(20)
		if strings.Contains(tail, "alice") || strings.Contains(tail, "hunter2") || strings.Contains(tail, "token=secret") {
			t.Fatalf("tail %q", tail)
		}
	})
	t.Run("keeps a healthy idle websocket open when heartbeat acknowledgements arrive (issue #32997)", func(t *testing.T) {
		c, d, k, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		c.attach((&recorder{}).send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{"heartbeat":true}}}`)
		eventually(t, "the heartbeat", func() bool { return k.dueIn(heartbeatInterval) && s.last("client.capabilities") != nil })
		var acked []string
		ackPings := func() {
			for _, frame := range s.sentFrames() {
				id, _ := frame["id"].(string)
				if frame["method"] == "gateway.ping" && !slices.Contains(acked, id) {
					acked = append(acked, id)
					s.message(`{"id":"` + id + `","jsonrpc":"2.0","result":{"ok":true}}`)
				}
			}
			eventually(t, "the acks", s.drained)
		}
		k.advance(heartbeatInterval)
		if frames := s.sentFrames(); frames[len(frames)-1]["method"] != "gateway.ping" {
			t.Fatalf("last %v", frames[len(frames)-1])
		}
		for elapsed := time.Duration(0); elapsed < 2*heartbeatDeadline; elapsed += heartbeatInterval {
			ackPings()
			k.advance(heartbeatInterval)
		}
		if len(acked) <= 2 || s.isClosed() || d.count() != 1 {
			t.Fatalf("acked %d closed %v sockets %d", len(acked), s.isClosed(), d.count())
		}
	})
	t.Run("auto-reconnects after a missing heartbeat acknowledgement (issue #32997)", func(t *testing.T) {
		c, d, k, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		c.attach((&recorder{}).send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{"heartbeat":true}}}`)
		eventually(t, "the heartbeat", func() bool { return k.dueIn(heartbeatInterval) && s.last("client.capabilities") != nil })
		k.advance(heartbeatInterval)
		if frames := s.sentFrames(); frames[len(frames)-1]["method"] != "gateway.ping" {
			t.Fatalf("last %v", frames[len(frames)-1])
		}
		k.advance(heartbeatDeadline + heartbeatInterval)
		eventually(t, "the dropped socket", s.isClosed)
		k.advance(reconnectBase)
		eventually(t, "the redial", func() bool { return d.count() >= 2 })
	})
	t.Run("does not heartbeat an older backend that omits the capability", func(t *testing.T) {
		c, d, k, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		c.attach((&recorder{}).send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{}}}`)
		eventually(t, "the ready", s.drained)
		k.advance(heartbeatDeadline + heartbeatInterval)
		if s.isClosed() || slices.Contains(s.methods(), "gateway.ping") || d.count() != 1 {
			t.Fatalf("closed %v methods %v sockets %d", s.isClosed(), s.methods(), d.count())
		}
	})
	t.Run("does not double-reconnect when the exit subscriber restarts immediately", func(t *testing.T) {
		c, d, k, first := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		rec := &recorder{}
		rec.hook = func(m tea.Msg) {
			if _, isExit := m.(gatewayExitMsg); isExit {
				c.start()
			}
		}
		c.attach(rec.send)
		first.closeWith(1011)
		eventually(t, "the restart", func() bool { return d.count() == 2 })
		k.advance(reconnectBase)
		if d.count() != 2 {
			t.Fatalf("sockets %d", d.count())
		}
	})
	t.Run("keeps delivering events to the mounted subscriber across reconnects, with growing backoff (#111594)", func(t *testing.T) {
		c, d, k, first := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		rec := &recorder{}
		var ready []int
		rec.hook = func(m tea.Msg) {
			if ev, isEvent := m.(gatewayEventMsg); isEvent && ev.Type == "gateway.ready" {
				ready = append(ready, d.count())
			}
		}
		c.attach(rec.send)
		readyFrame := `{"jsonrpc":"2.0","method":"event","params":{"payload":{},"type":"gateway.ready"}}`
		first.message(readyFrame)
		eventually(t, "readiness", func() bool { return len(rec.events("gateway.ready")) == 1 })
		first.closeWith(1006)
		for i := 1; i <= 3; i++ {
			eventually(t, "the reconnect timer", func() bool { return len(rec.events("gateway.reconnecting")) == i })
			k.advance(reconnectMax)
			eventually(t, "the redial", func() bool { return d.count() == i+1 })
			if i < 3 {
				d.at(-1).closeWith(1006)
			}
		}
		var delays []float64
		for _, ev := range rec.events("gateway.reconnecting") {
			delays = append(delays, ev.Payload["delay_ms"].(float64))
		}
		if len(delays) != 3 || delays[2] <= delays[0] {
			t.Fatalf("delays %v", delays)
		}
		last := d.at(-1)
		last.open()
		last.message(readyFrame)
		eventually(t, "second readiness", func() bool { return len(rec.events("gateway.ready")) == 2 })
		if !reflect.DeepEqual(ready, []int{1, d.count()}) {
			t.Fatalf("ready %v", ready)
		}
	})
	t.Run("does not auto-reconnect after an intentional kill() (issue #32997)", func(t *testing.T) {
		c, d, k, _ := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		c.kill("test")
		k.advance(heartbeatDeadline + reconnectMax + time.Second)
		time.Sleep(10 * time.Millisecond)
		if d.count() != 1 {
			t.Fatalf("sockets %d", d.count())
		}
	})
}

func TestGatewayClientHeartbeatLiveness(t *testing.T) {
	t.Run("constructs the channel with heartbeatLiveness any-inbound", func(t *testing.T) {
		c, _, k, s := openExplicit(t, "ws://gateway.test/api/ws?token=abc")
		c.attach((&recorder{}).send)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"gateway.ready","payload":{"heartbeat":true}}}`)
		eventually(t, "the heartbeat", func() bool { return k.dueIn(heartbeatInterval) })
		for i := 0; i < 6; i++ {
			k.advance(heartbeatInterval)
			s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"message.delta","payload":{}}}`)
			eventually(t, "the frame", s.drained)
		}
		if s.isClosed() {
			t.Fatal("a streaming socket that never pongs was dropped")
		}
	})
}

func TestGatewayClientKillLatch(t *testing.T) {
	t.Run("a killed transport late close does not respawn a replacement gateway", func(t *testing.T) {
		c, d, k, rec, s := startLocal(t, nil)
		rec.hook = func(m tea.Msg) {
			if _, isExit := m.(gatewayExitMsg); isExit && len(rec.exits()) == 1 {
				c.start()
			}
		}
		eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
		c.kill("graceful-exit-cleanup")
		s.closeWith(1006)
		k.advance(time.Minute)
		time.Sleep(10 * time.Millisecond)
		if len(rec.exits()) != 0 || d.count() != 1 || k.pending() != 0 {
			t.Fatalf("exits %v sockets %d timers %d", rec.exits(), d.count(), k.pending())
		}
	})
	t.Run("start() after kill() is refused even when called directly", func(t *testing.T) {
		c, d, k, _, _ := startLocal(t, nil)
		c.kill("app.die")
		c.start()
		k.advance(0)
		if d.count() != 1 {
			t.Fatalf("sockets %d", d.count())
		}
	})
	t.Run("an unexpected transport close still respawns through the recovery subscriber", func(t *testing.T) {
		c, d, _, rec, s := startLocal(t, nil)
		rec.hook = func(m tea.Msg) {
			if _, isExit := m.(gatewayExitMsg); isExit && len(rec.exits()) == 1 {
				c.start()
			}
		}
		eventually(t, "runtime.describe", func() bool { return s.last("runtime.describe") != nil })
		s.closeWith(1011)
		eventually(t, "the respawn", func() bool { return d.count() == 2 })
		if !reflect.DeepEqual(rec.exits(), []int{1011}) {
			t.Fatalf("exits %v", rec.exits())
		}
	})
}

func TestGatewayLocalLaunch(t *testing.T) {
	t.Run("reports failed canonical bootstrap without creating an independent owner or retrying ensure", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "")
		boot := &fakeBootstrap{outcomes: []error{errors.New("gateway draining: update_paused")}}
		c, d, _ := newTestClient(t, boot)
		rec := &recorder{}
		c.start()
		c.attach(rec.send)
		eventually(t, "the start timeout", func() bool {
			for _, ev := range rec.events("gateway.start_timeout") {
				if strings.Contains(ev.Payload["stderr_tail"].(string), "update_paused") {
					return true
				}
			}
			return false
		})
		if _, err := c.request(context.Background(), "session.create", nil, 0); err == nil || !strings.Contains(err.Error(), "update_paused") {
			t.Fatalf("err %v", err)
		}
		if len(boot.callList()) != 1 || d.count() != 0 {
			t.Fatalf("calls %v sockets %d", boot.callList(), d.count())
		}
	})
	t.Run("reports a bootstrap that cannot load Icebath once and stops retrying (#138878)", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_GATEWAY_URL", "")
		boot := &fakeBootstrap{outcomes: []error{errors.New("gateway unavailable: bootstrap_dependencies\nNo module named 'ruamel'")}}
		c, _, k := newTestClient(t, boot)
		rec := &recorder{}
		c.start()
		c.attach(rec.send)
		eventually(t, "the start timeout", func() bool { return len(rec.events("gateway.start_timeout")) == 1 })
		k.advance(5 * time.Minute)
		if len(boot.callList()) != 1 || len(rec.events("gateway.start_timeout")) != 1 || len(rec.events("gateway.reconnecting")) != 0 {
			t.Fatalf("calls %d events %v", len(boot.callList()), rec.events(""))
		}
		if !strings.Contains(c.logTail(5), "not retrying: gateway unavailable: bootstrap_dependencies") {
			t.Fatalf("logs %q", c.logTail(5))
		}
	})
}
