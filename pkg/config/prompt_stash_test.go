package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

type fakeStashClock struct{ now float64 }

func (c *fakeStashClock) read() float64 { return c.now }

func newTestStash() *PromptStash {
	c := &fakeStashClock{now: 1000}
	return NewPromptStashWith(MaxStashItems, c.read)
}

func stashTexts(s *PromptStash) []string {
	var out []string
	for _, e := range s.Items() {
		out = append(out, e.Text)
	}
	return out
}

func threeStash() *PromptStash {
	s := newTestStash()
	s.Stash("oldest", nil)
	s.Stash("middle", nil)
	s.Stash("newest", nil)
	s.OpenPanel()
	return s
}

// Fails unless a ctrl s press answered the wanted action and payload
func wantCtrlS(t *testing.T, action string, payload *StashPayload, wantAction string, wantPayload *StashPayload) {
	t.Helper()
	if action != wantAction || !reflect.DeepEqual(payload, wantPayload) {
		t.Fatalf("got (%q, %+v), want (%q, %+v)", action, payload, wantAction, wantPayload)
	}
}

func TestStashNoOp(t *testing.T) {
	t.Run("test_stash_blank_buffer_is_noop", func(t *testing.T) {
		for _, text := range []string{"", "  \n \n  ", ""} {
			s := newTestStash()
			if s.Stash(text, nil) {
				t.Fatalf("%q was stashed", text)
			}
			if s.Len() != 0 || s.Indicator() != "" {
				t.Fatalf("%q left %d entries, indicator %q", text, s.Len(), s.Indicator())
			}
		}
	})
	t.Run("test_pop_empty_stash_returns_none", func(t *testing.T) {
		if text, images, ok := newTestStash().Pop(0); ok || text != "" || images != nil {
			t.Fatalf("pop of an empty stash gave (%q, %v, %v)", text, images, ok)
		}
	})
	t.Run("test_open_panel_on_empty_stash_refused", func(t *testing.T) {
		s := newTestStash()
		if s.OpenPanel() {
			t.Fatal("the panel opened on an empty stash")
		}
		if s.PanelOpen {
			t.Fatal("panel_open set on an empty stash")
		}
	})
	t.Run("test_delete_on_empty_stash_is_noop", func(t *testing.T) {
		if newTestStash().DeleteAtCursor() {
			t.Fatal("delete on an empty stash reported a drop")
		}
	})
	t.Run("test_restore_at_cursor_on_empty_stash", func(t *testing.T) {
		if text, images, ok := newTestStash().RestoreAtCursor(); ok || text != "" || images != nil {
			t.Fatalf("restore on an empty stash gave (%q, %v, %v)", text, images, ok)
		}
	})
	t.Run("test_images_only_draft_is_stashable", func(t *testing.T) {
		s := newTestStash()
		if !s.Stash("", []any{"/tmp/a.png"}) {
			t.Fatal("an images only draft was refused")
		}
		if s.Len() != 1 {
			t.Fatalf("len %d", s.Len())
		}
		if e, ok := s.Peek(0); !ok || e.Preview != "(images only)" {
			t.Fatalf("preview %q ok %v", e.Preview, ok)
		}
	})
}

func TestRoundTrip(t *testing.T) {
	t.Run("test_stash_then_restore_round_trips_exactly", func(t *testing.T) {
		for _, text := range []string{
			"\n  leading blank and indented\n",
			"para one\n\npara two\n\n\npara three",
			"tabs\there\tand\there   ",
			"unicode ünïcödé 中文 🎉 mixed",
		} {
			s := newTestStash()
			if !s.Stash(text, nil) {
				t.Fatalf("%q was refused", text)
			}
			restored, images, ok := s.Pop(0)
			if !ok {
				t.Fatalf("%q could not be popped", text)
			}
			if restored != text {
				t.Fatalf("restored %q, want %q", restored, text)
			}
			if images == nil || len(images) != 0 {
				t.Fatalf("images %#v, want an empty list", images)
			}
			if s.Len() != 0 {
				t.Fatalf("popping left %d entries", s.Len())
			}
		}
	})
	t.Run("test_round_trip_through_resolve_ctrl_s", func(t *testing.T) {
		s := newTestStash()
		draft := "a long prompt\nwith several lines\n"
		action, payload := ResolveCtrlS(s, draft, nil)
		wantCtrlS(t, action, payload, ActionStashed, nil)
		if s.Len() != 1 {
			t.Fatalf("len %d after stashing", s.Len())
		}
		action, payload = ResolveCtrlS(s, "", nil)
		wantCtrlS(t, action, payload, ActionRestored, &StashPayload{Text: draft, Images: []any{}})
		if s.Len() != 0 {
			t.Fatalf("len %d after restoring", s.Len())
		}
	})
	t.Run("test_images_round_trip", func(t *testing.T) {
		s := newTestStash()
		imgs := make([]any, 2, 3)
		imgs[0], imgs[1] = "/tmp/one.png", "/tmp/two.png"
		s.Stash("with pics", imgs)
		text, restored, _ := s.Pop(0)
		if text != "with pics" {
			t.Fatalf("text %q", text)
		}
		if !reflect.DeepEqual(restored, []any{"/tmp/one.png", "/tmp/two.png"}) {
			t.Fatalf("restored %v", restored)
		}
		imgs = append(imgs, "/tmp/three.png")
		imgs[0] = "/tmp/changed.png"
		if !reflect.DeepEqual(restored, []any{"/tmp/one.png", "/tmp/two.png"}) {
			t.Fatalf("the parked entry followed the caller's list, %v after %v", restored, imgs)
		}
	})
}

