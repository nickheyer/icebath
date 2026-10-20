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

var call = protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}

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
	custom.Format = func(c protocol.ToolCall) string { return "RUN " + c.Name }
	out.Reset()
	custom.Ask(ctx, call)
	if !strings.HasPrefix(out.String(), "RUN Bash\n") {
		t.Fatalf("%q", out.String())
	}
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
