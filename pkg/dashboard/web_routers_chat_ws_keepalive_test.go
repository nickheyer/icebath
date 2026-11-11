package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Idle bridge recording the input and sizes it is given, refusing input when told to
type keepaliveBridge struct {
	mu          sync.Mutex
	alive       bool
	acceptInput bool
	written     []byte
	resized     [][2]int
	blockOn     string
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	slowClose   time.Duration
	closeBegun  chan struct{}
	closeOnce   sync.Once
}

func newKeepaliveBridge() *keepaliveBridge {
	return &keepaliveBridge{alive: true, acceptInput: true, entered: make(chan struct{}), release: make(chan struct{}),
		closeBegun: make(chan struct{})}
}

func (b *keepaliveBridge) Pid() int { return 0 }

func (b *keepaliveBridge) IsAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive
}

func (b *keepaliveBridge) Read(timeout time.Duration) ([]byte, error) {
	time.Sleep(min(timeout, 5*time.Millisecond))
	return []byte{}, nil
}

func (b *keepaliveBridge) Write(ctx context.Context, data []byte, _ time.Duration, _ tools.PtyWriteFence) (bool, error) {
	if b.blockOn != "" && string(data) == b.blockOn {
		b.enterOnce.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return false, nil
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.acceptInput {
		return false, nil
	}
	b.written = append(b.written, data...)
	return true, nil
}

func (b *keepaliveBridge) Resize(cols, rows int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resized = append(b.resized, [2]int{cols, rows})
}

func (b *keepaliveBridge) Close() error {
	b.closeOnce.Do(func() { close(b.closeBegun) })
	time.Sleep(b.slowClose)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alive = false
	return nil
}

func (b *keepaliveBridge) input() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.written)
}

// Argv and bridge of every PTY a case spawned
type keepaliveSpawns struct {
	mu        sync.Mutex
	argv      [][]string
	bridges   []*keepaliveBridge
	slowClose time.Duration
}

// First bridge a case spawned, waited for until the socket handler reaches its spawn
func (s *keepaliveSpawns) first(t *testing.T) *keepaliveBridge {
	t.Helper()
	waitUntil(t, "the PTY spawn", func() bool { _, bridges := s.snapshot(); return len(bridges) > 0 })
	_, bridges := s.snapshot()
	return bridges[0]
}

func (s *keepaliveSpawns) snapshot() ([][]string, []*keepaliveBridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.argv), slices.Clone(s.bridges)
}

// Test application whose chat PTYs are idle fake bridges over a resolver that maps resume parent to child
func ptyKeepaliveHarness(t *testing.T) (*FastAPI, *testClient, *keepaliveSpawns) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	app := dashboardTestApp(t)
	ptyRegistry.Reopen()
	spawns := &keepaliveSpawns{}
	savedSpawn, savedResolve, savedAvailable, savedMarker := chatSpawnBridge, chatResolveArgv, chatPtyBridgeAvailable, chatActiveSessionFile
	chatSpawnBridge = func(_ context.Context, _ *tools.Env, argv []string, _ tools.PtySpawnOptions) (tools.TerminalBridge, error) {
		spawns.mu.Lock()
		defer spawns.mu.Unlock()
		b := newKeepaliveBridge()
		b.slowClose = spawns.slowClose
		spawns.argv = append(spawns.argv, slices.Clone(argv))
		spawns.bridges = append(spawns.bridges, b)
		return b, nil
	}
	chatResolveArgv = func(_ context.Context, _ *FastAPI, resume, _, _, _, _ string) ([]string, string, map[string]string, error) {
		if resume == "parent" {
			resume = "child"
		}
		env := map[string]string{}
		label := "fresh"
		if resume != "" {
			env["ICEBATH_TUI_RESUME"] = resume
			label = resume
		}
		return []string{"x", label}, os.TempDir(), env, nil
	}
	t.Cleanup(func() {
		chatSpawnBridge, chatResolveArgv, chatPtyBridgeAvailable, chatActiveSessionFile = savedSpawn, savedResolve, savedAvailable, savedMarker
		for _, key := range ptyRegistry.Keys() {
			if session := ptyRegistry.Pop(key); session != nil {
				_ = session.Close(context.Background())
			}
		}
		ptyRegistry.Reopen()
	})
	return app, newTestClient(t, app), spawns
}

func sendBytes(conn *testWebSocket, data string) {
	conn.toApp <- Message{Type: "websocket.receive", Bytes: []byte(data)}
}

// Opens a chat PTY socket, sends one input frame and disconnects
func ptyExchange(t *testing.T, client *testClient, input string, params ...string) {
	t.Helper()
	conn := connectOK(t, client, tokenURL("/api/pty", params...))
	sendBytes(conn, input)
	if err := conn.close(1000); err != nil {
		t.Fatalf("handler error %v", err)
	}
}

