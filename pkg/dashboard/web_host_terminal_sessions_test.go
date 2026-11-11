package dashboard

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bridge whose chunks are output, an empty chunk an idle tick and a nil chunk end of file
type hostFakeBridge struct {
	mu      sync.Mutex
	chunks  [][]byte
	sizes   [][2]int
	dead    bool
	closed  chan struct{}
	once    sync.Once
	reading *atomic.Int32
	printed chan []byte
}

func newHostFakeBridge() *hostFakeBridge {
	return &hostFakeBridge{closed: make(chan struct{}), sizes: [][2]int{{80, 24}}, printed: make(chan []byte, 16)}
}

func (b *hostFakeBridge) Pid() int { return 0 }

func (b *hostFakeBridge) IsAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.dead
}

func (b *hostFakeBridge) Read(timeout time.Duration) ([]byte, error) {
	if b.reading != nil {
		b.reading.Add(1)
		defer b.reading.Add(-1)
		<-b.closed
		return nil, nil
	}
	select {
	case chunk := <-b.printed:
		return chunk, nil
	case <-b.closed:
		return nil, nil
	case <-time.After(timeout):
		return []byte{}, nil
	}
}

func (b *hostFakeBridge) Write(ctx context.Context, data []byte, _ time.Duration, _ tools.PtyWriteFence) (bool, error) {
	return true, nil
}

func (b *hostFakeBridge) Resize(cols, rows int) {
	b.mu.Lock()
	b.sizes = append(b.sizes, [2]int{cols, rows})
	b.mu.Unlock()
}

func (b *hostFakeBridge) Size() (int, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	last := b.sizes[len(b.sizes)-1]
	return last[0], last[1], true
}

func (b *hostFakeBridge) sizesNow() [][2]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.sizes)
}

func (b *hostFakeBridge) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// Viewer collecting every byte frame it is sent
type hostTab struct {
	mu     sync.Mutex
	output []byte
}

func (t *hostTab) SendBytes(_ context.Context, data []byte) error {
	t.mu.Lock()
	t.output = append(t.output, data...)
	t.mu.Unlock()
	return nil
}

func (t *hostTab) SendText(context.Context, string) error { return nil }

func (t *hostTab) Close(context.Context, int, string) error { return nil }

func (t *hostTab) has(marker string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Contains(t.output, []byte(marker))
}

func hostWaitUntil(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// test_web_host_terminal_drain_pool.py test_idle_host_shells_leave_the_default_executor_free
func TestIdleHostShellsLeaveTheDefaultExecutorFree(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := newHostTerminalRegistry()
	var reading atomic.Int32
	var shells []*hostFakeBridge
	spawn := func() (tools.TerminalBridge, error) {
		b := newHostFakeBridge()
		b.reading = &reading
		shells = append(shells, b)
		return b, nil
	}
	defer func() {
		for _, shell := range shells {
			_ = shell.Close()
		}
		registry.closeAll(context.Background())
		hostWaitUntil(t, func() bool { return reading.Load() == 0 }, 5*time.Second)
	}()
	for range hostTerminalMaxShells {
		if _, _, err := registry.AttachOrSpawn(ctx, tokenURLSafe(32), spawn, ""); err != nil {
			t.Fatal(err)
		}
	}
	hostWaitUntil(t, func() bool { return reading.Load() >= hostTerminalMaxShells }, 5*time.Second)
	ran := make(chan string, 1)
	go func() { ran <- "ran" }()
	if got := <-ran; got != "ran" {
		t.Fatalf("goroutine result %q", got)
	}
}

func hostTextFrame(text string) hostInputItem {
	return hostInputItem{msg: &Message{Type: "websocket.receive", Text: &text}}
}

func hostBytesFrame(data []byte) hostInputItem {
	return hostInputItem{msg: &Message{Type: "websocket.receive", Bytes: data}}
}

// Non ASCII keys that together fill the input queue at its byte bound with at least its frame count
func hostKeysFillingTheInputQueue() []string {
	key := strings.Repeat("é", (hostInputQueueBytes/hostInputQueueFrames-hostFrameOverheadBytes)/2+1)
	keys := make([]string, 0, hostInputQueueFrames)
	charged := 0
	for len(keys) < hostInputQueueFrames || charged < hostInputQueueBytes {
		keys = append(keys, key)
		charged += len(key) + hostFrameOverheadBytes
	}
	return keys
}

// test_host_terminal_viewer_ownership.py test_text_frames_count_their_utf8_bytes_against_the_input_queue
func TestTextFramesCountTheirUtf8BytesAgainstTheInputQueue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keys := hostKeysFillingTheInputQueue()
	inbox := newHostInputQueue()
	want := 0
	for _, key := range keys {
		if inbox.fullLocked() {
			t.Fatal("the input queue filled before its byte bound")
		}
		if err := inbox.put(context.Background(), hostTextFrame(key), 0); err != nil {
			t.Fatal(err)
		}
		want += len([]byte(key)) + hostFrameOverheadBytes
	}
	if !inbox.fullLocked() {
		t.Fatal("text frames filled the input queue by characters, not bytes")
	}
	if inbox.queued != want {
		t.Fatalf("queued %d want %d", inbox.queued, want)
	}
	for {
		if _, ok := inbox.getNowait(); !ok {
			break
		}
	}
	if inbox.queued != 0 {
		t.Fatalf("queued %d after draining", inbox.queued)
	}
	frame := hostTextFrame(keys[0])
	runs := hostInputRuns([]hostInputItem{frame})
	if len(runs) != 1 {
		t.Fatalf("runs %d", len(runs))
	}
	if hostFrameCost(frame) != len(runs[0].data)+hostFrameOverheadBytes {
		t.Fatalf("cost %d written %d", hostFrameCost(frame), len(runs[0].data))
	}
}

