package provider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var pcMarker = map[string]any{"type": "ephemeral"}

// Same underlying map object, the Go form of a Python identity check
func pcSame(a, b map[string]any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

func pcCopy(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i], _ = asMap(deepCopyAny(m))
	}
	return out
}

// Indexes of messages carrying a top level or part level marker
func pcMarkerIndexes(msgs []map[string]any) map[int]bool {
	out := map[int]bool{}
	for i, m := range msgs {
		if _, has := m["cache_control"]; has || hasPartMarker(m["content"]) {
			out[i] = true
		}
	}
	return out
}

func pcHistory(t *testing.T) []map[string]any {
	return anthMsgs(t, `[
		{"role":"system","content":"stable prefix\nvolatile suffix"},
		{"role":"user","content":"first request"},
		{"role":"assistant","content":"","tool_calls":[{"id":"first","function":{"name":"tool_00","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"first","content":"first result"},
		{"role":"user","content":"second request"},
		{"role":"assistant","content":"","tool_calls":[{"id":"second","function":{"name":"tool_01","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"second","content":"second result"}]`)
}

func pcThirdExchange(t *testing.T) []map[string]any {
	return anthMsgs(t, `[
		{"role":"user","content":"third request"},
		{"role":"assistant","content":"","tool_calls":[{"id":"third","function":{"name":"tool_02","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"third","content":"third result"}]`)
}

func pcTools() []map[string]any {
	tools := make([]map[string]any, 0, 28)
	for i := 0; i < 28; i++ {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": fmt.Sprintf("tool_%02d", i), "description": fmt.Sprintf("Deterministic tool %d", i),
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}})
	}
	return tools
}

func pcNativePlan(msgs, tools []map[string]any, direct bool) ([]map[string]any, []map[string]any) {
	return buildPromptCachePlan(msgs, tools, cachePlanOptions{ttl: "5m", nativeAnthropic: true, staticSystemPrefix: "stable prefix", directNativeToolCache: direct, toolPartMarkers: true})
}

func TestPromptCacheT20880ToolHeavyNativeLoopReproduction(t *testing.T) {
	tools := pcTools()
	beforeMsgs, _ := pcNativePlan(pcHistory(t), tools, true)
	afterMsgs, afterTools := pcNativePlan(append(pcHistory(t), pcThirdExchange(t)...), tools, true)
	before, after := pcMarkerIndexes(beforeMsgs), pcMarkerIndexes(afterMsgs)
	shared := false
	for i := range before {
		if i != 0 && after[i] {
			shared = true
		}
	}
	if _, has := afterTools[len(afterTools)-1]["cache_control"]; !has {
		t.Fatal("final tool must carry a marker")
	}
	if !shared {
		t.Fatalf("no shared transaction endpoint before=%v after=%v", before, after)
	}
	if n := countCacheMarkers(afterMsgs, afterTools); n > 4 {
		t.Fatalf("markers %d", n)
	}
}