func TestAttachTokenReusesSameSession(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	ptyExchange(t, client, "hi", "attach", "TOK1")
	ptyExchange(t, client, "again", "attach", "TOK1")
	argv, bridges := spawns.snapshot()
	if len(argv) != 1 {
		t.Fatalf("%d PTYs spawned, want one reattached", len(argv))
	}
	if got := bridges[0].input(); got != "hi\x0cagain" {
		t.Fatalf("bridge input %q", got)
	}
}

func TestStalledInputClosesOnlyTheKeepaliveSocket(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	conn := connectOK(t, client, tokenURL("/api/pty", "attach", "TOK1"))
	bridge := spawns.first(t)
	bridge.mu.Lock()
	bridge.acceptInput = false
	bridge.mu.Unlock()
	sendBytes(conn, "input")
	if m := conn.next(); m.Type != "websocket.close" || m.Code != 1013 {
		t.Fatalf("expected close 1013, got %+v", m)
	}
	_ = conn.close(1000)
	if session := ptyRegistry.Get("TOK1"); session == nil || session.Alive() {
		t.Fatalf("session %v still alive", session)
	}
}

func TestChatWithoutAPtyBridgeSaysWhatToInstallAsOnMain(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	chatPtyBridgeAvailable = func() bool { return false }
	conn := connectOK(t, client, tokenURL("/api/pty", "attach", "TOK1"))
	text, gone := conn.receiveText()
	if gone != nil {
		t.Fatalf("closed before the message: %v", gone)
	}
	if _, closed := conn.receiveText(); closed == nil || closed.Code != 1011 {
		t.Fatalf("expected close 1011, got %v", closed)
	}
	_ = conn.close(1000)
	for _, want := range []string{"Chat unavailable: the embedded terminal requires a POSIX PTY",
		"Install Icebath inside WSL2 to use the dashboard's /chat tab"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message %q lacks %q", text, want)
		}
	}
	if argv, _ := spawns.snapshot(); len(argv) != 0 {
		t.Fatalf("spawned %v", argv)
	}
}

// Tab that feeds its frames then waits, recording every close code
type fakeTab struct {
	mu     sync.Mutex
	frames chan Message
	closes []int
}

func newFakeTab(frames ...Message) *fakeTab {
	tab := &fakeTab{frames: make(chan Message, 8)}
	for _, f := range frames {
		tab.frames <- f
	}
	return tab
}

