package tuiserver

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stdio transport over a real OS pipe whose reader decodes every frame
type fanoutPipeClient struct {
	transport *tuihost.StdioTransport
	writer    *os.File
	closed    atomic.Bool
	frames    chan map[string]any
}

func newFanoutPipeClient(t *testing.T) *fanoutPipeClient {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &fanoutPipeClient{writer: writer, frames: make(chan map[string]any, 1024)}
	c.transport = tuihost.NewStdioTransport(func() io.Writer { return c.writer }, &sync.Mutex{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				c.frames <- frame
			}
		}
	}()
	t.Cleanup(func() {
		writer.Close()
		<-done
		reader.Close()
	})
	return c
}

func (c *fanoutPipeClient) Write(obj *pyjson.Object) bool { return c.transport.Write(obj) }
func (c *fanoutPipeClient) Close()                        { c.closed.Store(true) }
func (c *fanoutPipeClient) Closed() bool                  { return c.closed.Load() }

func (c *fanoutPipeClient) receive(t *testing.T) map[string]any {
	t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived within five seconds")
		return nil
	}
}

// Whether no frame is waiting once in flight writes had a moment to land
func (c *fanoutPipeClient) empty() bool {
	time.Sleep(50 * time.Millisecond)
	return len(c.frames) == 0
}
