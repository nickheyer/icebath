package tuigateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Subscriber of a fanout backed by a kernel buffer the test can fill
type kernelClient interface {
	peer() Transport
	receive(t *testing.T) string
	writeCount() int64
	closeReader()
}

// Fills the kernel buffer behind a descriptor and leaves the descriptor blocking
func fillKernelBuffer(t *testing.T, fd int) {
	t.Helper()
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for {
		if _, err := syscall.Write(fd, chunk); err != nil {
			if err == syscall.EAGAIN {
				break
			}
			t.Fatal(err)
		}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
}

// Reads lines of a kernel buffer into a queue until the descriptor closes
func readLines(reader *os.File, frames chan string, done *sync.WaitGroup) {
	defer done.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		frames <- scanner.Text()
	}
}

func receiveLine(t *testing.T, frames chan string) string {
	t.Helper()
	select {
	case line := <-frames:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived within five seconds")
		return ""
	}
}

// Stdio transport over a real pipe, counting the writes the fanout makes
type pipeClient struct {
	transport      *StdioTransport
	reader, writer *os.File
	writes         atomic.Int64
	closed         atomic.Bool
	frames         chan string
	reading        sync.WaitGroup
	readerOnce     sync.Once
}

func newPipeClient(t *testing.T, reading bool) *pipeClient {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	if !reading {
		fillKernelBuffer(t, fds[1])
	}
	c := &pipeClient{reader: os.NewFile(uintptr(fds[0]), "pipe-reader"), writer: os.NewFile(uintptr(fds[1]), "pipe-writer"),
		frames: make(chan string, 1<<16)}
	c.transport = NewStdioTransport(func() io.Writer { return c.writer }, &sync.Mutex{})
	if reading {
		c.reading.Add(1)
		go readLines(c.reader, c.frames, &c.reading)
	}
	t.Cleanup(func() {
		c.writer.Close()
		c.reading.Wait()
		c.closeReader()
	})
	return c
}

func (c *pipeClient) Write(obj *pyjson.Object) bool {
	c.writes.Add(1)
	return c.transport.Write(obj)
}

func (c *pipeClient) Close()                      { c.closed.Store(true) }
func (c *pipeClient) Closed() bool                { return c.closed.Load() }
func (c *pipeClient) peer() Transport             { return c }
func (c *pipeClient) receive(t *testing.T) string { return receiveLine(t, c.frames) }
func (c *pipeClient) writeCount() int64           { return c.writes.Load() }
func (c *pipeClient) closeReader()                { c.readerOnce.Do(func() { c.reader.Close() }) }

// WebSocket transport whose socket send is backed by a kernel socketpair
type socketClient struct {
	*WSTransport
	reader, writer *os.File
	writes         atomic.Int64
	frames         chan string
	reading        sync.WaitGroup
	readerOnce     sync.Once
}

func newSocketClient(t *testing.T, reading bool) *socketClient {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reading {
		fillKernelBuffer(t, fds[1])
	}
	c := &socketClient{reader: os.NewFile(uintptr(fds[0]), "socket-reader"), writer: os.NewFile(uintptr(fds[1]), "socket-writer"),
		frames: make(chan string, 1<<16)}
	c.WSTransport = NewWSTransport(c, "", nil)
	if reading {
		c.reading.Add(1)
		go readLines(c.reader, c.frames, &c.reading)
	}
	t.Cleanup(func() {
		c.WSTransport.Close()
		c.writer.Close()
		c.reading.Wait()
		c.closeReader()
	})
	return c
}

func (c *socketClient) SendText(ctx context.Context, payload string) error {
	c.writes.Add(1)
	_, err := c.writer.WriteString(payload + "\n")
	return err
}

func (c *socketClient) Close(code int) error        { return nil }
func (c *socketClient) peer() Transport             { return c.WSTransport }
func (c *socketClient) receive(t *testing.T) string { return receiveLine(t, c.frames) }
func (c *socketClient) writeCount() int64           { return c.writes.Load() }
func (c *socketClient) closeReader()                { c.readerOnce.Do(func() { c.reader.Close() }) }

func TestBackpressureNeverBlocksLaterFramesOrOtherSubscribers(t *testing.T) {
	clientTypes := map[string]func(t *testing.T, reading bool) kernelClient{
		"PipeClient":   func(t *testing.T, reading bool) kernelClient { return newPipeClient(t, reading) },
		"SocketClient": func(t *testing.T, reading bool) kernelClient { return newSocketClient(t, reading) },
	}
	for _, clientType := range []string{"PipeClient", "SocketClient"} {
		for _, slowFirst := range []bool{true, false} {
			for _, onLoop := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/slow_first=%v/on_loop=%v", clientType, slowFirst, onLoop), func(t *testing.T) {
					setDuration(t, &wsWriteTimeout, 10*time.Millisecond)
					setDuration(t, &wsTokenCoalesce, 0)
					healthy, slow := clientTypes[clientType](t, true), clientTypes[clientType](t, false)
					peers := []Transport{healthy.peer(), slow.peer()}
					if slowFirst {
						peers = []Transport{slow.peer(), healthy.peer()}
					}
					fan := NewFanoutTransport(peers...)
					kinds := []string{"message.start", "message.delta", "message.complete"}
					returned := make(chan bool, 1)
					go func() {
						ok := true
						for _, kind := range kinds {
							ok = fan.Write(wsEvent(kind)) && ok
						}
						returned <- ok
					}()
					closed := false
					defer func() {
						if !closed {
							slow.closeReader()
							fan.Close()
						}
					}()
					select {
					case ok := <-returned:
						if !ok {
							t.Fatal("a fanout write failed")
						}
					case <-time.After(3 * time.Second):
						t.Fatal("slow subscriber blocked the emitting turn")
					}
					for _, kind := range kinds {
						if got, want := healthy.receive(t), frameText(t, wsEvent(kind)); got != want {
							t.Fatalf("frame = %s want %s", got, want)
						}
					}
					for n := range fanoutMaxPendingFrames + 64 {
						frame := pyjson.ObjectOf("params", pyjson.ObjectOf("type", "message.delta", "n", n))
						if !fan.Write(frame) {
							t.Fatal("a fanout write failed")
						}
						if got, want := healthy.receive(t), frameText(t, frame); got != want {
							t.Fatalf("frame = %s want %s", got, want)
						}
						if !fan.Contains(slow.peer()) {
							break
						}
					}
					if fan.Contains(slow.peer()) {
						t.Fatal("slow backlog grew without bound")
					}
					reattach := pyjson.ObjectOf("reattach", true)
					for range 16 {
						if !fan.Attach(slow.peer()) || !fan.Write(reattach) {
							t.Fatal("reattach failed")
						}
						if got, want := healthy.receive(t), frameText(t, reattach); got != want {
							t.Fatalf("frame = %s want %s", got, want)
						}
						fan.Detach(slow.peer())
					}
					if writes := slow.writeCount(); writes != 1 {
						t.Fatalf("reattach spawned more writers behind blocked I/O: %d writes", writes)
					}
					if !fan.Contains(healthy.peer()) || !fan.Write(wsEvent("message.complete")) {
						t.Fatal("the healthy peer lost its subscription")
					}
					if got, want := healthy.receive(t), frameText(t, wsEvent("message.complete")); got != want {
						t.Fatalf("frame = %s want %s", got, want)
					}
					slow.closeReader()
					fan.Close()
					closed = true
					if fan.Write(pyjson.ObjectOf("after", "close")) {
						t.Fatal("a closed fanout accepted a frame")
					}
				})
			}
		}
	}
}
