package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func sample() *Session {
	s := New()
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{
		protocol.Text{Text: "look"},
		protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
		protocol.Document{MediaType: "application/pdf", Data: []byte{4}, Name: "spec.pdf"},
	}})
	s.AddResponse("root", protocol.Response{Model: "m-1", Content: []protocol.Part{
		protocol.Thinking{Text: "hmm", Signature: "sig", Origin: protocol.AnthropicMessages},
		protocol.Thinking{Redacted: true, Signature: "blob", Origin: protocol.AnthropicMessages},
		protocol.Text{Text: "calling", Citations: []protocol.Citation{{Title: "Doc", URL: "https://x", Start: 0, End: 3, Cited: "cal"}}},
		protocol.ToolCall{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "ts", Origin: protocol.GoogleGenerativeAI},
		protocol.ServerToolCall{ID: "s1", Name: "web_search", Args: json.RawMessage(`{"query":"x"}`), Origin: protocol.AnthropicMessages},
		protocol.ServerToolResult{CallID: "s1", Name: "web_search", Content: json.RawMessage(`[{"url":"https://x"}]`), Origin: protocol.AnthropicMessages},
	}, Usage: protocol.Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 1, Reasoning: 3}})
	s.AddResults("root", []protocol.ToolResult{{CallID: "c1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "found"}}, IsError: true}})
	s.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "done"}}, Usage: protocol.Usage{Input: 20, Output: 2}})
	return s
}

func TestUsageTotalsAndSnapshotIsolation(t *testing.T) {
	s := sample()
	if s.Usage() != (protocol.Usage{Input: 30, Output: 7, CacheRead: 2, CacheWrite: 1, Reasoning: 3}) || s.Last() != (protocol.Usage{Input: 20, Output: 2}) || s.Context() != 20 || s.Model() != "m-1" {
		t.Fatalf("%+v %+v", s.Usage(), s.Last())
	}
	snap := s.Snapshot()
	if len(snap) != 4 || s.Len() != 4 || snap[2].Role != protocol.RoleTool {
		t.Fatalf("%+v", snap)
	}
	snap[0].Content[0] = protocol.Text{Text: "mutated"}
	snap[0].Content[1].(protocol.Image).Data[0] = 9
	if s.Snapshot()[0].Content[0].(protocol.Text).Text != "look" || s.Snapshot()[0].Content[1].(protocol.Image).Data[0] != 1 {
		t.Fatal("snapshot aliases the session")
	}
	if s.Updated().Before(s.Created()) || s.Updated().IsZero() {
		t.Fatal("updated")
	}
	entries := s.Entries()
	if entries[1].Agent != "root" || entries[1].Model != "m-1" || entries[1].Usage.Input != 10 || entries[0].Time.IsZero() || entries[3].Seq != 3 {
		t.Fatalf("entry metadata %+v", entries[1])
	}
	entries[1].Content[3] = protocol.Text{Text: "x"}
	if _, ok := s.Entries()[1].Content[3].(protocol.ToolCall); !ok {
		t.Fatal("entries alias the session")
	}
}

func TestRecordsShareTheTimeline(t *testing.T) {
	s := sample()
	r, err := s.Log("root", "approval", map[string]any{"tool": "Bash", "verdict": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 4 || r.Kind != "approval" || r.Agent != "root" || !strings.Contains(string(r.Data), `"Bash"`) {
		t.Fatalf("%+v", r)
	}
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "next"}}})
	if got := s.Entries()[4].Seq; got != 5 {
		t.Fatalf("entry after record got seq %d", got)
	}
	if _, err := s.Log("", "bad", make(chan int)); err == nil {
		t.Fatal("unmarshalable data accepted")
	}
	recs := s.Records()
	recs[0].Data[0] = 'x'
	if s.Records()[0].Data[0] == 'x' {
		t.Fatal("records alias the session")
	}
	if s.Info().Records != 1 {
		t.Fatal("info records")
	}
}

