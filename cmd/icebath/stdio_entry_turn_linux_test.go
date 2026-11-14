//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// A running stdio gateway child with the pipe its requests go into
type stdioEntryChild struct {
	process   *exec.Cmd
	stdin     io.WriteCloser
	exited    chan error
	stopped   bool
	stderr    string
	home      string
	mu        sync.Mutex
	malformed []string
}

// Stderr of the child and the failure lines of its home logs
func (c *stdioEntryChild) logs() string {
	errText, _ := os.ReadFile(c.stderr)
	c.mu.Lock()
	malformed := strings.Join(c.malformed, "\n")
	c.mu.Unlock()
	return "--- stderr\n" + string(errText) + "--- malformed stdout lines\n" + malformed + "\n--- home logs\n" + homeFailureLines(c.home)
}

// Boots the binary as the stdio gateway entry and routes the JSON lines of its stdout
func startStdioEntryChild(t *testing.T, h, user string, peer *modelPeer) (*stdioEntryChild, *rpcStream) {
	t.Helper()
	child := &stdioEntryChild{process: exec.Command(os.Args[0], tuigateway.TUIGatewayEntry), exited: make(chan error, 1),
		stderr: filepath.Join(t.TempDir(), "stderr.log"), home: h}
	child.process.Env = serveTurnEnv(h, user, peer)
	errOut, err := os.Create(child.stderr)
	if err != nil {
		t.Fatal(err)
	}
	child.process.Stderr = errOut
	if child.stdin, err = child.process.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := child.process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.process.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	drained := make(chan struct{})
	read := func() (map[string]any, error) {
		for lines.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(lines.Bytes(), &frame); err != nil {
				child.mu.Lock()
				child.malformed = append(child.malformed, string(lines.Bytes()[:min(len(lines.Bytes()), 400)]))
				child.mu.Unlock()
				continue
			}
			return frame, nil
		}
		close(drained)
		if err := lines.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	go func() {
		<-drained
		child.exited <- child.process.Wait()
		errOut.Close()
	}()
	var writeMu sync.Mutex
	send := func(frame map[string]any) error {
		raw, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = child.stdin.Write(append(raw, '\n'))
		return err
	}
	t.Cleanup(func() {
		if child.stopped {
			return
		}
		_ = child.process.Process.Kill()
		select {
		case <-child.exited:
		case <-time.After(15 * time.Second):
		}
	})
	return child, newRPCStream(read, send, child.logs)
}

// Fails when the child wrote anything but JSON RPC frames on its stdout
func (c *stdioEntryChild) requireOnlyFrames(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.malformed) > 0 {
		t.Fatalf("stdout carried %d lines that are not JSON frames:\n%s", len(c.malformed), strings.Join(c.malformed, "\n"))
	}
}

// Closes stdin, the goodbye of the TUI parent, and fails unless the child exits with status 0
func (c *stdioEntryChild) closeAndWait(t *testing.T) {
	t.Helper()
	if err := c.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.exited:
		c.stopped = true
		if err != nil {
			t.Fatalf("entry exit = %v\n%s", err, c.logs())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the entry did not exit on stdin EOF\n%s", c.logs())
	}
}

func TestStdioEntryRunsATurnOverItsPipes(t *testing.T) {
	peer := newModelPeer(t)
	h, user := serveTurnHomes(t, peer)
	child, stream := startStdioEntryChild(t, h, user, peer)
	driveServedTurn(t, stream, stream)
	if got := peer.chatRequests(); got != 1 {
		t.Fatalf("model requests = %d", got)
	}
	child.closeAndWait(t)
	child.requireOnlyFrames(t)
}