func TestPromptCachePlan(t *testing.T) {
	t.Run("copies_sections_and_keeps_canonical_tools_plain", func(t *testing.T) {
		msgs, tools := pcHistory(t), pcTools()
		origMsgs, origTools := pcCopy(msgs), pcCopy(tools)
		planMsgs, planTools := pcNativePlan(msgs, tools, true)
		anthMustEqualJSON(t, msgs, origMsgs)
		anthMustEqualJSON(t, tools, origTools)
		if &planMsgs[0] == &msgs[0] || &planTools[0] == &tools[0] {
			t.Fatal("plan sections must be new lists")
		}
		if _, has := tools[len(tools)-1]["cache_control"]; has {
			t.Fatal("caller tool marked")
		}
		anthMustEqualJSON(t, planTools[len(planTools)-1]["cache_control"], pcMarker)
		if n := countCacheMarkers(planMsgs, planTools); n != 4 {
			t.Fatalf("markers %d", n)
		}
	})
	t.Run("shares_only_unmodified_history_rows", func(t *testing.T) {
		msgs := append(pcHistory(t), pcThirdExchange(t)...)
		for _, direct := range []bool{false, true} {
			orig := pcCopy(msgs)
			planMsgs, _ := pcNativePlan(msgs, pcTools(), direct)
			anthMustEqualJSON(t, msgs, orig)
			if &planMsgs[0] == &msgs[0] {
				t.Fatal("plan list must be a new list")
			}
			if !pcSame(planMsgs[1], msgs[1]) || pcSame(planMsgs[0], msgs[0]) || pcSame(planMsgs[len(planMsgs)-1], msgs[len(msgs)-1]) {
				t.Fatalf("direct=%v sharing wrong", direct)
			}
		}
	})
	t.Run("unmarked_text_parts_keep_string_equivalent_cache_plan", func(t *testing.T) {
		plain := pcHistory(t)
		partForm := pcCopy(plain)
		partForm[0]["content"] = []any{map[string]any{"type": "text", "text": "stable prefix"}, map[string]any{"type": "text", "text": "\nvolatile suffix"}}
		partForm[1]["content"] = []any{map[string]any{"type": "text", "text": "first request"}}
		orig := pcCopy(partForm)
		for _, native := range []bool{false, true} {
			for _, direct := range []bool{false, true} {
				opts := cachePlanOptions{ttl: "5m", nativeAnthropic: native, staticSystemPrefix: "stable prefix", directNativeToolCache: direct, toolPartMarkers: true}
				am, at := buildPromptCachePlan(partForm, pcTools(), opts)
				em, et := buildPromptCachePlan(plain, pcTools(), opts)
				anthMustEqualJSON(t, am, em)
				anthMustEqualJSON(t, at, et)
				anthMustEqualJSON(t, partForm, orig)
			}
		}
	})
	t.Run("unmarkable_endpoint_does_not_consume_a_slot", func(t *testing.T) {
		msgs := anthMsgs(t, `[{"role":"system","content":"stable prefix\nvolatile"},
			{"role":"assistant","content":"","tool_calls":[{"id":"pending","function":{"name":"tool_00","arguments":"{}"}}]}]`)
		planMsgs, planTools := pcNativePlan(msgs, pcTools(), true)
		if n := countCacheMarkers(planMsgs, planTools); n != 2 {
			t.Fatalf("markers %d", n)
		}
		if _, has := planMsgs[len(planMsgs)-1]["cache_control"]; has {
			t.Fatal("pending assistant marked")
		}
	})
	t.Run("static_prefix_equal_to_whole_prompt_emits_no_empty_block", func(t *testing.T) {
		msgs := anthMsgs(t, `[{"role":"system","content":"stable prefix"},{"role":"user","content":"lookup"}]`)
		planMsgs, planTools := pcNativePlan(msgs, pcTools(), true)
		parts, ok := asList(planMsgs[0]["content"])
		if !ok {
			t.Fatalf("system content %#v", planMsgs[0]["content"])
		}
		marked := false
		for _, p := range parts {
			pm, _ := asMap(p)
			if asStr(pm["text"]) == "" {
				t.Fatal("empty text block on the wire")
			}
			if _, has := pm["cache_control"]; has {
				marked = true
			}
		}
		if !marked {
			t.Fatal("system not marked")
		}
		anthMustEqualJSON(t, planTools[len(planTools)-1]["cache_control"], pcMarker)
	})
	t.Run("tool_strip_is_request_local", func(t *testing.T) {
		tools := pcTools()
		tools[len(tools)-1]["cache_control"] = pcMarker
		stripped := stripAnthropicToolCacheControl(tools)
		if _, has := tools[len(tools)-1]["cache_control"]; !has {
			t.Fatal("caller tool lost its marker")
		}
		if _, has := stripped[len(stripped)-1]["cache_control"]; has {
			t.Fatal("stripped tool kept its marker")
		}
	})
	t.Run("direct_tool_cache_with_no_tools_falls_back_safely", func(t *testing.T) {
		msgs := anthMsgs(t, `[{"role":"system","content":"System prompt"},{"role":"user","content":"Question"},{"role":"assistant","content":"Answer"}]`)
		planMsgs, planTools := buildPromptCachePlan(msgs, nil, cachePlanOptions{ttl: "5m", nativeAnthropic: true, directNativeToolCache: true, toolPartMarkers: true})
		if n := countCacheMarkers(planMsgs, planTools); n != 3 || len(planTools) != 0 {
			t.Fatalf("markers %d tools %d", n, len(planTools))
		}
	})
}

