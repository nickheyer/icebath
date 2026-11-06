package state

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func (h *fakeSearchHost) GetMessagesAround(ctx context.Context, sessionID string, aroundMessageID int64, window int) (*pyjson.Object, error) {
	window = max(window, 0)
	out := pyjson.NewObject()
	anchor, err := queryObjects(ctx, h.reader, "SELECT 1 FROM messages WHERE id = ? AND session_id = ? LIMIT 1", aroundMessageID, sessionID)
	if err != nil {
		return nil, err
	}
	if len(anchor) == 0 {
		out.Set("window", []any{})
		out.Set("messages_before", int64(0))
		out.Set("messages_after", int64(0))
		return out, nil
	}
	before, err := queryObjects(ctx, h.reader, "SELECT * FROM messages WHERE session_id = ? AND id <= ? ORDER BY id DESC LIMIT ?", sessionID, aroundMessageID, window+1)
	if err != nil {
		return nil, err
	}
	after, err := queryObjects(ctx, h.reader, "SELECT * FROM messages WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT ?", sessionID, aroundMessageID, window)
	if err != nil {
		return nil, err
	}
	slices.Reverse(before)
	msgs := []any{}
	for _, r := range append(before, after...) {
		msg, _ := h.rowToMessageDict(r, "get_messages_around", false)
		msgs = append(msgs, msg)
	}
	out.Set("window", msgs)
	out.Set("messages_before", int64(max(0, len(before)-1)))
	out.Set("messages_after", int64(len(after)))
	return out, nil
}

func (h *fakeSearchHost) rowToMessageDict(row *pyjson.Object, warnContext string, summaryFlag bool) (*pyjson.Object, error) {
	row.Delete("display_identity")
	row.Delete("display_order")
	content, _ := row.Get("content")
	row.Set("content", h.decodeContent(content))
	row.Set("_hydrated_by", warnContext)
	return row, nil
}

func viewIDs(t *testing.T, view *pyjson.Object, key string) []int64 {
	t.Helper()
	raw, _ := view.Get(key)
	var ids []int64
	for _, item := range raw.([]any) {
		ids = append(ids, objInt(item.(*pyjson.Object), "id"))
	}
	return ids
}