// test_host_terminal_viewer_ownership.py test_a_paste_or_keys_alone_do_not_fill_the_input_queue
func TestAPasteOrKeysAloneDoNotFillTheInputQueue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	inbox := newHostInputQueue()
	frames := [][]byte{bytes.Repeat([]byte("P"), 2*hostInputQueueBytes)}
	for range 48 {
		frames = append(frames, []byte("k"))
	}
	for _, frame := range frames {
		if err := inbox.put(context.Background(), hostBytesFrame(frame), 0); err != nil {
			t.Fatal(err)
		}
		if inbox.fullLocked() {
			t.Fatal("a paste over the byte bound filled the input queue")
		}
	}
	for {
		if _, ok := inbox.getNowait(); !ok {
			break
		}
	}
	for range 1024 {
		if err := inbox.put(context.Background(), hostBytesFrame([]byte("k")), 0); err != nil {
			t.Fatal(err)
		}
		if inbox.fullLocked() {
			t.Fatal("keys filled the input queue by count alone")
		}
	}
}

// test_pty_session_close_reason.py test_a_shell_reaped_while_its_redial_is_validated_closes_the_redial_as_an_exit
func TestAShellReapedWhileItsRedialIsValidatedClosesTheRedialAsAnExit(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.Mkdir(h, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	resolved, err := pypath.Resolve(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity := [2]string{"local", "owner"}
	registry := newHostTerminalRegistry()
	defer registry.closeAll(ctx)
	token := tokenURLSafe(32)
	bridge := newHostFakeBridge()
	bridge.dead = true
	if _, _, err := registry.AttachOrSpawn(ctx, token, func() (tools.TerminalBridge, error) { return bridge, nil }, ""); err != nil {
		t.Fatal(err)
	}
	registry.setOwner(token, &hostOwner{identity: identity, home: resolved, shell: "sh", cwd: tmp})
	validating, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	previous := hostTerminalRequestHome
	t.Cleanup(func() { hostTerminalRequestHome = previous })
	hostTerminalRequestHome = func(ctx context.Context, profile string) (string, error) {
		close(validating)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return previous(ctx, profile)
	}
	redial := NewWebSocket(&Scope{Type: "websocket", QueryString: "attach=" + url.QueryEscape(token)}, nil, nil)
	resolving := make(chan error, 1)
	go func() {
		_, _, err := registry.resolve(ctx, token, redial, identity, false)
		resolving <- err
	}()
	select {
	case <-validating:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve never validated the profile")
	}
	registry.ReapIdle(ctx, time.Time{})
	if registry.Get(token) != nil {
		t.Fatal("the reaper left the exited shell registered")
	}
	registry.mu.Lock()
	remembered := slices.ContainsFunc(registry.exits, func(e hostExit) bool { return e.token == token })
	registry.mu.Unlock()
	if !remembered {
		t.Fatal("the reaper did not remember the exit")
	}
	releaseOnce.Do(func() { close(release) })
	err = <-resolving
	expired, ok := asTerminalExpired(err)
	if !ok || !expired.exited {
		t.Fatalf("resolve returned %v, want TerminalExited", err)
	}
}

// test_host_terminal_redraw.py test_the_step_back_is_applied_only_after_output_that_follows_the_step
func TestTheStepBackIsAppliedOnlyAfterOutputThatFollowsTheStep(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	previous := hostResizeAnswer
	hostResizeAnswer = 60 * time.Second
	t.Cleanup(func() { hostResizeAnswer = previous })
	ctx := context.Background()
	bridge := newHostFakeBridge()
	session := tools.NewPtySession("terminal", bridge, tools.PtySessionOptions{BufferCap: 1 << 16, ReadTimeout: 10 * time.Millisecond})
	tab := &hostTab{}
	session.Start()
	defer func() { _ = session.Close(ctx) }()
	if ok, err := session.Attach(ctx, tab, tools.PtyAttachOptions{}); err != nil || !ok {
		t.Fatalf("attach %v %v", ok, err)
	}
	bridge.printed <- []byte("before the step")
	hostWaitUntil(t, func() bool { return tab.has("before the step") }, 10*time.Second)
	undo, err := hostResize(ctx, session, tab, [2]int{80, 23}, nil)
	if err != nil {
		t.Fatal(err)
	}
	back := make(chan error, 1)
	go func() {
		_, err := hostResize(ctx, session, tab, [2]int{80, 24}, undo)
		back <- err
	}()
	select {
	case <-back:
		t.Fatal("the step back was applied before the program answered")
	case <-time.After(200 * time.Millisecond):
	}
	if got := bridge.sizesNow(); !slices.Equal(got, [][2]int{{80, 24}, {80, 23}}) {
		t.Fatalf("sizes %v", got)
	}
	bridge.printed <- []byte("redrawn at 23 rows")
	select {
	case err := <-back:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the step back never applied")
	}
	if got := bridge.sizesNow(); !slices.Equal(got, [][2]int{{80, 24}, {80, 23}, {80, 24}}) {
		t.Fatalf("sizes %v", got)
	}
}

// Session stand in recording the waits a resize asks for
type hostScriptedSession struct {
	current   [2]int
	knowsSize bool
	waits     [][2]any
}

func (s *hostScriptedSession) Size() (int, int, bool) {
	return s.current[0], s.current[1], s.knowsSize
}

func (s *hostScriptedSession) Resize(_ tools.PtyViewer, cols, rows int) {
	s.current = [2]int{cols, rows}
}

func (s *hostScriptedSession) Outputs() int { return 7 }

func (s *hostScriptedSession) WaitForOutput(_ context.Context, since int, timeout time.Duration) error {
	s.waits = append(s.waits, [2]any{since, timeout})
	return nil
}

// test_host_terminal_redraw.py test_only_a_resize_that_undoes_the_previous_one_waits
func TestOnlyAResizeThatUndoesThePreviousOneWaits(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, knowsSize := range []bool{true, false} {
		session := &hostScriptedSession{current: [2]int{80, 24}, knowsSize: knowsSize}
		var undo *hostResizeUndo
		for _, step := range [][2]int{{80, 23}, {80, 24}, {100, 30}, {100, 30}, {80, 24}, {90, 24}} {
			var err error
			if undo, err = hostResize(context.Background(), session, nil, step, undo); err != nil {
				t.Fatal(err)
			}
			if session.current != step {
				t.Fatalf("size %v want %v", session.current, step)
			}
		}
		var want [][2]any
		if knowsSize {
			want = [][2]any{{7, hostResizeAnswer}}
		}
		if !slices.Equal(session.waits, want) {
			t.Fatalf("knows_size=%v waits %v want %v", knowsSize, session.waits, want)
		}
	}
}
