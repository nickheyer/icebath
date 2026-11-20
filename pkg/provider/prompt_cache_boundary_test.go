package provider

import (
	"fmt"
	"strings"
	"testing"
)

const pcSingleSkillInstruction = "The user has provided the following instruction alongside the skill invocation: "

// Empties the stable prefix registry before and after the test
func pcIsolatedRegistry(t *testing.T) {
	clear := func() {
		stablePrefixMu.Lock()
		stablePrefixList = nil
		stablePrefixMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func pcSetMaxChars(t *testing.T, n int) {
	old := stablePrefixMaxChars
	stablePrefixMaxChars = n
	t.Cleanup(func() { stablePrefixMaxChars = old })
}

func pcUser(content string) []map[string]any {
	return []map[string]any{{"role": "user", "content": content}}
}

func pcApply(msgs []map[string]any) []map[string]any {
	return applyAnthropicCacheControl(msgs, "5m", false, "", true)
}

func pcTurns(first string, later int) []map[string]any {
	msgs := pcUser(first)
	for i := 0; i < later; i++ {
		for _, role := range []string{"assistant", "user"} {
			msgs = append(msgs, map[string]any{"role": role, "content": fmt.Sprintf("turn %d", i)})
		}
	}
	return msgs
}

func pcParts(t *testing.T, content any) []map[string]any {
	t.Helper()
	list, ok := asList(content)
	if !ok {
		t.Fatalf("content not a list %#v", content)
	}
	out := make([]map[string]any, len(list))
	for i, p := range list {
		out[i], _ = asMap(p)
	}
	return out
}

func TestPromptCacheBoundaryRegistry(t *testing.T) {
	t.Run("requires_proper_prefix", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("scaffold")
		if findStablePrefix("scaffold volatile") != "scaffold" {
			t.Fatal("proper prefix missed")
		}
		for _, c := range []string{"scaffold", "scaffold   ", "scaffold\n\n\t", "other"} {
			if p := findStablePrefix(c); p != "" {
				t.Fatalf("%q matched %q", c, p)
			}
		}
	})
	t.Run("longest_registered_prefix_wins", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("scaffold")
		RegisterStablePrefix("scaffold extended")
		if findStablePrefix("scaffold extended tail") != "scaffold extended" {
			t.Fatal("longest prefix lost")
		}
	})
	t.Run("lru_eviction_falls_back_safely", func(t *testing.T) {
		pcIsolatedRegistry(t)
		for i := 0; i <= stablePrefixMaxEntries; i++ {
			RegisterStablePrefix(fmt.Sprintf("scaffold-%d ", i))
		}
		if findStablePrefix("scaffold-0 volatile") != "" || findStablePrefix("scaffold-1 volatile") != "scaffold-1 " {
			t.Fatal("eviction order wrong")
		}
	})
	t.Run("empty_prefix_never_registered", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("")
		if findStablePrefix("anything") != "" {
			t.Fatal("empty prefix registered")
		}
	})
	t.Run("lookup_refreshes_the_entry_lru_position", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("hot-scaffold ")
		for i := 0; i < stablePrefixMaxEntries-1; i++ {
			RegisterStablePrefix(fmt.Sprintf("cold-%d ", i))
		}
		if findStablePrefix("hot-scaffold volatile") != "hot-scaffold " {
			t.Fatal("hot scaffold missing")
		}
		RegisterStablePrefix("newcomer ")
		if findStablePrefix("hot-scaffold volatile") != "hot-scaffold " || findStablePrefix("cold-0 volatile") != "" {
			t.Fatal("lookup did not refresh the LRU slot")
		}
	})
	t.Run("total_char_cap_evicts_oldest_and_keeps_newest", func(t *testing.T) {
		pcIsolatedRegistry(t)
		pcSetMaxChars(t, 100)
		RegisterStablePrefix(strings.Repeat("a", 80))
		RegisterStablePrefix(strings.Repeat("b", 80))
		if findStablePrefix(strings.Repeat("a", 80)+"tail") != "" || findStablePrefix(strings.Repeat("b", 80)+"tail") != strings.Repeat("b", 80) {
			t.Fatal("char cap eviction wrong")
		}
	})
	t.Run("char_cap_counts_characters_not_bytes", func(t *testing.T) {
		pcIsolatedRegistry(t)
		pcSetMaxChars(t, 100)
		RegisterStablePrefix(strings.Repeat("é", 45))
		RegisterStablePrefix(strings.Repeat("è", 45))
		if findStablePrefix(strings.Repeat("é", 45)+"tail") == "" {
			t.Fatal("90 characters fit under a 100 character cap")
		}
	})
	t.Run("single_oversized_prefix_still_registers", func(t *testing.T) {
		pcIsolatedRegistry(t)
		pcSetMaxChars(t, 10)
		oversized := strings.Repeat("x", 500)
		RegisterStablePrefix(oversized)
		if findStablePrefix(oversized+"tail") != oversized {
			t.Fatal("oversized prefix dropped")
		}
	})
}

