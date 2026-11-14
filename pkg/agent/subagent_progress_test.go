package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type progressCall struct {
	event, name string
	preview     any
}

type progressRecorder struct {
	mu    sync.Mutex
	calls []progressCall
}

func (r *progressRecorder) cb(event, name string, preview any, _ any, _ map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, progressCall{event, name, preview})
}

func (r *progressRecorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.event
	}
	return out
}

func (r *progressRecorder) last() progressCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// Spinner writing to buf that reports running without an animation goroutine
func bufferedRunningSpinner(message string, buf *bytes.Buffer) *kawaiiSpinner {
	s := newKawaiiSpinner(context.Background(), message, "dots", nil)
	s.out = buf
	s.running = true
	return s
}

func recordingParent(spinner *kawaiiSpinner) (*AIAgent, *progressRecorder) {
	parent, rec := &AIAgent{}, &progressRecorder{}
	if spinner != nil {
		parent.delegateSpinner = spinner
	} else {
		parent.toolProgressCallback = rec.cb
	}
	return parent, rec
}

func childCallback(taskIndex int, goal string, parent *AIAgent, taskCount int) *flushingProgressCallback {
	return buildChildProgressCallback(taskIndex, goal, parent, taskCount, "", "", nil, "", nil, nil, nil).(*flushingProgressCallback)
}

func toolStarted(cb *flushingProgressCallback, name, preview string) {
	callProgressShape(cb, "tool.started", name, preview, map[string]any{}, nil)
}

func repeated(event string, n int) string {
	return strings.TrimSuffix(strings.Repeat(event+",", n), ",")
}

func TestPrintAboveWithSpinnerRunning(t *testing.T) {
	var buf bytes.Buffer
	spinner := bufferedRunningSpinner("test", &buf)
	spinner.PrintAbove("tool line")
	if out := buf.String(); !strings.Contains(out, "tool line") || !strings.Contains(out, "\r") {
		t.Fatalf("output %q", out)
	}
}

func TestPrintAboveUsesCapturedStdout(t *testing.T) {
	var buf, redirected bytes.Buffer
	spinner := newKawaiiSpinner(context.Background(), "test", "dots", nil)
	spinner.out = &buf
	prev := statusStdout
	statusStdout = &redirected
	func() {
		defer func() { statusStdout = prev }()
		spinner.PrintAbove("should go to buf")
	}()
	if !strings.Contains(buf.String(), "should go to buf") || redirected.Len() != 0 {
		t.Fatalf("buf %q redirected %q", buf.String(), redirected.String())
	}
}

func TestBuildChildProgressCallbackGatewayBatchedProgress(t *testing.T) {
	parent, rec := recordingParent(nil)
	cb := childCallback(0, "test goal", parent, 1)
	for i := range 4 {
		toolStarted(cb, fmt.Sprintf("tool_%d", i), fmt.Sprintf("arg_%d", i))
	}
	if got := strings.Join(rec.events(), ","); got != repeated("subagent.tool", 4) {
		t.Fatalf("events %s", got)
	}
	toolStarted(cb, "tool_4", "arg_4")
	if got := strings.Join(rec.events(), ","); got != repeated("subagent.tool", 5)+",subagent.progress" {
		t.Fatalf("events %s", got)
	}
	summary := fmt.Sprint(rec.last().preview)
	if !strings.Contains(summary, "tool_0") || !strings.Contains(summary, "tool_4") {
		t.Fatalf("summary %q", summary)
	}
}

func TestBuildChildProgressCallbackParallelCallbacksIndependent(t *testing.T) {
	parent, rec := recordingParent(nil)
	cb0 := childCallback(0, "goal a", parent, 1)
	cb1 := childCallback(1, "goal b", parent, 1)
	for i := range 3 {
		toolStarted(cb0, fmt.Sprintf("tool_%d", i), fmt.Sprintf("a_%d", i))
		toolStarted(cb1, fmt.Sprintf("other_%d", i), fmt.Sprintf("b_%d", i))
	}
	events := rec.events()
	tools, progress := 0, 0
	for _, e := range events {
		switch e {
		case "subagent.tool":
			tools++
		case "subagent.progress":
			progress++
		}
	}
	if tools != 6 || progress != 0 {
		t.Fatalf("events %v", events)
	}
}

func TestBuildChildProgressCallbackTaskIndexPrefixInBatchMode(t *testing.T) {
	freshBatchOrdinals(t)
	var buf bytes.Buffer
	parent, _ := recordingParent(bufferedRunningSpinner("delegating", &buf))
	cb0 := childCallback(0, "test goal", parent, 3)
	toolStarted(cb0, "web_search", "test")
	if out := buf.String(); !strings.Contains(out, "[1/3]") {
		t.Fatalf("output %q", out)
	}
	buf.Reset()
	cb2 := childCallback(2, "test goal", parent, 3)
	toolStarted(cb2, "web_search", "test")
	if out := buf.String(); !strings.Contains(out, "[3/3]") {
		t.Fatalf("output %q", out)
	}
}

func TestBatchFlushFlushSendsRemainingBatch(t *testing.T) {
	parent, rec := recordingParent(nil)
	cb := childCallback(0, "test goal", parent, 1)
	toolStarted(cb, "web_search", "query1")
	toolStarted(cb, "read_file", "file.txt")
	toolStarted(cb, "write_file", "out.txt")
	if got := strings.Join(rec.events(), ","); got != repeated("subagent.tool", 3) {
		t.Fatalf("events %s", got)
	}
	cb.flush()
	events := rec.events()
	if events[len(events)-1] != "subagent.progress" {
		t.Fatalf("events %v", events)
	}
	summary := fmt.Sprint(rec.last().preview)
	if !strings.Contains(summary, "web_search") || !strings.Contains(summary, "write_file") {
		t.Fatalf("summary %q", summary)
	}
}

func TestBatchFlushFlushNoopWhenBatchEmpty(t *testing.T) {
	parent, rec := recordingParent(nil)
	cb := childCallback(0, "test goal", parent, 1)
	cb.flush()
	if events := rec.events(); len(events) != 0 {
		t.Fatalf("events %v", events)
	}
}

func TestBatchFlushFlushNoopWhenNoParentCallback(t *testing.T) {
	var buf bytes.Buffer
	parent, _ := recordingParent(bufferedRunningSpinner("test", &buf))
	cb := childCallback(0, "test goal", parent, 1)
	toolStarted(cb, "web_search", "test")
	cb.flush()
}