func TestMetadataForkTruncate(t *testing.T) {
	s := sample()
	s.SetTitle("t")
	s.SetModel("m-2")
	s.Label("host", "web-1")
	s.Label("tmp", "x")
	s.Label("tmp", "")
	info := s.Info()
	if info.Title != "t" || info.Model != "m-2" || info.Messages != 4 || info.Labels["host"] != "web-1" || len(info.Labels) != 1 || info.Usage.Input != 30 {
		t.Fatalf("%+v", info)
	}
	info.Labels["host"] = "changed"
	if s.Labels()["host"] != "web-1" {
		t.Fatal("info aliases the labels")
	}
	s.Log("root", "note", "x")
	f := s.Fork()
	if f.ID() == s.ID() || f.Parent() != s.ID() || f.Title() != "t" || f.Model() != "m-2" || f.Usage() != (protocol.Usage{}) || f.Last() != s.Last() || f.Len() != 4 || f.Labels()["host"] != "web-1" || len(f.Records()) != 1 {
		t.Fatalf("%+v", f.Info())
	}
	f.Label("host", "web-2")
	f.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "forked"}}})
	if s.Labels()["host"] != "web-1" || s.Len() != 4 || f.Entries()[4].Seq != 5 {
		t.Fatal("fork aliases the parent or lost the sequence")
	}
	s.Truncate(2)
	if s.Len() != 2 || s.Last() != (protocol.Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 1, Reasoning: 3}) || len(s.Records()) != 0 {
		t.Fatalf("truncate len %d last %+v records %d", s.Len(), s.Last(), len(s.Records()))
	}
	s.Truncate(-1)
	if s.Len() != 0 || s.Last() != (protocol.Usage{}) {
		t.Fatal(s.Len())
	}
	empty := New()
	if empty.Fork().Entries() != nil || empty.Fork().Labels() != nil {
		t.Fatal("empty fork")
	}
	infos := []Info{{ID: "a", Updated: time.Unix(1, 0)}, {ID: "b", Updated: time.Unix(2, 0)}, {ID: "c", Updated: time.Unix(2, 0)}}
	SortInfos(infos)
	if infos[0].ID != "c" || infos[1].ID != "b" || infos[2].ID != "a" {
		t.Fatalf("%+v", infos)
	}
}

func roles(s *Session) string {
	var out []string
	for _, m := range s.Snapshot() {
		out = append(out, string(m.Role))
	}
	return strings.Join(out, ",")
}

func TestCompactKeepsToolRoundsWhole(t *testing.T) {
	s := sample()
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "next"}}})
	s.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "ok"}}, Usage: protocol.Usage{Input: 40}})
	if err := s.Compact("", 2); err == nil {
		t.Fatal("empty summary accepted")
	}
	if err := s.Compact("earlier we looked", 3); err != nil {
		t.Fatal(err)
	}
	if roles(s) != "user,assistant,user,assistant" {
		t.Fatal(roles(s))
	}
	if s.Last() != (protocol.Usage{}) || s.Context() != 0 {
		t.Fatal("compaction must reset the context measurement")
	}
	msgs := s.Snapshot()
	if msgs[0].Content[0].(protocol.Text).Text != "earlier we looked" || msgs[1].Content[0].(protocol.Text).Text != Ack || msgs[2].Content[0].(protocol.Text).Text != "next" {
		t.Fatalf("%+v", msgs)
	}
	entries := s.Entries()
	if entries[0].Seq <= entries[3].Seq || entries[2].Seq != 4 {
		t.Fatalf("summary entries must take fresh sequence numbers %+v", entries)
	}
	s = sample()
	if err := s.Compact("all of it", 0); err != nil {
		t.Fatal(err)
	}
	if roles(s) != "user" || s.Snapshot()[0].Content[0].(protocol.Text).Text != "all of it" {
		t.Fatal(roles(s))
	}
	s = sample()
	s.Compact("summary", 100)
	if roles(s) != "user,assistant,user,assistant,tool,assistant" {
		t.Fatal(roles(s))
	}
	s = sample()
	s.Compact("summary", 2)
	if roles(s) != "user" {
		t.Fatalf("a cut inside a tool round must move past it: %s", roles(s))
	}
}

