package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// fakeLease is the active session slot the tests hand the CLI
type fakeLease struct{ released atomic.Bool }

func (l *fakeLease) Release() error { l.released.Store(true); return nil }

// Installs doubles for the cmd/icebath wiring hooks the chat path reaches
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry(context.Background())
	DispatchUninstallModuleEntry(context.Background())
	TryAcquireActiveSession = func(ctx context.Context, sessionID, surface string, cfg map[string]any, metadata map[string]any) (ActiveSessionLease, *activesessions.Refusal, error) {
		return &fakeLease{}, nil, nil
	}
	FormatRefusalStderr = activesessions.FormatRefusalStderr
	EmergencyCleanupAllBrowserSessions = func() {}
	SecurityAdvisoriesStartupBanner = func(ctx context.Context) string { return "" }
	installAgentWiring()
	os.Exit(m.Run())
}

// Serves one chat completions answer and counts the turn calls, session title requests answered uncounted
func chatCompletionsServer(t *testing.T, answer string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test/model", "object": "model"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if !strings.Contains(string(raw), "You name chat sessions") {
			calls.Add(1)
		}
		if body["stream"] == true {
			chunk := func(delta map[string]any, finish any, usage map[string]any) string {
				row := map[string]any{"id": "chatcmpl-main", "object": "chat.completion.chunk", "created": 1, "model": "test/model",
					"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
				if usage != nil {
					row["usage"] = usage
				}
				line, _ := json.Marshal(row)
				return "data: " + string(line) + "\n\n"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant", "content": answer}, nil, nil))
			_, _ = io.WriteString(w, chunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-main", "object": "chat.completion", "model": "test/model",
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15},
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer, "tool_calls": nil}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// Resets the once per process cleanup latches so every test runs the full exit path
func resetProcessState(t *testing.T) {
	t.Helper()
	cleanupMu.Lock()
	cleanupDone, cleanupInProgress = false, false
	cleanupMu.Unlock()
	setActiveAgent(nil)
	sessionIDSetsMu.Lock()
	singleQueryFinalizeAttemptedSessionIDs = map[string]bool{}
	handedOffSessionIDs = map[string]bool{}
	sessionIDSetsMu.Unlock()
	t.Setenv("ICEBATH_EXIT_WATCHDOG_S", "0")
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	_ = os.Unsetenv("ICEBATH_KANBAN_TASK")
}

func TestMainQuietSingleQueryAnswersThroughRunConversation(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetProcessState(t)
	out, errOut := captureStreams(t)
	srv, calls := chatCompletionsServer(t, "hi there")
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "test/model", "provider": "openai", "base_url": srv.URL + "/v1"},
		"agent": map[string]any{"max_turns": 5}, "display": map[string]any{}, "skills": map[string]any{"auto_load": []any{}}})
	code := Main(context.Background(), MainOptions{Query: "say hi", Quiet: true, Model: "test/model", Provider: "openai",
		APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", MaxTurns: ptr(5)})
	if code != 0 {
		t.Fatalf("exit code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls %d", calls.Load())
	}
	if got := out.String(); got != "hi there\n" {
		t.Fatalf("stdout %q", got)
	}
	if !strings.Contains(errOut.String(), "\nsession_id: ") {
		t.Fatalf("stderr %q lacks the session_id trailer", errOut.String())
	}
}

func TestMainQuietSingleQueryFailedTurnExitsOne(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetProcessState(t)
	out, errOut := captureStreams(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not supported"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "test/model", "provider": "openai", "base_url": srv.URL + "/v1"},
		"agent": map[string]any{"max_turns": 2}, "display": map[string]any{}})
	code := Main(context.Background(), MainOptions{Query: "say hi", Quiet: true, Model: "test/model", Provider: "openai",
		APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", MaxTurns: ptr(2)})
	if code != 1 {
		t.Fatalf("exit code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "rejected the request and retrying won't help") || !strings.Contains(out.String(), "model not supported") {
		t.Fatalf("stdout %q lacks the failure copy the agent returns as its final response", out.String())
	}
}

func TestTurnExitCodeContract(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		o      TurnExitOptions
		want   int
	}{
		{"completed", map[string]any{"completed": true, "final_response": "ok"}, TurnExitOptions{}, 0},
		{"interrupted", map[string]any{"interrupted": true}, TurnExitOptions{}, 130},
		{"failed", map[string]any{"failed": true}, TurnExitOptions{}, 1},
		{"partial", map[string]any{"partial": true}, TurnExitOptions{}, 1},
		{"not_completed", map[string]any{"completed": false}, TurnExitOptions{}, 1},
		{"no_result", nil, TurnExitOptions{}, 1},
		{"worker_rate_limit", map[string]any{"failed": true, "failure_reason": "rate_limit"}, TurnExitOptions{KanbanWorker: true}, 75},
		{"worker_terminal", map[string]any{"failed": true, "failure_reason": "auth"}, TurnExitOptions{KanbanWorker: true}, 78},
		{"worker_credentials_rate_limited", nil, TurnExitOptions{KanbanWorker: true, CredentialsRateLimited: true}, 75},
		{"worker_credentials_terminal", nil, TurnExitOptions{KanbanWorker: true, CredentialsTerminal: true}, 78},
		{"non_worker_rate_limit", map[string]any{"failed": true, "failure_reason": "rate_limit"}, TurnExitOptions{}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnExitCode(tc.result, tc.o); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestParseSkillsArgumentDedupesAndSplitsCommas(t *testing.T) {
	got := parseSkillsArgument([]string{"a, b", "b", "", "c,,a"})
	if strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("got %v", got)
	}
	if len(parseSkillsArgument(nil)) != 0 {
		t.Fatal("nil must give an empty list")
	}
}

func TestNormalizeMoaModelSelectsTheVirtualProvider(t *testing.T) {
	if p, m := normalizeMoaModel(" MoA:fast "); p != "moa" || m != "fast" {
		t.Fatalf("got %q %q", p, m)
	}
	if p, m := normalizeMoaModel("moa:"); p != "" || m != "moa:" {
		t.Fatalf("got %q %q", p, m)
	}
	if p, m := normalizeMoaModel("openai/gpt-4o"); p != "" || m != "openai/gpt-4o" {
		t.Fatalf("got %q %q", p, m)
	}
}

func TestReadTurnReportRejectsOtherPids(t *testing.T) {
	path := t.TempDir() + "/report.json"
	writeTurnReport(context.Background(), path, 3, "boom", "reply")
	if got := readTurnReport(path, os.Getpid()); got == nil || got["exit_code"].(float64) != 3 || got["reply"] != "reply" {
		t.Fatalf("got %v", got)
	}
	if readTurnReport(path, os.Getpid()+1) != nil {
		t.Fatal("another pid's report must read as absent")
	}
	if readTurnReport(t.TempDir()+"/missing.json", os.Getpid()) != nil {
		t.Fatal("a missing report must read as absent")
	}
}

func TestTakeTurnReportPathPopsTheEnv(t *testing.T) {
	t.Setenv(TurnReportFileEnv, "/tmp/x.json")
	if got := takeTurnReportPath(); got != "/tmp/x.json" {
		t.Fatalf("got %q", got)
	}
	if _, ok := os.LookupEnv(TurnReportFileEnv); ok {
		t.Fatal("the report path must not survive for subprocesses")
	}
	if takeTurnReportPath() != "" {
		t.Fatal("a second read must be empty")
	}
}

func TestCollectQueryImagesPlainTextHasNoImages(t *testing.T) {
	msg, images, err := collectQueryImages("hello world", "")
	if err != nil || msg != "hello world" || len(images) != 0 {
		t.Fatalf("got %q %v %v", msg, images, err)
	}
	if _, _, err := collectQueryImages("x", t.TempDir()+"/missing.png"); err == nil || !strings.Contains(err.Error(), "Image file not found") {
		t.Fatalf("got %v", err)
	}
}

func TestStatusBarVisibleFromDisplayConfig(t *testing.T) {
	if !statusBarVisibleFromDisplayConfig(nil) || !statusBarVisibleFromDisplayConfig(map[string]any{"statusbar": "top"}) {
		t.Fatal("default and top must be visible")
	}
	for _, v := range []any{false, "off", "hidden", "0", "no", "false"} {
		if statusBarVisibleFromDisplayConfig(map[string]any{"statusbar": v}) {
			t.Fatalf("%v must hide the status bar", v)
		}
	}
	if !statusBarVisibleFromDisplayConfig(map[string]any{"tui_statusbar": "bottom"}) {
		t.Fatal("the legacy key must still read")
	}
}