func (f *fakeTab) Receive(ctx context.Context) (Message, error) {
	select {
	case m := <-f.frames:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (f *fakeTab) SendBytes(context.Context, []byte) error { return nil }

func (f *fakeTab) SendText(context.Context, string) error { return nil }

func (f *fakeTab) Close(_ context.Context, code int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, code)
	return nil
}

func TestATakeoverDuringInputLeavesTheChatSessionToTheNewTab(t *testing.T) {
	bridge := newKeepaliveBridge()
	bridge.blockOn = "old"
	session := tools.NewPtySession("k", bridge, tools.PtySessionOptions{BufferCap: 64, ReadTimeout: 10 * time.Millisecond})
	session.Start()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := newFakeTab(Message{Type: "websocket.receive", Bytes: []byte("old")})
	if _, err := session.Attach(ctx, old, tools.PtyAttachOptions{}); err != nil {
		t.Fatal(err)
	}
	pumped := make(chan error, 1)
	go func() { pumped <- pumpKeepaliveInput(ctx, old, session) }()
	<-bridge.entered
	fresh := newFakeTab()
	if _, err := session.Attach(ctx, fresh, tools.PtyAttachOptions{}); err != nil {
		t.Fatal(err)
	}
	close(bridge.release)
	select {
	case err := <-pumped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old tab writer did not end")
	}
	session.Resize(old, 5, 6)
	session.Resize(fresh, 80, 24)
	if !session.Alive() {
		t.Fatal("session died")
	}
	old.mu.Lock()
	closes := slices.Clone(old.closes)
	old.mu.Unlock()
	if !slices.Equal(closes, []int{tools.WSCloseSuperseded}) {
		t.Fatalf("old tab closes %v", closes)
	}
	bridge.mu.Lock()
	resized := slices.Clone(bridge.resized)
	bridge.mu.Unlock()
	if !slices.Equal(resized, [][2]int{{80, 24}}) {
		t.Fatalf("resizes %v", resized)
	}
	if ok, err := session.Write(ctx, fresh, []byte("new"), tools.PtyWriteTimeout, nil); err != nil || !ok {
		t.Fatalf("write from the new tab %v %v", ok, err)
	}
	if got := bridge.input(); got != "new" {
		t.Fatalf("bridge input %q", got)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttachTokenReusesSameResume(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	ptyExchange(t, client, "hi", "attach", "TOK1", "resume", "same")
	ptyExchange(t, client, "again", "attach", "TOK1", "resume", "same")
	if argv, _ := spawns.snapshot(); len(argv) != 1 || !slices.Equal(argv[0], []string{"x", "same"}) {
		t.Fatalf("spawned %v", argv)
	}
}

func TestAttachTokenReusesCanonicalResume(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	ptyExchange(t, client, "hi", "attach", "TOK1", "resume", "parent")
	ptyExchange(t, client, "again", "attach", "TOK1", "resume", "child")
	if argv, _ := spawns.snapshot(); len(argv) != 1 || !slices.Equal(argv[0], []string{"x", "child"}) {
		t.Fatalf("spawned %v", argv)
	}
}

func TestAttachTokenReusesDefaultChatAfterActiveSessionFallback(t *testing.T) {
	_, client, spawns := ptyKeepaliveHarness(t)
	activeSessionFile := filepath.Join(t.TempDir(), "active-session.json")
	chatActiveSessionFile = func(*FastAPI, string) (string, error) { return activeSessionFile, nil }
	ptyExchange(t, client, "hi", "attach", "TOK1", "channel", "CHAT")
	writeTestFile(t, activeSessionFile, `{"session_id": "existing"}`)
	ptyExchange(t, client, "again", "attach", "TOK1", "channel", "CHAT")
	if argv, _ := spawns.snapshot(); len(argv) != 1 || !slices.Equal(argv[0], []string{"x", "fresh"}) {
		t.Fatalf("spawned %v", argv)
	}
}

func TestChannelMarkerLeavesNoDictEntryAfterPtyLifecycle(t *testing.T) {
	app, client, _ := ptyKeepaliveHarness(t)
	markers := getPtyActiveSessionFiles(app)
	ptyExchange(t, client, "hi", "attach", "TOK63553", "channel", "CHAN63553")
	markerPath, registered := markers.Get("CHAN63553")
	if !registered {
		t.Fatal("no marker while the PTY is live")
	}
	ptyRegistry.CloseAll(context.Background())
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker file left behind: %v", err)
	}
	if _, left := markers.Get("CHAN63553"); left {
		t.Fatal("marker entry left behind")
	}
}

func TestChannelMarkerReconnectReusesOneEntry(t *testing.T) {
	app, client, _ := ptyKeepaliveHarness(t)
	markers := getPtyActiveSessionFiles(app)
	for range 3 {
		ptyExchange(t, client, "hi", "attach", "TOK63553R", "channel", "CHAN63553R")
	}
	if _, ok := markers.Get("CHAN63553R"); !ok || markers.Len() != 1 {
		t.Fatalf("%d marker entries", markers.Len())
	}
	ptyRegistry.CloseAll(context.Background())
	if markers.Len() != 0 {
		t.Fatalf("%d marker entries after close", markers.Len())
	}
}

func TestLegacyChannelConnectPopsMarkerOnDisconnect(t *testing.T) {
	app, client, _ := ptyKeepaliveHarness(t)
	markers := getPtyActiveSessionFiles(app)
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", "LEGACY63553"))
	sendBytes(conn, "hi")
	waitUntil(t, "the marker of the open socket", func() bool { _, ok := markers.Get("LEGACY63553"); return ok })
	_ = conn.close(1000)
	waitUntil(t, "the legacy marker to go", func() bool { _, ok := markers.Get("LEGACY63553"); return !ok })
}

func TestLegacyMarkerDroppedWhenCancelLandsMidClose(t *testing.T) {
	app, client, spawns := ptyKeepaliveHarness(t)
	spawns.slowClose = 500 * time.Millisecond
	markers := getPtyActiveSessionFiles(app)
	conn := connectOK(t, client, tokenURL("/api/pty", "channel", "LEGACYCANCEL"))
	sendBytes(conn, "hi")
	waitUntil(t, "the marker of the open socket", func() bool { _, ok := markers.Get("LEGACYCANCEL"); return ok })
	conn.toApp <- Message{Type: "websocket.disconnect", Code: 1000}
	select {
	case <-spawns.first(t).closeBegun:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge close never started")
	}
	waitUntil(t, "the legacy marker to go", func() bool { _, ok := markers.Get("LEGACYCANCEL"); return !ok })
	_ = conn.finish()
}

func TestFreshStartPopsStaleMarkerAndReregistersChildPath(t *testing.T) {
	app, client, _ := ptyKeepaliveHarness(t)
	markers := getPtyActiveSessionFiles(app)
	ptyExchange(t, client, "hi", "attach", "TOKFRESH", "channel", "CHANFRESH")
	stalePath, _ := markers.Get("CHANFRESH")
	writeTestFile(t, stalePath, `{"session_id": "stale"}`)
	ptyExchange(t, client, "hi", "attach", "TOKFRESH2", "channel", "CHANFRESH", "fresh", "1")
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale file left: %v", err)
	}
	if kept, _ := markers.Get("CHANFRESH"); kept != stalePath {
		t.Fatalf("mapping %q, want %q", kept, stalePath)
	}
	ptyExchange(t, client, "hi", "attach", "TOKFRESH2", "channel", "CHANFRESH")
	if kept, _ := markers.Get("CHANFRESH"); kept != stalePath {
		t.Fatalf("mapping after reconnect %q, want %q", kept, stalePath)
	}
	ptyRegistry.CloseAll(context.Background())
	if markers.Len() != 0 {
		t.Fatalf("%d marker entries after close", markers.Len())
	}
}
