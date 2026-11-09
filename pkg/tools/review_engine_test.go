package tools

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type reviewParentDouble struct{ prompt string }

func (p *reviewParentDouble) EphemeralSystemPrompt() string { return p.prompt }

func swapReviewConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	saved := reviewLoadConfigReadonly
	reviewLoadConfigReadonly = func(context.Context, *Env) (map[string]any, error) { return cfg, nil }
	t.Cleanup(func() { reviewLoadConfigReadonly = saved })
}

func userMsg(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

func TestSnapshotTakesLastTenChatMessagesOnly(t *testing.T) {
	msgs := []map[string]any{{"role": "system", "content": "sys"}}
	for i := 0; i < 15; i++ {
		msgs = append(msgs, userMsg("m"+strconv.Itoa(i)))
	}
	msgs = append(msgs, map[string]any{"role": "tool", "content": "tool out"})
	snap := SnapshotRecentMessages(msgs, DefaultReviewContextMessages)
	if len(snap) != 10 || snap[0]["text"] != "m5" || snap[9]["text"] != "m14" {
		t.Fatalf("snapshot %v", snap)
	}
	for _, m := range snap {
		if m["role"] != "user" {
			t.Fatalf("role %q", m["role"])
		}
	}
}

func TestSnapshotSkipsToolcallStubAssistantMessages(t *testing.T) {
	msgs := []map[string]any{
		userMsg("make a PR"),
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "x"}}},
		{"role": "tool", "content": "created"},
		{"role": "assistant", "content": "PR #123: https://example.com/pr/123"},
	}
	snap := SnapshotRecentMessages(msgs, DefaultReviewContextMessages)
	var texts []string
	for _, m := range snap {
		texts = append(texts, m["text"])
	}
	if !reflect.DeepEqual(texts, []string{"make a PR", "PR #123: https://example.com/pr/123"}) {
		t.Fatalf("texts %q", texts)
	}
}

func TestSnapshotHandlesMultimodalContentLists(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look at this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
	}}}
	snap := SnapshotRecentMessages(msgs, DefaultReviewContextMessages)
	if snap[0]["text"] != "look at this\n[image_url]" {
		t.Fatalf("text %q", snap[0]["text"])
	}
}

func TestSnapshotCapsOversizedMessages(t *testing.T) {
	snap := SnapshotRecentMessages([]map[string]any{userMsg(strings.Repeat("x", 50_000))}, DefaultReviewContextMessages)
	if len(snap[0]["text"]) >= 13_000 || !strings.HasSuffix(snap[0]["text"], "[... truncated ...]") {
		t.Fatalf("text length %d tail %q", len(snap[0]["text"]), snap[0]["text"][len(snap[0]["text"])-30:])
	}
}

func TestBuildReviewTaskIncludesExcerptAndPrompt(t *testing.T) {
	snap := []map[string]string{{"role": "user", "text": "review my PR"}, {"role": "assistant", "text": "PR #99 opened"}}
	prompt := "focus on security\n" + strings.Repeat("keep these instructions intact ", 20)
	goal, context := BuildReviewTask(snap, prompt, nil)
	if !strings.HasPrefix(goal, "Review: focus on security ") || len([]rune(goal)) > 80 || strings.Contains(goal, "\n") || !strings.HasSuffix(goal, "…") {
		t.Fatalf("goal %q", goal)
	}
	for _, want := range []string{ReviewGoal, "[USER]", "[PRIMARY AGENT]", "PR #99 opened", strings.TrimSpace(prompt)} {
		if !strings.Contains(context, want) {
			t.Fatalf("context lacks %q", want)
		}
	}
}

