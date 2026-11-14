package dashboard

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bridge that yields one frame and then ends, or stays idle until closed
type oneFrameBridge struct {
	mu     sync.Mutex
	sent   bool
	closed bool
	idle   bool
}

func (b *oneFrameBridge) Pid() int { return 0 }

func (b *oneFrameBridge) IsAlive() bool { return !b.isClosed() }

func (b *oneFrameBridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *oneFrameBridge) Read(timeout time.Duration) ([]byte, error) {
	b.mu.Lock()
	if b.idle && b.closed {
		b.mu.Unlock()
		return nil, nil
	}
	if !b.sent {
		b.sent = true
		b.mu.Unlock()
		return []byte("ready"), nil
	}
	b.mu.Unlock()
	if !b.idle {
		return nil, nil
	}
	time.Sleep(min(timeout, 10*time.Millisecond))
	return []byte{}, nil
}

func (b *oneFrameBridge) Write(context.Context, []byte, time.Duration, tools.PtyWriteFence) (bool, error) {
	return true, nil
}

func (b *oneFrameBridge) Resize(int, int) {}

func (b *oneFrameBridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// Bridges a case spawned, in order
type bridgeLog struct {
	mu      sync.Mutex
	bridges []*oneFrameBridge
}

func (l *bridgeLog) all() []*oneFrameBridge {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*oneFrameBridge(nil), l.bridges...)
}

// Test application with a fresh marker table and the PTY spawner replaced by one frame bridges
func ptyTestClient(t *testing.T, idle bool) (*FastAPI, *testClient, *bridgeLog) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	app := dashboardTestApp(t)
	app.State.Set("pty_active_session_files", NewPtySessionFiles())
	ptyRegistry.Reopen()
	log := &bridgeLog{}
	savedSpawn, savedResolve := chatSpawnBridge, chatResolveArgv
	chatSpawnBridge = func(context.Context, *tools.Env, []string, tools.PtySpawnOptions) (tools.TerminalBridge, error) {
		b := &oneFrameBridge{idle: idle}
		log.mu.Lock()
		log.bridges = append(log.bridges, b)
		log.mu.Unlock()
		return b, nil
	}
	t.Cleanup(func() { chatSpawnBridge, chatResolveArgv = savedSpawn, savedResolve })
	return app, newTestClient(t, app), log
}

// Resolver answering a tiny fake command for any chat
func fakeChatArgv(context.Context, *FastAPI, string, string, string, string, string) ([]string, string, map[string]string, error) {
	return []string{"fake-icebath-tui"}, "", nil, nil
}

// Socket URL carrying the session token and query parameters
func tokenURL(path string, params ...string) string {
	query := url.Values{"token": {SessionToken()}}
	for i := 0; i+1 < len(params); i += 2 {
		query.Set(params[i], params[i+1])
	}
	return path + "?" + query.Encode()
}

// Next frame as bytes, failing the case for any other message
func receiveBytes(t *testing.T, conn *testWebSocket) []byte {
	t.Helper()
	m := conn.next()
	if m.Type != "websocket.send" || m.Bytes == nil {
		t.Fatalf("expected a bytes frame, got %+v", m)
	}
	return m.Bytes
}

// Next text frame decoded as a JSON object
func receiveJSON(t *testing.T, conn *testWebSocket) map[string]any {
	t.Helper()
	text, gone := conn.receiveText()
	if gone != nil {
		t.Fatalf("socket closed %v", gone)
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("frame %q: %v", text, err)
	}
	obj, ok := pyjson.Plain(parsed).(map[string]any)
	if !ok {
		t.Fatalf("frame %q is not an object", text)
	}
	return obj
}

func connectOK(t *testing.T, c *testClient, target string) *testWebSocket {
	t.Helper()
	conn, refused := c.websocketConnect(target)
	if refused != nil {
		t.Fatalf("%s refused: %v", target, refused)
	}
	return conn
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFreshParamIgnoresChannelActiveSessionFile(t *testing.T) {
	app, client, _ := ptyTestClient(t, false)
	channel := "fresh-chan"
	activeFile, err := activeSessionFileForChannel(app, channel)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, activeFile, `{"session_id": "sess-old"}`)
	var gotResume, gotFile string
	chatResolveArgv = func(_ context.Context, _ *FastAPI, resume, _, _, activeSessionFile, _ string) ([]string, string, map[string]string, error) {
		gotResume, gotFile = resume, activeSessionFile
		return []string{"fake-icebath-tui"}, "", nil, nil
	}
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", channel, "fresh", "1"))
	if got := receiveBytes(t, conn); string(got) != "ready" {
		t.Fatalf("first frame %q", got)
	}
	_ = conn.close(1000)
	if gotResume != "" || gotFile != activeFile {
		t.Fatalf("resume %q file %q, want none and %q", gotResume, gotFile, activeFile)
	}
	if _, err := os.Stat(activeFile); !os.IsNotExist(err) {
		t.Fatalf("marker still exists: %v", err)
	}
}