func TestMarkdown(t *testing.T) {
	s := sample()
	s.SetTitle("Probe")
	s.Log("root", "approval", map[string]string{"tool": "Bash"})
	md := s.Markdown()
	for _, want := range []string{"# Probe\n", "- model: m-1\n", "- usage: 30 in, 7 out, 2 cache read, 1 cache write, 3 reasoning\n", "## User\n\nlook\n\n[image image/png, 3 bytes]\n\n[spec.pdf application/pdf, 1 bytes]", "## Assistant\n\n> hmm\n\n> [redacted thinking]\n\ncalling\n\n- [Doc](https://x)\n\n**Call c1** `lookup`\n\n```json\n{\"q\":\"x\"}\n```", "**Server call s1** `web_search`", "**Server result s1** `web_search`", "## Tool results\n\n**Result c1** `lookup` (error)\n\nfound\n", "## Assistant\n\ndone\n"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Records") {
		t.Fatal("records must be opt in")
	}
	full := s.MarkdownWith(MarkdownOptions{NoThinking: true, NoAttachments: true, Timestamps: true, Records: true})
	if strings.Contains(full, "> hmm") || strings.Contains(full, "[image") || !strings.Contains(full, " by root on m-1_") || !strings.Contains(full, "## Records\n\n- ") || !strings.Contains(full, "approval (root) `{\"tool\":\"Bash\"}`") {
		t.Fatalf("options ignored:\n%s", full)
	}
	if !strings.HasPrefix(New().Markdown(), "# 20") {
		t.Fatal("untitled sessions use the id")
	}
	f := FromHeader(Header{ID: "x", Parent: "p-1"}, nil, nil)
	if !strings.Contains(f.Markdown(), "- forked from: p-1\n") {
		t.Fatal("parent line")
	}
}

func TestMarshalRoundTripAndMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := sample()
	s.SetTitle("t")
	s.Label("k", "v")
	s.Log("root", "approval", map[string]string{"tool": "Bash"})
	data, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"usage":{"input":30,`) || !strings.Contains(string(data), `"origin":"anthropic-messages"`) || !strings.Contains(string(data), `"file_name":"spec.pdf"`) {
		t.Fatalf("encoding %s", data)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != s.ID() || !got.Created().Equal(s.Created()) || !got.Updated().Equal(s.Updated()) || got.Title() != "t" || got.Model() != "m-1" || got.Labels()["k"] != "v" || got.Usage() != s.Usage() || got.Last() != s.Last() {
		t.Fatalf("round trip\n%+v\n%+v", got.Info(), s.Info())
	}
	if !reflect.DeepEqual(normalize(got.Entries()), normalize(s.Entries())) {
		t.Fatalf("entries\n%+v\n%+v", got.Entries(), s.Entries())
	}
	if recs := got.Records(); len(recs) != 1 || recs[0].Kind != "approval" || recs[0].Seq != 4 {
		t.Fatalf("records %+v", recs)
	}
	got.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "after load"}}})
	if got.Entries()[4].Seq != 5 {
		t.Fatal("sequence must continue after load")
	}
	h, err := ReadHeader(data)
	if err != nil || h.ID != s.ID() || h.Messages != 4 || h.Records != 1 || h.Usage != s.Usage() {
		t.Fatalf("%v %+v", err, h)
	}
	for _, bad := range []string{"{", "[]", `{"title":"no id"}`, "junk"} {
		if _, err := ReadHeader([]byte(bad)); err == nil {
			t.Fatalf("bad header %q accepted", bad)
		}
	}
	if _, err := Unmarshal([]byte(`{"id":"x","entries":[{"role":"user","content":[{"type":"video"}]}]}`)); err == nil {
		t.Fatal("unknown part accepted")
	}
	if _, err := Unmarshal([]byte(`{"id":"x","entries":[{"role":"tool","content":[{"type":"tool_result","content":[{"type":"video"}]}]}]}`)); err == nil {
		t.Fatal("nested unknown part accepted")
	}
	if _, err := Unmarshal([]byte("junk")); err == nil {
		t.Fatal("junk accepted")
	}
	legacy, err := Unmarshal([]byte(`{"id":"old","entries":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","content":[{"type":"text","text":"b"}]}]}`))
	if err != nil || legacy.Entries()[1].Seq != 1 {
		t.Fatalf("entries without sequence numbers must be numbered in order: %v", err)
	}
	raw, err := MarshalEntry(s.Entries()[1])
	if err != nil {
		t.Fatal(err)
	}
	e, err := UnmarshalEntry(raw)
	if err != nil || !reflect.DeepEqual(normalize([]Entry{e}), normalize(s.Entries()[1:2])) {
		t.Fatalf("%v %+v", err, e)
	}
	if _, err := UnmarshalEntry([]byte("junk")); err == nil {
		t.Fatal("junk entry accepted")
	}
	rec := s.Records()[0]
	rawRec, _ := MarshalRecord(rec)
	back, err := UnmarshalRecord(rawRec)
	if err != nil || back.Kind != "approval" || back.Seq != rec.Seq {
		t.Fatalf("%v %+v", err, back)
	}
	if _, err := UnmarshalRecord([]byte(`{"seq":1}`)); err == nil {
		t.Fatal("record without kind accepted")
	}
	parts, err := UnmarshalParts(must(MarshalParts(s.Entries()[1].Content)))
	if err != nil || len(parts) != 6 {
		t.Fatalf("parts %v %d", err, len(parts))
	}
	type alien struct{ protocol.Part }
	if _, err := MarshalEntry(Entry{Message: protocol.Message{Content: []protocol.Part{alien{}}}}); err == nil {
		t.Fatal("unknown part marshalled")
	}
	rebuilt := FromHeader(s.Header(), s.Entries(), s.Records())
	if rebuilt.ID() != s.ID() || rebuilt.Len() != 4 {
		t.Fatal("from header")
	}
	var store Memory
	if list, err := store.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
	if err := store.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	older := FromHeader(Header{ID: "older", Updated: s.Updated().Add(-time.Hour)}, nil, nil)
	store.Save(ctx, older)
	loaded, err := store.Load(ctx, s.ID())
	if err != nil || loaded.Len() != 4 {
		t.Fatalf("%v", err)
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != s.ID() || list[1].ID != "older" || list[0].Messages != 4 {
		t.Fatalf("%+v %v", list, err)
	}
	if ok, _ := store.Exists(ctx, "older"); !ok {
		t.Fatal("exists")
	}
	if err := store.Delete(ctx, "older"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "older"); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "older"); err != ErrNotFound {
		t.Fatal(err)
	}
	if ok, _ := store.Exists(ctx, "older"); ok {
		t.Fatal("deleted session exists")
	}
	if err := store.Save(ctx, FromHeader(Header{ID: "bad/id"}, nil, nil)); err == nil {
		t.Fatal("bad id saved")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if store.Save(cancelled, s) == nil || store.Delete(cancelled, s.ID()) == nil {
		t.Fatal("cancelled context accepted")
	}
	if _, err := store.Load(cancelled, s.ID()); err == nil {
		t.Fatal("cancelled load")
	}
	if _, err := store.List(cancelled); err == nil {
		t.Fatal("cancelled list")
	}
	if _, err := store.Exists(cancelled, s.ID()); err == nil {
		t.Fatal("cancelled exists")
	}
	var _ Store = &store
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// Strips time zone pointers so round trips compare by instant
func normalize(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		e.Time = e.Time.UTC().Truncate(0)
		out[i] = e
	}
	return out
}
