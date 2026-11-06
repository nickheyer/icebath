package tools

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Bound on a ConPTY write once its bytes reached the console, also applied to a no timeout write
const winPtyWriteTimeout = 10 * time.Second

// Grace a cancelled or terminated ConPTY write gets to land before the console is torn down
var winPtyWriteShutdownGrace = 1 * time.Second

// Logger of the ConPTY bridge, nil meaning the process default
var winPtyLog *slog.Logger

func winPtyLogger() *slog.Logger {
	if winPtyLog != nil {
		return winPtyLog
	}
	return slog.Default()
}

// ConPTY child seen by the bridge, the pywinpty PtyProcess surface
type winPtyChild interface {
	Pid() int
	IsAlive() bool
	// Output waiting at most timeout, nil data without error when nothing became readable
	Read(timeout time.Duration) ([]byte, error)
	WriteText(text string) error
	SetWinSize(rows, cols int) error
	Terminate(force bool) error
}

// WinPtyBridge streams bytes between a ConPTY child and the dashboard terminal with the PtyBridge contract
type WinPtyBridge struct {
	child      winPtyChild
	closed     atomic.Bool
	lateWrites atomic.Int64
}

func newWinPtyBridge(child winPtyChild) *WinPtyBridge { return &WinPtyBridge{child: child} }

// Result of a write running in its worker goroutine, observable by several waiters
type winWriteFuture struct {
	done chan struct{}
	ok   bool
	err  error
}

func (f *winWriteFuture) wait(timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.done:
		return true
	case <-t.C:
		return false
	}
}

func (b *WinPtyBridge) Pid() int { return b.child.Pid() }

func (b *WinPtyBridge) IsAlive() bool { return !b.closed.Load() && b.child.IsAlive() }

// Up to 64 KiB of output within timeout, empty when nothing arrived and nil at end of file or after close
func (b *WinPtyBridge) Read(timeout time.Duration) ([]byte, error) {
	if b.closed.Load() {
		return nil, nil
	}
	data, err := b.child.Read(timeout)
	if err != nil {
		return nil, nil
	}
	if len(data) > 65536 {
		data = data[:65536]
	}
	if data == nil {
		return []byte{}, nil
	}
	return []byte(pystr.EncodeUTF8Replace(pystr.DecodeUTF8Replace(data))), nil
}

// Hands the bytes to ConPTY as text, false once closed or when the console refuses them
func (b *WinPtyBridge) writeBlocking(data []byte) bool {
	if b.closed.Load() {
		return false
	}
	if len(data) == 0 {
		return true
	}
	return b.child.WriteText(pystr.DecodeUTF8Replace(data)) == nil
}

// Writes off the caller goroutine and tears ConPTY down when its input wedges past the deadline
func (b *WinPtyBridge) Write(ctx context.Context, data []byte, timeout time.Duration, fence PtyWriteFence) (bool, error) {
	if b.closed.Load() {
		return false, nil
	}
	if len(data) == 0 {
		return true, nil
	}
	entered := make(chan struct{})
	var once sync.Once
	markEntered := func() { once.Do(func() { close(entered) }) }
	f := &winWriteFuture{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		if fence == nil {
			markEntered()
			f.ok = b.writeBlocking(data)
			return
		}
		_, f.ok, f.err = fence(func() (int, bool, error) {
			markEntered()
			if b.writeBlocking(data) {
				return len(data), true, nil
			}
			return 0, false, nil
		})
	}()
	deadline := winPtyWriteTimeout
	if timeout != PtyNoTimeout {
		deadline = max(0, timeout)
	}
	select {
	case <-entered:
	case <-f.done:
		select {
		case <-entered:
		default:
			return f.ok, f.err
		}
	case <-ctx.Done():
		select {
		case <-entered:
			b.settleOrStopWrite(f)
		default:
			b.boundLateWrite(entered, f, deadline)
		}
		return false, ctx.Err()
	}
	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case <-f.done:
		return f.ok, f.err
	case <-t.C:
		b.stopStalledWrite(f)
		return false, nil
	case <-ctx.Done():
		b.settleOrStopWrite(f)
		return false, ctx.Err()
	}
}

// Holds a write cancelled before its fence admitted it to the deadline once it reaches ConPTY
func (b *WinPtyBridge) boundLateWrite(entered chan struct{}, f *winWriteFuture, deadline time.Duration) {
	b.lateWrites.Add(1)
	go func() {
		defer b.lateWrites.Add(-1)
		select {
		case <-entered:
			if !f.wait(deadline) {
				b.stopStalledWrite(f)
				return
			}
		case <-f.done:
		}
		<-f.done
		if f.err != nil {
			winPtyLogger().Debug("Host terminal write failed after its caller left", "error", f.err)
		}
	}()
}

// Lets a cancelled write finish within the grace window and terminates ConPTY only when it stalls
func (b *WinPtyBridge) settleOrStopWrite(f *winWriteFuture) {
	if f.wait(winPtyWriteShutdownGrace) {
		return
	}
	b.stopStalledWrite(f)
}

// Closes ConPTY and waits for the worker blocked in the write, logging a worker that never returns
func (b *WinPtyBridge) stopStalledWrite(f *winWriteFuture) {
	b.Close()
	if !f.wait(winPtyWriteShutdownGrace) {
		winPtyLogger().Warn("ConPTY write worker did not exit within " + strconv.FormatFloat(winPtyWriteShutdownGrace.Seconds(), 'f', 1, 64) + "s of terminate(); thread leaked")
	}
}

// Forwards a clamped resize to ConPTY, silent after close or on failure
func (b *WinPtyBridge) Resize(cols, rows int) {
	if b.closed.Load() {
		return
	}
	b.child.SetWinSize(clampDimension(rows, ptyMaxRows), clampDimension(cols, ptyMaxCols))
}

// Force terminates the ConPTY child once, later calls do nothing
func (b *WinPtyBridge) Close() error {
	if b.closed.Swap(true) {
		return nil
	}
	b.child.Terminate(true)
	return nil
}