func TestActiveSessionFallbackSendsResumeControlMessage(t *testing.T) {
	app, client, _ := ptyTestClient(t, false)
	channel := "implicit-resume-chan"
	activeFile, err := activeSessionFileForChannel(app, channel)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, activeFile, `{"session_id": "sess-old"}`)
	chatResolveArgv = fakeChatArgv
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", channel))
	if frame := receiveJSON(t, conn); len(frame) != 2 || frame["type"] != "resume" || frame["id"] != "sess-old" {
		t.Fatalf("control frame %v", frame)
	}
	if got := receiveBytes(t, conn); string(got) != "ready" {
		t.Fatalf("frame %q", got)
	}
	_ = conn.close(1000)
}

func TestExplicitResumeSendsNoControlMessage(t *testing.T) {
	_, client, _ := ptyTestClient(t, false)
	chatResolveArgv = fakeChatArgv
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", "explicit-resume-chan", "resume", "sess-explicit"))
	if got := receiveBytes(t, conn); string(got) != "ready" {
		t.Fatalf("first frame %q", got)
	}
	_ = conn.close(1000)
}

func TestChildEOFClosesSocketAndBridge(t *testing.T) {
	_, client, log := ptyTestClient(t, false)
	chatResolveArgv = fakeChatArgv
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", "eof-chan"))
	if got := receiveBytes(t, conn); string(got) != "ready" {
		t.Fatalf("first frame %q", got)
	}
	if m := conn.next(); m.Type != "websocket.close" {
		t.Fatalf("expected the server to close after child EOF, got %+v", m)
	}
	_ = conn.close(1000)
	bridges := log.all()
	if len(bridges) != 1 {
		t.Fatalf("%d bridges spawned", len(bridges))
	}
	waitUntil(t, "the bridge to close", bridges[0].isClosed)
}

func TestProfileSwitchClosesPreviousProfileKeepalivePty(t *testing.T) {
	_, client, log := ptyTestClient(t, true)
	chatResolveArgv = fakeChatArgv
	attach := "attach-125287"
	t.Cleanup(func() {
		for _, key := range ptyRegistry.Keys() {
			if strings.HasPrefix(key, attach) {
				if session := ptyRegistry.Pop(key); session != nil {
					_ = session.Close(context.Background())
				}
			}
		}
	})
	first := connectOK(t, client, tokenURL("/api/pty", "channel", "chan-a", "attach", attach, "profile", "alpha"))
	if got := receiveBytes(t, first); string(got) != "ready" {
		t.Fatalf("first frame %q", got)
	}
	_ = first.close(1000)
	if bridges := log.all(); len(bridges) != 1 || bridges[0].isClosed() {
		t.Fatalf("keep alive PTY did not survive the detach: %d bridges", len(bridges))
	}
	second := connectOK(t, client, tokenURL("/api/pty", "channel", "chan-b", "attach", attach, "profile", "beta"))
	if got := receiveBytes(t, second); string(got) != "ready" {
		t.Fatalf("first frame %q", got)
	}
	bridges := log.all()
	if len(bridges) != 2 {
		t.Fatalf("%d bridges spawned", len(bridges))
	}
	waitUntil(t, "the previous profile PTY to close", bridges[0].isClosed)
	if bridges[1].isClosed() {
		t.Fatal("the new profile PTY closed")
	}
	_ = second.close(1000)
}

// Console client with the engine replaced when a case gives one
func consoleTestClient(t *testing.T, engine func(ctx context.Context, line string, confirmed bool) (string, string, string, string, error)) *testClient {
	t.Helper()
	app := dashboardTestApp(t)
	app.State.Set("auth_required", false)
	app.State.Del("bound_host")
	savedEngine := ConsoleCLI.NewEngine
	if engine != nil {
		ConsoleCLI.NewEngine = func(context.Context, int) func(context.Context, string, bool) (string, string, string, string, error) {
			return engine
		}
	}
	t.Cleanup(func() { ConsoleCLI.NewEngine = savedEngine })
	return newTestClient(t, app)
}

