package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// test_delegate_group_schema.py test_legacy_group_replay_remains_accepted_and_delivery_policy_controls_units normalize half
func TestNormalizeTaskListKeepsLegacyGroupTasks(t *testing.T) {
	tasks := []any{map[string]any{"goal": "Review first module", "group": "join"}, map[string]any{"goal": "Review second module", "group": "join"}, map[string]any{"goal": "Review third module"}}
	normalized, msg, err := NormalizeTaskList(nil, nil, tasks, nil, "leaf", 3)
	if err != nil || msg != "" || len(normalized) != 3 || normalized[0]["group"] != "join" {
		t.Fatalf("normalized %v %q %v", normalized, msg, err)
	}
	for _, tc := range []struct {
		tasks any
		goal  any
		want  string
	}{
		{"", nil, "Provide either 'goal' (single task) or 'tasks' (batch)."},
		{"[oops", nil, "tasks must be a JSON array of task objects; received a string that could not be parsed as JSON (Expecting value)."},
		{`{"goal": "x"}`, nil, "tasks must be a JSON array of task objects; parsed dict instead."},
		{[]any{}, nil, "No tasks provided. Pass tasks=[{goal: '...', context: '...'}, ...] — one entry per subagent (a single task is a one-entry array)."},
		{[]any{map[string]any{"goal": "a"}, map[string]any{"goal": "b"}, map[string]any{"goal": "c"}, map[string]any{"goal": "d"}}, nil, "Too many tasks: 4 provided, but max_concurrent_children is 3."},
		{[]any{"x"}, nil, "Task 0 must be an object, got str."},
		{[]any{map[string]any{"goal": "  "}}, nil, "Task 0 is missing a 'goal'."},
		{[]any{map[string]any{"goal": "TODO"}}, nil, "Task 0 has a placeholder goal ('TODO')."},
		{[]any{map[string]any{"goal": "Fix <file name> now"}}, nil, "Task 0 goal contains an unexpanded template marker ('<file name>')."},
		{[]any{map[string]any{"goal": "Fix it"}, map[string]any{"goal": "Review the whole repository"}}, nil, "Task 0 goal is too short ('Fix it')."},
	} {
		_, msg, err := NormalizeTaskList(tc.goal, nil, tc.tasks, nil, "leaf", 3)
		if err != nil || !strings.HasPrefix(msg, tc.want) {
			t.Fatalf("%v: %q %v", tc.tasks, msg, err)
		}
	}
	single, msg, _ := NormalizeTaskList("Fix it", "ctx", nil, map[string]any{"type": "object"}, "orchestrator", 3)
	if msg != "" || !reflect.DeepEqual(single[0], map[string]any{"goal": "Fix it", "context": "ctx", "role": "orchestrator", "output_schema": map[string]any{"type": "object"}}) {
		t.Fatalf("legacy goal %v %q", single, msg)
	}
	recovered, msg, _ := NormalizeTaskList(nil, nil, `[{"goal": "Fix it"}]`, nil, "leaf", 3)
	if msg != "" || recovered[0]["goal"] != "Fix it" {
		t.Fatalf("json string tasks %v %q", recovered, msg)
	}
	if _, _, err := NormalizeTaskList(nil, nil, []any{map[string]any{"goal": 5}}, nil, "leaf", 3); err == nil {
		t.Fatal("non string goal accepted")
	}
	schemas, msg := CoerceTaskSchemas([]map[string]any{{"goal": "a", "output_schema": map[string]any{"type": int64(42)}}}, nil)
	if schemas != nil || !strings.HasPrefix(msg, "Task 0 output_schema invalid: output_schema is not a valid JSON Schema: ") {
		t.Fatalf("schema coercion %v %q", schemas, msg)
	}
}

