package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type titleAtTurnStartCall struct {
	userText string
	opts     autoTitleOptions
}

// Turn start title double recording every dispatch
func fakeMaybeAutoTitle(t *testing.T) *[]titleAtTurnStartCall {
	t.Helper()
	var mu sync.Mutex
	calls := &[]titleAtTurnStartCall{}
	swapTitleSeam(t, &maybeAutoTitleFn, func(_ context.Context, _ any, _, userText string, o autoTitleOptions) (any, bool) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, titleAtTurnStartCall{userText, o})
		return nil, false
	})
	return calls
}

// Agent with a created session row the turn start titler addresses
func titleInputAgent(t *testing.T, platform string) *AIAgent {
	t.Helper()
	return &AIAgent{sessionDB: newTitleTestDB(t, "cli", "test"), sessionID: "test", sessionDBCreated: true, platform: platform}
}

func TestTitleInputOverrideIsOptionalAndHistoryIsUnchanged(t *testing.T) {
	cases := []struct {
		override *string
		content  func() any
		want     *string
	}{
		{nil, func() any { return "Ordinary request" }, sp("Ordinary request")},
		{sp("Actual question"), func() any { return "Injected model context" }, sp("Actual question")},
		{sp("  Actual question  "), func() any { return "Injected model context" }, sp("Actual question")},
		{sp(""), func() any { return "Injected model context" }, nil},
		{sp("   "), func() any { return "Injected model context" }, nil},
		{nil, func() any {
			return []any{map[string]any{"type": "text", "text": "Image caption"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}
		}, sp("Image caption")},
		{nil, func() any {
			return []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}
		}, nil},
	}
	for i, c := range cases {
		titleTestHome(t)
		calls := fakeMaybeAutoTitle(t)
		a := titleInputAgent(t, "")
		mk := func() []*state.Message { return []*state.Message{{Role: "user", Content: c.content()}} }
		messages := mk()
		maybeTitleSessionAtTurnStart(context.Background(), a, messages, c.override)
		if !reflect.DeepEqual(messages, mk()) {
			t.Fatalf("case %d mutated history %#v", i, messages[0])
		}
		if c.want == nil {
			if len(*calls) != 0 {
				t.Fatalf("case %d titled %v", i, *calls)
			}
			continue
		}
		if len(*calls) != 1 || (*calls)[0].userText != *c.want || &(*calls)[0].opts.ConversationHistory[0] != &messages[0] {
			t.Fatalf("case %d calls %v", i, *calls)
		}
	}
}

func TestTitleOverridePreservesUpstreamPastePreview(t *testing.T) {
	for _, c := range []struct {
		override *string
		want     string
	}{{nil, "attachment"}, {sp("Original question"), "Original question"}} {
		titleTestHome(t)
		calls := fakeMaybeAutoTitle(t)
		mk := func() []*state.Message {
			return []*state.Message{{Role: "user", Content: "attachment", DisplayMetadata: map[string]any{"title_preview": "pasted topic"}}}
		}
		messages := mk()
		maybeTitleSessionAtTurnStart(context.Background(), titleInputAgent(t, ""), messages, c.override)
		if len(*calls) != 1 {
			t.Fatalf("calls %v", *calls)
		}
		call := (*calls)[0]
		if call.userText != c.want || call.opts.TitlePreview == nil || *call.opts.TitlePreview != "pasted topic" || &call.opts.ConversationHistory[0] != &messages[0] {
			t.Fatalf("call %+v", call)
		}
		if !reflect.DeepEqual(messages, mk()) {
			t.Fatalf("mutated history %#v", messages[0])
		}
	}
}

func TestTitleOverridePreservesExistingControlMessageEligibility(t *testing.T) {
	for text, titleable := range map[string]bool{"/help": true, "[CONTEXT COMPACTION] synthetic handoff": false} {
		titleTestHome(t)
		started := fakeAutoTitleSession(t)
		maybeTitleSessionAtTurnStart(context.Background(), titleInputAgent(t, ""), []*state.Message{{Role: "user", Content: "injected context"}}, sp(text))
		waitForTitleUpgrades(10 * time.Second)
		if got := started.Load() == 1; got != titleable {
			t.Fatalf("%q spawned the upgrade %v", text, got)
		}
	}
}

func TestTitleOverrideCannotEnableTitlingForMachineRuns(t *testing.T) {
	for _, platform := range []string{"cron", "subagent", "CRON"} {
		titleTestHome(t)
		calls := fakeMaybeAutoTitle(t)
		maybeTitleSessionAtTurnStart(context.Background(), titleInputAgent(t, platform), nil, sp("Actual question"))
		if len(*calls) != 0 {
			t.Fatalf("%s titled %v", platform, *calls)
		}
	}
}
