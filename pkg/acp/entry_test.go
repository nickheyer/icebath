package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func quietEntry(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swap(t, &setupLogging, func() error { return nil })
	swap(t, &loadEnv, func(context.Context) error { return nil })
	swap(t, &installTruststore, func() bool { return true })
}

func runMain(argv ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), argv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestEntryArgumentParserMatchesArgparse(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("NO_COLOR", "1")
	raw, err := os.ReadFile(filepath.Join("testdata", "entry_argparse_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Argv []string        `json:"argv"`
		NS   map[string]bool `json:"ns"`
		Code *int            `json:"code"`
		Out  string          `json:"out"`
		Err  string          `json:"err"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("only %d recorded cases", len(cases))
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		args, code, done := parseEntryArgs(tc.Argv, &stdout, &stderr)
		if stdout.String() != tc.Out || stderr.String() != tc.Err {
			t.Fatalf("%v\nstdout %q\nwant   %q\nstderr %q\nwant   %q", tc.Argv, stdout.String(), tc.Out, stderr.String(), tc.Err)
		}
		if tc.Code != nil {
			if !done || code != *tc.Code {
				t.Fatalf("%v exited %v %d, want %d", tc.Argv, done, code, *tc.Code)
			}
			continue
		}
		got := map[string]bool{"version": args.Version, "check": args.Check, "setup": args.Setup, "setup_browser": args.SetupBrowser, "assume_yes": args.AssumeYes}
		if done || !reflect.DeepEqual(got, tc.NS) {
			t.Fatalf("%v parsed %v (done %v), want %v", tc.Argv, got, done, tc.NS)
		}
	}
}

func TestMainEnablesUnstableProtocol(t *testing.T) {
	quietEntry(t)
	var seen []bool
	swap(t, &runAgent, func(agent any, useUnstableProtocol bool) error {
		seen = append(seen, useUnstableProtocol)
		return nil
	})
	if code, _, _ := runMain(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !reflect.DeepEqual(seen, []bool{true}) {
		t.Fatalf("use_unstable_protocol = %v", seen)
	}
}

func recordDiscovery(t *testing.T) *int {
	t.Helper()
	calls := 0
	var mu sync.Mutex
	previous := mcp.SwapDiscoveryPass(func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil
	})
	t.Cleanup(func() {
		timeout := 2 * time.Second
		mcp.JoinDiscovery(context.Background(), &timeout)
		mcp.SwapDiscoveryPass(previous)
	})
	config.ResetCache()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("mcp_servers:\n  demo:\n    url: https://mcp.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &calls
}

func TestMainSkipsConfiguredMcpDiscoveryWhenRequested(t *testing.T) {
	quietEntry(t)
	t.Setenv("ICEBATH_ACP_SKIP_CONFIGURED_MCP", "1")
	calls := recordDiscovery(t)
	swap(t, &runAgent, func(any, bool) error { return nil })
	if code, _, _ := runMain(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if *calls != 0 {
		t.Fatalf("discovery ran %d times", *calls)
	}
}

func TestMainSetupOffersBrowserInstallWhenTty(t *testing.T) {
	quietEntry(t)
	swap(t, &RunModelSetup, func(context.Context) int { return 0 })
	swap(t, &stdinIsTTY, func() bool { return true })
	swap(t, &readInput, func(string) (string, error) { return "y", nil })
	var bootstrap []bool
	swap(t, &runSetupBrowser, func(ctx context.Context, assumeYes bool, stderr io.Writer) int {
		bootstrap = append(bootstrap, assumeYes)
		return 0
	})
	if code, _, _ := runMain("--setup"); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !reflect.DeepEqual(bootstrap, []bool{false}) {
		t.Fatalf("bootstrap calls = %v", bootstrap)
	}
}

func TestMainSetupBrowserPropagatesBrowserFailure(t *testing.T) {
	quietEntry(t)
	swap(t, &pmEnsure, func(ctx context.Context, name string, explicit bool) error {
		return &pm.InstallError{Package: name, Cause: "download failed"}
	})
	code, _, stderr := runMain("--setup-browser")
	if code != 1 || !strings.Contains(stderr, "Browser setup failed: chromium: download failed") {
		t.Fatalf("exit code %d stderr %q", code, stderr)
	}
}

func TestSetupBrowserIsOneExplicitPackageRequest(t *testing.T) {
	quietEntry(t)
	type call struct {
		name     string
		explicit bool
	}
	var calls []call
	swap(t, &pmEnsure, func(ctx context.Context, name string, explicit bool) error {
		calls = append(calls, call{name, explicit})
		return nil
	})
	if code, _, _ := runMain("--setup-browser", "--yes"); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !reflect.DeepEqual(calls, []call{{"chromium", true}}) {
		t.Fatalf("calls = %v", calls)
	}
}

type closingAgent struct {
	*GatewayACPAgent
	calls *[]string
}

func (c closingAgent) Close() { *c.calls = append(*c.calls, "close") }

func TestEntryClosesTransportWithoutDiscoveringMcp(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "crash"}[crash], func(t *testing.T) {
			quietEntry(t)
			os.Unsetenv("ICEBATH_ACP_SKIP_CONFIGURED_MCP")
			discovered := recordDiscovery(t)
			var calls []string
			agent := closingAgent{GatewayACPAgent: NewGatewayACPAgent(), calls: &calls}
			swap(t, &newEntryAgent, func() entryAgent { return agent })
			swap(t, &runAgent, func(instance any, useUnstableProtocol bool) error {
				if served, ok := instance.(closingAgent); !ok || served.GatewayACPAgent != agent.GatewayACPAgent {
					t.Errorf("served %T", instance)
				}
				calls = append(calls, "serve")
				if crash {
					return errors.New("owned transport failure")
				}
				return nil
			})
			code, _, _ := runMain()
			if crash && code != 1 || !crash && code != 0 {
				t.Fatalf("exit code %d", code)
			}
			if !reflect.DeepEqual(calls, []string{"serve", "close"}) || *discovered != 0 {
				t.Fatalf("calls %v discovery %d", calls, *discovered)
			}
		})
	}
}

func probeRecord(msg string, err error) *logging.Record {
	rec := logging.MakeRecord(context.Background(), "root", logging.LevelError, msg)
	if err != nil {
		rec.Msg += "\n" + err.Error()
		rec.ExcInfo = err
	}
	return rec
}

func TestFilterSuppressesBenignProbe(t *testing.T) {
	for _, method := range []string{"ping", "health", "healthcheck"} {
		if (benignProbeMethodFilter{}).Filter(probeRecord("Background task failed", pyacp.MethodNotFound(method))) {
			t.Fatalf("%s probe noise passed the filter", method)
		}
	}
}

func TestFilterAllowsDifferentMessageEvenForPing(t *testing.T) {
	if !(benignProbeMethodFilter{}).Filter(probeRecord("Some other context", pyacp.MethodNotFound("ping"))) {
		t.Fatal("another message was muted")
	}
}

type probeAgent struct{}

func (probeAgent) Initialize(ctx context.Context, req *pyacp.InitializeRequest) (*pyacp.InitializeResponse, error) {
	return &pyacp.InitializeResponse{ProtocolVersion: 1, AgentCapabilities: &pyacp.AgentCapabilities{}}, nil
}
func (probeAgent) NewSession(ctx context.Context, req *pyacp.NewSessionRequest) (*pyacp.NewSessionResponse, error) {
	return &pyacp.NewSessionResponse{SessionID: "test"}, nil
}
func (probeAgent) Prompt(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error) {
	return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
}
func (probeAgent) Cancel(ctx context.Context, req *pyacp.CancelNotification) error { return nil }
func (probeAgent) Authenticate(ctx context.Context, req *pyacp.AuthenticateRequest) (*pyacp.AuthenticateResponse, error) {
	return nil, nil
}

type filteredBuffer struct {
	mu     sync.Mutex
	filter logging.Filter
	lines  []string
}

func (h *filteredBuffer) Handle(ctx context.Context, rec *logging.Record) bool {
	if !h.filter.Filter(rec) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, rec.Name+"|"+rec.Levelname+"|"+rec.Msg)
	return true
}
func (h *filteredBuffer) Level() int   { return 0 }
func (h *filteredBuffer) Close() error { return nil }
func (h *filteredBuffer) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

func swapRootHandlers(t *testing.T, replacement logging.Handler) {
	t.Helper()
	root := logging.GetLogger("root")
	prior, level := root.Handlers(), root.Level()
	for _, h := range prior {
		root.RemoveHandler(h)
	}
	if replacement != nil {
		root.AddHandler(replacement)
	}
	root.SetLevel(logging.LevelInfo)
	t.Cleanup(func() {
		for _, h := range root.Handlers() {
			root.RemoveHandler(h)
		}
		for _, h := range prior {
			root.AddHandler(h)
		}
		root.SetLevel(level)
	})
}

func TestBarePingRequestProducesProperResponseAndNoStderrNoise(t *testing.T) {
	handler := &filteredBuffer{filter: benignProbeMethodFilter{}}
	swapRootHandlers(t, handler)
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- pyacp.RunAgent(probeAgent{}, agentToClientW, clientToAgentR, true, 1024*1024) }()
	if _, err := io.WriteString(clientToAgentW, `{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": {}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	lineCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 1024)
		for !bytes.Contains(buf, []byte("\n")) {
			n, err := agentToClientR.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err != nil {
				break
			}
		}
		lineCh <- string(buf)
	}()
	var line string
	select {
	case line = <-lineCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no response to the ping")
	}
	time.Sleep(200 * time.Millisecond)
	response, err := pyjson.Loads(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("response %q: %v", line, err)
	}
	failure, _ := response.(*pyjson.Object).GetOr("error", nil).(*pyjson.Object)
	if failure == nil || failure.GetOr("code", nil) != int64(-32601) {
		t.Fatalf("response = %s", line)
	}
	data, _ := failure.GetOr("data", nil).(*pyjson.Object)
	if data == nil || !reflect.DeepEqual(data.Vals, map[string]any{"method": "ping"}) {
		t.Fatalf("error data = %s", line)
	}
	if logs := handler.text(); strings.Contains(logs, "Background task failed") {
		t.Fatalf("ping noise leaked to stderr:\n%s", logs)
	}
	clientToAgentW.Close()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop at end of input")
	}
}

const loggingSecret = "sk-proj-AbCdEf1234567890SecretValue999"

func TestAcpStderrHandlerRedactsSecrets(t *testing.T) {
	swapRootHandlers(t, nil)
	if err := setupLogging(); err != nil {
		t.Fatal(err)
	}
	handlers := logging.GetLogger("root").Handlers()
	if len(handlers) == 0 {
		t.Fatal("ACP logging setup installed no handler")
	}
	stream, ok := handlers[0].(logging.StreamHandler)
	if !ok {
		t.Fatalf("handler is %T", handlers[0])
	}
	rec := logging.MakeRecord(context.Background(), "acp.test", logging.LevelError, "request failed: OPENROUTER_API_KEY="+loggingSecret)
	out, err := stream.Formatter().Format(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, loggingSecret) || !strings.Contains(out, "OPENROUTER_API_KEY=") {
		t.Fatalf("formatted %q", out)
	}
}
