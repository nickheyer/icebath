package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Metered tool env of the context profile carrying the profile secrets a terminal call reads
func terminalEnv(ctx context.Context) *tools.Env {
	env := loopEnv(ctx, true)
	env.Secrets = config.NewSecrets(env.Home)
	return env
}

// Exit code of one terminal tool call through the metered env
func terminalExit(t *testing.T, ctx context.Context, env *tools.Env, req tools.TerminalRequest) int {
	t.Helper()
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	raw := tools.TerminalTool(ctx, env, req)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return out.ExitCode
}

func backendRow(errorClass, outcome string, count int) string {
	return "backend=local,error_class=" + errorClass + ",kind=terminal,outcome=" + outcome + "|" + strconv.Itoa(count)
}

func seconds(n int) *int { return &n }

func TestTerminalCallsCountAgainstTheConfiguredBackend(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	env := terminalEnv(ctx)
	if code := terminalExit(t, ctx, env, tools.TerminalRequest{Command: "echo hi"}); code != 0 {
		t.Fatalf("echo exit %d", code)
	}
	if code := terminalExit(t, ctx, env, tools.TerminalRequest{Command: "exit 3"}); code != 3 {
		t.Fatalf("exit 3 read %d", code)
	}
	tools.TerminalTool(ctx, env, tools.TerminalRequest{Command: "echo control-plane", HostLocal: true})
	want := []string{backendRow("none", "success", 2)}
	if rows := observability.StoredRowsForTest(t, ctx, observability.ExecutionBackendMetric); !slices.Equal(rows, want) {
		t.Fatalf("rows %v want %v", rows, want)
	}
}

func TestForegroundTerminalTimeoutIsAFailedTimeout(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	env := terminalEnv(ctx)
	if code := terminalExit(t, ctx, env, tools.TerminalRequest{Command: "sleep 5", Timeout: seconds(1)}); code != 124 {
		t.Fatalf("sleep exit %d", code)
	}
	want := []string{backendRow("timeout", "failed", 1)}
	if rows := observability.StoredRowsForTest(t, ctx, observability.ExecutionBackendMetric); !slices.Equal(rows, want) {
		t.Fatalf("rows %v want %v", rows, want)
	}
}

func TestBackgroundReviewForkTerminalCallsAreNotBackendUsage(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	review := terminalEnv(ctx)
	review.Values.Store("write_origin", "background_review")
	if code := terminalExit(t, ctx, review, tools.TerminalRequest{Command: "echo review"}); code != 0 {
		t.Fatalf("review exit %d", code)
	}
	tools.TerminalTool(ctx, terminalEnv(ctx), tools.TerminalRequest{Command: "echo user-work"})
	want := []string{backendRow("none", "success", 1)}
	if rows := observability.StoredRowsForTest(t, ctx, observability.ExecutionBackendMetric); !slices.Equal(rows, want) {
		t.Fatalf("rows %v want %v", rows, want)
	}
}

func TestEveryShippedTerminalBackendHasItsOwnBucket(t *testing.T) {
	raw, err := os.ReadFile("schemas/icebath.shared_metrics.v4.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties struct {
				Dimensions struct {
					Properties struct {
						Backend struct {
							Enum []string `json:"enum"`
						} `json:"backend"`
					} `json:"properties"`
				} `json:"dimensions"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	enum := schema.Defs["execution_backend_counter"].Properties.Dimensions.Properties.Backend.Enum
	backends := tools.BuiltinTerminalBackends()
	if len(backends) == 0 {
		t.Fatal("no shipped terminal backends")
	}
	for _, backend := range backends {
		if fields := observability.ExecutionBackendFields("terminal", backend, "{}", nil); fields["backend"] != backend {
			t.Fatalf("%s read %v", backend, fields)
		}
		if !slices.Contains(enum, backend) {
			t.Fatalf("%s missing from the schema enum %v", backend, enum)
		}
	}
}

func TestForegroundTerminalCommandsCountByKindAndOutcome(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	t.Setenv("TERMINAL_ENV", "local")
	env := terminalEnv(ctx)
	var codes []int
	for _, c := range []struct {
		command string
		timeout int
	}{{"ls /definitely/not/here", 30}, {"echo ok", 30}, {"sh -c 'exit 124'", 30}, {"sleep 5", 1}} {
		codes = append(codes, terminalExit(t, ctx, env, tools.TerminalRequest{Command: c.command, TaskID: "harness", Timeout: seconds(c.timeout)}))
	}
	if !slices.Equal(codes, []int{2, 0, 124, 124}) {
		t.Fatalf("exit codes %v", codes)
	}
	tools.TerminalTool(ctx, env, tools.TerminalRequest{Command: "true", TaskID: "harness", HostLocal: true})
	want := []string{
		"backend=local,command_kind=file_ops,outcome=nonzero|1", "backend=local,command_kind=other,outcome=timeout|1",
		"backend=local,command_kind=shell,outcome=nonzero|1", "backend=local,command_kind=shell_builtin,outcome=ok|1",
	}
	rows := observability.StoredRowsForTest(t, ctx, observability.TerminalOutcomeMetric)
	if !slices.Equal(rows, want) {
		t.Fatalf("rows %v want %v", rows, want)
	}
	if strings.Contains(strings.Join(rows, " "), "definitely") {
		t.Fatalf("command text stored %v", rows)
	}
}

// Local backend under another name whose every command raises a connect timeout
type connectTimeoutBackend struct{ tools.BackendProvider }

func (connectTimeoutBackend) Name() string { return "connect-timeout" }

func (b connectTimeoutBackend) CreateEnvironment(ctx context.Context, env *tools.Env, opts tools.CreateOptions) (tools.Environment, error) {
	e, err := b.BackendProvider.CreateEnvironment(ctx, env, opts)
	if err != nil {
		return nil, err
	}
	return connectTimeoutEnv{e}, nil
}

type connectTimeoutEnv struct{ tools.Environment }

func (connectTimeoutEnv) Execute(context.Context, string, tools.ExecOptions) (*tools.ExecResult, error) {
	return nil, errors.New("ssh: connect to host example port 22: Connection timeout")
}

var registerConnectTimeout sync.Once

func TestBackendExceptionMentioningTimeoutIsNotATerminalTimeout(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	registerConnectTimeout.Do(func() {
		if err := tools.RegisterTerminalBackend(connectTimeoutBackend{tools.GetTerminalBackend("local")}); err != nil {
			t.Fatal(err)
		}
	})
	t.Setenv("TERMINAL_ENV", "connect-timeout")
	env := terminalEnv(ctx)
	tools.CleanupAllEnvironments(env)
	t.Cleanup(func() { tools.CleanupAllEnvironments(env) })
	if code := terminalExit(t, ctx, env, tools.TerminalRequest{Command: "echo hi", TaskID: "harness-exc"}); code != 124 {
		t.Fatalf("exit %d", code)
	}
	if rows := observability.StoredRowsForTest(t, ctx, observability.TerminalOutcomeMetric); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}