func TestLoadReviewCredentialsCfgReadsConfig(t *testing.T) {
	swapReviewConfig(t, map[string]any{"auxiliary": map[string]any{"review": map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.6"}}})
	cfg := loadReviewCredentialsCfg(context.Background(), nil)
	want := map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.6", "base_url": "", "api_key": "", "api_mode": ""}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestLoadReviewCredentialsCfgAutoMeansInherit(t *testing.T) {
	swapReviewConfig(t, map[string]any{"auxiliary": map[string]any{"review": map[string]any{"provider": "auto", "model": ""}}})
	if cfg := loadReviewCredentialsCfg(context.Background(), nil); cfg != nil {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestLoadReviewCredentialsCfgMissingSection(t *testing.T) {
	swapReviewConfig(t, map[string]any{"auxiliary": map[string]any{}})
	if cfg := loadReviewCredentialsCfg(context.Background(), nil); cfg != nil {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestStartReviewRejectsEmptyConversation(t *testing.T) {
	_, err := StartReview(context.Background(), nil, &reviewParentDouble{}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err %v", err)
	}
}

func TestStartReviewRequiresAgent(t *testing.T) {
	_, err := StartReview(context.Background(), nil, nil, []map[string]any{userMsg("x")}, "")
	if err == nil || !strings.Contains(err.Error(), "No active agent") {
		t.Fatalf("err %v", err)
	}
	var typedNil *reviewParentDouble
	if _, err := StartReview(context.Background(), nil, typedNil, []map[string]any{userMsg("x")}, ""); err == nil {
		t.Fatal("typed nil parent accepted")
	}
}

func skillCall(args string) map[string]any {
	return map[string]any{"function": map[string]any{"name": "skill_view", "arguments": args}}
}

func TestCollectSkillsFromPreloadedPromptAndHistory(t *testing.T) {
	parent := &reviewParentDouble{prompt: "[IMPORTANT: The user launched this CLI session with the " +
		"\"hermes-agent-dev\" skill preloaded. Treat its instructions as " +
		"active guidance for the duration of this session unless the user " +
		"overrides them.]"}
	msgs := []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []any{
			skillCall(`{"name": "github-pr-workflow"}`),
			skillCall(`{"name": "hermes-agent-dev", "file_path": "references/x.md"}`),
			map[string]any{"function": map[string]any{"name": "read_file", "arguments": `{"path": "/tmp/x"}`}},
		}},
		{"role": "assistant", "content": "", "tool_calls": []any{skillCall(`{"name": "github-pr-workflow"}`)}},
	}
	names := CollectParentLoadedSkills(parent, msgs, 8)
	if !reflect.DeepEqual(names, []string{"hermes-agent-dev", "github-pr-workflow"}) {
		t.Fatalf("names %q", names)
	}
}

func TestCollectSkillsEmptyWhenNoneLoaded(t *testing.T) {
	if names := CollectParentLoadedSkills(&reviewParentDouble{}, []map[string]any{userMsg("hi")}, 8); len(names) != 0 {
		t.Fatalf("names %q", names)
	}
}

func TestCollectSkillsCapsAtLimit(t *testing.T) {
	var msgs []map[string]any
	for i := 0; i < 15; i++ {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{skillCall(`{"name": "skill-` + strconv.Itoa(i) + `"}`)}})
	}
	if names := CollectParentLoadedSkills(&reviewParentDouble{}, msgs, 8); len(names) != 8 {
		t.Fatalf("names %q", names)
	}
}

func TestBriefingIncludesLoadedSkillsInstruction(t *testing.T) {
	_, context := BuildReviewTask([]map[string]string{{"role": "user", "text": "review my PR"}}, "", []string{"hermes-agent-dev", "xitter"})
	if !strings.Contains(context, "hermes-agent-dev, xitter") || !strings.Contains(context, "skill_view") {
		t.Fatalf("context %q", context)
	}
}

func TestFormatDispatchNoteDispatched(t *testing.T) {
	result := pyjson.NewObject()
	result.Set("status", "dispatched")
	result.Set("review_model", "opus")
	note := FormatDispatchNote(result, strings.Repeat("security\n", 100))
	if strings.Contains(note, "security") || strings.Contains(note, "\n") || len(note) >= 80 {
		t.Fatalf("note %q", note)
	}
}
