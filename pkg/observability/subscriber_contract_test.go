package observability

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func loadSchema(t *testing.T, version string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("schemas", "icebath.shared_metrics."+version+".schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func dig(v any, path ...string) any {
	for _, key := range path {
		v = v.(map[string]any)[key]
	}
	return v
}

func schemaSet(v any) ValueSet {
	out := ValueSet{}
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			out[item.(string)] = struct{}{}
		}
	case map[string]any:
		for key := range t {
			out[key] = struct{}{}
		}
	}
	return out
}

func dimensionSchema(t *testing.T, kind string) map[string]any {
	d := dig(loadSchema(t, "v4"), "$defs", kind, "properties", "dimensions").(map[string]any)
	if one, ok := d["oneOf"].([]any); ok {
		return one[0].(map[string]any)
	}
	return d
}

func expectSet(t *testing.T, label string, got, want ValueSet) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %v, want %v", label, got.Sorted(), want.Sorted())
	}
}

func TestPackageSchemaMatchesTheContracts(t *testing.T) {
	schema := loadSchema(t, "v4")
	if dig(schema, "properties", "schema_version", "const") != "icebath.shared_metrics.v4" {
		t.Fatal("schema version")
	}
	model := dimensionSchema(t, "model_route_counter")
	props := model["properties"].(map[string]any)
	expectSet(t, "model props", schemaSet(props), Set("call_role", "error_class", "model", "outcome", "provider", "ttft_bucket"))
	expectSet(t, "model required", schemaSet(model["required"]), Set("model", "provider"))
	if dig(props, "model", "maxLength") != float64(ModelIdentifierMaxLength) || dig(props, "provider", "maxLength") != float64(ProviderIdentifierMaxLength) {
		t.Fatal("identifier lengths")
	}
	for _, key := range []string{"model", "provider"} {
		if _, ok := props[key].(map[string]any)["enum"]; ok {
			t.Fatalf("%s has an enum", key)
		}
	}
	resource := dig(schema, "properties", "resource").(map[string]any)
	fields := Set("architecture", "icebath_version", "install_method", "os_family")
	expectSet(t, "resource required", schemaSet(resource["required"]), fields)
	expectSet(t, "resource props", schemaSet(resource["properties"]), fields)
	expectSet(t, "os", schemaSet(dig(resource, "properties", "os_family", "enum")), ClientOSFamilies)
	expectSet(t, "arch", schemaSet(dig(resource, "properties", "architecture", "enum")), ClientArchitectures)
	expectSet(t, "install", schemaSet(dig(resource, "properties", "install_method", "enum")), ClientInstallMethods)
	defs := schema["$defs"].(map[string]any)
	enum := func(name string) ValueSet { return schemaSet(dig(defs, name, "enum")) }
	expectSet(t, "surfaces", enum("execution_surface"), ExecutionSurfaces)
	expectSet(t, "entrypoints", enum("task_entrypoint"), TaskEntrypoints)
	expectSet(t, "durations", enum("duration_bucket"), DurationBuckets)
	expectSet(t, "counts", enum("count_bucket"), CountBuckets)
	start := dimensionSchema(t, "task_started_counter")["properties"].(map[string]any)
	terminal := dimensionSchema(t, "task_finished_counter")["properties"].(map[string]any)
	if !reflect.DeepEqual(start["entrypoint"], map[string]any{"$ref": "#/$defs/task_entrypoint"}) {
		t.Fatal("entrypoint ref")
	}
	expectSet(t, "end reasons", schemaSet(dig(terminal, "end_reason", "enum")), TaskEndReasons)
	expectSet(t, "outcomes", schemaSet(dig(terminal, "outcome", "enum")), TaskOutcomes)
	expectSet(t, "terminations", schemaSet(dig(terminal, "termination", "enum")), TaskTerminations)
	tool := dimensionSchema(t, "tool_call_counter")["properties"].(map[string]any)
	approval := dimensionSchema(t, "tool_approval_counter")["properties"].(map[string]any)
	expectSet(t, "tool categories", schemaSet(dig(tool, "tool_category", "enum")), ToolCategories)
	expectSet(t, "tool outcomes", schemaSet(dig(tool, "outcome", "enum")), ToolOutcomes)
	expectSet(t, "approval outcomes", schemaSet(dig(tool, "approval_outcome", "enum")), ToolApprovalOutcomes)
	if !reflect.DeepEqual(tool["latency_bucket"], map[string]any{"$ref": "#/$defs/tool_latency_bucket"}) ||
		!reflect.DeepEqual(tool["retry_count_bucket"], map[string]any{"$ref": "#/$defs/tool_retry_bucket"}) {
		t.Fatal("tool refs")
	}
	expectSet(t, "latency", enum("tool_latency_bucket"), ToolLatencyBuckets)
	expectSet(t, "retry", enum("tool_retry_bucket"), ToolRetryBuckets)
	expectSet(t, "attribution", schemaSet(dig(approval, "attribution", "enum")), ToolApprovalAttributions)
	expectSet(t, "approval", schemaSet(dig(approval, "outcome", "enum")), ToolApprovalOutcomes.Minus("not_required"))
	lifecycle := dimensionSchema(t, "skill_lifecycle_counter")["properties"].(map[string]any)
	load := dimensionSchema(t, "skill_load_counter")["properties"].(map[string]any)
	expectSet(t, "actions", schemaSet(dig(lifecycle, "action", "enum")), SkillLifecycleActions)
	expectSet(t, "provenances", enum("skill_provenance"), SkillProvenances)
	expectSet(t, "reuse", schemaSet(dig(load, "reuse_state", "enum")), SkillReuseStates)
	expectSet(t, "post patch", schemaSet(dig(load, "post_patch_state", "enum")), SkillPostPatchStates)
	if !reflect.DeepEqual(load["use_count_bucket"], map[string]any{"$ref": "#/$defs/count_bucket"}) {
		t.Fatal("use count ref")
	}
	legacy := loadSchema(t, "v1")
	counter := dig(legacy, "$defs", "model_call_counter")
	if dig(legacy, "properties", "schema_version", "const") != "icebath.shared_metrics.v1" || dig(counter, "properties", "name", "const") != LegacyModelCallMetric {
		t.Fatal("legacy schema")
	}
	expectSet(t, "legacy dims", schemaSet(dig(counter, "properties", "dimensions", "properties")),
		Set("call_role", "locality", "model_family", "outcome", "provider_family"))
}

