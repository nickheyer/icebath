package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func rowsOf(rows ...map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

func TestToTranscriptMessages(t *testing.T) {
	t.Run("preserves assistant tool-call rows so resume does not drop prior turns", func(t *testing.T) {
		rows := rowsOf(map[string]any{"role": "user", "text": "first prompt"}, map[string]any{"role": "tool", "context": "repo", "name": "search_files", "text": "ignored raw result"},
			map[string]any{"role": "assistant", "text": "first answer"}, map[string]any{"role": "user", "text": "second prompt"})
		got := toTranscriptMessages(rows)
		var pairs [][2]string
		for _, m := range got {
			pairs = append(pairs, [2]string{m.Role, m.Text})
		}
		if !reflect.DeepEqual(pairs, [][2]string{{"user", "first prompt"}, {"assistant", "first answer"}, {"user", "second prompt"}}) || !strings.Contains(got[1].Tools[0], "Search Files") {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("skips hidden display_kind rows entirely", func(t *testing.T) {
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": "visible prompt"},
			map[string]any{"role": "user", "text": "[CONTEXT COMPACTION — REFERENCE ONLY]", "display_kind": "hidden"}, map[string]any{"role": "assistant", "text": "visible reply"}))
		if len(got) != 2 || got[0].Text != "visible prompt" || got[1].Text != "visible reply" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("projects model_switch as an event with replaced text", func(t *testing.T) {
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": "hello"},
			map[string]any{"role": "user", "text": "[System: model changed to gpt-5]", "display_kind": "model_switch"}, map[string]any{"role": "assistant", "text": "hi"}))
		if len(got) != 3 || got[0].Kind != "" || got[1].Kind != "event" || got[1].Role != "system" || strings.Contains(got[1].Text, "[System:") || got[2].Text != "hi" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("projects async_delegation_complete with task_count metadata", func(t *testing.T) {
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": "do work"}, map[string]any{"role": "assistant", "text": "done"},
			map[string]any{"role": "user", "text": "[IMPORTANT: delegation done]", "display_kind": "async_delegation_complete", "display_metadata": map[string]any{"task_count": float64(3)}},
			map[string]any{"role": "assistant", "text": "merged"}))
		if len(got) != 4 || got[2].Kind != "event" || !strings.Contains(got[2].Text, "3") || got[3].Text != "merged" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("uses the display-only completion title without exposing the model payload", func(t *testing.T) {
		text := "[ASYNC DELEGATION BATCH COMPLETE — private]\nFull result evidence"
		title := "Subagent Task Failed: Review changes"
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": text, "display_kind": "async_delegation_complete", "display_metadata": map[string]any{"display_text": title}}))
		if got[0].Kind != "event" || got[0].Role != "system" || got[0].Text != title {
			t.Fatalf("got %#v", got)
		}
		if plain := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": text})); plain[0].Text != text {
			t.Fatalf("plain %#v", plain)
		}
	})
	t.Run("projects async_delegation_complete without metadata as generic text", func(t *testing.T) {
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": "event", "display_kind": "async_delegation_complete"}))
		if got[0].Kind != "event" || got[0].Text == "" || got[0].Text == "event" {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestUpsert(t *testing.T) {
	t.Run("appends when last role differs", func(t *testing.T) {
		if got := upsert([]msg{{Role: "user", Text: "hi"}}, "assistant", "hello"); len(got) != 2 {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("replaces when last role matches", func(t *testing.T) {
		if got := upsert([]msg{{Role: "assistant", Text: "partial"}}, "assistant", "full"); got[0].Text != "full" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("appends to empty", func(t *testing.T) {
		if got := upsert(nil, "user", "first"); !reflect.DeepEqual(got, []msg{{Role: "user", Text: "first"}}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("does not mutate", func(t *testing.T) {
		prev := []msg{{Role: "user", Text: "hi"}}
		upsert(prev, "assistant", "yo")
		if len(prev) != 1 {
			t.Fatal("mutated")
		}
	})
}

func TestCapTranscriptHistory(t *testing.T) {
	t.Run("keeps the intro and the newest bounded display rows", func(t *testing.T) {
		rows := []msg{{Kind: "intro", Role: "system"}}
		for i := 0; i < 1_005; i++ {
			rows = append(rows, msg{Role: "user", Text: fmt.Sprintf("m%d", i)})
		}
		capped := capTranscriptHistory(rows)
		if len(capped) != maxHistory || capped[0].Kind != "intro" || capped[1].Text != fmt.Sprintf("m%d", 1_005-(maxHistory-1)) || capped[len(capped)-1].Text != "m1004" {
			t.Fatalf("capped %d %#v", len(capped), capped[1])
		}
	})
}

func TestDisplayTimestamps(t *testing.T) {
	t.Run("threads persisted transcript timestamps onto rehydrated rows", func(t *testing.T) {
		got := toTranscriptMessages(rowsOf(map[string]any{"role": "user", "text": "when was this", "timestamp": float64(1_750_000_000)},
			map[string]any{"role": "assistant", "text": "right then", "timestamp": float64(1_750_000_060)}))
		if *got[0].CreatedAt != 1_750_000_000 || *got[1].CreatedAt != 1_750_000_060 {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("stamps live rows at append and preserves supplied times", func(t *testing.T) {
		live := appendTranscriptMessage(nil, msg{Role: "user", Text: "now"}, func() float64 { return 1_800_000_000 })
		if *live[0].CreatedAt != 1_800_000_000 {
			t.Fatalf("live %#v", live)
		}
		at := float64(123)
		kept := appendTranscriptMessage(nil, msg{CreatedAt: &at, Role: "user", Text: "then"}, func() float64 { return 1 })
		if *kept[0].CreatedAt != 123 {
			t.Fatalf("kept %#v", kept)
		}
	})
}

func TestStickyPromptFromViewport(t *testing.T) {
	turns := []msg{{Role: "user", Text: "older prompt"}, {Role: "assistant", Text: "older answer"}, {Role: "user", Text: "current prompt"}, {Role: "assistant", Text: "current answer"}}
	current := []msg{{Role: "user", Text: "current prompt"}, {Role: "assistant", Text: "current answer"}}
	t.Run("hides the sticky prompt when a newer user message is already visible", func(t *testing.T) {
		if got := stickyPromptFromViewport(turns, []int{0, 2, 10, 12, 20}, 8, 16, false); got != "" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("shows the latest user message above the viewport when no user message is visible", func(t *testing.T) {
		if got := stickyPromptFromViewport(turns, []int{0, 2, 10, 12, 20}, 16, 20, false); got != "current prompt" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("shows the last prompt once the viewport starts after the history tail", func(t *testing.T) {
		if got := stickyPromptFromViewport([]msg{{Role: "user", Text: "current prompt"}, {Role: "assistant", Text: "completed answer"}}, []int{0, 2, 5}, 8, 14, false); got != "current prompt" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("shows a prompt as soon as its full row is above the viewport", func(t *testing.T) {
		if got := stickyPromptFromViewport(current, []int{0, 2, 10}, 2, 8, false); got != "current prompt" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("hides the sticky prompt at the bottom", func(t *testing.T) {
		if got := stickyPromptFromViewport(current, []int{0, 2, 10}, 8, 10, true); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestMessageGroup(t *testing.T) {
	t.Run("classifies each block kind into its visual band", func(t *testing.T) {
		cases := []struct {
			m    msg
			want string
		}{
			{msg{Role: "assistant"}, "model"}, {msg{Role: "assistant", Kind: "diff"}, "diff"}, {msg{Role: "system", Kind: "trail"}, "trail"},
			{msg{Role: "system"}, "note"}, {msg{Role: "user"}, "user"}, {msg{Role: "user", Kind: "slash"}, "slash"},
			{msg{Role: "system", Kind: "intro"}, "intro"}, {msg{Role: "system", Kind: "panel"}, "intro"},
		}
		for _, c := range cases {
			if got := messageGroup(c.m); got != c.want {
				t.Fatalf("%#v gave %q", c.m, got)
			}
		}
	})
}

func TestHasLeadGap(t *testing.T) {
	trail, model, note := &msg{Role: "system", Kind: "trail"}, &msg{Role: "assistant"}, &msg{Role: "system"}
	user, diff, slash := &msg{Role: "user"}, &msg{Role: "assistant", Kind: "diff"}, &msg{Role: "user", Kind: "slash"}
	check := func(t *testing.T, prev, cur *msg, want bool) {
		t.Helper()
		if got := hasLeadGap(prev, *cur); got != want {
			t.Fatalf("%#v then %#v gave %v", prev, cur, got)
		}
	}
	t.Run("opens a gap only at a boundary between working-area groups", func(t *testing.T) {
		check(t, trail, model, true)
		check(t, model, trail, true)
		check(t, model, note, true)
		check(t, note, model, true)
	})
	t.Run("keeps same-group neighbours flush (the grouping)", func(t *testing.T) {
		check(t, trail, trail, false)
		check(t, model, model, false)
		check(t, note, note, false)
	})
	t.Run("never gaps the first block (no predecessor)", func(t *testing.T) {
		check(t, nil, model, false)
		check(t, nil, trail, false)
	})
	t.Run("suppresses the gap after blocks that already paint a trailing line", func(t *testing.T) {
		check(t, user, trail, false)
		check(t, user, model, false)
		check(t, diff, model, false)
	})
	t.Run("still gaps after a slash echo (it has no trailing margin)", func(t *testing.T) {
		check(t, slash, model, true)
		check(t, slash, trail, true)
	})
	t.Run("lets user / slash / diff own their spacing (never managed here)", func(t *testing.T) {
		check(t, model, user, false)
		check(t, model, slash, false)
		check(t, model, diff, false)
	})
}

func TestBlockRenders(t *testing.T) {
	trail := msg{Role: "system", Kind: "trail", Tools: []string{"Edit foo.ts"}}
	todos := msg{Role: "system", Kind: "trail", Todos: []todoItem{{Content: "a", ID: "1", Status: "pending"}}}
	t.Run("always renders non-trail blocks", func(t *testing.T) {
		if !blockRenders(msg{Role: "assistant", Text: "hi"}, detailsCtx{DetailsMode: "hidden", CommandOverride: true}) {
			t.Fatal("model hidden")
		}
	})
	t.Run("renders a content-bearing trail unless every section is hidden", func(t *testing.T) {
		if !blockRenders(trail, detailsCtx{DetailsMode: "collapsed"}) || !blockRenders(trail, detailsCtx{DetailsMode: "expanded"}) || blockRenders(trail, detailsCtx{DetailsMode: "hidden", CommandOverride: true}) {
			t.Fatal("trail")
		}
	})
	t.Run("does not render a content-less trail (e.g. finalDetails with only a token tally)", func(t *testing.T) {
		if blockRenders(msg{Role: "system", Kind: "trail", ToolTokens: 40}, detailsCtx{DetailsMode: "expanded"}) {
			t.Fatal("tally rendered")
		}
	})
	t.Run("keeps todo trails visible even when details are hidden", func(t *testing.T) {
		if !blockRenders(todos, detailsCtx{DetailsMode: "hidden", CommandOverride: true}) {
			t.Fatal("todos hidden")
		}
	})
}

func TestPrevRenderedMsg(t *testing.T) {
	hidden, shown := detailsCtx{CommandOverride: true, DetailsMode: "hidden"}, detailsCtx{DetailsMode: "collapsed"}
	rows := []msg{{Role: "user", Text: "q"}, {Role: "system", Kind: "trail", Tools: []string{"Edit foo.ts"}}, {Role: "assistant", Text: "first"},
		{Role: "system", Kind: "trail", Tools: []string{"Edit bar.ts"}}, {Role: "assistant", Text: "second"}}
	at := func(i int) *msg {
		if i < 0 || i >= len(rows) {
			return nil
		}
		return &rows[i]
	}
	t.Run("returns the literal predecessor when everything renders", func(t *testing.T) {
		if prevRenderedMsg(at, 2, shown) != &rows[1] || prevRenderedMsg(at, 4, shown) != &rows[3] {
			t.Fatal("predecessor")
		}
	})
	t.Run("skips hidden trails so grouping sees the nearest visible block", func(t *testing.T) {
		if prevRenderedMsg(at, 2, hidden) != &rows[0] || prevRenderedMsg(at, 4, hidden) != &rows[2] {
			t.Fatal("skip")
		}
	})
	t.Run("returns undefined at the top of the transcript", func(t *testing.T) {
		if prevRenderedMsg(at, 0, shown) != nil {
			t.Fatal("top")
		}
	})
}