func TestPromptCacheApplyCacheMarker(t *testing.T) {
	msg := map[string]any{"role": "tool", "content": "result"}
	applyCacheMarker(msg, pcMarker, true, true)
	anthMustEqualJSON(t, msg["cache_control"], pcMarker)
	msg = map[string]any{"role": "tool", "content": "result"}
	applyCacheMarker(msg, pcMarker, false, true)
	if _, has := msg["cache_control"]; has {
		t.Fatal("envelope tool got a top level marker")
	}
	msg = map[string]any{"role": "user", "content": "Hello"}
	applyCacheMarker(msg, pcMarker, false, true)
	anthMustEqualJSON(t, msg["content"], []any{map[string]any{"type": "text", "text": "Hello", "cache_control": pcMarker}})
}

func TestPromptCacheCanCarryMarker(t *testing.T) {
	cases := []struct {
		msg    string
		native bool
		want   bool
	}{
		{`{"role":"assistant","content":""}`, false, false},
		{`{"role":"assistant","content":null}`, false, false},
		{`{"role":"tool","content":"result"}`, false, true},
		{`{"role":"tool","content":""}`, false, false},
		{`{"role":"assistant","content":null}`, true, true},
		{`{"role":"user","content":[{"type":"text","text":"a"}]}`, false, true},
		{`{"role":"user","content":[{"type":"text","text":"a"},"trailing raw"]}`, false, false},
		{`{"role":"user","content":[]}`, false, false},
	}
	for _, c := range cases {
		msg, _ := asMap(anthJSON(t, c.msg))
		if got := canCarryMarker(msg, c.native, true); got != c.want {
			t.Fatalf("%s native=%v got %v", c.msg, c.native, got)
		}
	}
}

func TestPromptCacheApplyAnthropicCacheControl(t *testing.T) {
	t.Run("caller_list_not_mutated_and_unmarked_msgs_shared", func(t *testing.T) {
		msgs := anthMsgs(t, `[{"role":"system","content":"System"},{"role":"user","content":"middle-unmarked-1"},
			{"role":"assistant","content":"middle-unmarked-2"},{"role":"user","content":"m3"},{"role":"assistant","content":"m4"},{"role":"user","content":"m5"}]`)
		before := pcCopy(msgs)
		ptrs := []map[string]any{msgs[0], msgs[1], msgs[2], msgs[5]}
		result := applyAnthropicCacheControl(msgs, "5m", false, "", true)
		anthMustEqualJSON(t, msgs, before)
		if !pcSame(msgs[0], ptrs[0]) || !pcSame(msgs[5], ptrs[3]) {
			t.Fatal("caller list elements replaced")
		}
		if !pcSame(result[1], msgs[1]) || !pcSame(result[2], msgs[2]) {
			t.Fatal("unmarked messages must be shared")
		}
		if pcSame(result[0], msgs[0]) || pcSame(result[5], msgs[5]) {
			t.Fatal("marked messages must be copies")
		}
		result[0]["content"] = "TAMPERED"
		if msgs[0]["content"] != "System" {
			t.Fatal("tamper bled into caller")
		}
	})
	t.Run("output_equivalent_to_full_deepcopy_impl", func(t *testing.T) {
		reference := func(msgs []map[string]any, ttl string, native bool) []map[string]any {
			msgs = pcCopy(msgs)
			marker := buildCacheMarker(ttl)
			used := 0
			if asStr(msgs[0]["role"]) == "system" {
				applyCacheMarker(msgs[0], marker, native, true)
				used++
			}
			var nonSys []int
			for i, m := range msgs {
				if asStr(m["role"]) != "system" {
					nonSys = append(nonSys, i)
				}
			}
			for _, i := range nonSys[len(nonSys)-(4-used):] {
				applyCacheMarker(msgs[i], marker, native, true)
			}
			return msgs
		}
		base := anthMsgs(t, `[{"role":"system","content":"System"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},
			{"role":"user","content":"u2"},{"role":"assistant","content":"a2"},{"role":"user","content":"u3"}]`)
		for _, native := range []bool{true, false} {
			for _, ttl := range []string{"5m", "1h"} {
				anthMustEqualJSON(t, applyAnthropicCacheControl(pcCopy(base), ttl, native, "", true), reference(base, ttl, native))
			}
		}
	})
	t.Run("static_system_prefix_gets_its_own_marker", func(t *testing.T) {
		msgs := anthMsgs(t, `[{"role":"system","content":"stable prefix\n\nper-session context"},{"role":"user","content":"old request"},
			{"role":"assistant","content":"old response"},{"role":"user","content":"new request"}]`)
		result := applyAnthropicCacheControl(msgs, "5m", false, "stable prefix", true)
		anthMustEqualJSON(t, result[0]["content"], anthJSON(t, `[{"type":"text","text":"stable prefix","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"\n\nper-session context","cache_control":{"type":"ephemeral"}}]`))
		if result[1]["content"] != "old request" {
			t.Fatalf("old request %#v", result[1]["content"])
		}
		for _, i := range []int{2, 3} {
			parts, _ := asList(result[i]["content"])
			p0, _ := asMap(parts[0])
			anthMustEqualJSON(t, p0["cache_control"], pcMarker)
		}
	})
	t.Run("1h_ttl", func(t *testing.T) {
		result := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"}]`), "1h", false, "", true)
		parts, ok := asList(result[0]["content"])
		if !ok {
			t.Fatal("system content not a list")
		}
		p0, _ := asMap(parts[0])
		cc, _ := asMap(p0["cache_control"])
		if cc["ttl"] != "1h" {
			t.Fatalf("ttl %#v", cc)
		}
	})
}

