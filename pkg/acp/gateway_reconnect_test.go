package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type pipeSocket struct {
	mu       sync.Mutex
	toClient chan string
	toOwner  chan string
	closed   chan struct{}
	once     sync.Once
}

func newPipeSocket() *pipeSocket {
	return &pipeSocket{toClient: make(chan string, 64), toOwner: make(chan string, 64), closed: make(chan struct{})}
}

func (p *pipeSocket) Recv() (string, error) {
	select {
	case frame := <-p.toClient:
		return frame, nil
	case <-p.closed:
		select {
		case frame := <-p.toClient:
			return frame, nil
		default:
		}
		return "", errors.New("connection closed")
	}
}

func (p *pipeSocket) Send(data string) error {
	select {
	case <-p.closed:
		return errors.New("connection closed")
	default:
	}
	select {
	case p.toOwner <- data:
		return nil
	case <-p.closed:
		return errors.New("connection closed")
	}
}

func (p *pipeSocket) Open() bool {
	select {
	case <-p.closed:
		return false
	default:
		return true
	}
}

func (p *pipeSocket) Subprotocol() string { return "" }

func (p *pipeSocket) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

type restartOwner struct {
	profileID, epoch, dieOnSubmit string
	mu                            sync.Mutex
	calls                         []string
	configs                       []map[string]any
}

func (o *restartOwner) methods() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *restartOwner) serve(sock *pipeSocket) {
	for {
		var raw string
		select {
		case raw = <-sock.toOwner:
		case <-sock.closed:
			return
		}
		decoded, err := pyjson.Loads(raw)
		if err != nil {
			return
		}
		frame := decoded.(*pyjson.Object)
		method := pyStr(frame.GetOr("method", nil))
		params, _ := frame.GetOr("params", nil).(*pyjson.Object)
		o.mu.Lock()
		o.calls = append(o.calls, method)
		if method == "config.set" {
			plain, _ := pyjson.Plain(params).(map[string]any)
			o.configs = append(o.configs, plain)
		}
		o.mu.Unlock()
		if method == "prompt.submit" && o.dieOnSubmit == "before_ack" {
			sock.Close()
			return
		}
		result := pyjson.NewObject()
		admission := ""
		switch method {
		case "runtime.describe":
			result.Set("profile_id", o.profileID)
		case "session.resume":
			result = pyjson.ObjectOf("session_id", params.GetOr("session_id", nil), "messages", []any{}, "pending", []any{}, "prompts", []any{},
				"replay_epoch", o.epoch, "last_sequence", int64(0))
		case "prompt.submit":
			admission = fmt.Sprintf("%s-%s", o.epoch, pyStr(params.GetOr("input_id", nil)))
			result.Set("admission_id", admission)
		}
		reply, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", frame.GetOr("id", nil), "result", result))
		sock.toClient <- reply
		if method == "prompt.submit" && o.dieOnSubmit == "after_ack" {
			time.Sleep(20 * time.Millisecond)
			sock.Close()
			return
		}
		if method == "prompt.submit" {
			event := pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", pyjson.ObjectOf(
				"session_id", params.GetOr("session_id", nil), "type", "message.complete", "replay_epoch", o.epoch, "seq", int64(1),
				"admission_id", admission, "payload", pyjson.ObjectOf("outcome", "completed", "text", "AFTER-RESTART")))
			text, _ := pyjson.Dumps(event)
			sock.toClient <- text
		}
	}
}

