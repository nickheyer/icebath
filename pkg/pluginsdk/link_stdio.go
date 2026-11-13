//go:build !wasip1

package pluginsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
)

// Frames as lines on the standard streams of an external program
type stdioLink struct {
	out     *os.File
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan []byte
}

func (l *stdioLink) form() string { return "program" }

func (l *stdioLink) serveContext(any) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func (l *stdioLink) write(frame []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_, err := l.out.Write(append(frame, '\n'))
	return err
}

func (l *stdioLink) roundTrip(ctx context.Context, id int64, frame []byte) ([]byte, error) {
	reply := make(chan []byte, 1)
	l.mu.Lock()
	l.pending[id] = reply
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()
	if err := l.write(frame); err != nil {
		return nil, err
	}
	select {
	case data := <-reply:
		if data == nil {
			return nil, errors.New("icebath closed the plugin channel")
		}
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Serves the plugin wire on standard input and output until icebath closes it, then exits
func Main() {
	l := &stdioLink{out: os.Stdout, pending: map[int64]chan []byte{}}
	os.Stdout = os.Stderr
	p := newPeer(l)
	reader := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if frame := bytes.TrimSpace(line); len(frame) > 0 {
			l.route(p, frame)
		}
		if err != nil {
			break
		}
	}
	l.mu.Lock()
	for id, reply := range l.pending {
		close(reply)
		delete(l.pending, id)
	}
	l.mu.Unlock()
	_, _ = p.dispatch(context.Background(), "shutdown", map[string]any{})
	os.Exit(0)
}

// Hands a reply to its waiting request and serves a request on its own goroutine
func (l *stdioLink) route(p *peer, frame []byte) {
	var head struct {
		ID     json.Number `json:"id"`
		Method *string     `json:"method"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return
	}
	if head.Method == nil {
		id, err := head.ID.Int64()
		if err != nil {
			return
		}
		l.mu.Lock()
		reply := l.pending[id]
		delete(l.pending, id)
		l.mu.Unlock()
		if reply != nil {
			reply <- frame
		}
		return
	}
	method := *head.Method
	go func() {
		reply := p.serve(frame)
		if reply != nil {
			_ = l.write(reply)
		}
		if method == "shutdown" {
			os.Exit(0)
		}
	}()
}