func TestPromptCacheNormalizationOrdering(t *testing.T) {
	marked := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"user","content":"hello  \n"}]`), "5m", false, "", true)
	parts, ok := asList(marked[0]["content"])
	if !ok {
		t.Fatal("marked content stayed a string")
	}
	if p0, _ := asMap(parts[0]); p0["text"] != "hello  \n" {
		t.Fatalf("text %#v", p0["text"])
	}
	raw := "file1\nfile2\n"
	outOfWindow := pyStripSpace(raw)
	marked = applyAnthropicCacheControl([]map[string]any{{"role": "tool", "content": pyStripSpace(raw), "tool_call_id": "t1"}}, "5m", false, "", true)
	parts, _ = asList(marked[0]["content"])
	if p0, _ := asMap(parts[0]); p0["text"] != outOfWindow {
		t.Fatalf("in window %#v", p0["text"])
	}
}

func TestPromptCacheStripAnthropicCacheControl(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]`), "5m", true, "", true)
	if len(pcMarkerIndexes(msgs)) == 0 {
		t.Fatal("decoration produced no marker")
	}
	stripAnthropicCacheControl(msgs)
	if n := countCacheMarkers(msgs, nil); n != 0 {
		t.Fatalf("markers left %d", n)
	}
	msgs = anthMsgs(t, `[{"role":"user","content":[{"type":"text","text":"see","cache_control":{"type":"ephemeral"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,xx"}}]}]`)
	stripAnthropicCacheControl(msgs)
	parts, ok := asList(msgs[0]["content"])
	if !ok || len(parts) != 2 {
		t.Fatalf("content %#v", msgs[0]["content"])
	}
	anthMustEqualJSON(t, parts[0], map[string]any{"type": "text", "text": "see"})
	if p1, _ := asMap(parts[1]); p1["type"] != "image_url" {
		t.Fatalf("part1 %#v", p1)
	}
}

func TestPromptCacheEffectiveCacheTTL(t *testing.T) {
	cases := []struct{ ttl, provider, model, want string }{
		{"", "", "", "5m"}, {"", "anthropic", "claude-x", "5m"},
		{"5m", "", "", "5m"}, {"5m", "opencode", "qwen3.6-plus", "5m"},
		{"1h", "anthropic", "claude-opus-4.8", "1h"}, {"1h", "openrouter", "claude-3-5-sonnet", "1h"}, {"1h", "", "", "1h"},
		{"1h", "openrouter", "qwen3.6-plus", "5m"}, {"1h", "anthropic", "Qwen-Max", "5m"},
		{"1h", "opencode-go", "qwen3.8-max", "1h"}, {"1h", "opencode-go", "glm-5.2", "1h"},
		{"1h", "opencode-go", "minimax-m2.5", "5m"}, {"1h", "opencode-go", "vendor/MiniMax-M2.5", "5m"},
		{"1h", "opencode", "qwen3.6-plus", "5m"}, {"1h", "opencode-zen", "qwen3.6-plus", "5m"},
	}
	for _, p := range []string{"opencode", "opencode-zen", "alibaba"} {
		cases = append(cases, struct{ ttl, provider, model, want string }{"1h", p, "qwen-max", "5m"})
		cases = append(cases, struct{ ttl, provider, model, want string }{"1h", strings.ToUpper(p), "claude-x", "5m"})
	}
	for _, c := range cases {
		if got := effectiveCacheTTL(c.ttl, c.model, c.provider); got != c.want {
			t.Fatalf("%+v got %s", c, got)
		}
	}
	anthMustEqualJSON(t, buildCacheMarker(effectiveCacheTTL("1h", "qwen3.6-plus", "opencode")), pcMarker)
	anthMustEqualJSON(t, buildCacheMarker(effectiveCacheTTL("1h", "claude-x", "anthropic")), map[string]any{"type": "ephemeral", "ttl": "1h"})
}