func TestEditorReconnectsToARestartedGatewayWithoutResending(t *testing.T) {
	for _, die := range []string{"before_ack", "after_ack"} {
		t.Run(die, func(t *testing.T) {
			a := newGatewayAgent(t)
			editor := &editorFake{}
			onLoop(a, func() bool { a.conn = editor; return true })
			owners := []*restartOwner{{profileID: a.home, epoch: "first", dieOnSubmit: die}, {profileID: a.home, epoch: "second"}}
			var mu sync.Mutex
			next := 0
			swap(t, &connectGateway, func(ctx context.Context) (gatewayLink, error) {
				mu.Lock()
				owner := owners[next]
				next++
				mu.Unlock()
				sock := newPipeSocket()
				go owner.serve(sock)
				return &clientLink{client: gateway.NewGatewayClient(sock).Start()}, nil
			})
			defer a.Close()
			held, _ := pyacp.WithResponseScope(context.Background())
			if _, err := a.LoadSession(held, &pyacp.LoadSessionRequest{Cwd: "/", SessionID: "s"}); err != nil {
				t.Fatal(err)
			}
			_, err := promptOn(a, context.Background(), "s", "in flight").wait(t, 5*time.Second)
			if err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
				t.Fatalf("err = %v", err)
			}
			resp, err := promptOn(a, context.Background(), "s", "after restart").wait(t, 5*time.Second)
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			if failure := onLoop(a, func() error { return a.failure }); failure != nil {
				t.Fatalf("failure = %v", failure)
			}
			if calls := owners[1].methods(); !reflect.DeepEqual(calls, []string{"runtime.describe", "session.resume", "config.set", "prompt.submit"}) {
				t.Fatalf("second owner calls = %v", calls)
			}
			if !reflect.DeepEqual(owners[1].configs, []map[string]any{{"session_id": "s", "key": "show_reasoning", "value": "1"}}) {
				t.Fatalf("configs = %v", owners[1].configs)
			}
			if countMethod(owners[0].methods(), "prompt.submit") != 1 {
				t.Fatalf("first owner calls = %v", owners[0].methods())
			}
			epoch := onLoop(a, func() any {
				snapshot, _ := a.snapshots.Get("s")
				return dget(snapshot, "replay_epoch")
			})
			if epoch != "second" {
				t.Fatalf("replay epoch = %v", epoch)
			}
			count := 0
			for _, text := range emittedText(editor) {
				if text == "AFTER-RESTART" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("emitted = %v", emittedText(editor))
			}
		})
	}
}

func TestARecoveredSnapshotRepaintsTheSessionsEditMode(t *testing.T) {
	for _, path := range []string{"reconnect", "gap"} {
		t.Run(path, func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": [], "replay_epoch": "old", "last_sequence": 0}`})
			editor := &editorFake{}
			onLoop(a, func() bool { a.conn = editor; a.modes["s"] = "default"; return true })
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method != "session.resume" && method != "config.set" {
					t.Errorf("unexpected %s", method)
				}
				if method == "session.resume" {
					return jsonObj(t, `{"session_id": "s", "pending": [], "prompts": [], "replay_epoch": "new", "last_sequence": 99, "info": {"edit_approval_policy": "session"}}`), nil
				}
				return pyjson.NewObject(), nil
			})
			defer a.Close()
			err := onLoop(a, func() error {
				if path == "reconnect" {
					return a.reattach(context.Background(), link)
				}
				_, err := a.gapGuard(context.Background(), "session.replay_gap", "s", "")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			var modes []string
			for _, u := range updatesOf(editor) {
				if update, ok := u.(*pyacp.CurrentModeUpdate); ok {
					modes = append(modes, update.CurrentModeID)
				}
			}
			if mode := onLoop(a, func() string { return a.modes["s"] }); mode != "dont_ask" || !reflect.DeepEqual(modes, []string{"dont_ask"}) {
				t.Fatalf("mode %q updates %v", mode, modes)
			}
		})
	}
}