// test_delegate_images.py test_normalize_task_images_shapes_and_limit
func TestNormalizeTaskImagesShapesAndLimit(t *testing.T) {
	if c, m := NormalizeTaskImages(map[string]any{"goal": "g"}, 0); c != nil || m != "" {
		t.Fatal("absent images")
	}
	if c, m := NormalizeTaskImages(map[string]any{"images": []any{}}, 0); c != nil || m != "" {
		t.Fatal("empty images")
	}
	if c, _ := NormalizeTaskImages(map[string]any{"images": "/tmp/a.png"}, 0); !reflect.DeepEqual(c, []string{"/tmp/a.png"}) {
		t.Fatalf("scalar image %v", c)
	}
	if c, _ := NormalizeTaskImages(map[string]any{"images": []any{" /tmp/a.png ", "https://x.test/b.jpg"}}, 0); !reflect.DeepEqual(c, []string{"/tmp/a.png", "https://x.test/b.jpg"}) {
		t.Fatalf("stripped images %v", c)
	}
	for _, bad := range []any{int64(5), []any{""}, []any{int64(3)}} {
		if c, m := NormalizeTaskImages(map[string]any{"images": bad}, 2); c != nil || !strings.HasPrefix(m, "Task 2 ") {
			t.Fatalf("bad %v: %v %q", bad, c, m)
		}
	}
	many := make([]any, MaxTaskImages+1)
	for i := range many {
		many[i] = fmt.Sprintf("/tmp/%d.png", i)
	}
	if c, m := NormalizeTaskImages(map[string]any{"images": many}, 1); c != nil || !strings.Contains(m, "the per-task limit is 8") {
		t.Fatalf("limit %v %q", c, m)
	}
	per, m := CoerceTaskImages([]map[string]any{{"goal": "g"}}, []any{"/tmp/x.png"})
	if m != "" || !reflect.DeepEqual(per, [][]string{{"/tmp/x.png"}}) {
		t.Fatalf("legacy images %v %q", per, m)
	}
}

