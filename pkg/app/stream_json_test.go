package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Installs a buffer console and stderr for the test and returns both buffers
func captureStreams(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}, errOut)
	t.Cleanup(restore)
	return out, errOut
}

// Parses every non empty stdout line as a JSON object
func streamEvents(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

type fakeStreamAgent struct{ delta, progress any }

func (f *fakeStreamAgent) SetStreamDeltaCallback(cb any)  { f.delta = cb }
func (f *fakeStreamAgent) SetToolProgressCallback(cb any) { f.progress = cb }

func TestEmitterEventStreamIsValidJsonl(t *testing.T) {
	out, errOut := captureStreams(t)
	emitter := NewStreamJSONEmitter("test-model", "s-1")
	emitter.OnTextDelta("hel")
	emitter.OnTextDelta("\n  ")
	emitter.OnTextDelta("lo")
	emitter.OnTextDelta(nil)
	emitter.OnTextDelta("")
	emitter.OnToolProgress("tool.started", "read_file", "preview", map[string]any{"path": "x"}, map[string]any{"tool_call_id": "call-a"})
	emitter.OnToolProgress("tool.started", "read_file", "preview", map[string]any{"path": "y"}, map[string]any{"tool_call_id": "call-b"})
	emitter.OnToolProgress("reasoning.available", "_thinking", "hmm", nil, nil)
	emitter.OnToolProgress("tool.completed", "read_file", "", nil, map[string]any{"tool_call_id": "call-b", "result": "y"})
	emitter.OnToolProgress("tool.completed", "read_file", "", nil, map[string]any{"tool_call_id": "call-a", "duration": 0.5, "is_error": false, "result": strings.Repeat("x", 6000)})
	code := emitter.EmitResult(map[string]any{"final_response": "", "failed": true, "error": "boom", "input_tokens": 3}, "", 0)

	events := streamEvents(t, out)
	var types []string
	var text []string
	var resultIDs []string
	for _, e := range events {
		types = append(types, e["type"].(string))
		if e["type"] == "text" {
			text = append(text, e["text"].(string))
		}
		if e["type"] == "tool_result" {
			resultIDs = append(resultIDs, e["tool_call_id"].(string))
		}
		if _, ok := e["timestamp"]; !ok {
			t.Fatalf("event without timestamp: %v", e)
		}
	}
	if strings.Join(types, ",") != "system,text,text,text,tool_use,tool_use,tool_result,tool_result,result" {
		t.Fatalf("types %v", types)
	}
	if strings.Join(text, "") != "hel\n  lo" {
		t.Fatalf("text %q", strings.Join(text, ""))
	}
	if events[0]["subtype"] != "init" || events[0]["model"] != "test-model" {
		t.Fatalf("init %v", events[0])
	}
	if input := events[4]["input"].(map[string]any); input["path"] != "x" || events[4]["tool_call_id"] != "call-a" {
		t.Fatalf("tool_use %v", events[4])
	}
	if strings.Join(resultIDs, ",") != "call-b,call-a" {
		t.Fatalf("result ids %v", resultIDs)
	}
	if events[6]["duration_ms"].(float64) >= 500 {
		t.Fatalf("duration %v", events[6]["duration_ms"])
	}
	output := events[7]["output"].(string)
	if events[7]["duration_ms"].(float64) != 500 || !strings.HasSuffix(output, "...") || len(output) != 5003 {
		t.Fatalf("capped result %v", events[7])
	}
	last := events[len(events)-1]
	if code != 1 || last["exit_code"].(float64) != 1 || last["error"] != "boom" || last["session_id"] != "s-1" {
		t.Fatalf("result %v code %d", last, code)
	}
	if last["tokens"].(map[string]any)["input"].(float64) != 3 {
		t.Fatalf("tokens %v", last["tokens"])
	}
	if !strings.Contains(errOut.String(), "\nsession_id: s-1\n") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestEmitterAttachRoutesAgentCallbacksAndBindsSession(t *testing.T) {
	out, _ := captureStreams(t)
	emitter := NewStreamJSONEmitter("m", "")
	agent := &fakeStreamAgent{}
	if emitter.Attach(agent) != emitter || agent.delta == nil || agent.progress == nil {
		t.Fatal("attach left a callback slot empty")
	}
	agent.delta.(func(any))("hi")
	agent.progress.(func(string, string, string, map[string]any, map[string]any))("tool.started", "", "", nil, nil)
	emitter.BindSession("")
	emitter.BindSession("gw-9")
	emitter.EmitResult(nil, "", 0)
	events := streamEvents(t, out)
	if events[0]["session_id"] != "" || events[1]["text"] != "hi" || events[2]["name"] != "unknown" {
		t.Fatalf("events %v", events)
	}
	if _, ok := events[2]["input"]; ok {
		t.Fatalf("input emitted without dict args %v", events[2])
	}
	last := events[len(events)-1]
	if last["session_id"] != "gw-9" || last["exit_code"].(float64) != 0 || last["text"] != "" {
		t.Fatalf("result %v", last)
	}
	if _, ok := last["error"]; ok {
		t.Fatalf("error key without an error %v", last)
	}
}

func TestEmitResultReportsReplayGapAndStringResults(t *testing.T) {
	out, _ := captureStreams(t)
	emitter := NewStreamJSONEmitter("m", "s")
	if code := emitter.EmitResult(map[string]any{"final_response": "done", "replay_gap": true}, "s-2", 3); code != 3 {
		t.Fatalf("code %d", code)
	}
	if code := emitter.EmitResult("plain", "", 0); code != 0 {
		t.Fatalf("code %d", code)
	}
	events := streamEvents(t, out)
	if events[1]["replay_gap"] != true || events[1]["session_id"] != "s-2" || events[1]["text"] != "done" {
		t.Fatalf("replay result %v", events[1])
	}
	if events[2]["text"] != "plain" || events[2]["tokens"].(map[string]any)["total"].(float64) != 0 {
		t.Fatalf("string result %v", events[2])
	}
}

func TestStreamJSONRequestedForcesQuietAndRefusesInteractiveCombinations(t *testing.T) {
	_, errOut := captureStreams(t)
	quiet := false
	if ok, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "text", Quiet: &quiet}); ok || code != 0 || quiet {
		t.Fatalf("text format ok=%v code=%d quiet=%v", ok, code, quiet)
	}
	if ok, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "stream-json", Query: "hello", Quiet: &quiet}); !ok || code != 0 || !quiet {
		t.Fatalf("requested ok=%v code=%d quiet=%v", ok, code, quiet)
	}
	if ok, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "stream-json", QueryFile: "-"}); !ok || code != 0 {
		t.Fatalf("query file ok=%v code=%d", ok, code)
	}
	if _, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "stream-json"}); code != 2 || !strings.Contains(errOut.String(), "requires -q/--query") {
		t.Fatalf("missing query code=%d stderr=%q", code, errOut.String())
	}
	errOut.Reset()
	if _, code := StreamJSONRequested(StreamJSONArgs{OutputFormat: "stream-json", Query: "hi", TUI: true}); code != 2 || !strings.Contains(errOut.String(), "cannot be used with --tui") {
		t.Fatalf("tui code=%d stderr=%q", code, errOut.String())
	}
}

