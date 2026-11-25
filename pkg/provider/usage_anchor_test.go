package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func anchorMsg(role, content string) *state.Message {
	return &state.Message{Role: role, Content: content}
}

func anchorImageMsg() *state.Message {
	return &state.Message{Role: "user", Content: []any{map[string]any{"type": "text", "text": "look at this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 40000)}}}}
}

func anchorPlainHistory() []*state.Message {
	return []*state.Message{anchorMsg("user", "start"), anchorMsg("assistant", "hello"), anchorMsg("user", "do the thing"), anchorMsg("assistant", "done")}
}

func anchorImageHistory(n int) []*state.Message {
	msgs := []*state.Message{anchorMsg("user", "start")}
	for i := 0; i < n; i++ {
		msgs = append(msgs, anchorMsg("assistant", "taking screenshot "+string(rune('0'+i))), anchorImageMsg())
	}
	return append(msgs, anchorMsg("assistant", "done looking"))
}

func TestAnchorArithmeticAnchorPlusSmallDelta(t *testing.T) {
	messages := anchorImageHistory(10)
	anchor := CaptureUsageAnchor(50_000, 250, messages)
	if anchor == nil || anchor.PromptTokens != 50_000 || anchor.BaseCount != len(messages) {
		t.Fatalf("anchor %+v", anchor)
	}
	messages = append(messages, anchorMsg("assistant", "the anchored reply itself"), anchorMsg("user", "short follow-up"))
	got, ok := AnchoredContextTokens(context.Background(), messages, anchor, true)
	delta := EstimateMessagesTokens(messages[len(messages)-1:], DefaultImageTokenCost, true)
	if !ok || got != 50_000+250+delta || delta >= 50 {
		t.Fatalf("anchored %d delta %d", got, delta)
	}
}

func TestAnchorArithmeticImageHeavyDivergenceEliminated(t *testing.T) {
	messages := anchorImageHistory(10)
	anchor := CaptureUsageAnchor(12_000, 100, messages)
	messages = append(messages, anchorMsg("assistant", "reply"), anchorMsg("user", "ok"))
	rough := EstimateMessagesTokens(messages, DefaultImageTokenCost, true)
	got, ok := AnchoredContextTokens(context.Background(), messages, anchor, true)
	if rough < 15_000 || !ok || got >= 12_200 || rough-got <= 2_800 {
		t.Fatalf("rough %d anchored %d", rough, got)
	}
}

func TestAnchorArithmeticNoUsageReturnsNone(t *testing.T) {
	messages := []*state.Message{anchorMsg("user", "hi")}
	if CaptureUsageAnchor(0, 0, messages) != nil || CaptureUsageAnchor(nil, nil, messages) != nil || CaptureUsageAnchor("garbage", 1, messages) != nil {
		t.Fatal("unusable usage must not anchor")
	}
	if CaptureUsageAnchor("12", "3", nil) != nil || CaptureUsageAnchor("12", "-3", messages).CompletionTokens != 0 {
		t.Fatal("integer text parses and an empty transcript never anchors")
	}
}

func TestAnchorArithmeticMissingAnchorFallsBack(t *testing.T) {
	if _, ok := AnchoredContextTokens(context.Background(), anchorImageHistory(2), nil, true); ok {
		t.Fatal("missing anchor falls back")
	}
}

func TestAnchorInvalidationCompactionRewriteFailsClosed(t *testing.T) {
	messages := anchorImageHistory(4)
	anchor := CaptureUsageAnchor(30_000, 50, messages)
	if _, ok := AnchoredContextTokens(context.Background(), []*state.Message{anchorMsg("user", "summary handoff"), anchorMsg("assistant", "[compressed summary]")}, anchor, true); ok {
		t.Fatal("compaction rewrite")
	}
}

func TestAnchorInvalidationMiddleSpliceShiftsBaseAndFailsClosed(t *testing.T) {
	messages := anchorImageHistory(4)
	anchor := CaptureUsageAnchor(30_000, 50, messages)
	spliced := append([]*state.Message{messages[0], anchorMsg("assistant", "[marker]")}, messages[5:]...)
	if _, ok := AnchoredContextTokens(context.Background(), spliced, anchor, true); ok {
		t.Fatal("middle splice")
	}
}

func TestAnchorInvalidationPrefixRewritePreservingBaseMessageFailsClosed(t *testing.T) {
	plain := anchorPlainHistory()
	pa := CaptureUsageAnchor(30_000, 50, plain)
	last := *plain[3]
	compacted := []*state.Message{anchorMsg("user", "summary handoff"), anchorMsg("assistant", "[compressed summary]"), anchorMsg("user", "new follow-up"), &last,
		anchorMsg("user", "post-anchor text must not be added to the stale base")}
	if _, ok := AnchoredContextTokens(context.Background(), compacted, pa, true); ok {
		t.Fatal("prefix rewrite preserving the base message")
	}
}

func TestAnchorInvalidationReloadedTranscriptWithSameContentStillMatches(t *testing.T) {
	messages := anchorImageHistory(4)
	anchor := CaptureUsageAnchor(30_000, 50, messages)
	var reloaded, edited []*state.Message
	for i, m := range messages {
		c := *m
		c.Timestamp, c.RowID = 1.0, int64(i)
		reloaded = append(reloaded, &c)
		e := *m
		edited = append(edited, &e)
	}
	if got, ok := AnchoredContextTokens(context.Background(), reloaded, anchor, true); !ok || got != 30_050 {
		t.Fatalf("reloaded %d %v", got, ok)
	}
	edited[len(edited)-1].Content = "different last message"
	if _, ok := AnchoredContextTokens(context.Background(), edited, anchor, true); ok {
		t.Fatal("edited last message must fail closed")
	}
}

// anchorStore is an in memory session row model config
type anchorStore struct {
	rows map[string]map[string]any
	fail bool
}

func (s *anchorStore) PatchModelConfig(id string, patch map[string]any) error {
	if s.fail {
		return errors.New("no row")
	}
	if s.rows[id] == nil {
		s.rows[id] = map[string]any{}
	}
	for k, v := range patch {
		s.rows[id][k] = v
	}
	return nil
}

func (s *anchorStore) GetModelConfigValue(id, key string) (any, bool, error) {
	v, ok := s.rows[id][key]
	return v, ok, nil
}

func TestAnchorInvalidationPersistAndRestoreAcrossProcesses(t *testing.T) {
	store := &anchorStore{rows: map[string]map[string]any{}}
	messages := anchorPlainHistory()
	first := &UsageAnchorState{SessionID: "s1", Store: store}
	anchor := CaptureUsageAnchor(30_000, 50, messages)
	first.Set(anchor, true)
	if first.TurnBaseAnchor != anchor || store.rows["s1"][UsageAnchorModelConfigKey] == nil {
		t.Fatal("set must install the turn base and persist")
	}
	fresh := &UsageAnchorState{SessionID: "s1", Store: store}
	fresh.Restore(messages, true)
	if fresh.Anchor == nil || *fresh.Anchor != *anchor {
		t.Fatalf("restored %+v", fresh.Anchor)
	}
	if got, ok := PersistedAnchorTokens(context.Background(), store, "s1", append(messages, anchorMsg("assistant", "reply")), true); !ok || got != 30_050 {
		t.Fatalf("persisted tokens %d %v", got, ok)
	}
	stale := &UsageAnchorState{SessionID: "s1", Store: store}
	stale.Restore([]*state.Message{anchorMsg("user", "other")}, true)
	if stale.Anchor != nil || store.rows["s1"][UsageAnchorModelConfigKey] != nil {
		t.Fatal("a stale blob is cleared and never adopted")
	}
	if _, ok := PersistedAnchorTokens(context.Background(), store, "s1", messages, true); ok {
		t.Fatal("cleared blob yields no figure")
	}
	store.rows["s2"] = map[string]any{UsageAnchorModelConfigKey: map[string]any{"prompt_tokens": "12", "completion_tokens": nil, "base_count": 4.0,
		"base_last_role": 7, "base_last_fp": "x", "base_prefix_fp": "y"}}
	if a := usageAnchorFromAny(store.rows["s2"][UsageAnchorModelConfigKey]); a == nil || a.PromptTokens != 12 || a.BaseLastRole != "" {
		t.Fatalf("serialized %+v", a)
	}
	disabled := &UsageAnchorState{SessionID: "s3", Store: store, PersistDisabled: true}
	disabled.Set(anchor, false)
	if _, ok := store.rows["s3"]; ok || disabled.TurnBaseAnchor != nil {
		t.Fatal("persist disabled writes nothing and a non turn base set keeps the turn anchor")
	}
	(&UsageAnchorState{SessionID: "s4", Store: &anchorStore{fail: true}}).Set(nil, false)
}

func TestAnchorFingerprintsMatchTheReferenceDigests(t *testing.T) {
	if got := MessageFingerprint(anchorMsg("user", "markdown")); got != "4c2fa7cd7d8f7ccbeff8580c143b346e85c30005e7a9993766e6dd0987f70f86" {
		t.Fatalf("user fingerprint %s", got)
	}
	if got := pricedPrefixFingerprint([]*state.Message{anchorMsg("user", "markdown")}, 1); got != "78785a98c15c63ae0b58342873a12b4e6ba6defbf825ac5a687a4d0ca9ec891d" {
		t.Fatalf("prefix fingerprint %s", got)
	}
	messages := []*state.Message{anchorMsg("user", "café 日本語 \U0001f680"),
		{Role: "assistant", ToolCalls: []state.ToolCall{{ID: "call_1", Type: "function", Function: state.ToolCallFunction{Name: "terminal", Arguments: `{"command": "ls"}`}}}},
		{Role: "tool", Content: "ok", ToolCallID: "call_1"}}
	want := []string{"09bce09fb701cca555bc4039e7dce7b10151fb614eb95fed2e40abc0861ab4f8", "7ddd43e27343f19a1c4804853db9f8e5f313aa87bfd83f3c58892a795bd88f46",
		"7443da779ab0f3c84d3c59d67de556efc233da5bea6944797af0a9ca8b19c32f"}
	for i, m := range messages {
		if got := MessageFingerprint(m); got != want[i] {
			t.Fatalf("message %d fingerprint %s", i, got)
		}
	}
	anchor := CaptureUsageAnchor(1200, 80, messages)
	if anchor.BasePrefixFP != "beeb87e2a90759f2974a00d6681e056a254a147eefda5e98b624dbb5975457ca" {
		t.Fatalf("prefix fingerprint %s", anchor.BasePrefixFP)
	}
	store := &anchorStore{rows: map[string]map[string]any{}}
	(&UsageAnchorState{SessionID: "s1", Store: store}).Set(anchor, true)
	text, err := jsonutil.Encode(store.rows["s1"][UsageAnchorModelConfigKey])
	if err != nil || text != `{"prompt_tokens":1200,"completion_tokens":80,"base_count":3,"base_last_role":"tool","base_last_fp":"7443da779ab0f3c84d3c59d67de556efc233da5bea6944797af0a9ca8b19c32f","base_prefix_fp":"beeb87e2a90759f2974a00d6681e056a254a147eefda5e98b624dbb5975457ca"}` {
		t.Fatalf("persisted blob %s %v", text, err)
	}
}

func TestAnchorRestoresFromTheBlobTheSessionRowLoads(t *testing.T) {
	messages := anchorPlainHistory()
	anchor := CaptureUsageAnchor(30_000, 50, messages)
	text, err := jsonutil.Encode(anchor.toObject())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := jsonutil.Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	store := &anchorStore{rows: map[string]map[string]any{"s1": {UsageAnchorModelConfigKey: loaded}}}
	fresh := &UsageAnchorState{SessionID: "s1", Store: store}
	fresh.Restore(messages, true)
	if fresh.Anchor == nil || *fresh.Anchor != *anchor {
		t.Fatalf("restored %+v from %T", fresh.Anchor, loaded)
	}
}

func TestPlanPromptIncludesTaskVerbatimAndInfersWithoutOne(t *testing.T) {
	task := "migrate the auth provider to OIDC with zero downtime"
	prompt := BuildPlanPrompt("  " + task + "\n")
	if !strings.Contains(prompt, "Task to plan:\n"+task+"\n") || !strings.HasPrefix(prompt, "[/plan — plan mode]\n\nFor this turn, you are in PLAN MODE — planning only.\n\n- Do not implement code.") {
		t.Fatalf("prompt %q", prompt[:120])
	}
	if p := BuildPlanPrompt(" "); !strings.Contains(p, "No explicit task was given with /plan") || !strings.Contains(p, ".icebath/plans/") {
		t.Fatal("empty task infers from context")
	}
}