func TestNoSilentClobber(t *testing.T) {
	t.Run("test_second_stash_keeps_first", func(t *testing.T) {
		s := newTestStash()
		s.Stash("first draft", nil)
		s.Stash("second draft", nil)
		if s.Len() != 2 {
			t.Fatalf("len %d", s.Len())
		}
		texts := strings.Join(stashTexts(s), "\x00")
		if !strings.Contains(texts, "first draft") || !strings.Contains(texts, "second draft") {
			t.Fatalf("texts %q", stashTexts(s))
		}
	})
	t.Run("test_newest_first_ordering", func(t *testing.T) {
		s := newTestStash()
		s.Stash("oldest", nil)
		s.Stash("middle", nil)
		s.Stash("newest", nil)
		if got := stashTexts(s); !reflect.DeepEqual(got, []string{"newest", "middle", "oldest"}) {
			t.Fatalf("order %q", got)
		}
		if text, _, _ := s.Pop(0); text != "newest" {
			t.Fatalf("default pop gave %q", text)
		}
	})
	t.Run("test_two_items_opens_panel_instead_of_guessing", func(t *testing.T) {
		s := newTestStash()
		s.Stash("first", nil)
		s.Stash("second", nil)
		action, payload := ResolveCtrlS(s, "", nil)
		wantCtrlS(t, action, payload, ActionOpenPanel, nil)
		if !s.PanelOpen {
			t.Fatal("the panel stayed closed")
		}
		if s.Len() != 2 {
			t.Fatalf("len %d, nothing may be consumed", s.Len())
		}
	})
	t.Run("test_stash_cap_drops_oldest_not_newest", func(t *testing.T) {
		s := NewPromptStashWith(3, (&fakeStashClock{now: 1000}).read)
		for i := range 5 {
			s.Stash("draft "+strconv.Itoa(i), nil)
		}
		if s.Len() != 3 {
			t.Fatalf("len %d", s.Len())
		}
		if got := stashTexts(s); !reflect.DeepEqual(got, []string{"draft 4", "draft 3", "draft 2"}) {
			t.Fatalf("kept %q", got)
		}
	})
	t.Run("test_default_cap_is_bounded", func(t *testing.T) {
		s := newTestStash()
		for i := range MaxStashItems + 10 {
			s.Stash("d"+strconv.Itoa(i), nil)
		}
		if s.Len() != MaxStashItems {
			t.Fatalf("len %d, want %d", s.Len(), MaxStashItems)
		}
	})
}

func TestIndicatorState(t *testing.T) {
	t.Run("test_empty_stash_has_no_indicator", func(t *testing.T) {
		s := newTestStash()
		if s.Indicator() != "" {
			t.Fatalf("indicator %q", s.Indicator())
		}
		if s.ComposerHint() != "" {
			t.Fatalf("hint %q", s.ComposerHint())
		}
		if s.Len() != 0 {
			t.Fatal("an empty stash must be falsy")
		}
	})
	t.Run("test_count_grows_with_stash", func(t *testing.T) {
		s := newTestStash()
		for i, text := range []string{"a", "b", "c"} {
			s.Stash(text, nil)
			if want := "📌 " + strconv.Itoa(i+1); s.Indicator() != want {
				t.Fatalf("indicator %q, want %q", s.Indicator(), want)
			}
		}
	})
	t.Run("test_indicator_clears_after_restoring_last_item", func(t *testing.T) {
		s := newTestStash()
		s.Stash("only", nil)
		s.Pop(0)
		if s.Indicator() != "" {
			t.Fatalf("indicator %q", s.Indicator())
		}
	})
	t.Run("test_placeholder_hint_single_shows_preview", func(t *testing.T) {
		s := newTestStash()
		s.Stash("write the migration guide", nil)
		hint := s.ComposerHint()
		if !strings.Contains(hint, "Ctrl+S") {
			t.Fatalf("hint %q names no key", hint)
		}
		if !strings.Contains(hint, "write the migration guide") {
			t.Fatalf("hint %q shows no preview", hint)
		}
	})
	t.Run("test_clear_resets_all_state", func(t *testing.T) {
		s := newTestStash()
		s.Stash("a", nil)
		s.Stash("b", nil)
		s.OpenPanel()
		s.Clear()
		if s.Len() != 0 || s.PanelOpen || s.PanelCursor != 0 || s.Indicator() != "" {
			t.Fatalf("len %d open %v cursor %d indicator %q", s.Len(), s.PanelOpen, s.PanelCursor, s.Indicator())
		}
	})
}