func TestPromptCacheBoundaryRequestLocalSplit(t *testing.T) {
	scaffold := "stable scaffold\n\n" + pcSingleSkillInstruction
	t.Run("volatile_tail_does_not_change_marked_prefix", func(t *testing.T) {
		pcIsolatedRegistry(t)
		sk := "stable skill scaffold\n\n" + pcSingleSkillInstruction
		RegisterStablePrefix(sk)
		first := pcParts(t, pcApply(pcUser(sk + "ticket=one time=10:00"))[0]["content"])
		second := pcParts(t, pcApply(pcUser(sk + "ticket=two time=10:01"))[0]["content"])
		if len(first) != 2 || len(second) != 2 {
			t.Fatalf("lengths %d %d", len(first), len(second))
		}
		anthMustEqualJSON(t, first[0], second[0])
		anthMustEqualJSON(t, first[0], map[string]any{"type": "text", "text": sk, "cache_control": pcMarker})
		if _, has := first[1]["cache_control"]; has || first[1]["text"] != "ticket=one time=10:00" || second[1]["text"] != "ticket=two time=10:01" {
			t.Fatalf("tails %#v %#v", first[1], second[1])
		}
	})
	t.Run("unregistered_message_keeps_whole_block_layout", func(t *testing.T) {
		pcIsolatedRegistry(t)
		content := "stable-looking scaffold\n\n" + pcSingleSkillInstruction + "tail"
		anthMustEqualJSON(t, pcApply(pcUser(content))[0]["content"], []any{map[string]any{"type": "text", "text": content, "cache_control": pcMarker}})
	})
	t.Run("assistant_string_matching_a_prefix_is_not_split", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("scaffold ")
		marked := pcApply([]map[string]any{{"role": "user", "content": "question"}, {"role": "assistant", "content": "scaffold reply"}})
		anthMustEqualJSON(t, marked[1]["content"], []any{map[string]any{"type": "text", "text": "scaffold reply", "cache_control": pcMarker}})
	})
	t.Run("canonical_message_stays_a_string_in_plan", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix("stable scaffold ")
		msgs := pcUser("stable scaffold volatile")
		planMsgs, _ := buildPromptCachePlan(msgs, nil, cachePlanOptions{ttl: "5m", nativeAnthropic: true, toolPartMarkers: true})
		anthMustEqualJSON(t, msgs, pcUser("stable scaffold volatile"))
		parts := pcParts(t, planMsgs[0]["content"])
		anthMustEqualJSON(t, parts[0]["cache_control"], pcMarker)
		if _, has := parts[1]["cache_control"]; has {
			t.Fatal("tail marked")
		}
	})
	for _, later := range []int{0, 4} {
		t.Run(fmt.Sprintf("strip_reconstructs_exact_string_and_redecorates_identically_%d", later), func(t *testing.T) {
			pcIsolatedRegistry(t)
			RegisterStablePrefix(scaffold)
			original := pcTurns(scaffold+"ticket=one", later)
			marked := pcApply(pcCopy(original))
			parts := pcParts(t, marked[0]["content"])
			if len(parts) != 2 || parts[0]["text"] != scaffold || parts[1]["text"] != "ticket=one" {
				t.Fatalf("parts %#v", parts)
			}
			firstWire := pcCopy(marked)
			stripped := stripAnthropicCacheControl(marked)
			anthMustEqualJSON(t, stripped, original)
			anthMustEqualJSON(t, pcApply(pcCopy(stripped)), firstWire)
		})
	}
	t.Run("unmarked_split_re_entering_the_window_marks_the_scaffold", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix(scaffold)
		original := pcTurns(scaffold+"ticket=one", 4)
		unmarked := pcApply(pcCopy(original))
		for _, p := range pcParts(t, unmarked[0]["content"]) {
			if _, has := p["cache_control"]; has {
				t.Fatal("scaffold outside the window carries a marker")
			}
		}
		again := pcApply(pcCopy(unmarked[:1]))
		anthMustEqualJSON(t, again, pcApply(pcCopy(original[:1])))
		parts := pcParts(t, again[0]["content"])
		anthMustEqualJSON(t, parts[0]["cache_control"], pcMarker)
		if _, has := parts[1]["cache_control"]; has {
			t.Fatal("tail marked")
		}
	})
	t.Run("direct_tool_cache_plan_keeps_the_split_outside_the_marker_window", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix(scaffold)
		history := pcTurns(scaffold+"ticket=one", 4)
		tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "terminal", "parameters": map[string]any{}}}}
		planMsgs, _ := buildPromptCachePlan(history, tools, cachePlanOptions{ttl: "5m", nativeAnthropic: true, directNativeToolCache: true, toolPartMarkers: true})
		anthMustEqualJSON(t, planMsgs[0]["content"], []any{map[string]any{"type": "text", "text": scaffold}, map[string]any{"type": "text", "text": "ticket=one"}})
		if _, ok := history[0]["content"].(string); !ok {
			t.Fatal("stored message lost its string form")
		}
	})
	t.Run("strip_flattens_even_after_the_prefix_was_evicted", func(t *testing.T) {
		pcIsolatedRegistry(t)
		RegisterStablePrefix(scaffold)
		original := pcUser(scaffold + "ticket=one")
		marked := pcApply(pcCopy(original))
		for i := 0; i <= stablePrefixMaxEntries; i++ {
			RegisterStablePrefix(fmt.Sprintf("unrelated-scaffold-%d ", i))
		}
		if findStablePrefix(scaffold+"ticket=one") != "" {
			t.Fatal("scaffold still registered")
		}
		anthMustEqualJSON(t, stripAnthropicCacheControl(marked), original)
	})
	t.Run("strip_leaves_organic_two_part_user_content_structured", func(t *testing.T) {
		pcIsolatedRegistry(t)
		organic := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "part one"}, map[string]any{"type": "text", "text": "part two"}}}}
		anthMustEqualJSON(t, stripAnthropicCacheControl(pcCopy(organic)), organic)
	})
	t.Run("strip_leaves_two_part_user_content_with_an_empty_first_part_structured", func(t *testing.T) {
		pcIsolatedRegistry(t)
		organic := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": "part two"}}}}
		anthMustEqualJSON(t, stripAnthropicCacheControl(pcCopy(organic)), organic)
	})
}
