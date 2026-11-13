package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCronAgentOptionsCarryEveryKeywordCronPasses(t *testing.T) {
	env := &tools.Env{Home: t.TempDir()}
	o := cron.AgentOptions{
		Model: "m1", Runtime: &provider.Runtime{Provider: "custom"}, MaxIterations: 7,
		ReasoningConfig: map[string]any{"effort": "high"}, PrefillMessages: []any{map[string]any{"role": "user", "content": "x"}},
		FallbackModel: []map[string]any{{"model": "m2"}}, CredentialPool: &provider.Pool{},
		ProvidersAllowed: []any{"a"}, ProvidersIgnored: []any{"b"}, ProvidersOrder: []any{"c"}, ProviderSort: "price",
		OpenRouterMinCodingScore: 0.5, EnabledToolsets: []string{"web"}, DisabledToolsets: []string{"browser"},
		QuietMode: true, SkipContextFiles: true, LoadSoulIdentity: true, SkipMemory: true, SkipBackgroundReview: true,
		Platform: "cron", SessionID: "cron_j_1", SessionDB: &state.DB{}, Env: env, Workdir: "/work",
	}
	got := reflect.ValueOf(cronAgentOptions(o))
	want := reflect.ValueOf(o)
	for i := 0; i < want.NumField(); i++ {
		name := want.Type().Field(i).Name
		if want.Field(i).IsZero() {
			t.Fatalf("the fixture leaves %s at its zero value", name)
		}
		if name == "Workdir" {
			continue
		}
		field := got.FieldByName(name)
		if field.IsValid() && field.Kind() == reflect.Pointer && want.Field(i).Kind() != reflect.Pointer && !field.IsNil() {
			field = field.Elem()
		}
		if !field.IsValid() || !reflect.DeepEqual(field.Interface(), want.Field(i).Interface()) {
			t.Fatalf("%s did not reach the agent keywords", name)
		}
	}
	if cwd := got.FieldByName("Cwd").String(); cwd != "" {
		t.Fatalf("the job workdir became the agent cwd keyword %q", cwd)
	}
}

func TestCronAgentRunsTheCronTurnUnderItsTaskID(t *testing.T) {
	srv := fakeAgentProvider(t, nil, 0)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	built, err := newCronAgent(ctx, cron.AgentOptions{
		Model: "test-model", Runtime: &provider.Runtime{APIKey: "test-key", BaseURL: srv.URL + "/v1"}, MaxIterations: 3,
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, SkipBackgroundReview: true, Platform: "cron", SessionID: "cron_probe_1", Env: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	linked, ok := built.(cronAgent)
	if !ok {
		t.Fatalf("factory returned %T", built)
	}
	t.Cleanup(func() { built.Close() })
	linked.DisableStreaming()
	if built.SessionID() != "cron_probe_1" || linked.Platform() != "cron" || linked.Model() != "test-model" || linked.BaseURL() != srv.URL+"/v1" {
		t.Fatalf("agent %q %q %q %q", built.SessionID(), linked.Platform(), linked.Model(), linked.BaseURL())
	}
	prevStart := startTaskRunFn
	t.Cleanup(func() { startTaskRunFn = prevStart })
	var started [][2]string
	startTaskRunFn = func(_ context.Context, _ *tools.Env, sessionID, taskID, _, _ string) {
		started = append(started, [2]string{sessionID, taskID})
	}
	result, err := built.RunConversation(ctx, "edit main.py", "task-77")
	if err != nil {
		t.Fatal(err)
	}
	if result["final_response"] != "Edited main.py." || built.SessionAPICalls() != 1 {
		t.Fatalf("result %v after %d calls", result["final_response"], built.SessionAPICalls())
	}
	if !reflect.DeepEqual(started, [][2]string{{"cron_probe_1", "task-77"}}) {
		t.Fatalf("task runs started %v", started)
	}
}

type recordingCronAgent struct {
	cron.CronAgent
	message, toolReason string
	calls               int
}

func (r *recordingCronAgent) HardInterrupt(message, toolReason string) {
	r.message, r.toolReason = message, toolReason
	r.calls++
}

func TestCronRequestHardInterruptHandsTheToolReasonToTheAgent(t *testing.T) {
	target := &recordingCronAgent{}
	cronRequestHardInterrupt(target, "Cron job timed out (inactivity)", "cron inactivity watchdog")
	if target.calls != 1 || target.message != "Cron job timed out (inactivity)" || target.toolReason != "cron inactivity watchdog" {
		t.Fatalf("got %+v", target)
	}
	if _, ok := any(cronAgent{}).(hardInterruptWithReason); !ok {
		t.Fatal("the linked agent does not expose the hard interrupt that takes a tool reason")
	}
}

func TestCronTurnCompletionExplanationTreatsNilAsTheOmittedKeyword(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	model, cause := "fake-model", "corrupt"
	for _, reason := range []string{"max_iterations_reached", "session_persistence_failed", "text_response(stop)", "", "unknown_reason"} {
		if got, want := cronTurnCompletionExplanation(ctx, reason, nil, nil), formatTurnCompletionExplanation(ctx, reason, "", "", ""); got != want {
			t.Fatalf("%s bare got %q want %q", reason, got, want)
		}
		if got, want := cronTurnCompletionExplanation(ctx, reason, &cause, &model), formatTurnCompletionExplanation(ctx, reason, "corrupt", "", "fake-model"); got != want {
			t.Fatalf("%s full got %q want %q", reason, got, want)
		}
	}
	if cronTurnCompletionExplanation(ctx, "max_iterations_reached", nil, nil) == "" {
		t.Fatal("an abnormal ending rendered no explanation")
	}
}
