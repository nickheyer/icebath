package app

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Reader that counts every read so a case can prove stdin stayed untouched
type countingReader struct{ reads atomic.Int32 }

func (r *countingReader) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, nil
}

func TestBareHatchUsesThreadAwarePromptNotRawInput(t *testing.T) {
	c, app := newModalTestCLI(t, nil)
	in := &countingReader{}
	out := &bytes.Buffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: in, Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}, out))
	c.handleHatchCommand("/hatch")
	if in.reads.Load() != 0 {
		t.Fatalf("raw stdin read %d times from the hatch handler", in.reads.Load())
	}
	app.mu.Lock()
	invalidated := app.invalidated
	app.mu.Unlock()
	if invalidated == 0 {
		t.Fatal("expected /hatch to route through the thread aware prompt")
	}
	if got := strings.TrimSpace(out.String()); got != c.commandsT("hatch.usage", nil) {
		t.Fatalf("output %q", got)
	}
}
