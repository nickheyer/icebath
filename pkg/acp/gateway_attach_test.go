package acp

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var pngFixture = append([]byte("\x89PNG\r\n\x1a\n"), []byte(strings.Repeat("0", 32))...)

func snapshotPeer(t *testing.T, snapshots ...string) func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
	var mu sync.Mutex
	return func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "session.resume":
			if len(snapshots) == 0 {
				t.Errorf("no snapshot left for session.resume")
				return pyjson.NewObject(), nil
			}
			next := snapshots[0]
			snapshots = snapshots[1:]
			return jsonObj(t, next), nil
		case "prompt.resolve_unknown":
			return jsonObj(t, `{"status": "terminal", "outcome": "interrupted"}`), nil
		}
		t.Errorf("unexpected %s %v", method, params)
		return pyjson.NewObject(), nil
	}
}

func TestDiscardPreservesBufferedReply(t *testing.T) {
	old := `{"pending": [{"admission_id": "lost", "status": "unknown", "execution_generation": 1}], "replay_epoch": "owner", "last_sequence": 1}`
	fresh := `{"pending": [], "replay_epoch": "owner", "last_sequence": 4}`
	for _, route := range []string{"existing_pending_refresh", "new_discard"} {
		t.Run(route, func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": old})
			if route == "new_discard" {
				link.set(snapshotPeer(t, old, fresh))
			} else {
				link.set(snapshotPeer(t, fresh))
			}
			editor := &editorFake{}
			marker := make(chan struct{})
			var once sync.Once
			editor.onUpdate = func(sessionID string, update pyacp.SessionUpdate) error {
				if chunk, ok := update.(*pyacp.AgentMessageChunk); ok && chunk.Content.(*pyacp.TextContentBlock).Text == "MARKER" {
					once.Do(func() { close(marker) })
				}
				return nil
			}
			onLoop(a, func() bool { a.conn = editor; return true })
			err := onLoop(a, func() error {
				if route == "new_discard" {
					_, _, err := runGatewayCommand(context.Background(), a, "s", "discard", "lost")
					return err
				}
				return a.refreshUnknown(context.Background(), "s")
			})
			if err != nil {
				t.Fatal(err)
			}
			link.put(ownerEvent(t, 3, "message.delta", "follower", `{"text": "BUFFERED FOLLOWER ANSWER"}`))
			link.put(ownerEvent(t, 5, "message.delta", "follower", `{"text": "MARKER"}`))
			startPump(t, a)
			select {
			case <-marker:
			case <-time.After(5 * time.Second):
				t.Fatal("the marker never reached the editor")
			}
			found := false
			for _, text := range emittedText(editor) {
				found = found || text == "BUFFERED FOLLOWER ANSWER"
			}
			if !found {
				t.Fatalf("messages = %v", emittedText(editor))
			}
		})
	}
}

func TestPausedImageRefusalDoesNotStageImage(t *testing.T) {
	unknown := `{"pending": [{"admission_id": "lost", "status": "unknown"}], "replay_epoch": "owner", "last_sequence": 1}`
	a, link := bareGatewayAgent(t, map[string]string{"s": unknown})
	dir := t.TempDir()
	link.set(snapshotPeer(t, unknown))
	onLoop(a, func() bool {
		a.home = dir
		a.unacked.Set("s", retainedSubmit{key: submitKey{"s", "prior-retry"}, inputID: "prior-id", text: "prior-content"})
		return true
	})
	blocks := []pyacp.ContentBlock{pyacp.TextBlock("explain image"),
		&pyacp.ImageContentBlock{Data: base64.StdEncoding.EncodeToString(pngFixture), MimeType: "image/png"}}
	_, err := a.Prompt(context.Background(), &pyacp.PromptRequest{Prompt: blocks, SessionID: "s"})
	if !isUnknownExecution(err) {
		t.Fatalf("err = %v", err)
	}
	var staged []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".png") {
			staged = append(staged, path)
		}
		return nil
	})
	if len(staged) != 0 {
		t.Fatalf("staged = %v", staged)
	}
	if !onLoop(a, func() bool { return a.unacked.Has("s") }) {
		t.Fatal("the retained retry identity was retired")
	}
}

type lostOwner struct {
	profileID, epoch, pending string
	mu                        sync.Mutex
	calls                     []string
	socks                     []*pipeSocket
}

func (o *lostOwner) methods() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *lostOwner) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, sock := range o.socks {
		sock.Close()
	}
}