// test_gitspawn_config_injection.py git_state_hint cases of the malicious repo and plain filter repo
func TestGitStateHintRunsNoRepoConfiguredCommand(t *testing.T) {
	tmp := t.TempDir()
	repo, marker := makeMaliciousRepo(t, tmp)
	hint := GitStateHint(repo)
	if hint == "" || !strings.Contains(hint, "uncommitted file(s)) in "+repo) {
		t.Fatalf("hint %q", hint)
	}
	if fired := firedSinks(marker); len(fired) != 0 {
		t.Fatalf("sinks fired %v", fired)
	}
	if GitStateHint(filepath.Join(tmp, "missing")) != "" || GitStateHint(tmp) != "" {
		t.Fatal("hint outside a checkout")
	}
	transcript := filepath.Join(tmp, "t.log")
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeFileT(t, transcript, b.String())
	tail := TranscriptTail(transcript, TranscriptTailLines, TranscriptTailChars)
	if !strings.HasPrefix(tail, "line 10\n") || !strings.HasSuffix(tail, "line 29") {
		t.Fatalf("tail %q", tail)
	}
	if got := TranscriptTail(transcript, 20, 5); got != "…ne 29" {
		t.Fatalf("char bound %q", got)
	}
	if err := os.WriteFile(transcript+".empty", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tails := TranscriptTails(map[string]string{"0": transcript, "1": transcript + ".empty", "2": filepath.Join(tmp, "nope")})
	if len(tails) != 1 || tails["0"] != tail {
		t.Fatalf("tails %v", tails)
	}
}

type fakeRouteAgent struct {
	provider, model, baseURL, apiKey string
	overrides                        map[string]any
}

func (f *fakeRouteAgent) Provider() string                     { return f.provider }
func (f *fakeRouteAgent) Model() string                        { return f.model }
func (f *fakeRouteAgent) BaseURL() string                      { return f.baseURL }
func (f *fakeRouteAgent) APIKey() string                       { return f.apiKey }
func (f *fakeRouteAgent) RequestOverrides() map[string]any     { return f.overrides }
func (f *fakeRouteAgent) SetRequestOverrides(m map[string]any) { f.overrides = m }

// delegate_tool_tuning.py speed tier checks over stubbed provider gates
func TestTaskServiceTierChecksTheChildRoute(t *testing.T) {
	saved := DelegateServiceTierHooks
	t.Cleanup(func() { DelegateServiceTierHooks = saved })
	DelegateServiceTierHooks.ParseExactServiceTier = func(raw string) (string, error) {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "", "normal":
			return "", nil
		case "fast":
			return "priority", nil
		case "ultrafast":
			return "ultrafast", nil
		}
		return "", fmt.Errorf("unknown service tier: %s", raw)
	}
	DelegateServiceTierHooks.IsStaticTier = func(tier string) bool { return tier == "priority" || tier == "ultrafast" }
	var seen []string
	DelegateServiceTierHooks.ResolveFastModeOverrides = func(model, prov, base, tier string) map[string]any {
		seen = append(seen, strings.Join([]string{model, prov, base, tier}, "|"))
		if model == "slow-model" {
			return nil
		}
		return map[string]any{"service_tier": tier}
	}
	DelegateServiceTierHooks.UltrafastOffered = func(model, prov, base, token string) (bool, error) { return token == "plan-token", nil }
	parent := &fakeRouteAgent{provider: "openai-codex", model: "gpt-5", baseURL: "https://parent", apiKey: "parent-key", overrides: map[string]any{"speed": "x", "temperature": 0.2, "extra_body": map[string]any{"service_tier": "flex"}}}
	out, err := TaskServiceTier("fast", map[string]any{}, parent)
	if err != nil || !reflect.DeepEqual(out, map[string]any{"request_overrides": map[string]any{"temperature": 0.2, "service_tier": "priority"}, "service_tier": "priority", "task_tier_overrides": map[string]any{"service_tier": "priority"}}) {
		t.Fatalf("fast tier %v %v", out, err)
	}
	if seen[0] != "gpt-5|openai-codex|https://parent|priority" {
		t.Fatalf("route %q", seen[0])
	}
	if _, err := TaskServiceTier("ultrafast", map[string]any{"provider": "openai-codex", "base_url": "https://pinned", "api_key": "plan-token"}, parent); err != nil {
		t.Fatalf("pinned ultrafast %v", err)
	}
	if _, err := TaskServiceTier("ultrafast", map[string]any{}, parent); err == nil || err.Error() != "service_tier 'ultrafast' is not available for model 'gpt-5' on provider 'openai-codex'. Omit service_tier or choose a model/provider that offers it." {
		t.Fatalf("unoffered ultrafast %v", err)
	}
	if _, err := TaskServiceTier("warp", map[string]any{}, parent); err == nil || err.Error() != "Unknown service_tier 'warp'. Use normal, fast, or ultrafast." {
		t.Fatalf("unknown word %v", err)
	}
	normal, err := TaskServiceTier("normal", map[string]any{"model": "slow-model"}, parent)
	if err != nil || normal["service_tier"] != nil || len(normal["task_tier_overrides"].(map[string]any)) != 0 {
		t.Fatalf("normal %v %v", normal, err)
	}
	child := &fakeRouteAgent{overrides: map[string]any{"extra_body": map[string]any{"speed": "merged"}, "top_p": 1}}
	PinChildServiceTier(child, out)
	if !reflect.DeepEqual(child.overrides, map[string]any{"top_p": 1, "service_tier": "priority"}) {
		t.Fatalf("pinned child %v", child.overrides)
	}
	untouched := &fakeRouteAgent{overrides: map[string]any{"service_tier": "flex"}}
	PinChildServiceTier(untouched, map[string]any{})
	if untouched.overrides["service_tier"] != "flex" {
		t.Fatal("tierless task touched the child")
	}
	if got := WithoutServiceTier("not a dict"); len(got) != 0 {
		t.Fatalf("non dict %v", got)
	}
}