func TestBuildPreview(t *testing.T) {
	t.Run("test_empty_text", func(t *testing.T) {
		if got := BuildPreview("", PromptStashPreviewWidth); got != "" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_single_line_passthrough", func(t *testing.T) {
		if got := BuildPreview("hello world", PromptStashPreviewWidth); got != "hello world" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_newlines_collapse_to_marker", func(t *testing.T) {
		if got := BuildPreview("a\nb", PromptStashPreviewWidth); got != "a ⏎ b" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_crlf_normalized", func(t *testing.T) {
		if got := BuildPreview("a\r\nb", PromptStashPreviewWidth); got != "a ⏎ b" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_preview_is_always_single_line", func(t *testing.T) {
		if got := BuildPreview(strings.Repeat("x\n", 30), 200); strings.Contains(got, "\n") {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_long_text_ellipsized_to_width", func(t *testing.T) {
		got := BuildPreview(strings.Repeat("y", 500), 20)
		if utf8.RuneCountInString(got) != 20 {
			t.Fatalf("length %d", utf8.RuneCountInString(got))
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("%q", got)
		}
	})
	t.Run("test_whitespace_runs_collapsed", func(t *testing.T) {
		if got := BuildPreview("a     b\t\tc", PromptStashPreviewWidth); got != "a b c" {
			t.Fatalf("%q", got)
		}
	})
}

func TestStashEntry(t *testing.T) {
	t.Run("test_as_dict_shape_matches_panel_renderer", func(t *testing.T) {
		s := newTestStash()
		s.Stash("draft text", nil)
		row := s.PanelRows()[0]
		for _, key := range []string{"text", "images", "stashed_at", "preview"} {
			if _, ok := row[key]; !ok {
				t.Fatalf("row lacks %q, %v", key, row)
			}
		}
		if row["text"] != "draft text" || row["preview"] != "draft text" {
			t.Fatalf("row %v", row)
		}
	})
	t.Run("test_as_dict_copies_images", func(t *testing.T) {
		imgs := make([]any, 1, 4)
		imgs[0] = "/tmp/a.png"
		entry := StashEntry{Text: "t", Images: imgs}
		row := entry.AsDict()
		row["images"] = append(row["images"].([]any), "/tmp/b.png")
		row["images"].([]any)[0] = "/tmp/changed.png"
		if !reflect.DeepEqual(imgs, []any{"/tmp/a.png"}) || imgs[:2][1] != nil {
			t.Fatalf("the entry's images followed the row, %v", imgs)
		}
	})
	t.Run("test_stashed_at_uses_injected_clock", func(t *testing.T) {
		clock := &fakeStashClock{now: 500}
		s := NewPromptStashWith(MaxStashItems, clock.read)
		s.Stash("first", nil)
		clock.now += 60
		s.Stash("second", nil)
		entries := s.Items()
		if entries[0].StashedAt != 560.0 || entries[1].StashedAt != 500.0 {
			t.Fatalf("stamps %v %v", entries[0].StashedAt, entries[1].StashedAt)
		}
	})
}

func TestPanelBrowsing(t *testing.T) {
	t.Run("test_open_panel_starts_at_top", func(t *testing.T) {
		three := threeStash()
		if !three.PanelOpen || three.PanelCursor != 0 {
			t.Fatalf("open %v cursor %d", three.PanelOpen, three.PanelCursor)
		}
	})
	t.Run("test_cursor_moves_and_clamps", func(t *testing.T) {
		three := threeStash()
		for _, step := range []struct{ delta, want int }{{1, 1}, {1, 2}, {1, 2}, {-5, 0}} {
			if got := three.MoveCursor(step.delta); got != step.want {
				t.Fatalf("move %d gave %d, want %d", step.delta, got, step.want)
			}
		}
	})
	t.Run("test_restore_at_cursor_picks_highlighted_entry", func(t *testing.T) {
		three := threeStash()
		three.MoveCursor(1)
		text, images, ok := three.RestoreAtCursor()
		if !ok || text != "middle" || images == nil || len(images) != 0 {
			t.Fatalf("restored (%q, %#v, %v)", text, images, ok)
		}
		if three.PanelOpen {
			t.Fatal("the panel stayed open")
		}
		if got := stashTexts(three); !reflect.DeepEqual(got, []string{"newest", "oldest"}) {
			t.Fatalf("left %q", got)
		}
	})
	t.Run("test_delete_at_cursor_removes_only_that_entry", func(t *testing.T) {
		three := threeStash()
		three.MoveCursor(1)
		if !three.DeleteAtCursor() {
			t.Fatal("nothing was deleted")
		}
		if got := stashTexts(three); !reflect.DeepEqual(got, []string{"newest", "oldest"}) {
			t.Fatalf("left %q", got)
		}
		if !three.PanelOpen {
			t.Fatal("the panel closed with entries left")
		}
	})
	t.Run("test_delete_last_row_reclamps_cursor", func(t *testing.T) {
		three := threeStash()
		three.MoveCursor(2)
		three.DeleteAtCursor()
		if three.PanelCursor != 1 {
			t.Fatalf("cursor %d", three.PanelCursor)
		}
	})
	t.Run("test_deleting_everything_closes_panel", func(t *testing.T) {
		three := threeStash()
		for range 3 {
			three.DeleteAtCursor()
		}
		if three.Len() != 0 || three.PanelOpen || three.PanelCursor != 0 {
			t.Fatalf("len %d open %v cursor %d", three.Len(), three.PanelOpen, three.PanelCursor)
		}
	})
	t.Run("test_new_stash_closes_open_panel", func(t *testing.T) {
		three := threeStash()
		three.Stash("brand new", nil)
		if three.PanelOpen || three.PanelCursor != 0 {
			t.Fatalf("open %v cursor %d", three.PanelOpen, three.PanelCursor)
		}
	})
	t.Run("test_panel_rows_ordered_newest_first", func(t *testing.T) {
		var texts []any
		for _, row := range threeStash().PanelRows() {
			texts = append(texts, row["text"])
		}
		if !reflect.DeepEqual(texts, []any{"newest", "middle", "oldest"}) {
			t.Fatalf("rows %v", texts)
		}
	})
	t.Run("test_pop_out_of_range_is_none", func(t *testing.T) {
		three := threeStash()
		for _, index := range []int{99, -1} {
			if text, images, ok := three.Pop(index); ok || text != "" || images != nil {
				t.Fatalf("pop %d gave (%q, %v, %v)", index, text, images, ok)
			}
		}
		if three.Len() != 3 {
			t.Fatalf("len %d", three.Len())
		}
	})
}

func TestResolveCtrlS(t *testing.T) {
	t.Run("test_empty_buffer_empty_stash_is_noop", func(t *testing.T) {
		action, payload := ResolveCtrlS(newTestStash(), "", nil)
		wantCtrlS(t, action, payload, ActionNoop, nil)
	})
	t.Run("test_content_stashes", func(t *testing.T) {
		s := newTestStash()
		action, payload := ResolveCtrlS(s, "some draft", nil)
		wantCtrlS(t, action, payload, ActionStashed, nil)
		if s.Len() != 1 {
			t.Fatalf("len %d", s.Len())
		}
	})
	t.Run("test_open_panel_then_ctrl_s_closes_it", func(t *testing.T) {
		s := newTestStash()
		s.Stash("a", nil)
		s.Stash("b", nil)
		s.OpenPanel()
		action, payload := ResolveCtrlS(s, "", nil)
		wantCtrlS(t, action, payload, ActionClosePanel, nil)
		if s.PanelOpen {
			t.Fatal("the panel stayed open")
		}
	})
	t.Run("test_close_panel_takes_priority_over_stashing", func(t *testing.T) {
		s := newTestStash()
		s.Stash("a", nil)
		s.Stash("b", nil)
		s.OpenPanel()
		if action, _ := ResolveCtrlS(s, "text the user typed", nil); action != ActionClosePanel {
			t.Fatalf("action %q", action)
		}
		if s.Len() != 2 {
			t.Fatalf("len %d, nothing new may be pushed", s.Len())
		}
	})
	t.Run("test_images_only_buffer_stashes", func(t *testing.T) {
		if action, _ := ResolveCtrlS(newTestStash(), "", []any{"/tmp/x.png"}); action != ActionStashed {
			t.Fatalf("action %q", action)
		}
	})
	t.Run("test_whitespace_only_buffer_with_stash_restores", func(t *testing.T) {
		s := newTestStash()
		s.Stash("real draft", nil)
		action, payload := ResolveCtrlS(s, "   ", nil)
		wantCtrlS(t, action, payload, ActionRestored, &StashPayload{Text: "real draft", Images: []any{}})
	})
}