func pcIdempotencyFixture() []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": "STATIC_PREFIX rest of the prompt"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("Hello %d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("Hi %d", i)})
	}
	return msgs
}

func TestPromptCacheApplyIdempotency(t *testing.T) {
	if out := applyAnthropicCacheControl([]map[string]any{}, "5m", false, "", true); len(out) != 0 {
		t.Fatal("empty input must stay empty")
	}
	round1 := applyAnthropicCacheControl(pcIdempotencyFixture(), "5m", false, "STATIC_PREFIX", true)
	round2 := applyAnthropicCacheControl(round1, "5m", false, "STATIC_PREFIX", true)
	round3 := applyAnthropicCacheControl(round2, "5m", false, "STATIC_PREFIX", true)
	anthMustEqualJSON(t, round1, round2)
	anthMustEqualJSON(t, round2, round3)
	if n := countCacheMarkers(round1, nil); n != 4 {
		t.Fatalf("markers %d", n)
	}
	history := make([]map[string]any, 5)
	for i := range history {
		history[i] = map[string]any{"role": "user", "content": fmt.Sprintf("u%d", i), "cache_control": map[string]any{"type": "ephemeral"}}
	}
	snapshot := pcCopy(history)
	applyAnthropicCacheControl(history, "5m", false, "", true)
	anthMustEqualJSON(t, history, snapshot)
	for i := range history {
		history[i] = map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": fmt.Sprintf("u%d", i), "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "tail"}}}
	}
	snapshot = pcCopy(history)
	result := applyAnthropicCacheControl(history, "5m", false, "", true)
	anthMustEqualJSON(t, history, snapshot)
	if n := countCacheMarkers(result, nil); n > 4 {
		t.Fatalf("markers %d", n)
	}
}

func TestPromptCacheOpenCodeGoOneHourPrecedence(t *testing.T) {
	const deployed = "qwen3.7-plus"
	anthMustEqualJSON(t, buildCacheMarker(effectiveCacheTTL("1h", deployed, "opencode-go")), map[string]any{"type": "ephemeral", "ttl": "1h"})
	for _, m := range []string{"qwen3.7-plus", "qwen3.8-max", "Qwen-Max"} {
		if effectiveCacheTTL("1h", m, "openrouter") != "5m" || effectiveCacheTTL("1h", m, "opencode-go") != "1h" {
			t.Fatalf("ordering broken for %s", m)
		}
	}
	for _, p := range []string{"minimax", "minimax-cn", "anthropic", "openrouter"} {
		if effectiveCacheTTL("1h", "MiniMax-M2.5", p) != "1h" {
			t.Fatalf("no 1h denial leaked to %s", p)
		}
	}
	for _, m := range []string{"qwen3.7-plus", "qwen3.8-max", "qwen3.6-plus", "qwen-max", "glm-5.2"} {
		if effectiveCacheTTL("1h", m, "opencode-go") != "1h" {
			t.Fatalf("allowance missing for %s", m)
		}
	}
	for _, p := range []string{"opencode-go", "OpenCode-Go", "OPENCODE-GO"} {
		if effectiveCacheTTL("1h", deployed, p) != "1h" {
			t.Fatalf("spelling %s", p)
		}
	}
}

func TestPromptCacheAutoTierFollowsSessionPace(t *testing.T) {
	if AutoCacheTTLForSource("cli") != "1h" || AutoCacheTTLForSource("telegram") != "1h" || AutoCacheTTLForSource("") != "1h" {
		t.Fatal("human paced sources must resolve to 1h")
	}
	if len(machinePacedSources) != 8 {
		t.Fatalf("machine paced sources %v", machinePacedSources)
	}
	for source := range machinePacedSources {
		if AutoCacheTTLForSource(source) != "5m" {
			t.Fatalf("%s must resolve to 5m", source)
		}
	}
}