func (o *lostOwner) serve(sock *pipeSocket) {
	o.mu.Lock()
	o.socks = append(o.socks, sock)
	o.mu.Unlock()
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
		o.mu.Unlock()
		result := pyjson.NewObject()
		switch method {
		case "runtime.describe":
			result.Set("profile_id", o.profileID)
		case "session.resume":
			pending, _ := pyjson.Loads(o.pending)
			result = pyjson.ObjectOf("session_id", params.GetOr("session_id", nil), "messages", []any{}, "prompts", []any{},
				"pending", pending, "replay_epoch", o.epoch, "last_sequence", int64(0))
		case "prompt.submit":
			result.Set("admission_id", "next")
		}
		reply, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", frame.GetOr("id", nil), "result", result))
		sock.toClient <- reply
		if method == "prompt.submit" {
			event := pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", pyjson.ObjectOf(
				"session_id", params.GetOr("session_id", nil), "type", "message.complete", "seq", int64(1), "replay_epoch", o.epoch,
				"admission_id", "next", "payload", pyjson.ObjectOf("outcome", "completed")))
			text, _ := pyjson.Dumps(event)
			sock.toClient <- text
		}
	}
}

func connectOwners(t *testing.T, owners ...*lostOwner) {
	t.Helper()
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
}

func waitPump(t *testing.T, a *GatewayACPAgent) {
	t.Helper()
	task := onLoop(a, func() *loopTask { return a.eventTask })
	select {
	case <-task.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pump did not record the transport loss")
	}
}