func TestConsoleWsRejectsMissingOrBadToken(t *testing.T) {
	client := consoleTestClient(t, nil)
	if _, refused := client.websocketConnect("/api/console"); refused == nil || refused.Code != 4401 {
		t.Fatalf("missing token: %v", refused)
	}
	if _, refused := client.websocketConnect("/api/console?token=wrong"); refused == nil || refused.Code != 4401 {
		t.Fatalf("bad token: %v", refused)
	}
}

// Next console frame of a type and status, earlier frames dropped
func recvUntil(t *testing.T, conn *testWebSocket, frameType, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame := receiveJSON(t, conn)
		if frame["type"] == frameType && (status == "" || frame["status"] == status) {
			return frame
		}
	}
	t.Fatalf("timed out waiting for a %s frame", frameType)
	return nil
}

func TestConsoleWsCancelReturnsToPrompt(t *testing.T) {
	client := consoleTestClient(t, func(_ context.Context, line string, _ bool) (string, string, string, string, error) {
		time.Sleep(200 * time.Millisecond)
		return "ok", "late", line, "", nil
	})
	conn := connectOK(t, client, tokenURL("/api/console"))
	if frame := receiveJSON(t, conn); frame["type"] != "ready" {
		t.Fatalf("first frame %v", frame)
	}
	conn.sendText(`{"type": "input", "line": "status"}`)
	conn.sendText(`{"type": "cancel"}`)
	if complete := recvUntil(t, conn, "complete", "cancelled"); complete["prompt"] != "icebath> " {
		t.Fatalf("complete frame %v", complete)
	}
	_ = conn.close(1000)
}

// Claims the single writer lease of a session in a registry home
func claimSessionLease(t *testing.T, registryHome, sessionID string) *activesessions.Lease {
	t.Helper()
	lease, refusal, err := activesessions.TryAcquire(context.Background(), sessionID, "tui", map[string]any{},
		map[string]any{"live_session_id": "x"}, registryHome, false)
	if err != nil || lease == nil {
		t.Fatalf("lease refused: %v %v", refusal, err)
	}
	return lease
}

func TestLeaseHolderIsReadFromTheChatsProfileHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "default"))
	profileHome, launchHome := filepath.Join(root, "profiles", "work"), filepath.Join(root, "default")
	for _, dir := range []string{profileHome, launchHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lease := claimSessionLease(t, profileHome, "sess-a")
	defer lease.Release()
	ctx := context.Background()
	if pid := leaseHolderPid(ctx, "sess-a", profileHome); pid == nil || *pid != os.Getpid() {
		t.Fatalf("holder in the profile home %v", pid)
	}
	if pid := leaseHolderPid(ctx, "sess-a", launchHome); pid != nil {
		t.Fatalf("holder in the launch home %d", *pid)
	}
	if pid := leaseHolderPid(ctx, "sess-b", profileHome); pid != nil {
		t.Fatalf("holder of another session %d", *pid)
	}
	if pid := leaseHolderPid(ctx, "", profileHome); pid != nil {
		t.Fatalf("holder of no session %d", *pid)
	}
}

func TestUnreadableRegistryYieldsNoHolder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	writeTestFile(t, filepath.Join(root, "runtime", "active_sessions.json"), "{not json")
	if pid := leaseHolderPid(context.Background(), "sess-a", root); pid != nil {
		t.Fatalf("holder %d", *pid)
	}
}

// Bridge whose child reads no input, every write waiting until it is cancelled
type backpressuredBridge struct {
	oneFrameBridge
	waiting chan struct{}
	once    sync.Once
}

func (b *backpressuredBridge) Read(timeout time.Duration) ([]byte, error) {
	time.Sleep(min(timeout, 10*time.Millisecond))
	return []byte{}, nil
}

func (b *backpressuredBridge) Write(ctx context.Context, _ []byte, _ time.Duration, _ tools.PtyWriteFence) (bool, error) {
	b.once.Do(func() { close(b.waiting) })
	<-ctx.Done()
	return false, nil
}

// Viewer that feeds its frames and then reads until closed, a second close failing like Starlette
type fakeViewer struct {
	mu      sync.Mutex
	closed  []int
	inbound chan Message
}

func newFakeViewer(frames ...[]byte) *fakeViewer {
	v := &fakeViewer{inbound: make(chan Message, 8)}
	for _, frame := range frames {
		v.inbound <- Message{Type: "websocket.receive", Bytes: frame}
	}
	return v
}