func TestPathAndUnknownSlashTextIsSubmittedAsThePrompt(t *testing.T) {
	for _, text := range []string{"/etc/hosts is wrong", "/tmp/build.log shows the failure", "/notacommand please"} {
		t.Run(text, func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method != "prompt.submit" {
					t.Errorf("unexpected %s", method)
				}
				projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "a", "payload": {"outcome": "completed", "text": "ok"}}`)
				return jsonObj(t, `{"admission_id": "a"}`), nil
			})
			resp, err := promptOn(a, context.Background(), "s", text).wait(t, 5*time.Second)
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			calls := link.log()
			if got := calls[len(calls)-1].params["text"]; got != text {
				t.Fatalf("submitted text = %v", got)
			}
		})
	}
}

func TestRegisteredCommandTheOwnerRefusesIsANamedReplyNotAFailedPrompt(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	link.set(func(context.Context, string, map[string]any) (*pyjson.Object, error) {
		return nil, &gateway.GatewayRPCError{Msg: "unsupported_command"}
	})
	resp, err := promptOn(a, context.Background(), "s", "/rollback").wait(t, 5*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if calls := link.log(); !reflect.DeepEqual(calls, []rpcCall{{"slash.exec", map[string]any{"session_id": "s", "command": "/rollback"}}}) {
		t.Fatalf("calls = %v", calls)
	}
	if shown := lastAgentText(editor); !strings.Contains(shown, "/rollback is not available on the shared gateway") {
		t.Fatalf("shown = %q", shown)
	}
}

func retainedCount(t *testing.T, a *GatewayACPAgent) int {
	t.Helper()
	counter, ok := a.mutations.(interface{ Len() int })
	if !ok {
		t.Fatalf("mutations is %T", a.mutations)
	}
	return onLoop(a, counter.Len)
}

func TestPickerIdentityReachesPreparedMutation(t *testing.T) {
	cases := []struct {
		modelID  string
		expected map[string]any
	}{
		{"deepseek:deepseek-flash", map[string]any{"provider": "deepseek", "model": "deepseek-flash"}},
		{"openrouter:deepseek/example", map[string]any{"provider": "openrouter", "model": "deepseek/example"}},
		{"anthropic/claude-example:beta", map[string]any{"model": "anthropic/claude-example:beta"}},
		{"plain-model", map[string]any{"model": "plain-model"}},
		{"", map[string]any{"model": ""}},
		{"custom:fixture:local-model:tag", map[string]any{"provider": "custom:fixture", "model": "local-model:tag"}},
	}
	for _, tc := range cases {
		t.Run("model_"+tc.modelID, func(t *testing.T) {
			dir := testingHome(t)
			config.ResetCache()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("custom_providers:\n  - name: fixture\n    base_url: http://127.0.0.1:9/v1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			a := NewGatewayACPAgent()
			link := &scriptedGateway{}
			link.fakeGateway = newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method == "session.resume" {
					return jsonObj(t, `{"execution_generation": 3, "revision": 7}`), nil
				}
				if method != "session.mutate" {
					t.Errorf("unexpected %s", method)
				}
				return jsonObj(t, `{"status": "applied"}`), nil
			})
			onLoop(a, func() bool { a.gateway = link; a.aliases.Set("editor-tip", "logical-session"); return true })
			if _, err := a.SetSessionModel(context.Background(), &pyacp.SetSessionModelRequest{ModelID: tc.modelID, SessionID: "editor-tip"}); err != nil {
				t.Fatal(err)
			}
			var mutation map[string]any
			for _, call := range link.log() {
				if call.method == "session.mutate" {
					mutation = call.params
					break
				}
			}
			if got := pyjson.Plain(mutation["payload"]); !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("payload = %v", got)
			}
			if mutation["session_id"] != "logical-session" || mutation["expected_generation"] != int64(3) || mutation["expected_revision"] != int64(7) {
				t.Fatalf("mutation = %v", mutation)
			}
			if n := retainedCount(t, a); n != 0 {
				t.Fatalf("%d mutations retained", n)
			}
		})
	}
}

func TestPrefixedPickPreservesMutationFailureContract(t *testing.T) {
	for _, reason := range []string{"connection_lost", "session_busy"} {
		t.Run(reason, func(t *testing.T) {
			a := newGatewayAgent(t)
			var mu sync.Mutex
			var calls []map[string]any
			link := newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method == "session.resume" {
					return jsonObj(t, `{"execution_generation": 3, "revision": 7}`), nil
				}
				mu.Lock()
				calls = append(calls, params)
				first := len(calls) == 1
				mu.Unlock()
				if first {
					return nil, &gateway.GatewayClientError{Msg: reason}
				}
				return jsonObj(t, `{"status": "applied"}`), nil
			})
			onLoop(a, func() bool { a.gateway = link; return true })
			req := &pyacp.SetSessionModelRequest{ModelID: "deepseek:example", SessionID: "session"}
			if _, err := a.SetSessionModel(context.Background(), req); !refusedWith(err, reason) {
				t.Fatalf("err = %v", err)
			}
			if onLoop(a, func() bool { return a.snapshots.Has("session") }) {
				t.Fatal("a snapshot was adopted after the failure")
			}
			if _, err := a.SetSessionModel(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"provider": "deepseek", "model": "example"}
			if !reflect.DeepEqual(pyjson.Plain(calls[0]["payload"]), want) || !reflect.DeepEqual(pyjson.Plain(calls[1]["payload"]), want) {
				t.Fatalf("payloads = %v %v", calls[0]["payload"], calls[1]["payload"])
			}
			if same := calls[0]["request_id"] == calls[1]["request_id"]; same != (reason == "connection_lost") {
				t.Fatalf("request ids %v %v", calls[0]["request_id"], calls[1]["request_id"])
			}
			if n := retainedCount(t, a); n != 0 {
				t.Fatalf("%d mutations retained", n)
			}
		})
	}
}