func TestPrintNoninteractiveSetupGuidanceListsTheConfigCommands(t *testing.T) {
	out, _ := captureStreams(t)
	PrintNoninteractiveSetupGuidance("No interactive TTY detected for the first-run setup prompt.")
	text := out.String()
	for _, want := range []string{"Icebath Setup — Non-interactive mode", "No interactive TTY detected", "icebath config set model.provider custom",
		"icebath config set model.base_url http://localhost:8080/v1", "Run 'icebath setup' in an interactive terminal"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if IsInteractiveStdin() {
		t.Fatal("buffer stdin reported as a terminal")
	}
}

func TestConfigureWindowsStdioIsIdempotentAndQuietOffWindows(t *testing.T) {
	stdioMu.Lock()
	stdioConfigured = false
	stdioMu.Unlock()
	if IsWindows() {
		t.Setenv("ICEBATH_DISABLE_WINDOWS_UTF8", "1")
	}
	if ConfigureWindowsStdio() || ConfigureWindowsStdio() {
		t.Fatal("nothing should change off Windows or when opted out")
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	before := os.Getenv("PATH")
	augmentPathWithKnownTools()
	if IsWindows() == false && os.Getenv("PATH") != before {
		t.Fatal("PATH changed off Windows")
	}
	for _, v := range []string{"1", "true", "True", "yes"} {
		t.Setenv("ICEBATH_DISABLE_WINDOWS_UTF8", v)
		if !windowsUTF8Disabled() {
			t.Fatalf("%q should opt out", v)
		}
	}
	t.Setenv("ICEBATH_DISABLE_WINDOWS_UTF8", "0")
	if windowsUTF8Disabled() {
		t.Fatal("0 should not opt out")
	}
}

func TestReconfigureStreamReplacesInvalidUTF8OnEveryStream(t *testing.T) {
	out, errOut := captureStreams(t)
	in := &bytes.Buffer{}
	console().In = in
	reconfigureStream()
	c := console()
	c.Out.Write([]byte("ok \xff\xfe caf\xc3"))
	c.Out.Write([]byte("\xa9\n"))
	stderrWriter().Write([]byte("bad\x80"))
	if out.String() != "ok ?? café\n" || errOut.String() != "bad?" {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
	in.Write([]byte("hi \xff\xc3\xa9\xc3"))
	got, err := io.ReadAll(c.In)
	if err != nil || string(got) != "hi �é�" {
		t.Fatalf("stdin %q %v", got, err)
	}
	if c.StdinIsTTY() || c.StdoutIsTTY() {
		t.Fatal("tty checks changed by the wrappers")
	}
}
