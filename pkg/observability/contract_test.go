package observability

import (
	"context"
	"testing"
)

func TestClientResourceBoundsEveryField(t *testing.T) {
	got := ClientResource(" 1.2.3 ", "Darwin", "AMD64", "nix")
	want := map[string]string{"architecture": "x86_64", "icebath_version": "1.2.3", "install_method": "nixos", "os_family": "macos"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s %q want %q", k, got[k], v)
		}
	}
	if !ClientResourceIsValid(anyDims(got)) || ClientArchitecture("armv7l") != "arm" || ClientOSFamily("plan9") != "unknown" {
		t.Fatal("resource taxonomy")
	}
}

func TestTaskTerminalStateAndFailureClass(t *testing.T) {
	cases := []struct {
		kwargs  map[string]any
		outcome string
		failure string
	}{
		{map[string]any{"interrupted": true}, "cancelled", "none"},
		{map[string]any{"turn_exit_reason": "tool_timeout"}, "timed_out", "none"},
		{map[string]any{"turn_exit_reason": "max_iterations_reached"}, "failed", "other"},
		{map[string]any{"completed": true}, "success", "none"},
		{map[string]any{"failed": true, "turn_exit_reason": "compaction_failed"}, "failed", "context_compression"},
		{map[string]any{"failed": true, "failure_reason": "interpreter_shutdown"}, "failed", "shutdown"},
		{map[string]any{}, "unknown", "none"},
	}
	for _, c := range cases {
		outcome, _, _ := TaskTerminalState(c.kwargs)
		if outcome != c.outcome || TaskFailureClass(c.kwargs, "") != c.failure {
			t.Fatalf("%v gave %s %s", c.kwargs, outcome, TaskFailureClass(c.kwargs, ""))
		}
	}
}

func TestToolCategoryAndConnectorErrorClass(t *testing.T) {
	if ToolCategory(map[string]any{"toolset": "icebath-yuanbao"}) != "communication" ||
		ToolCategory(map[string]any{"toolset": "todo"}) != "planning" ||
		ToolCategory(map[string]any{"tool_name": "connectors__gh"}) != "connector" ||
		ToolCategory(map[string]any{}) != "unknown" {
		t.Fatal("tool categories")
	}
	kwargs := map[string]any{"tool_name": "connectors__gh", "status": "error", "error_type": "tool_error",
		"result": `{"error": {"code": "connection_expired"}}`}
	if got := ToolErrorClass(kwargs, ""); got != "connection_required" {
		t.Fatalf("connector class %q", got)
	}
	if ToolLatencyBucket(true, 120) != "100ms_to_250ms" || ToolRetryBucket(-1) != "unknown" || ToolRetryBucket(4) != "3_to_5" {
		t.Fatal("tool buckets")
	}
}

func TestModelMetricNameHidesLocations(t *testing.T) {
	ResetCatalogCaches()
	for _, model := range []string{"arn:aws:bedrock:x", "http://h:/m", "llama.gguf", "localhost:8080", "10.0.0.1/m", "box:11434"} {
		if got := ModelMetricName(model, "openai", ModelIdentifierMaxLength); got != Custom {
			t.Fatalf("%s gave %s", model, got)
		}
	}
	if ModelMetricName("anthropic.claude-v1:0", "bedrock", ModelIdentifierMaxLength) != "anthropic.claude-v1:0" ||
		ModelMetricName("gpt-5", "unknown", ModelIdentifierMaxLength) != Custom ||
		ModelMetricName("  ", "openai", ModelIdentifierMaxLength) != "unknown" {
		t.Fatal("model names")
	}
}

func TestExtensionInstallRowsAgreeAcrossFields(t *testing.T) {
	row := map[string]any{"kind": "plugin", "name": "custom", "outcome": "failed", "source": "url",
		"failure_class": "git_missing", "registry": "none"}
	if !CounterDimensionsAreValid(ExtensionInstallMetric, row) {
		t.Fatal("valid plugin failure rejected")
	}
	row["failure_class"] = "none"
	if CounterDimensionsAreValid(ExtensionInstallMetric, row) {
		t.Fatal("failed row with no class accepted")
	}
	row["outcome"], row["registry"] = "success", "github"
	if CounterDimensionsAreValid(ExtensionInstallMetric, row) {
		t.Fatal("plugin row naming a registry accepted")
	}
}

func TestDecisionMarkAndTokenSums(t *testing.T) {
	meta := map[string]any{SchemaKey: SchemaVersion}
	ev := &RelayEvent{Kind: "mark", Name: ProcessExitMark, Metadata: meta,
		Data: map[string]any{"crash_class": "none", "exit_kind": "clean", "process_kind": "cli"}}
	if c, ok := DecisionCounter(ev); !ok || c.Metric != ProcessExitMetric || c.Dimensions["exit_kind"] != "clean" {
		t.Fatalf("decision %v %v", c, ok)
	}
	ev.Category = "llm"
	if _, ok := DecisionCounter(ev); ok {
		t.Fatal("mark with a category accepted")
	}
	tokens := &RelayEvent{Kind: "mark", Name: ModelTokensMark, Metadata: meta, Data: map[string]any{
		"aux_task": "none", "call_role": "primary", "model": "gpt-5", "provider": "custom",
		"input": 10, "output": 0, "cache_read": 3, "cache_write": 0, "reasoning": 0}}
	sums := ModelTokenCounters(tokens)
	if len(sums) != 2 || sums[0].Dimensions["token_type"] != "cache_read" || sums[0].Amount != 3 || sums[1].Amount != 10 {
		t.Fatalf("sums %+v", sums)
	}
}

func TestTaskStartFieldsNameTheSurface(t *testing.T) {
	got := TaskStartFields(context.Background(), map[string]any{"platform": "cron", "parent_session_id": "p"})
	if got["execution_surface"] != "scheduled_task" || got["entrypoint"] != "delegated" || got["platform"] != "none" {
		t.Fatalf("fields %v", got)
	}
	got = TaskStartFields(context.Background(), map[string]any{"platform": "telegram"})
	if got["execution_surface"] != "gateway" || got["entrypoint"] != "gateway_message" || got["platform"] != "telegram" {
		t.Fatalf("gateway fields %v", got)
	}
}