func TestGetAnchoredView(t *testing.T) {
	ctx := context.Background()
	s, _ := searchStore(t, [][3]string{
		{"user", "goal: build the parser", ""}, {"assistant", "plan", ""}, {"tool", "tool output", "t"},
		{"user", "", ""}, {"assistant", "a5", ""}, {"user", "u6", ""}, {"tool", "t7", "t"},
		{"assistant", "a8", ""}, {"user", "u9", ""}, {"assistant", "a10", ""}, {"tool", "t11", "t"},
		{"assistant", "resolution", ""},
	})
	view := func(anchor int64, o AnchoredViewOptions) *pyjson.Object {
		t.Helper()
		v, err := s.GetAnchoredView(ctx, "s1", anchor, o)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	o := NewAnchoredViewOptions()
	o.Window = 1
	v := view(8, o)
	if got := fmt.Sprint(viewIDs(t, v, "window"), viewIDs(t, v, "bookend_start"), viewIDs(t, v, "bookend_end")); got != "[8 9] [1 2 5] [10 12]" {
		t.Fatalf("filtered window and role bookends %s", got)
	}
	if objInt(v, "messages_before") != 1 || objInt(v, "messages_after") != 1 {
		t.Fatalf("counts %s", objJSON(t, v))
	}
	if strings.Join(v.Keys, ",") != "window,messages_before,messages_after,bookend_start,bookend_end" {
		t.Fatalf("key order %v", v.Keys)
	}
	raw, _ := v.Get("bookend_start")
	if by := objStr(raw.([]any)[0].(*pyjson.Object), "_hydrated_by"); by != "get_anchored_view" {
		t.Fatalf("bookends hydrate through the row dict: %q", by)
	}
	if got := viewIDs(t, view(7, o), "window"); fmt.Sprint(got) != "[6 7 8]" {
		t.Fatalf("anchor survives the role filter: %v", got)
	}
	o.KeepRoles = nil
	v = view(8, o)
	if got := fmt.Sprint(viewIDs(t, v, "window"), viewIDs(t, v, "bookend_start"), viewIDs(t, v, "bookend_end")); got != "[7 8 9] [1 2 3] [10 11 12]" {
		t.Fatalf("no role filter %s", got)
	}
	o.KeepRoles = []string{}
	v = view(8, o)
	if got := fmt.Sprint(viewIDs(t, v, "window"), viewIDs(t, v, "bookend_start"), viewIDs(t, v, "bookend_end")); got != "[8] [] []" {
		t.Fatalf("empty role tuple keeps only the anchor %s", got)
	}
	o = NewAnchoredViewOptions()
	o.Window, o.Bookend = 1, -4
	v = view(8, o)
	if got := fmt.Sprint(viewIDs(t, v, "bookend_start"), viewIDs(t, v, "bookend_end")); got != "[] []" {
		t.Fatalf("negative bookend clamps to none %s", got)
	}
	o.Window, o.Bookend = 20, 3
	v = view(8, o)
	if got := fmt.Sprint(viewIDs(t, v, "bookend_start"), viewIDs(t, v, "bookend_end")); got != "[] []" {
		t.Fatalf("a window covering the session leaves no bookends %s", got)
	}
	if got := objJSON(t, view(999, NewAnchoredViewOptions())); got != `{"window":[],"messages_before":0,"messages_after":0,"bookend_start":[],"bookend_end":[]}` {
		t.Fatalf("foreign anchor %s", got)
	}
}

func TestListRecentUserMessages(t *testing.T) {
	ctx := context.Background()
	skill := skillInvocationPrefix + "\"work\" skill. " + singleSkillMarker + "\nBODY"
	merged := "kept tail\n" + MergedSummaryDelimiter + "\n  " + SummaryPrefix + "\nsummary"
	s, host := searchStore(t, [][3]string{
		{"user", "first   question\nhere", ""},
		{"assistant", "answer", ""},
		{"user", SummaryPrefix + "\nold summary", ""},
		{"user", skill, ""},
		{"user", "\x00json:" + `[{"type": "text", "text": "look  at\nthis"}, {"type": "image_url", "image_url": {"url": "x"}}]`, ""},
		{"user", strings.Repeat("é", 100), ""},
		{"user", "steered input", ""},
		{"user", "queued bookkeeping", ""},
		{"user", "rewound turn", ""},
		{"user", merged, ""},
		{"user", "\x00json:" + `[{"type": "image_url", "image_url": {"url": "x"}}]`, ""},
	})
	mustExecConn(t, host.conn, "UPDATE messages SET display_kind = 'steer' WHERE id = 7")
	mustExecConn(t, host.conn, "UPDATE messages SET display_kind = 'queued' WHERE id = 8")
	mustExecConn(t, host.conn, "UPDATE messages SET active = 0 WHERE id = 9")
	list := func(limit int, inactive bool) string {
		t.Helper()
		rows, err := s.ListRecentUserMessages(ctx, "s1", limit, inactive)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			if strings.Join(r.Keys, ",") != "id,timestamp,preview" {
				t.Fatalf("keys %v", r.Keys)
			}
			out = append(out, fmt.Sprintf("%d:%s", objInt(r, "id"), objStr(r, "preview")))
		}
		return strings.Join(out, "|")
	}
	long := strings.Repeat("é", 77) + "..."
	want := "11:[multimodal content]|7:steered input|6:" + long + "|5:look at this|4:/work|1:first question here"
	if got := list(20, false); got != want {
		t.Fatalf("recent user turns\n got %s\nwant %s", got, want)
	}
	if got := list(20, true); got != "11:[multimodal content]|9:rewound turn|7:steered input|6:"+long+"|5:look at this|4:/work|1:first question here" {
		t.Fatalf("inactive included %s", got)
	}
	if got := list(2, false); got != "11:[multimodal content]|7:steered input" {
		t.Fatalf("limit %s", got)
	}
	if got := list(0, false); got != "" {
		t.Fatalf("zero limit %s", got)
	}
}

func TestSummaryContentClassification(t *testing.T) {
	if len(historicalSummaryPrefixes) != 5 || slices.Contains(historicalSummaryPrefixes, SummaryPrefix) {
		t.Fatalf("historical prefixes %d", len(historicalSummaryPrefixes))
	}
	cases := []struct {
		content any
		want    string
	}{
		{SummaryPrefix + "\nbody", "standalone"},
		{" \n\t" + LegacySummaryPrefix + " body", "standalone"},
		{historicalSummaryPrefixes[4] + "\nbody", "standalone"},
		{historicalSummaryPrefixes[0], "standalone"},
		{"tail\n" + MergedSummaryDelimiter + "\n\n" + SummaryPrefix, "merged"},
		{"tail\n" + MergedSummaryDelimiter + "\nno prefix " + SummaryPrefix, ""},
		{SummaryPrefix + "\n" + MergedSummaryDelimiter + "\nplain", ""},
		{[]any{map[string]any{"type": "text", "text": ""}, "  " + SummaryPrefix}, "standalone"},
		{[]any{pyjsonText(LegacySummaryPrefix), int64(3)}, "standalone"},
		{[]any{pyjsonText("before"), pyjsonText(LegacySummaryPrefix)}, ""},
		{"x" + SummaryPrefix, ""},
		{nil, ""},
		{int64(5), ""},
	}
	for i, c := range cases {
		if got := ClassifySummaryContent(c.content); got != c.want || IsContextSummaryContent(c.content) != (c.want != "") {
			t.Fatalf("case %d: %q want %q", i, got, c.want)
		}
	}
	text := ContentTextForContains([]any{"a", pyjsonText(""), map[string]any{"text": 5}, pyjsonText("b"), nil, map[string]any{"text": "c"}})
	if text != "a\nb\nc" {
		t.Fatalf("joined parts %q", text)
	}
	if ContentTextForContains(int64(5)) != "5" || ContentTextForContains(nil) != "" || ContentTextForContains(3.5) != "3.5" {
		t.Fatal("scalar content renders with str")
	}
}

func pyjsonText(text string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("type", "text")
	o.Set("text", text)
	return o
}
