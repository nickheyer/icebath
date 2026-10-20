package tty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

var call = policy.Request{Call: protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}}

func TestAnswers(t *testing.T) {
	var out bytes.Buffer
	a := New(strings.NewReader("y\nNO\nwhat\nalways\nv\n\n"), &out)
	ctx := context.Background()
	want := []policy.Verdict{policy.Yes, policy.No, policy.Always, policy.Never, policy.No}
	for i, w := range want {
		v, err := a.Ask(ctx, call)
		if err != nil || v != w {
			t.Fatalf("%d: %v %v", i, v, err)
		}
	}
	if _, err := a.Ask(ctx, call); !errors.Is(err, io.EOF) {
		t.Fatalf("%v", err)
	}
	text := out.String()
	if !strings.Contains(text, "Bash wants to run with arguments:\n  {\n    \"command\": \"ls\"\n  }") || strings.Count(text, prompt) != 7 || strings.Count(text, "unrecognised") != 1 {
		t.Fatalf("%q", text)
	}
	custom := New(strings.NewReader("y\n"), &out)
	custom.Format = func(r policy.Request) string { return "RUN " + r.Call.Name }
	out.Reset()
	custom.Ask(ctx, call)
	if !strings.HasPrefix(out.String(), "RUN Bash\n") {
		t.Fatalf("%q", out.String())
	}
	rich := New(strings.NewReader("y\n"), &out)
	out.Reset()
	rich.Ask(ctx, policy.Request{Call: call.Call, Agent: "root", Reason: "default", Key: "Bash(ls)", Tool: &protocol.Tool{Description: "Runs a command\nmore"}, Review: &policy.Review{By: "judge", Decision: policy.Ask, Reason: "unsure"}})
	text = out.String()
	for _, want := range []string{"[root] Bash wants to run", "tool: Runs a command\n", "asked because: default", "judge said ask: unsure", "always or never remembers: Bash(ls)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestCloseStopsAsking(t *testing.T) {
	pr, pw := io.Pipe()
	a := New(pr, io.Discard)
	done := make(chan error, 1)
	go func() {
		_, err := a.Ask(context.Background(), call)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close must wake a pending ask")
	}
	if _, err := a.Ask(context.Background(), call); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	if a.Close() != nil {
		t.Fatal("double close")
	}
	pw.Close()
}

func TestCancelAndReadError(t *testing.T) {
	pr, pw := io.Pipe()
	a := New(pr, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Ask(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	pw.Write([]byte("y\n"))
	time.Sleep(50 * time.Millisecond)
	go func() {
		time.Sleep(50 * time.Millisecond)
		pw.Write([]byte("n\n"))
	}()
	if v, err := a.Ask(context.Background(), call); err != nil || v != policy.No {
		t.Fatalf("an answer typed to a cancelled prompt must not answer the next one: %v %v", v, err)
	}
	pw.CloseWithError(errors.New("terminal gone"))
	if _, err := a.Ask(context.Background(), call); err == nil || !strings.Contains(err.Error(), "terminal gone") {
		t.Fatalf("%v", err)
	}
	if _, err := a.Ask(context.Background(), call); err == nil || !strings.Contains(err.Error(), "terminal gone") {
		t.Fatalf("every ask after input ends must report why: %v", err)
	}
	broken := New(strings.NewReader("y"), io.Discard)
	if v, err := broken.Ask(context.Background(), call); err != nil || v != policy.Yes {
		t.Fatalf("%v %v", v, err)
	}
}
