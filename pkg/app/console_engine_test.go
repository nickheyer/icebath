package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store of the isolated home with each session holding count user messages
func consoleSeedSessions(t *testing.T, sessions map[string]int, source string) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, state.ResolveDefaultDBPath(ctx), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, count := range sessions {
		if err := db.CreateSession(state.NewSession{ID: name, Source: source}); err != nil {
			t.Fatal(err)
		}
		var batch []map[string]any
		for i := range count {
			batch = append(batch, map[string]any{"role": "user", "content": fmt.Sprintf("%s-%d", name, i)})
		}
		if len(batch) > 0 {
			if _, err := db.AppendMessagesBatch(ctx, name, batch, state.AppendGuards{}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Writes the per session export budget into config.yaml of the home
func consoleExportLimit(t *testing.T, h string, limit int) {
	t.Helper()
	body := fmt.Sprintf("sessions:\n  max_export_messages: %d\n", limit)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func consoleExecute(t *testing.T, e *ConsoleEngine, line string, confirmed bool) ConsoleResult {
	t.Helper()
	result, err := e.Execute(line, confirmed)
	if err != nil {
		t.Fatalf("Execute(%q) escaped with %v", line, err)
	}
	return result
}

func TestSessionsOptimizeAcceptsTheForceOverrideItAdvertises(t *testing.T) {
	tempHome(t)
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "sessions optimize --force", true)
	if result.Status != ConsoleOK {
		t.Fatalf("status %q: %s", result.Status, result.Output)
	}
	if strings.Contains(result.Output, "Usage:") {
		t.Fatalf("output %q", result.Output)
	}
}

func TestSessionsListAndStatsUseIsolatedSessionStore(t *testing.T) {
	tempHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, state.ResolveDefaultDBPath(ctx), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []state.NewSession{{ID: "chat-session", Source: "cli", Model: "test/model"}, {ID: "tool-session", Source: "tool", Model: "test/model"}} {
		if err := db.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	engine := NewConsoleEngine(ctx, 20000)
	listed := consoleExecute(t, engine, "sessions list --limit 10", false)
	stats := consoleExecute(t, engine, "sessions stats", false)
	if listed.Status != ConsoleOK || !strings.Contains(listed.Output, "chat-session") || strings.Contains(listed.Output, "tool-session") {
		t.Fatalf("listed %+v", listed)
	}
	if !strings.Contains(stats.Output, "Total sessions: 2") || !strings.Contains(stats.Output, "Listable sessions: 1") {
		t.Fatalf("stats %q", stats.Output)
	}
}

func TestSessionsExportRejectsOversizedSingleBeforeTouchingOutput(t *testing.T) {
	h := tempHome(t)
	consoleSeedSessions(t, map[string]int{"too-large": 3}, "cli")
	consoleExportLimit(t, h, 2)
	var materialized []string
	saved := consoleExportSession
	consoleExportSession = func(db *state.SessionDB, ctx context.Context, id string, compacted, inactive bool) (*pyjson.Object, error) {
		materialized = append(materialized, id)
		return saved(db, ctx, id, compacted, inactive)
	}
	t.Cleanup(func() { consoleExportSession = saved })
	output := filepath.Join(t.TempDir(), "sessions.jsonl")
	if err := os.WriteFile(output, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "sessions export "+output+" --session-id too-large", true)
	if result.Status != ConsoleError || !strings.Contains(result.Output, "too-large") || !strings.Contains(result.Output, "streaming Export") {
		t.Fatalf("result %+v", result)
	}
	if strings.Contains(strings.ToLower(result.Output), "resume") || len(materialized) != 0 {
		t.Fatalf("output %q materialized %v", result.Output, materialized)
	}
	if raw, _ := os.ReadFile(output); string(raw) != "keep me\n" {
		t.Fatalf("output file %q", raw)
	}
}

func TestSessionsExportAllUsesPerSessionBudget(t *testing.T) {
	h := tempHome(t)
	consoleSeedSessions(t, map[string]int{"first-safe": 2, "second-safe": 2, "third-safe": 2}, "cli")
	consoleExportLimit(t, h, 3)
	output := filepath.Join(t.TempDir(), "all-sessions.jsonl")
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "sessions export "+output, true)
	if result.Status != ConsoleOK {
		t.Fatalf("result %+v", result)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		ids[row["id"].(string)] = true
	}
	if len(ids) != 3 || !ids["first-safe"] || !ids["second-safe"] || !ids["third-safe"] {
		t.Fatalf("exported ids %v", ids)
	}
}

func TestSessionsExportAllRejectsSingleOversizedSession(t *testing.T) {
	h := tempHome(t)
	consoleSeedSessions(t, map[string]int{"small": 2, "runaway": 4}, "cli")
	consoleExportLimit(t, h, 3)
	var calls []string
	saved := consoleExportAll
	consoleExportAll = func(db *state.SessionDB, ctx context.Context, source string, compacted, inactive bool) ([]*pyjson.Object, error) {
		calls = append(calls, source)
		return nil, errors.New("export_all must not run before every guard passes")
	}
	t.Cleanup(func() { consoleExportAll = saved })
	output := filepath.Join(t.TempDir(), "all-sessions.jsonl")
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "sessions export "+output, true)
	if result.Status != ConsoleError {
		t.Fatalf("result %+v", result)
	}
	for _, want := range []string{"runaway", "more than 3 exportable", "streaming Export", "max_export_messages"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("output %q lacks %q", result.Output, want)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("export_all calls %v", calls)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists: %v", err)
	}
}

func TestSessionsExportZeroLimitDisablesGuard(t *testing.T) {
	h := tempHome(t)
	consoleSeedSessions(t, map[string]int{"huge": 5}, "cli")
	consoleExportLimit(t, h, 0)
	output := filepath.Join(t.TempDir(), "huge.jsonl")
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "sessions export "+output+" --session-id huge", true)
	if result.Status != ConsoleOK {
		t.Fatalf("result %+v", result)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}

func TestCronPauseResumeAndRunRequireConfirmation(t *testing.T) {
	h := tempHome(t)
	ctx := context.Background()
	prompt, name := "say hello", "alpha"
	job, err := cron.CreateJob(ctx, h, cron.CreateJobOptions{Prompt: &prompt, Schedule: "every 1h", Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	stateOf := func() string {
		t.Helper()
		stored, err := cron.GetJob(ctx, h, job.ID)
		if err != nil || stored == nil {
			t.Fatalf("get_job: %v %v", stored, err)
		}
		return stored.State
	}
	engine := NewConsoleEngine(ctx, 20000)
	if pending := consoleExecute(t, engine, "cron pause "+job.ID, false); pending.Status != ConsoleConfirmRequired {
		t.Fatalf("pending %+v", pending)
	}
	if got := stateOf(); got != "scheduled" {
		t.Fatalf("state %q", got)
	}
	if paused := consoleExecute(t, engine, "cron pause "+job.ID, true); paused.Status != ConsoleOK {
		t.Fatalf("paused %+v", paused)
	}
	if got := stateOf(); got != "paused" {
		t.Fatalf("state %q", got)
	}
	if resumed := consoleExecute(t, engine, "cron resume alpha", true); resumed.Status != ConsoleOK {
		t.Fatalf("resumed %+v", resumed)
	}
	if got := stateOf(); got != "scheduled" {
		t.Fatalf("state %q", got)
	}
	triggered := consoleExecute(t, engine, "cron run alpha", true)
	if triggered.Status != ConsoleOK || !strings.Contains(triggered.Output, "Triggered job") {
		t.Fatalf("triggered %+v", triggered)
	}
}

func TestReplRunsNonInteractiveLinesWithoutPrompts(t *testing.T) {
	tempHome(t)
	var stdout, stderr bytes.Buffer
	interactive := false
	code, err := RunConsoleREPL(context.Background(), ConsoleREPLOptions{Stdin: strings.NewReader("help\nexit\n"), Stdout: &stdout,
		Stderr: &stderr, Interactive: &interactive})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(stdout.String(), "Icebath Console") || strings.Contains(stdout.String(), "icebath>") {
		t.Fatalf("stdout %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestCaptureOutputSurfacesStringExitCodeAsCommandError(t *testing.T) {
	_, err := consoleCaptureOutput(func() (int, error) {
		return 0, &commands.ExitError{Code: 1, Msg: "No credential matching \"nope\"."}
	})
	var console *ConsoleCommandError
	if !errors.As(err, &console) || !strings.Contains(console.Error(), "No credential matching") {
		t.Fatalf("err %v", err)
	}
}

func TestConfigSetOnUnparseableYamlReportsErrorNotCrash(t *testing.T) {
	h := tempHome(t)
	configPath := home.ConfigPath(h)
	original := "model:\n  default: keep\nbroken: [unterminated\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	result := consoleExecute(t, NewConsoleEngine(context.Background(), 20000), "config set model.default gpt-4o", true)
	if result.Status != ConsoleError || !strings.Contains(result.Output, "formatting error") {
		t.Fatalf("result %+v", result)
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != original {
		t.Fatalf("config rewritten: %q", raw)
	}
}
