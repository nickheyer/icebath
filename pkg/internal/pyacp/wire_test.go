package pyacp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type wireAgent struct {
	modeCalls int
}

func (a *wireAgent) Initialize(ctx context.Context, req *InitializeRequest) (*InitializeResponse, error) {
	return &InitializeResponse{
		ProtocolVersion:   ProtocolVersion,
		AgentCapabilities: &AgentCapabilities{LoadSession: Ptr(true), PromptCapabilities: &PromptCapabilities{Image: Ptr(true)}},
		AgentInfo:         &Implementation{Name: "icebath-agent", Version: "1.0"},
	}, nil
}

func (a *wireAgent) SetSessionMode(ctx context.Context, req *SetSessionModeRequest) (*SetSessionModeResponse, error) {
	a.modeCalls++
	return nil, nil
}

func (a *wireAgent) Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
	if req.SessionID == "boom" {
		return nil, errors.New("boom")
	}
	return &PromptResponse{StopReason: "end_turn"}, nil
}

type wirePeer struct {
	toAgent   *io.PipeWriter
	fromAgent *bufio.Reader
	conn      *AgentSideConnection
	done      chan error
}

func startWire(t *testing.T, agent any, unstable bool) *wirePeer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &wirePeer{toAgent: inW, fromAgent: bufio.NewReader(outR), done: make(chan error, 1)}
	p.conn = NewAgentSideConnection(agent, outW, inR, false, unstable, DefaultStdioBufferLimitBytes)
	go func() { p.done <- p.conn.Listen() }()
	t.Cleanup(func() {
		inW.Close()
		<-p.done
		outW.Close()
	})
	return p
}

func (p *wirePeer) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(p.toAgent, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (p *wirePeer) recv(t *testing.T) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		line, _ := p.fromAgent.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no frame from agent")
	}
	return ""
}

