package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Parent of test_delegate_capability_inheritance.py _parent on the trusted proxy route
func capabilityParent(capabilities map[string]bool) *AIAgent {
	return &AIAgent{provider: "custom:proxy", baseURL: "https://trusted-proxy.corp/v1", capabilities: capabilities}
}

func TestSameRouteChildInheritsCapabilityMap(t *testing.T) {
	got := inheritParentCapabilities(capabilityParent(map[string]bool{"openai_native_compaction": true}), "", "")
	if !reflect.DeepEqual(got, map[string]bool{"openai_native_compaction": true}) {
		t.Fatalf("got %v", got)
	}
}

func TestProviderOverrideStaysDefaultDeny(t *testing.T) {
	if got := inheritParentCapabilities(capabilityParent(map[string]bool{"openai_native_compaction": true}), "openai", ""); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestBaseUrlOverrideStaysDefaultDeny(t *testing.T) {
	if got := inheritParentCapabilities(capabilityParent(map[string]bool{"openai_native_compaction": true}), "", "https://other.example/v1"); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestNonDictParentCapabilitiesYieldNone(t *testing.T) {
	if got := inheritParentCapabilities(capabilityParent(nil), "", ""); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestInheritedMapIsSanitizedToStrBool(t *testing.T) {
	parent := capabilityParent(map[string]bool{"openai_native_compaction": true})
	got := inheritParentCapabilities(parent, "", "")
	if !reflect.DeepEqual(got, map[string]bool{"openai_native_compaction": true}) {
		t.Fatalf("got %v", got)
	}
	got["injected"] = true
	if _, leaked := parent.capabilities["injected"]; leaked {
		t.Fatal("inherited map shares the parent's storage")
	}
}

func toolsetTestContext(t *testing.T) context.Context {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	return tools.WithToolEnv(context.Background(), env)
}

func TestExpandParentToolsetsCompositeHermesCliExpandsWeb(t *testing.T) {
	expanded := expandParentToolsets(toolsetTestContext(t), map[string]bool{"icebath-cli": true})
	for _, want := range []string{"web", "terminal", "browser", "icebath-cli"} {
		if !expanded[want] {
			t.Fatalf("%q missing from %v", want, expanded)
		}
	}
}

func TestExpandParentToolsetsIncludedToolsetsOfCompositeParentAreGrantable(t *testing.T) {
	expanded := expandParentToolsets(toolsetTestContext(t), map[string]bool{"debugging": true})
	for _, want := range []string{"debugging", "terminal", "web", "file"} {
		if !expanded[want] {
			t.Fatalf("%q missing from %v", want, expanded)
		}
	}
	if expanded["browser"] || expanded["icebath-cli"] {
		t.Fatalf("expanded %v grants toolsets the parent lacks", expanded)
	}
}

func TestExpandParentToolsetsCompositesWithAllowedIncludedToolsAreNotStripped(t *testing.T) {
	got := stripBlockedToolsets(toolsetTestContext(t), []string{"safe", "icebath-gateway", "icebath-cli", "delegation", "kanban"})
	if !reflect.DeepEqual(got, []string{"safe", "icebath-gateway", "icebath-cli"}) {
		t.Fatalf("got %v", got)
	}
}

func TestToolsetIntersectionStripBlockedRemovesDelegation(t *testing.T) {
	child := stripBlockedToolsets(toolsetTestContext(t), []string{"terminal", "delegation", "clarify", "memory"})
	kept := map[string]bool{}
	for _, ts := range child {
		kept[ts] = true
	}
	if kept["delegation"] || kept["clarify"] || kept["memory"] || !kept["terminal"] {
		t.Fatalf("child toolsets %v", child)
	}
}

// Captures everything written to the process stdout and stderr while fn runs
func captureProcessOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()
	capture := func(target **os.File) (func() string, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		saved := *target
		*target = w
		return func() string {
			_ = w.Close()
			*target = saved
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			_ = r.Close()
			return buf.String()
		}, nil
	}
	readOut, err := capture(&os.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	readErr, err := capture(&os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	fn()
	return readOut(), readErr()
}

func TestEmitParentConsoleRoutesThroughParentSafePrintWhenAvailable(t *testing.T) {
	var captured []string
	parent := &AIAgent{printFn: func(line string) { captured = append(captured, line) }}
	var status bytes.Buffer
	prev := statusStdout
	statusStdout = &status
	t.Cleanup(func() { statusStdout = prev })
	out, errOut := captureProcessOutput(t, func() { emitParentConsole(parent, "  ✓ [1/3] Research done  (11.55s)") })
	if !reflect.DeepEqual(captured, []string{"  ✓ [1/3] Research done  (11.55s)"}) {
		t.Fatalf("captured %q", captured)
	}
	if out != "" || errOut != "" || status.Len() != 0 {
		t.Fatalf("stdout %q stderr %q status %q", out, errOut, status.String())
	}
}

// Writes the delegation block of the temp home config, an empty block clearing it
func writeDelegationBlock(t *testing.T, h string, block string) {
	t.Helper()
	body := "delegation: {}\n"
	if block != "" {
		body = "delegation:\n" + block
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sortedJSON(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func delegateDefinition(t *testing.T, ctx context.Context, env *tools.Env) map[string]any {
	t.Helper()
	defs := tools.Default.Definitions(ctx, env, map[string]bool{"delegate_task": true}, true)
	if len(defs) != 1 {
		t.Fatalf("definitions %v", defs)
	}
	return defs[0]
}

func TestGroupSchemaTracksDeliveryPolicyWithoutMutatingPreviousDefinitions(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	ctx := tools.WithToolEnv(context.Background(), env)
	original := sortedJSON(t, tools.DelegateTaskSchema)
	type snapshot struct {
		definition map[string]any
		serialized string
	}
	var snapshots []snapshot
	for _, cfg := range []struct {
		block   string
		enabled bool
	}{{"", false}, {"  independent_completions: true\n", true}, {"  independent_completions: false\n", false}} {
		writeDelegationBlock(t, env.Home, cfg.block)
		definition := delegateDefinition(t, ctx, env)
		function := definition["function"].(map[string]any)
		task := function["parameters"].(map[string]any)["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)
		_, hasGroup := task["properties"].(map[string]any)["group"]
		if hasGroup != cfg.enabled {
			t.Fatalf("enabled %v group property %v", cfg.enabled, hasGroup)
		}
		if bytes.Contains([]byte(function["description"].(string)), []byte("group")) != cfg.enabled {
			t.Fatalf("enabled %v description %q", cfg.enabled, function["description"])
		}
		if sortedJSON(t, delegateDefinition(t, ctx, env)) != sortedJSON(t, definition) {
			t.Fatal("two definition builds under one config differ")
		}
		for _, previous := range snapshots {
			if sortedJSON(t, previous.definition) != previous.serialized {
				t.Fatal("a previous definition was mutated")
			}
		}
		snapshots = append(snapshots, snapshot{definition, sortedJSON(t, definition)})
	}
	if sortedJSON(t, tools.DelegateTaskSchema) != original {
		t.Fatal("the static schema was mutated")
	}
}

func TestLegacyGroupReplayRemainsAcceptedAndDeliveryPolicyControlsUnits(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	ctx := tools.WithToolEnv(context.Background(), env)
	tasks := []any{
		map[string]any{"goal": "Review first module", "group": "join"},
		map[string]any{"goal": "Review second module", "group": "join"},
		map[string]any{"goal": "Review third module"},
	}
	stripped := tools.StripModelHiddenTaskFields(tasks)
	if reflect.ValueOf(stripped).Pointer() != reflect.ValueOf(tasks).Pointer() {
		t.Fatalf("stripped %v is a copy", stripped)
	}
	normalized, msg, err := tools.NormalizeTaskList(nil, nil, tasks, nil, "leaf", 3)
	if err != nil || msg != "" {
		t.Fatalf("normalize %q %v", msg, err)
	}
	wantNormalized := []map[string]any{tasks[0].(map[string]any), tasks[1].(map[string]any), tasks[2].(map[string]any)}
	if !reflect.DeepEqual(normalized, wantNormalized) {
		t.Fatalf("normalized %v", normalized)
	}
	batch := &delegateBatch{}
	for i, task := range normalized {
		batch.Children = append(batch.Children, delegateChild{Index: i, Task: task})
	}
	for _, enabled := range []bool{false, true} {
		block := "  independent_completions: false\n"
		if enabled {
			block = "  independent_completions: true\n"
		}
		writeDelegationBlock(t, env.Home, block)
		units := unitsOf(ctx, batch)
		if !enabled {
			if len(units) != 1 || units[0] != batch {
				t.Fatalf("units %v", units)
			}
			continue
		}
		var indices [][]int
		var groups []any
		for _, unit := range units {
			var idx []int
			for _, c := range unit.Children {
				idx = append(idx, c.Index)
			}
			sort.Ints(idx)
			indices = append(indices, idx)
			if unit.Group == nil {
				groups = append(groups, nil)
			} else {
				groups = append(groups, *unit.Group)
			}
		}
		if !reflect.DeepEqual(indices, [][]int{{0, 1}, {2}}) || !reflect.DeepEqual(groups, []any{"join", nil}) {
			t.Fatalf("indices %v groups %v", indices, groups)
		}
	}
}