func TestReattachAfterRestartPostsTheUnknownRecoveryNotice(t *testing.T) {
	a := newGatewayAgent(t)
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	first := &lostOwner{profileID: a.home, epoch: "first", pending: `[]`}
	second := &lostOwner{profileID: a.home, epoch: "second", pending: `[{"admission_id": "lost-1", "status": "unknown"}]`}
	connectOwners(t, first, second)
	defer a.Close()
	held, _ := pyacp.WithResponseScope(context.Background())
	if _, err := a.LoadSession(held, &pyacp.LoadSessionRequest{Cwd: "/", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	before := len(editor.seen())
	first.close()
	waitPump(t, a)
	if _, err := onLoop2(a, func() (gatewayLink, error) { return a.client(context.Background()) }); err != nil {
		t.Fatal(err)
	}
	var notices []connUpdate
	for _, u := range editor.seen()[before:] {
		if _, ok := u.update.(*pyacp.AgentMessageChunk); ok {
			notices = append(notices, u)
		}
	}
	if len(notices) != 1 || notices[0].sessionID != "s" ||
		!strings.Contains(notices[0].update.(*pyacp.AgentMessageChunk).Content.(*pyacp.TextContentBlock).Text, "/discard lost-1") {
		t.Fatalf("notices = %v", notices)
	}
}

func onLoop2[T any](a *GatewayACPAgent, fn func() (T, error)) (T, error) {
	a.loop.enter()
	defer a.loop.leave()
	return fn()
}

func TestNextPromptReconnectsAfterTheLostTurnWasDiscardedWhileOffline(t *testing.T) {
	a := newGatewayAgent(t)
	onLoop(a, func() bool { a.conn = &editorFake{}; return true })
	first := &lostOwner{profileID: a.home, epoch: "first", pending: `[{"admission_id": "lost-1", "status": "unknown"}]`}
	second := &lostOwner{profileID: a.home, epoch: "second", pending: `[]`}
	connectOwners(t, first, second)
	defer a.Close()
	if _, err := a.LoadSession(context.Background(), &pyacp.LoadSessionRequest{Cwd: "/", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, task := range onLoop(a, func() []*loopTask {
		var tasks []*loopTask
		for task := range a.background {
			tasks = append(tasks, task)
		}
		return tasks
	}) {
		<-task.done
	}
	first.close()
	waitPump(t, a)
	resp, err := promptOn(a, context.Background(), "s", "next").wait(t, 5*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if calls := second.methods(); !reflect.DeepEqual(calls, []string{"runtime.describe", "session.resume", "config.set", "prompt.submit"}) {
		t.Fatalf("second owner calls = %v", calls)
	}
}

func TestAModeChangeWhileTheAttachBuildsItsReplyReachesTheReply(t *testing.T) {
	const ask, dontAsk = `{"edit_approval_policy": "ask"}`, `{"edit_approval_policy": "session"}`
	snapshot := `{"session_id": "s", "messages": [], "prompts": [], "pending": [], "replay_epoch": "e", "last_sequence": 10, "execution_generation": 1, "revision": 1, "info": ` + ask + `}`
	for _, attach := range []string{"new", "load"} {
		for _, lookup := range []string{"model_state", "provenance_meta"} {
			t.Run(attach+"_"+lookup, func(t *testing.T) {
				swap(t, &logicalSessionID, func(ctx context.Context, path, held string) (string, error) { return "s", nil })
				a, link := bareGatewayAgent(t, nil)
				editor := &editorFake{}
				onLoop(a, func() bool { a.conn = editor; return true })
				defer a.Close()
				link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
					switch method {
					case "runtime.describe":
						return jsonObj(t, `{"session_create": {"sources": ["acp"]}, "capabilities": ["acp-editor-policy-v1"]}`), nil
					case "session.create", "session.resume":
						return jsonObj(t, snapshot), nil
					case "session.info":
						return jsonObj(t, ask), nil
					case "config.set", "session.stats":
						return pyjson.NewObject(), nil
					}
					t.Errorf("unexpected %s", method)
					return pyjson.NewObject(), nil
				})
				paused, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				hold := func() {
					once.Do(func() { close(paused) })
					select {
					case <-release:
					case <-time.After(5 * time.Second):
					}
				}
				if lookup == "model_state" {
					swap(t, &gatewayModelState, func(context.Context, *pyjson.Object) *pyacp.SessionModelState { hold(); return nil })
				} else {
					swap(t, &gatewayProvenanceMeta, func(context.Context, string, string, string) (*pyjson.Object, error) { hold(); return nil, nil })
				}
				startPump(t, a)
				type reply struct {
					modes *pyacp.SessionModeState
					err   error
				}
				attaching := make(chan reply, 1)
				held, _ := pyacp.WithResponseScope(context.Background())
				go func() {
					if attach == "new" {
						resp, err := a.NewSession(held, &pyacp.NewSessionRequest{Cwd: "/"})
						if err != nil {
							attaching <- reply{nil, err}
							return
						}
						attaching <- reply{resp.Modes, nil}
						return
					}
					resp, err := a.LoadSession(held, &pyacp.LoadSessionRequest{Cwd: "/", SessionID: "s"})
					if err != nil {
						attaching <- reply{nil, err}
						return
					}
					attaching <- reply{resp.Modes, nil}
				}()
				select {
				case <-paused:
				case <-time.After(5 * time.Second):
					t.Fatal("the attach never reached its metadata lookup")
				}
				link.put(pyjson.ObjectOf("params", pyjson.ObjectOf("session_id", "s", "type", "session.info", "replay_epoch", "e", "seq", int64(11), "payload", jsonObj(t, dontAsk))))
				until(t, "the pump projecting the change", func() bool {
					return onLoop(a, func() any {
						snapshot, _ := a.snapshots.Get("s")
						return dget(snapshot, "last_sequence")
					}) == int64(11)
				})
				close(release)
				var got reply
				select {
				case got = <-attaching:
				case <-time.After(5 * time.Second):
					t.Fatal("the attach did not answer")
				}
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.modes.CurrentModeID != "dont_ask" || onLoop(a, func() string { return a.modes["s"] }) != "dont_ask" {
					t.Fatalf("reply mode %q", got.modes.CurrentModeID)
				}
			})
		}
	}
}

func TestAcloseStopsAPausedPostAttachNoticeBeforeItReconnects(t *testing.T) {
	swap(t, &gatewayModelState, func(context.Context, *pyjson.Object) *pyacp.SessionModelState { return nil })
	swap(t, &gatewayProvenanceMeta, func(context.Context, string, string, string) (*pyjson.Object, error) { return nil, nil })
	var mu sync.Mutex
	var connects []string
	swap(t, &connectGateway, func(context.Context) (gatewayLink, error) {
		mu.Lock()
		connects = append(connects, "after shutdown")
		mu.Unlock()
		return nil, os.ErrDeadlineExceeded
	})
	a := newGatewayAgent(t)
	editor := &editorFake{}
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	editor.onUpdate = func(string, pyacp.SessionUpdate) error {
		once.Do(func() { close(paused) })
		<-release
		return nil
	}
	onLoop(a, func() bool { a.conn = editor; return true })
	if _, err := onLoop2(a, func() (*sessionFields, error) {
		return a.sessionFields(context.Background(), "s", pyjson.NewObject(), "s")
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("the post attach notice never started")
	}
	tasks := onLoop(a, func() []*loopTask {
		var tasks []*loopTask
		for task := range a.background {
			tasks = append(tasks, task)
		}
		return tasks
	})
	closed := make(chan struct{})
	go func() {
		a.Close()
		close(closed)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish")
	}
	for _, task := range tasks {
		if !task.cancelled() {
			t.Fatalf("task ended with %v, %v", task.result, task.err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if remaining := onLoop(a, func() int { return len(a.background) }); remaining != 0 || len(connects) != 0 {
		t.Fatalf("background %d connects %v", remaining, connects)
	}
}