func TestWireSessionUpdateFrameEscapesNonASCII(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	go func() {
		_ = p.conn.SessionUpdate(context.Background(), "s1", UpdateAgentMessageText("h\u00e9llo"), nil)
	}()
	want := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"content":{"text":"h\u00e9llo","type":"text"},"sessionUpdate":"agent_message_chunk"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireToolCallStartDropsNoneAndKeepsFieldOrder(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	raw := pyjson.NewObject()
	raw.Set("path", "/a")
	start := StartToolCall("tc1", "Read", ToolCallFields{
		Kind:      Ptr("read"),
		Status:    Ptr("pending"),
		Content:   []ToolCallContent{ToolContent(TextBlock("x"))},
		Locations: []ToolCallLocation{{Path: "/a"}},
		RawInput:  raw,
	})
	go func() { _ = p.conn.SessionUpdate(context.Background(), "s1", start, nil) }()
	want := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"content":[{"content":{"text":"x","type":"text"},"type":"content"}],"kind":"read","locations":[{"path":"/a"}],"rawInput":{"path":"/a"},"status":"pending","title":"Read","toolCallId":"tc1","sessionUpdate":"tool_call"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireInitializeResultExcludesUnsetFields(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	p.send(t, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`)
	want := `{"jsonrpc":"2.0","id":0,"result":{"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true}},"agentInfo":{"name":"icebath-agent","version":"1.0"},"protocolVersion":1}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireNormalizedNoneResultIsEmptyObject(t *testing.T) {
	a := &wireAgent{}
	p := startWire(t, a, true)
	p.send(t, `{"jsonrpc":"2.0","id":"m","method":"session/set_mode","params":{"session_id":"s","modeId":"ask"}}`)
	want := `{"jsonrpc":"2.0","id":"m","result":{}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if a.modeCalls != 1 {
		t.Fatalf("mode calls %d", a.modeCalls)
	}
}

func TestWireUnknownAndAbsentMethodsAreMethodNotFound(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	p.send(t, `{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}`)
	want := `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"Method not found","data":{"method":"session/new"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	p.send(t, `{"jsonrpc":"2.0","id":8,"method":"_hermes/x","params":{}}`)
	want = `{"jsonrpc":"2.0","id":8,"error":{"code":-32601,"message":"Method not found","data":{"method":"_hermes/x"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireUnstableMethodRefusedWithoutFlag(t *testing.T) {
	p := startWire(t, &wireAgent{}, false)
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"session/fork","params":{"cwd":"/","sessionId":"s"}}`)
	want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found","data":{"method":"session/fork"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireMissingFieldIsInvalidParamsWithPydanticErrors(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s"}}`)
	want := `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Invalid params","data":{"errors":[{"type":"missing","loc":["prompt"],"msg":"Field required","input":{"sessionId":"s"},"url":"https://errors.pydantic.dev/2.14/v/missing"}]}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireUnionMemberErrorsCarryClassNameLoc(t *testing.T) {
	_, err := Validate[PromptRequest](mustLoads(t, `{"sessionId":"s","prompt":[{"type":"text"}]}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err %v", err)
	}
	got, _ := pyjson.Dumps(ve.Errors(), pyjson.Separators(",", ":"))
	if !strings.Contains(got, `{"type":"missing","loc":["prompt",0,"TextContentBlock","text"],"msg":"Field required","input":{"type":"text"},"url":"https://errors.pydantic.dev/2.14/v/missing"}`) {
		t.Fatalf("errors %s", got)
	}
	if !strings.Contains(got, `{"type":"literal_error","loc":["prompt",0,"ImageContentBlock","type"],"msg":"Input should be 'image'","input":"text","ctx":{"expected":"'image'"},"url":"https://errors.pydantic.dev/2.14/v/literal_error"}`) {
		t.Fatalf("errors %s", got)
	}
}

func TestWireHandlerFailureIsInternalErrorWithDetails(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	p.send(t, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"boom","prompt":[]}}`)
	want := `{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"Internal error","data":{"details":"boom"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	p.send(t, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}}`)
	want = `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWireRequestPermissionRoundTrip(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	type res struct {
		resp *RequestPermissionResponse
		err  error
	}
	out := make(chan res, 1)
	go func() {
		resp, err := p.conn.RequestPermission(context.Background(), []PermissionOption{{Kind: "allow_once", Name: "Allow", OptionID: "allow"}}, "s1", &ToolCallUpdate{ToolCallID: "tc", Title: Ptr("Run"), Kind: Ptr("execute")}, nil)
		out <- res{resp, err}
	}()
	want := `{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"options":[{"kind":"allow_once","name":"Allow","optionId":"allow"}],"sessionId":"s1","toolCall":{"kind":"execute","title":"Run","toolCallId":"tc"}}}` + "\n"
	if got := p.recv(t); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	p.send(t, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`)
	r := <-out
	if r.err != nil {
		t.Fatal(r.err)
	}
	sel, ok := r.resp.Outcome.(*AllowedOutcome)
	if !ok || sel.OptionID != "allow" {
		t.Fatalf("outcome %#v", r.resp.Outcome)
	}
}

func TestWireErrorResponseRejectsOutgoingRequest(t *testing.T) {
	p := startWire(t, &wireAgent{}, true)
	out := make(chan error, 1)
	go func() {
		_, err := p.conn.ExtMethod(context.Background(), "hermes/elicit", pyjson.NewObject())
		out <- err
	}()
	if got := p.recv(t); got != `{"jsonrpc":"2.0","id":0,"method":"_hermes/elicit","params":{}}`+"\n" {
		t.Fatalf("frame %q", got)
	}
	p.send(t, `{"jsonrpc":"2.0","id":0,"error":{"code":-32001,"message":"nope"}}`)
	err := <-out
	var re *RequestError
	if !errors.As(err, &re) || re.Code != -32001 || re.Message != "nope" || re.Data != nil {
		t.Fatalf("err %#v", err)
	}
}

func TestDumpExcludeDefaultsVersusExcludeUnset(t *testing.T) {
	caps := &AgentCapabilities{LoadSession: Ptr(false), MCPCapabilities: &McpCapabilities{}}
	a, _ := pyjson.Dumps(Dump(caps, SerializeParams), pyjson.Separators(",", ":"))
	b, _ := pyjson.Dumps(Dump(caps, ResultDump), pyjson.Separators(",", ":"))
	c, _ := pyjson.Dumps(Dump(&AgentCapabilities{}, DumpOptions{}), pyjson.Separators(",", ":"))
	if a != `{}` || b != `{"loadSession":false,"mcpCapabilities":{}}` {
		t.Fatalf("defaults %s unset %s", a, b)
	}
	if c != `{"field_meta":null,"load_session":false,"mcp_capabilities":{"field_meta":null,"http":false,"sse":false},"prompt_capabilities":{"field_meta":null,"audio":false,"embedded_context":false,"image":false},"session_capabilities":{"field_meta":null,"close":null,"fork":null,"list":null,"resume":null}}` {
		t.Fatalf("full %s", c)
	}
}

func mustLoads(t *testing.T, s string) any {
	t.Helper()
	v, err := pyjson.Loads(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
