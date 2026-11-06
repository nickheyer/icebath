package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type wptFake struct {
	mu                 sync.Mutex
	written            []string
	release            chan struct{}
	releaseOnce        sync.Once
	releaseOnTerminate bool
	blockFor           time.Duration
	started, finished  chan struct{}
	terminated         chan struct{}
	startOnce, finOnce sync.Once
	termOnce           sync.Once
}

func newWptFake(blockFor time.Duration, releaseOnTerminate bool) *wptFake {
	f := &wptFake{blockFor: blockFor, releaseOnTerminate: releaseOnTerminate, started: make(chan struct{}),
		finished: make(chan struct{}), terminated: make(chan struct{})}
	if blockFor > 0 {
		f.release = make(chan struct{})
	}
	return f
}

func (f *wptFake) Pid() int                           { return 1 }
func (f *wptFake) IsAlive() bool                      { return true }
func (f *wptFake) Read(time.Duration) ([]byte, error) { return nil, nil }
func (f *wptFake) SetWinSize(rows, cols int) error    { return nil }
func (f *wptFake) unblock()                           { f.releaseOnce.Do(func() { close(f.release) }) }
func (f *wptFake) isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (f *wptFake) WriteText(text string) error {
	f.startOnce.Do(func() { close(f.started) })
	if f.release != nil {
		select {
		case <-f.release:
		case <-time.After(f.blockFor):
		}
	}
	f.mu.Lock()
	f.written = append(f.written, text)
	f.mu.Unlock()
	f.finOnce.Do(func() { close(f.finished) })
	return nil
}

func (f *wptFake) Terminate(force bool) error {
	f.termOnce.Do(func() { close(f.terminated) })
	if f.releaseOnTerminate && f.release != nil {
		f.unblock()
	}
	return nil
}

func (f *wptFake) writtenNow() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.written...)
}

func wptGrace(t *testing.T, d time.Duration) {
	prev := winPtyWriteShutdownGrace
	winPtyWriteShutdownGrace = d
	t.Cleanup(func() { winPtyWriteShutdownGrace = prev })
}

func wptWaitUntil(t *testing.T, cond func() bool, within time.Duration) {
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type wptResult struct {
	ok  bool
	err error
}

func wptWrite(b *WinPtyBridge, ctx context.Context, data string, timeout time.Duration, fence PtyWriteFence) chan wptResult {
	ch := make(chan wptResult, 1)
	go func() {
		ok, err := b.Write(ctx, []byte(data), timeout, fence)
		ch <- wptResult{ok, err}
	}()
	return ch
}

func TestWinPtyWriteHasNonblockingAsyncContract(t *testing.T) {
	f := newWptFake(0, false)
	b := newWinPtyBridge(f)
	if ok, err := b.Write(context.Background(), []byte("hello"), PtyWriteTimeout, nil); !ok || err != nil {
		t.Fatalf("write %v %v", ok, err)
	}
	if w := f.writtenNow(); len(w) != 1 || w[0] != "hello" {
		t.Fatalf("written %v", w)
	}
}

func TestWinPtyWriteTimeoutTerminatesConptyAndReapsWorker(t *testing.T) {
	f := newWptFake(2*time.Second, true)
	b := newWinPtyBridge(f)
	if ok, _ := b.Write(context.Background(), []byte("blocked"), 10*time.Millisecond, nil); ok {
		t.Fatal("stalled write succeeded")
	}
	if !f.isClosed(f.started) || !f.isClosed(f.terminated) || !f.isClosed(f.finished) || !b.closed.Load() {
		t.Fatal("stalled write was not torn down and reaped")
	}
}

func TestWinPtyCancelledWriteKeepsHealthyConptyAlive(t *testing.T) {
	f := newWptFake(2*time.Second, false)
	b := newWinPtyBridge(f)
	ctx, cancel := context.WithCancel(context.Background())
	res := wptWrite(b, ctx, "x", PtyWriteTimeout, nil)
	time.Sleep(50 * time.Millisecond)
	cancel()
	f.unblock()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("result %+v", r)
	}
	if f.isClosed(f.terminated) || b.closed.Load() {
		t.Fatal("healthy conpty terminated after cancel")
	}
}

func TestWinPtyCancelledWriteThatNeverLandsTerminatesConpty(t *testing.T) {
	wptGrace(t, 50*time.Millisecond)
	f := newWptFake(5*time.Second, true)
	b := newWinPtyBridge(f)
	ctx, cancel := context.WithCancel(context.Background())
	res := wptWrite(b, ctx, "x", PtyWriteTimeout, nil)
	time.Sleep(20 * time.Millisecond)
	cancel()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("result %+v", r)
	}
	if !f.isClosed(f.terminated) || !b.closed.Load() {
		t.Fatal("wedged conpty survived")
	}
}

type wptLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wptLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestWinPtyLeakedWriteWorkerIsLogged(t *testing.T) {
	wptGrace(t, 50*time.Millisecond)
	logs := &wptLogBuf{}
	winPtyLog = slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { winPtyLog = nil })
	f := newWptFake(5*time.Second, false)
	b := newWinPtyBridge(f)
	if ok, _ := b.Write(context.Background(), []byte("x"), 10*time.Millisecond, nil); ok {
		t.Fatal("stuck write succeeded")
	}
	f.unblock()
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if !strings.Contains(logs.buf.String(), "thread leaked") || !strings.Contains(logs.buf.String(), "level=WARN") {
		t.Fatalf("log %q", logs.buf.String())
	}
}

func TestWinPtyWaitingForTheFenceDoesNotCountAgainstTheWriteDeadline(t *testing.T) {
	f := newWptFake(0, false)
	b := newWinPtyBridge(f)
	leaseFree, waiting := make(chan struct{}), make(chan struct{})
	fence := func(attempt PtyWriteAttempt) (int, bool, error) {
		close(waiting)
		select {
		case <-leaseFree:
		case <-time.After(5 * time.Second):
		}
		return attempt()
	}
	res := wptWrite(b, context.Background(), "keys", 50*time.Millisecond, fence)
	<-waiting
	select {
	case r := <-res:
		t.Fatalf("write finished while waiting for the fence %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	close(leaseFree)
	if r := <-res; !r.ok || r.err != nil {
		t.Fatalf("result %+v", r)
	}
	if w := f.writtenNow(); len(w) != 1 || w[0] != "keys" || f.isClosed(f.terminated) {
		t.Fatalf("written %v", w)
	}
}

func TestWinPtyAFenceThatRaisesPropagatesAndKeepsConpty(t *testing.T) {
	f := newWptFake(0, false)
	b := newWinPtyBridge(f)
	busy := errors.New("profile lease busy")
	fence := func(PtyWriteAttempt) (int, bool, error) { return 0, false, busy }
	if _, err := b.Write(context.Background(), []byte("keys"), PtyWriteTimeout, fence); !errors.Is(err, busy) {
		t.Fatalf("error %v", err)
	}
	if len(f.writtenNow()) != 0 || f.isClosed(f.terminated) || b.closed.Load() {
		t.Fatal("refused fence touched conpty")
	}
}

func TestWinPtyAWriteWedgedInsideTheFenceTerminatesConptyAndFreesTheFence(t *testing.T) {
	f := newWptFake(5*time.Second, true)
	b := newWinPtyBridge(f)
	var lease sync.Mutex
	fence := func(attempt PtyWriteAttempt) (int, bool, error) {
		lease.Lock()
		defer lease.Unlock()
		return attempt()
	}
	if ok, _ := b.Write(context.Background(), []byte("x"), 50*time.Millisecond, fence); ok {
		t.Fatal("wedged write succeeded")
	}
	if !f.isClosed(f.terminated) || !b.closed.Load() {
		t.Fatal("wedged conpty survived")
	}
	if !lease.TryLock() {
		t.Fatal("fence lease still held")
	}
	lease.Unlock()
}

func TestWinPtyAWriteCancelledWhileWaitingForTheFenceLandsAndKeepsConpty(t *testing.T) {
	wptGrace(t, 50*time.Millisecond)
	f := newWptFake(0, false)
	b := newWinPtyBridge(f)
	leaseFree, waiting := make(chan struct{}), make(chan struct{})
	fence := func(attempt PtyWriteAttempt) (int, bool, error) {
		close(waiting)
		select {
		case <-leaseFree:
		case <-time.After(5 * time.Second):
		}
		return attempt()
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := wptWrite(b, ctx, "keys", 50*time.Millisecond, fence)
	<-waiting
	cancel()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("result %+v", r)
	}
	if f.isClosed(f.terminated) {
		t.Fatal("terminated before the lease freed")
	}
	close(leaseFree)
	wptWaitUntil(t, func() bool {
		w := f.writtenNow()
		return len(w) == 1 && w[0] == "keys" && b.lateWrites.Load() == 0
	}, 5*time.Second)
	if f.isClosed(f.terminated) || b.closed.Load() {
		t.Fatal("late write tore conpty down")
	}
}

func TestWinPtyResizeAndReadAfterClose(t *testing.T) {
	f := newWptFake(0, false)
	b := newWinPtyBridge(f)
	if got, _ := b.Read(10 * time.Millisecond); got == nil || len(got) != 0 {
		t.Fatalf("idle read %v", got)
	}
	b.Close()
	b.Close()
	b.Resize(100, 40)
	if got, _ := b.Read(10 * time.Millisecond); got != nil {
		t.Fatal("read after close")
	}
	if ok, _ := b.Write(context.Background(), []byte("x"), PtyWriteTimeout, nil); ok {
		t.Fatal("write after close")
	}
}
