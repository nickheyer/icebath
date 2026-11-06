package observability

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Finishes one compression attempt and returns its recorded outcome and class
func finishCompression(t *testing.T, m *updateMarks, commitStatus string, failureClass any) [][2]string {
	t.Helper()
	m.mu.Lock()
	m.rows = nil
	m.mu.Unlock()
	owner := new(int)
	BeginCompressionAttempt(owner, "auto", 80_000)
	FinishCompressionAttempt(context.Background(), owner, commitStatus, failureClass, 100_000, nil)
	var out [][2]string
	for _, row := range m.named(CompressionMark) {
		out = append(out, [2]string{row["outcome"], row["failure_class"]})
	}
	return out
}

func TestCommittedAttemptKeepsItsFallbackClass(t *testing.T) {
	m := captureUpdates(t)
	for _, c := range []struct {
		class any
		want  [2]string
	}{
		{nil, [2]string{"success", "none"}}, {"summary_generation_failed", [2]string{"success", "summary_generation_failed"}},
		{"feasibility_skip", [2]string{"success", "feasibility_skip"}},
		{"stall_deterministic_fallback", [2]string{"success", "stall_deterministic_fallback"}},
		{"summary_overload_degraded", [2]string{"success", "summary_overload_degraded"}},
		{"aux_model_fallback", [2]string{"success", "aux_model_fallback"}},
		{"summary_model_benched", [2]string{"success", "feasibility_skip"}},
		{"plugin_specific_fallback", [2]string{"success", "other"}},
	} {
		if got := finishCompression(t, m, "committed", c.class); !reflect.DeepEqual(got, [][2]string{c.want}) {
			t.Fatalf("%v: %v", c.class, got)
		}
	}
	if got := finishCompression(t, m, "aborted", "lock_contended"); !reflect.DeepEqual(got, [][2]string{{"skipped", "lock_contended"}}) {
		t.Fatalf("lock contended %v", got)
	}
	if got := finishCompression(t, m, "aborted", "summary_auth_failure"); !reflect.DeepEqual(got, [][2]string{{"failed", "summary_auth_failure"}}) {
		t.Fatalf("auth failure %v", got)
	}
	if got := finishCompression(t, m, "aborted", "summary_model_benched"); !reflect.DeepEqual(got, [][2]string{{"failed", "other"}}) {
		t.Fatalf("benched abort %v", got)
	}
	for _, row := range m.named(CompressionMark) {
		if !CounterDimensionsAreValid(CompressionMetric, anyDims(row)) {
			t.Fatalf("invalid %v", row)
		}
	}
}

func TestCompressionFieldsBucketTheFillAndCollapseTriggers(t *testing.T) {
	got := CompressionFields("Gateway_Hygiene", "noop", 95, 100, "exception:ValueError")
	want := map[string]string{"context_fill_bucket": "90_to_100", "failure_class": "exception", "outcome": "skipped", "trigger": "auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields %v", got)
	}
	if f := CompressionFields("slash", "boom", nil, 0, ""); f["context_fill_bucket"] != "unknown" || f["failure_class"] != "unknown" || f["trigger"] != "manual" || f["outcome"] != "failed" {
		t.Fatalf("fields %v", f)
	}
}

// pluginOperationError is a raise site error tagged with its failure class
type pluginOperationError struct{ msg, class string }

func (e *pluginOperationError) Error() string        { return e.msg }
func (e *pluginOperationError) FailureClass() string { return e.class }

// AcmeInternalError is an untagged third party error type
type AcmeInternalError struct{ msg string }

func (e *AcmeInternalError) Error() string { return e.msg }