func TestClientResourceClassificationIsBounded(t *testing.T) {
	if ClientOSFamily("Darwin") != "macos" || ClientOSFamily("Windows") != "windows" || ClientArchitecture("AMD64") != "x86_64" ||
		ClientArchitecture("aarch64") != "arm64" || ClientArchitecture("armv7l") != "arm" || ClientInstallMethod("Homebrew") != "homebrew" ||
		ClientInstallMethod("nix") != "nixos" || ClientInstallMethod("apt") != "apt" || ClientInstallMethod("microsoft-store") != "microsoft-store" {
		t.Fatal("classification")
	}
	got := ClientResource("", "privacy-os-canary", "privacy-arch-canary", "privacy-install-canary")
	want := map[string]string{"architecture": "unknown", "icebath_version": "unknown", "install_method": "unknown", "os_family": "unknown"}
	if !maps.Equal(got, want) {
		t.Fatalf("resource %v", got)
	}
}

func TestMicrosoftStoreStampReportsItsOwnInstallMethod(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := installProjectRoot
	installProjectRoot = func() string { return root }
	t.Cleanup(func() { installProjectRoot = prev })
	stamp := filepath.Join(root, "install-stamp.json")
	for mechanism, want := range map[string]string{"microsoft-store": "microsoft-store", "app-installer": "unknown"} {
		if err := os.WriteFile(stamp, []byte(`{"updateMechanism": "`+mechanism+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := DetectClientInstallMethod(t.Context()); err != nil || got != want {
			t.Fatalf("%s: %q %v", mechanism, got, err)
		}
	}
}

func metricMark(name string, data map[string]any) *RelayEvent {
	return &RelayEvent{Kind: "mark", Name: name, Metadata: map[string]any{SchemaKey: SchemaVersion}, Data: data}
}

func TestClientActiveMarkAcceptsOnlyAnEmptyAllowlistedPayload(t *testing.T) {
	event := metricMark("icebath.client.active", map[string]any{})
	if c, ok := ClientActiveCounter(event); !ok || c.Metric != ClientActiveMetric || len(c.Dimensions) != 0 {
		t.Fatalf("got %v %v", c, ok)
	}
	if _, ok := ClientActiveCounter(metricMark("icebath.client.active", map[string]any{"session_id": "privacy-canary"})); ok {
		t.Fatal("payload accepted")
	}
	wrong := metricMark("icebath.client.active", map[string]any{})
	wrong.Metadata = map[string]any{SchemaKey: "unknown"}
	if _, ok := ClientActiveCounter(wrong); ok {
		t.Fatal("wrong schema accepted")
	}
}

func TestToolFieldBuckets(t *testing.T) {
	for toolset, want := range map[string]string{"": "unknown", "file": "file", "browser-cdp": "browser", "feishu_doc": "communication", "mcp-github": "mcp", "private_plugin": "other"} {
		if got := ToolCategory(map[string]any{"toolset": toolset}); got != want {
			t.Fatalf("category %q = %q", toolset, got)
		}
	}
	if ToolCategory(map[string]any{"tool_name": "read_file"}) != "unknown" {
		t.Fatal("raw tool name classified")
	}
	for _, c := range []struct {
		status any
		want   string
	}{{"ok", "success"}, {"error", "failed"}, {"blocked", "blocked"}, {"cancelled", "cancelled"}, {"timeout", "timed_out"}, {"private", "unknown"}, {nil, "unknown"}} {
		if got := ToolOutcome(map[string]any{"status": c.status}); got != c.want {
			t.Fatalf("outcome %v = %q", c.status, got)
		}
	}
	for _, c := range []struct {
		choice any
		want   string
	}{{"smart_approve", "approved"}, {"smart_deny", "denied"}, {"timeout", "timed_out"}, {"cancelled", "cancelled"}, {nil, "unknown"}} {
		if got := ToolApprovalOutcome(map[string]any{"choice": c.choice}); got != c.want {
			t.Fatalf("approval %v = %q", c.choice, got)
		}
	}
	for _, c := range []struct {
		ms   any
		want string
	}{{0, "lt_100ms"}, {100, "100ms_to_250ms"}, {30_000, "gte_30s"}, {-1, "unknown"}, {true, "unknown"}, {"100", "unknown"}} {
		if got := ToolLatencyBucket(c.ms, nil); got != c.want {
			t.Fatalf("latency %v = %q", c.ms, got)
		}
	}
	for _, c := range []struct {
		count any
		want  string
	}{{0, "0"}, {1, "1"}, {2, "2"}, {3, "3_to_5"}, {6, "6_to_10"}, {11, "gte_11"}, {nil, "unknown"}, {-1, "unknown"}, {true, "unknown"}} {
		if got := ToolRetryBucket(c.count); got != c.want {
			t.Fatalf("retry %v = %q", c.count, got)
		}
	}
}

func TestModelCallFieldsReportTerminalModelAndShippedProvider(t *testing.T) {
	setCatalog(t, "provider_names", "openrouter", "anthropic")
	setCatalog(t, "user_named_model_providers", "custom", "lmstudio")
	check := func(in map[string]any, want map[string]string) {
		t.Helper()
		if got := ModelCallFields(in); !maps.Equal(got, want) {
			t.Fatalf("%v = %v, want %v", in, got, want)
		}
	}
	check(map[string]any{"model": "fallback/model", "response_model": "NVIDIA/Nemotron-3-Ultra", "provider": "OpenRouter",
		"base_url": "https://private-endpoint.example/v1"}, map[string]string{"model": "nvidia/nemotron-3-ultra", "provider": "openrouter"})
	check(map[string]any{"model": "ZAI/GLM-5.2", "provider": "Brev"}, map[string]string{"model": "custom", "provider": "custom"})
	for _, bad := range []string{"contains a space", strings.Repeat("x", ModelIdentifierMaxLength+1)} {
		check(map[string]any{"model": "nvidia/nemotron-3-ultra", "response_model": bad, "provider": "openrouter"},
			map[string]string{"model": "nvidia/nemotron-3-ultra", "provider": "openrouter"})
	}
	for _, c := range []struct {
		field string
		value any
	}{{"model", ""}, {"model", "contains a space"}, {"model", "contains\ncontrol"}, {"model", "_private"},
		{"model", strings.Repeat("x", ModelIdentifierMaxLength+1)}, {"model", struct{}{}}, {"provider", ""},
		{"provider", "private provider"}, {"provider", strings.Repeat("x", ProviderIdentifierMaxLength+1)}, {"provider", struct{}{}}} {
		in := map[string]any{"model": "nvidia/nemotron-3-ultra", "provider": "openrouter", c.field: c.value}
		if got := ModelCallFields(in)[c.field]; got != "unknown" {
			t.Fatalf("%s=%v gave %q", c.field, c.value, got)
		}
	}
	event := &RelayEvent{Kind: "scope", Category: "function", Name: tools.RelayLogicalLLMScope, ScopeCategory: "end",
		Data: map[string]any{"model": "Accepted/Model", "outcome": "success", "provider": "OpenRouter"},
		Metadata: map[string]any{tools.RelayRuntimeSchemaKey: tools.RelayRuntimeSchemaVersion,
			tools.RelayRuntimeInstanceKey: "runtime-1", "icebath.call_role": "auxiliary:compression"}}
	want := map[string]string{"call_role": "auxiliary", "error_class": "none", "model": "accepted/model", "outcome": "success",
		"provider": "openrouter", "ttft_bucket": "unknown"}
	if got, ok := ModelCallDimensions(event); !ok || !maps.Equal(got, want) {
		t.Fatalf("aux %v", got)
	}
	data := event.Data.(map[string]any)
	data["model"], data["response_model"] = "configured/model", "malformed response model"
	want["model"] = "configured/model"
	if got, ok := ModelCallDimensions(event); !ok || !maps.Equal(got, want) {
		t.Fatalf("aux fallback %v", got)
	}
	event.Metadata.(map[string]any)["icebath.call_role"] = "primary"
	if _, ok := ModelCallDimensions(event); ok {
		t.Fatal("primary projected")
	}
}

func TestToolSubscriberContractAcceptsOnlyBoundedEvents(t *testing.T) {
	data := map[string]any{"approval_outcome": "approved", "error_class": "none", "latency_bucket": "250ms_to_500ms",
		"outcome": "success", "retry_count_bucket": "0", "tool_category": "terminal", "tool_name": "terminal"}
	profile := map[string]any{}
	terminal := &RelayEvent{Kind: "scope", Category: "tool", CategoryProfile: profile, Name: "icebath.tool_call", ScopeCategory: "end",
		Metadata: map[string]any{SchemaKey: SchemaVersion}, Data: data}
	expect := func(got map[string]string, ok bool, want map[string]string) {
		t.Helper()
		if want == nil && ok || want != nil && (!ok || !maps.Equal(got, want)) {
			t.Fatalf("got %v %v, want %v", got, ok, want)
		}
	}
	d, ok := ToolCallDimensions(terminal)
	expect(d, ok, map[string]string{"approval_outcome": "approved", "outcome": "success", "tool_category": "terminal"})
	d, ok = ToolLatencyDimensions(terminal)
	expect(d, ok, map[string]string{"latency_bucket": "250ms_to_500ms", "retry_count_bucket": "0", "tool_category": "terminal"})
	d, ok = ToolUsageDimensions(terminal)
	expect(d, ok, map[string]string{"error_class": "none", "outcome": "success", "tool_name": "terminal"})
	data["tool_name"] = "private-plugin-tool"
	d, ok = ToolUsageDimensions(terminal)
	expect(d, ok, nil)
	d, ok = ToolCallDimensions(terminal)
	expect(d, ok, nil)
	data["tool_name"], data["result"] = "terminal", "must-not-pass"
	d, ok = ToolCallDimensions(terminal)
	expect(d, ok, nil)
	delete(data, "result")
	data["tool_category"] = "private-tool-name"
	d, ok = ToolCallDimensions(terminal)
	expect(d, ok, nil)
	data["tool_category"], profile["tool_name"] = "terminal", "must-not-pass"
	d, ok = ToolCallDimensions(terminal)
	expect(d, ok, nil)
	approval := metricMark("icebath.tool_approval", map[string]any{"attribution": "unattributed", "outcome": "denied"})
	c, ok := ToolApprovalCounter(approval)
	if !ok || c.Metric != "icebath.tool_approval.count" || !maps.Equal(c.Dimensions, map[string]string{"attribution": "unattributed", "outcome": "denied"}) {
		t.Fatalf("approval %v", c)
	}
	approval.Data.(map[string]any)["command"] = "must-not-pass"
	if _, ok := ToolApprovalCounter(approval); ok {
		t.Fatal("approval with command accepted")
	}
}

func TestSkillSubscriberContractAcceptsOnlyBoundedMarks(t *testing.T) {
	lifecycle := metricMark("icebath.skill.lifecycle", map[string]any{"action": "patched", "provenance": "agent_created"})
	if c, ok := SkillCounter(lifecycle); !ok || c.Metric != "icebath.skill.lifecycle.count" || len(c.Dimensions) != 2 {
		t.Fatalf("lifecycle %v", c)
	}
	loadData := map[string]any{"post_patch_state": "reused_after_patch", "provenance": "agent_created", "reuse_state": "reused",
		"skill_name": "custom", "use_count_bucket": "3_to_5"}
	load := metricMark("icebath.skill.load", loadData)
	load.Metadata = lifecycle.Metadata
	if c, ok := SkillCounter(load); !ok || c.Metric != "icebath.skill.load.count" || len(c.Dimensions) != 5 {
		t.Fatalf("load %v", c)
	}
	loadData["skill_name"] = "privacy-canary"
	if _, ok := SkillCounter(load); ok {
		t.Fatal("local skill name accepted")
	}
	loadData["skill_name"], loadData["provenance"] = "custom", "private-repository"
	if _, ok := SkillCounter(load); ok {
		t.Fatal("private provenance accepted")
	}
	lifecycle.Metadata.(map[string]any)["skill_name"] = "privacy-canary"
	if _, ok := SkillCounter(lifecycle); ok {
		t.Fatal("metadata skill name accepted")
	}
}

func TestSkillEventFieldsAreBoundedAndRejectMalformedUsage(t *testing.T) {
	prev := BundledSkillTreesHook
	BundledSkillTreesHook = func() (fs.FS, fs.FS) { return os.DirFS("../skills/bundled"), os.DirFS("../skills/optional") }
	SetCatalogSource("bundled_skill_names", bundledSkillNames)
	t.Cleanup(func() { BundledSkillTreesHook = prev; SetCatalogSource("bundled_skill_names", bundledSkillNames) })
	got, ok := SkillLifecycleFields(map[string]any{"action": "patched", "provenance": "agent_created", "skill_name": "privacy-canary"})
	if !ok || !maps.Equal(got, map[string]string{"action": "patched", "provenance": "agent_created"}) {
		t.Fatalf("lifecycle %v", got)
	}
	if _, ok := SkillLifecycleFields(map[string]any{"action": "deleted"}); ok {
		t.Fatal("deleted accepted")
	}
	got, ok = SkillLoadFields(map[string]any{"provenance": "private-repository", "use_count": 2, "reused": true,
		"reuse_after_patch": false, "skill_name": "privacy-canary"})
	want := map[string]string{"post_patch_state": "no_new_patch", "provenance": "unknown", "reuse_state": "reused",
		"skill_name": "custom", "use_count_bucket": "2"}
	if !ok || !maps.Equal(got, want) {
		t.Fatalf("load %v", got)
	}
	got, ok = SkillLoadFields(map[string]any{"use_count": 1, "reused": false, "reuse_after_patch": false, "skill_name": "codex"})
	want = map[string]string{"post_patch_state": "not_applicable", "provenance": "unknown", "reuse_state": "first_use",
		"skill_name": "codex", "use_count_bucket": "1"}
	if !ok || !maps.Equal(got, want) {
		t.Fatalf("bundled load %v", got)
	}
	if _, ok := SkillLoadFields(map[string]any{"use_count": 1, "reused": false, "reuse_after_patch": true}); ok {
		t.Fatal("patched first use accepted")
	}
}
