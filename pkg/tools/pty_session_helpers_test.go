package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Bridge contract fake whose chunks are output, an empty chunk an idle tick and a nil chunk end of file
type fakePtyBridge struct {
	mu          sync.Mutex
	chunks      [][]byte
	written     []byte
	writeResult bool
	closed      bool
	resized     [2]int
	dead        bool
	pid         int
	writeHook   func(ctx context.Context, data []byte) (bool, error)
	closeHook   func()
}

func newFakePtyBridge(chunks ...[]byte) *fakePtyBridge {
	return &fakePtyBridge{chunks: chunks, writeResult: true}
}

func (b *fakePtyBridge) Pid() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pid
}

func (b *fakePtyBridge) IsAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.dead
}

func (b *fakePtyBridge) Read(timeout time.Duration) ([]byte, error) {
	b.mu.Lock()
	if len(b.chunks) == 0 {
		b.mu.Unlock()
		time.Sleep(timeout)
		return []byte{}, nil
	}
	c := b.chunks[0]
	b.chunks = b.chunks[1:]
	b.mu.Unlock()
	if c == nil {
		return nil, nil
	}
	return append([]byte{}, c...), nil
}

func (b *fakePtyBridge) push(chunk []byte) {
	b.mu.Lock()
	b.chunks = append(b.chunks, chunk)
	b.mu.Unlock()
}

func (b *fakePtyBridge) Write(ctx context.Context, data []byte, _ time.Duration, _ PtyWriteFence) (bool, error) {
	if b.writeHook != nil {
		return b.writeHook(ctx, data)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writeResult {
		b.written = append(b.written, data...)
	}
	return b.writeResult, nil
}

func (b *fakePtyBridge) appendWritten(data []byte) {
	b.mu.Lock()
	b.written = append(b.written, data...)
	b.mu.Unlock()
}

func (b *fakePtyBridge) writtenBytes() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.written)
}

func (b *fakePtyBridge) Resize(cols, rows int) {
	b.mu.Lock()
	b.resized = [2]int{cols, rows}
	b.mu.Unlock()
}

func (b *fakePtyBridge) Close() error {
	if b.closeHook != nil {
		b.closeHook()
	}
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func (b *fakePtyBridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// One frame a fake viewer received
type ptySent struct {
	kind    string
	payload string
}

// Viewer fake recording frames, with optional hooks for failing, slow and narrow links
type fakePtyViewer struct {
	mu          sync.Mutex
	sent        []ptySent
	closeCode   int
	closeReason string
	sendBytes   func(ctx context.Context, data []byte) error
	sendText    func(ctx context.Context, text string) error
	closeHook   func(ctx context.Context) error
}

func (v *fakePtyViewer) record(kind, payload string) {
	v.mu.Lock()
	v.sent = append(v.sent, ptySent{kind, payload})
	v.mu.Unlock()
}

func (v *fakePtyViewer) SendBytes(ctx context.Context, data []byte) error {
	if v.sendBytes != nil {
		if err := v.sendBytes(ctx, data); err != nil {
			return err
		}
	}
	v.record("bytes", string(data))
	return nil
}

func (v *fakePtyViewer) SendText(ctx context.Context, text string) error {
	if v.sendText != nil {
		if err := v.sendText(ctx, text); err != nil {
			return err
		}
	}
	v.record("text", text)
	return nil
}

func (v *fakePtyViewer) Close(ctx context.Context, code int, reason string) error {
	if v.closeHook != nil {
		if err := v.closeHook(ctx); err != nil {
			return err
		}
	}
	v.mu.Lock()
	v.closeCode, v.closeReason = code, reason
	v.mu.Unlock()
	return nil
}

func (v *fakePtyViewer) frames() []ptySent {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ptySent{}, v.sent...)
}

func (v *fakePtyViewer) closed() (int, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closeCode, v.closeReason
}

// Concatenated binary frames
func (v *fakePtyViewer) bytesSent() string {
	out := ""
	for _, f := range v.frames() {
		if f.kind == "bytes" {
			out += f.payload
		}
	}
	return out
}

// Viewer whose client dropped, every send raising after an optional hold
type failingPtyViewer struct {
	fakePtyViewer
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func newFailingPtyViewer(hold bool) *failingPtyViewer {
	v := &failingPtyViewer{started: make(chan struct{}), release: make(chan struct{})}
	v.sendBytes = func(ctx context.Context, _ []byte) error {
		v.startedOnce.Do(func() { close(v.started) })
		if hold {
			select {
			case <-v.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return errors.New("socket closed")
	}
	return v
}

// Viewer over a link that needs seconds per frame
func newSlowLinkViewer(d time.Duration) *fakePtyViewer {
	return &fakePtyViewer{sendBytes: func(ctx context.Context, _ []byte) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

// Viewer whose socket buffers take capacity bytes and never drain a larger frame
func newNarrowLinkViewer(capacity int) *fakePtyViewer {
	return &fakePtyViewer{sendBytes: func(ctx context.Context, data []byte) error {
		if len(data) > capacity {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
}

// Sets a package variable for the test and restores it afterwards
func setPtyVar[T any](t *testing.T, target *T, value T) {
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}

// Fails the test unless cond turns true within fifteen seconds
func ptyWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Registry with the test defaults of make_registry
func newTestPtyRegistry(ttl time.Duration, maxSessions int) *PtySessionRegistry {
	return NewPtySessionRegistry(PtySessionRegistryOptions{TTL: ttl, MaxSessions: maxSessions, BufferCap: 1024, ReadTimeout: 10 * time.Millisecond})
}

// Session with a 1 KiB buffer over bridge
func newTestPtySession(key string, bridge TerminalBridge, bufferCap int) *PtySession {
	return NewPtySession(key, bridge, PtySessionOptions{BufferCap: bufferCap, ReadTimeout: 10 * time.Millisecond})
}

// Spawner returning bridge
func spawnBridge(bridge TerminalBridge) func() (TerminalBridge, error) {
	return func() (TerminalBridge, error) { return bridge, nil }
}