func (v *fakeViewer) Receive(ctx context.Context) (Message, error) {
	select {
	case m := <-v.inbound:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (v *fakeViewer) SendBytes(context.Context, []byte) error { return nil }

func (v *fakeViewer) SendText(context.Context, string) error { return nil }

func (v *fakeViewer) Close(_ context.Context, code int, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.closed) > 0 {
		return &RuntimeError{Msg: "Cannot call send once a close message has been sent."}
	}
	v.closed = append(v.closed, code)
	v.inbound <- Message{Type: "websocket.disconnect"}
	return nil
}

func (v *fakeViewer) closes() []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]int(nil), v.closed...)
}

func TestTakeoverDuringABackpressuredWriteIsNotReportedAsAStall(t *testing.T) {
	records, _ := captureLogs(t, slog.LevelWarn)
	logs := func() string {
		var text strings.Builder
		for _, r := range records() {
			text.WriteString(r.message + "\n")
		}
		return text.String()
	}
	bridge := &backpressuredBridge{waiting: make(chan struct{})}
	session := tools.NewPtySession("chat", bridge, tools.PtySessionOptions{BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
	session.Start()
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	ctx := context.Background()
	old, fresh := newFakeViewer([]byte("a long paste")), newFakeViewer()
	if ok, err := session.Attach(ctx, old, tools.PtyAttachOptions{}); err != nil || !ok {
		t.Fatalf("attach old %v %v", ok, err)
	}
	pumped := make(chan error, 1)
	go func() { pumped <- pumpKeepaliveInput(ctx, old, session) }()
	<-bridge.waiting
	if ok, err := session.Attach(ctx, fresh, tools.PtyAttachOptions{}); err != nil || !ok {
		t.Fatalf("attach new %v %v", ok, err)
	}
	select {
	case err := <-pumped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the superseded writer did not end")
	}
	if got := old.closes(); len(got) != 1 || got[0] != tools.WSCloseSuperseded {
		t.Fatalf("old viewer closes %v", got)
	}
	if strings.Contains(logs(), "stalled") {
		t.Fatalf("takeover logged as a stall: %s", logs())
	}
	if !session.Alive() || session.Viewer() != tools.PtyViewer(fresh) {
		t.Fatal("the session is not alive on the new viewer")
	}
}

func TestUnavailablePlatformClosesWithMessage(t *testing.T) {
	_, client, _ := ptyTestClient(t, false)
	chatResolveArgv = func(context.Context, *FastAPI, string, string, string, string, string) ([]string, string, map[string]string, error) {
		return []string{"/bin/cat"}, "", nil, nil
	}
	chatSpawnBridge = func(context.Context, *tools.Env, []string, tools.PtySpawnOptions) (tools.TerminalBridge, error) {
		return nil, &tools.PtyUnavailableError{Msg: "pty missing for tests"}
	}
	conn := connectOK(t, client, tokenURL("/api/pty"))
	msg, gone := conn.receiveText()
	if gone != nil {
		t.Fatalf("closed before any message: %v", gone)
	}
	if lower := strings.ToLower(msg); !strings.Contains(msg, "pty missing") && !strings.Contains(lower, "unavailable") && !strings.Contains(lower, "pty") {
		t.Fatalf("message %q", msg)
	}
	_ = conn.close(1000)
}

// Subscriber that records every frame sent to it
type fakeSubscriber struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSubscriber) SendText(_ context.Context, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, payload)
	return nil
}

func TestPubBroadcastsToEventsSubscribers(t *testing.T) {
	app := dashboardTestApp(t)
	a1, a2, other := &fakeSubscriber{}, &fakeSubscriber{}, &fakeSubscriber{}
	frame := `{"type":"tool.start","payload":{"tool_id":"t1"}}`
	channels, lock := getEventState(app)
	lock.Lock()
	channels["broadcast-test"] = map[eventSubscriber]struct{}{a1: {}, a2: {}}
	channels["other-channel"] = map[eventSubscriber]struct{}{other: {}}
	lock.Unlock()
	broadcastEvent(context.Background(), app, "broadcast-test", frame)
	lock.Lock()
	delete(channels, "broadcast-test")
	delete(channels, "other-channel")
	lock.Unlock()
	if len(a1.sent) != 1 || a1.sent[0] != frame || len(a2.sent) != 1 || a2.sent[0] != frame {
		t.Fatalf("subscribers got %v and %v", a1.sent, a2.sent)
	}
	if len(other.sent) != 0 {
		t.Fatalf("another channel got %v", other.sent)
	}
}
