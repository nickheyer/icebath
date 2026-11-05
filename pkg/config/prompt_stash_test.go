package config

import (
	"reflect"
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

func TestStashNoOps(t *testing.T) {
	for _, text := range []string{"", "  \n \n  "} {
		s := newTestStash()
		if s.Stash(text, nil) || s.Len() != 0 || s.Indicator() != "" {
			t.Fatalf("%q stashed", text)
		}
	}
	s := newTestStash()
	if _, _, ok := s.Pop(0); ok {
		t.Fatal("pop of empty stash")
	}
	if s.OpenPanel() || s.PanelOpen {
		t.Fatal("open panel on empty stash")
	}
	if s.DeleteAtCursor() {
		t.Fatal("delete on empty stash")
	}
	if _, _, ok := s.RestoreAtCursor(); ok {
		t.Fatal("restore on empty stash")
	}
	if !s.Stash("", []any{"/tmp/a.png"}) || s.Len() != 1 {
		t.Fatal("images only draft is stashable")
	}
	if e, _ := s.Peek(0); e.Preview != "(images only)" {
		t.Fatal(e.Preview)
	}
}

func TestStashThenRestoreRoundTripsExactly(t *testing.T) {
	for _, text := range []string{"\n  leading blank and indented\n", "para one\n\npara two\n\n\npara three", "tabs\there\tand\there   ", "unicode ünïcödé 中文 🎉 mixed"} {
		s := newTestStash()
		if !s.Stash(text, nil) {
			t.Fatal(text)
		}
		restored, images, ok := s.Pop(0)
		if !ok || restored != text || len(images) != 0 || s.Len() != 0 {
			t.Fatalf("%q -> %q", text, restored)
		}
	}
	s := newTestStash()
	draft := "a long prompt\nwith several lines\n"
	if action, payload := ResolveCtrlS(s, draft, nil); action != ActionStashed || payload != nil || s.Len() != 1 {
		t.Fatal(action)
	}
	if action, payload := ResolveCtrlS(s, "", nil); action != ActionRestored || payload == nil || payload.Text != draft || len(payload.Images) != 0 || s.Len() != 0 {
		t.Fatal(action, payload)
	}
	imgs := []any{"/tmp/one.png", "/tmp/two.png"}
	s.Stash("with pics", imgs)
	text, restored, _ := s.Pop(0)
	imgs[0] = "/tmp/changed.png"
	if text != "with pics" || !reflect.DeepEqual(restored, []any{"/tmp/one.png", "/tmp/two.png"}) {
		t.Fatal(text, restored)
	}
}

func TestStashNeverSilentlyClobbers(t *testing.T) {
	s := newTestStash()
	s.Stash("oldest", nil)
	s.Stash("middle", nil)
	s.Stash("newest", nil)
	if got := stashTexts(s); !reflect.DeepEqual(got, []string{"newest", "middle", "oldest"}) {
		t.Fatal(got)
	}
	if text, _, _ := s.Pop(0); text != "newest" {
		t.Fatal(text)
	}
	two := newTestStash()
	two.Stash("first", nil)
	two.Stash("second", nil)
	if action, payload := ResolveCtrlS(two, "", nil); action != ActionOpenPanel || payload != nil || !two.PanelOpen || two.Len() != 2 {
		t.Fatal(action)
	}
	capped := NewPromptStashWith(3, (&fakeStashClock{}).read)
	for _, d := range []string{"draft 0", "draft 1", "draft 2", "draft 3", "draft 4"} {
		capped.Stash(d, nil)
	}
	if got := stashTexts(capped); !reflect.DeepEqual(got, []string{"draft 4", "draft 3", "draft 2"}) {
		t.Fatal(got)
	}
	bounded := newTestStash()
	for i := 0; i < MaxStashItems+10; i++ {
		bounded.Stash("d"+strings.Repeat("x", i), nil)
	}
	if bounded.Len() != MaxStashItems {
		t.Fatal(bounded.Len())
	}
	if NewPromptStash().Stash("default clock", nil) != true {
		t.Fatal("default stash")
	}
}

func TestStashIndicatorState(t *testing.T) {
	s := newTestStash()
	if s.Indicator() != "" || s.ComposerHint() != "" || s.Len() != 0 {
		t.Fatal("empty state")
	}
	for i, want := range []string{"📌 1", "📌 2", "📌 3"} {
		s.Stash(string(rune('a'+i)), nil)
		if s.Indicator() != want {
			t.Fatal(s.Indicator())
		}
	}
	s.OpenPanel()
	if s.Indicator() != "📌 3 ▲" || s.ComposerHint() != "Ctrl+S to browse 3 stashed drafts" {
		t.Fatal(s.Indicator(), s.ComposerHint())
	}
	s.Clear()
	if s.Len() != 0 || s.PanelOpen || s.PanelCursor != 0 || s.Indicator() != "" {
		t.Fatal("clear resets all state")
	}
	s.Stash("only", nil)
	s.Pop(0)
	if s.Indicator() != "" {
		t.Fatal("indicator clears after the last restore")
	}
	s.Stash("write the migration guide", nil)
	if hint := s.ComposerHint(); !strings.Contains(hint, "Ctrl+S") || !strings.Contains(hint, "write the migration guide") {
		t.Fatal(hint)
	}
}

func TestBuildPreview(t *testing.T) {
	for in, want := range map[string]string{"": "", "hello world": "hello world", "a\nb": "a ⏎ b", "a\r\nb": "a ⏎ b", "a     b\t\tc": "a b c"} {
		if got := BuildPreview(in, PromptStashPreviewWidth); got != want {
			t.Fatalf("%q -> %q", in, got)
		}
	}
	if p := BuildPreview(strings.Repeat("x\n", 30), 200); strings.Contains(p, "\n") {
		t.Fatal(p)
	}
	if p := BuildPreview(strings.Repeat("y", 500), 20); utf8.RuneCountInString(p) != 20 || !strings.HasSuffix(p, "…") {
		t.Fatal(p)
	}
}

func TestStashEntryRows(t *testing.T) {
	s := newTestStash()
	s.Stash("draft text", nil)
	row := s.PanelRows()[0]
	for _, k := range []string{"text", "images", "stashed_at", "preview"} {
		if _, ok := row[k]; !ok {
			t.Fatal(k)
		}
	}
	if row["text"] != "draft text" || row["preview"] != "draft text" {
		t.Fatal(row)
	}
	imgs := []any{"/tmp/a.png"}
	entry := StashEntry{Text: "t", Images: imgs}
	copied := entry.AsDict()["images"].([]any)
	copied[0] = "/tmp/b.png"
	if imgs[0] != "/tmp/a.png" {
		t.Fatal("as dict copies images")
	}
	clock := &fakeStashClock{now: 500}
	timed := NewPromptStashWith(MaxStashItems, clock.read)
	timed.Stash("first", nil)
	clock.now += 60
	timed.Stash("second", nil)
	if items := timed.Items(); items[0].StashedAt != 560 || items[1].StashedAt != 500 {
		t.Fatal(items)
	}
}

func TestStashPanelBrowsing(t *testing.T) {
	s := threeStash()
	if !s.PanelOpen || s.PanelCursor != 0 {
		t.Fatal("panel opens at the top")
	}
	for _, step := range [][2]int{{1, 1}, {1, 2}, {1, 2}, {-5, 0}} {
		if got := s.MoveCursor(step[0]); got != step[1] {
			t.Fatal(step, got)
		}
	}
	s.MoveCursor(1)
	if text, images, ok := s.RestoreAtCursor(); !ok || text != "middle" || len(images) != 0 || s.PanelOpen {
		t.Fatal(text)
	}
	if got := stashTexts(s); !reflect.DeepEqual(got, []string{"newest", "oldest"}) {
		t.Fatal(got)
	}
	s = threeStash()
	s.MoveCursor(1)
	if !s.DeleteAtCursor() || !reflect.DeepEqual(stashTexts(s), []string{"newest", "oldest"}) || !s.PanelOpen {
		t.Fatal(stashTexts(s))
	}
	s = threeStash()
	s.MoveCursor(2)
	s.DeleteAtCursor()
	if s.PanelCursor != 1 {
		t.Fatal(s.PanelCursor)
	}
	s = threeStash()
	for i := 0; i < 3; i++ {
		s.DeleteAtCursor()
	}
	if s.Len() != 0 || s.PanelOpen || s.PanelCursor != 0 {
		t.Fatal("deleting everything closes the panel")
	}
	s = threeStash()
	s.Stash("brand new", nil)
	if s.PanelOpen || s.PanelCursor != 0 {
		t.Fatal("new stash closes the panel")
	}
	s = threeStash()
	var rows []string
	for _, r := range s.PanelRows() {
		rows = append(rows, r["text"].(string))
	}
	if !reflect.DeepEqual(rows, []string{"newest", "middle", "oldest"}) {
		t.Fatal(rows)
	}
	if _, _, ok := s.Pop(99); ok {
		t.Fatal("pop 99")
	}
	if _, _, ok := s.Pop(-1); ok || s.Len() != 3 {
		t.Fatal("pop -1")
	}
}

func TestResolveCtrlS(t *testing.T) {
	s := newTestStash()
	if action, payload := ResolveCtrlS(s, "", nil); action != ActionNoop || payload != nil {
		t.Fatal(action)
	}
	if action, payload := ResolveCtrlS(s, "some draft", nil); action != ActionStashed || payload != nil || s.Len() != 1 {
		t.Fatal(action)
	}
	s = newTestStash()
	s.Stash("a", nil)
	s.Stash("b", nil)
	s.OpenPanel()
	if action, payload := ResolveCtrlS(s, "", nil); action != ActionClosePanel || payload != nil || s.PanelOpen {
		t.Fatal(action)
	}
	s.OpenPanel()
	if action, _ := ResolveCtrlS(s, "text the user typed", nil); action != ActionClosePanel || s.Len() != 2 {
		t.Fatal(action)
	}
	if action, _ := ResolveCtrlS(newTestStash(), "", []any{"/tmp/x.png"}); action != ActionStashed {
		t.Fatal(action)
	}
	s = newTestStash()
	s.Stash("real draft", nil)
	if action, payload := ResolveCtrlS(s, "   ", nil); action != ActionRestored || payload.Text != "real draft" || len(payload.Images) != 0 {
		t.Fatal(action)
	}
}