func TestARaisedInstallErrorIsClassifiedByTypeAndNeverCarriesItsText(t *testing.T) {
	saved := SkillSourceIDAliasesHook
	t.Cleanup(func() { SkillSourceIDAliasesHook = saved })
	SkillSourceIDAliasesHook = func() map[string]string { return map[string]string{"skills.sh": "skills-sh"} }
	leaky := "/home/alice/secret-repo: Permission denied"
	for _, c := range []struct {
		kind string
		err  error
		want string
	}{
		{"plugin", &pluginOperationError{leaky, "clone_failed"}, "clone_failed"},
		{"plugin", &pluginOperationError{leaky, "other"}, "other"},
		{"mcp_server", &fs.PathError{Op: "open", Path: leaky, Err: syscall.EACCES}, "permission"},
		{"mcp_server", os.NewSyscallError("connect", syscall.ECONNREFUSED), "network"},
		{"skill", &fs.PathError{Op: "open", Path: leaky, Err: syscall.ENOENT}, "filesystem_error"},
		{"skill", &AcmeInternalError{leaky}, "exception"},
		{"skill", &pluginOperationError{"x", leaky}, "other"},
	} {
		dims := ExtensionInstallFields(c.kind, "url", leaky, "failed", nil, nil, c.err)
		if dims["failure_class"] != c.want {
			t.Fatalf("%s %T: %v", c.kind, c.err, dims)
		}
		text, _ := json.Marshal(dims)
		if strings.Contains(string(text), leaky) || !CounterDimensionsAreValid(ExtensionInstallMetric, anyDims(dims)) {
			t.Fatalf("leaked or invalid %v", dims)
		}
	}
	ok := ExtensionInstallFields("skill", "hub", "x", "success", nil, "skills.sh", nil)
	if ok["failure_class"] != "none" || ok["registry"] != "skills-sh" {
		t.Fatalf("ok %v", ok)
	}
	if ExtensionInstallFields("plugin", "url", nil, "ok", nil, nil, nil)["registry"] != "none" {
		t.Fatal("plugin registry")
	}
	if ExtensionInstallFields("theme", "url", nil, "ok", nil, nil, nil) != nil {
		t.Fatal("unknown kind kept")
	}
	if ExceptionFailureClass(&AcmeInternalError{"x"}) != "exception:AcmeInternalError" {
		t.Fatal("type name")
	}
}

// failoverReason is a provider enum with a string value
type failoverReason string

func TestSmallFieldBuilders(t *testing.T) {
	if f := FallbackFields(nil, nil, failoverReason("rate_limit")); f["error_class"] != ModelErrorClass(map[string]any{"reason": "rate_limit"}) {
		t.Fatalf("fallback %v", f)
	}
	if f := SetupCompletedFields("Desktop", nil); !reflect.DeepEqual(f, map[string]string{"provider": "none", "surface": "desktop"}) {
		t.Fatalf("setup %v", f)
	}
	if SetupCompletedFields("web", "x")["surface"] != "other" {
		t.Fatal("setup surface")
	}
	s := SessionFields(map[string]string{"platform": "telegram"}, 7, 1, "weird", -5)
	want := map[string]string{"active_duration_bucket": "lt_1m", "entrypoint": "unknown", "execution_surface": "unknown",
		"failed_turn_count_bucket": CountBucket(1), "last_outcome": "unknown", "platform": "telegram", "turn_count_bucket": SizeBucket(7)}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("session %v", s)
	}
	if TtftBucket(map[string]any{}) != "unknown" || TtftBucket(map[string]any{"started_at": 1.0}) != "not_streamed" ||
		TtftBucket(map[string]any{"started_at": 1.0, "first_chunk_at": 1.7}) != "500ms_to_1s" ||
		TtftBucket(map[string]any{"started_at": 1.0, "first_chunk_at": 20.0}) != "gte_15s" {
		t.Fatal("ttft")
	}
	if InstallAgeBucket(30) != "lt_1h" || InstallAgeBucket(-1) != "unknown" || InstallAgeBucket(100*86400) != "gte_90d" {
		t.Fatal("install age")
	}
	if ModelTokenFields(map[string]any{"input_tokens": 0, "output_tokens": true}, "m", "p", "primary", nil) != nil {
		t.Fatal("no usage counted")
	}
	tok := ModelTokenFields(map[string]any{"input_tokens": 5, "reasoning_tokens": -1}, "m", "p", "primary", "x")
	if tok["input"] != 5 || tok["reasoning"] != 0 || tok["aux_task"] != "none" || tok["call_role"] != "primary" {
		t.Fatalf("tokens %v", tok)
	}
}

func TestMilestonesFollowTheRecordedDimensions(t *testing.T) {
	for _, c := range []struct {
		metric string
		dims   map[string]string
		want   []string
	}{
		{TaskStartedMetric, map[string]string{"platform": "none", "entrypoint": "scheduled_task"}, []string{"first_task_started", "first_scheduled_task"}},
		{TaskStartedMetric, map[string]string{"platform": "telegram"}, []string{"first_task_started", "first_gateway_message"}},
		{ToolUsageMetric, map[string]string{"outcome": "success", "tool_name": "mcp"}, []string{"first_tool_success", "first_mcp_tool_success"}},
		{SkillLifecycleMetric, map[string]string{"action": "created", "provenance": "agent_created"}, nil},
		{SkillLifecycleMetric, map[string]string{"action": "created"}, []string{"first_skill_created"}},
		{SessionMetric, map[string]string{"turn_count_bucket": "3_to_5"}, nil},
		{SessionMetric, map[string]string{}, []string{"first_long_session"}},
		{SetupCompletedMetric, nil, []string{"setup_completed"}},
		{"icebath.unknown", nil, nil},
	} {
		if got := MilestonesFor(c.metric, c.dims); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s %v: %v", c.metric, c.dims, got)
		}
	}
}
