package session

import (
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
	}})
	s.AddResponse(protocol.Response{Model: "m-1", Content: []protocol.Part{
		protocol.Thinking{Text: "hmm", Signature: "sig"},
		protocol.Thinking{Redacted: true, Signature: "blob"},
		protocol.Text{Text: "calling"},
		protocol.ToolCall{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "ts"},
	}, Usage: protocol.Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 1, Reasoning: 3}})
	s.AddResults([]protocol.ToolResult{{CallID: "c1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "found"}}, IsError: true}})
	s.AddResponse(protocol.Response{Content: []protocol.Part{protocol.Text{Text: "done"}}, Usage: protocol.Usage{Input: 20, Output: 2}})
	return s
}

func TestUsageTotalsAndSnapshotIsolation(t *testing.T) {
	s := sample()
	if s.Usage != (protocol.Usage{Input: 30, Output: 7, CacheRead: 2, CacheWrite: 1, Reasoning: 3}) || s.Last != (protocol.Usage{Input: 20, Output: 2}) || s.Context() != 20 || s.Model != "m-1" {
		t.Fatalf("%+v %+v", s.Usage, s.Last)
	}
	snap := s.Snapshot()
	if len(snap) != 4 || s.Len() != 4 || snap[2].Role != protocol.RoleTool {
		t.Fatalf("%+v", snap)
	}
	snap[0].Content[0] = protocol.Text{Text: "mutated"}
	if s.Snapshot()[0].Content[0].(protocol.Text).Text != "look" {
		t.Fatal("snapshot aliases the session")
	}
	if s.Updated.Before(s.Created) || s.Updated.IsZero() {
		t.Fatal("updated")
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
	f := s.Fork()
	if f.ID == s.ID || f.Parent != s.ID || f.Title != "t" || f.Model != "m-2" || f.Usage != (protocol.Usage{}) || f.Last != s.Last || len(f.Messages) != 4 || f.Labels["host"] != "web-1" {
		t.Fatalf("%+v", f.Info())
	}
	f.Label("host", "web-2")
	f.Messages[0].Content[0] = protocol.Text{Text: "mutated"}
	if s.Labels["host"] != "web-1" || s.Messages[0].Content[0].(protocol.Text).Text != "look" {
		t.Fatal("fork aliases the parent")
	}
	s.Truncate(2)
	if s.Len() != 2 {
		t.Fatal(s.Len())
	}
	s.Truncate(-1)
	if s.Len() != 0 {
		t.Fatal(s.Len())
	}
	empty := New()
	if empty.Fork().Messages != nil || empty.Fork().Labels != nil {
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
	s.AddResponse(protocol.Response{Content: []protocol.Part{protocol.Text{Text: "ok"}}})
	if err := s.Compact("", 2); err == nil {
		t.Fatal("empty summary accepted")
	}
	s.Last = protocol.Usage{Input: 40}
	if err := s.Compact("earlier we looked", 3); err != nil {
		t.Fatal(err)
	}
	if roles(s) != "user,assistant,user,assistant" {
		t.Fatal(roles(s))
	}
	if s.Last != (protocol.Usage{}) || s.Context() != 0 {
		t.Fatal("compaction must reset the context measurement")
	}
	msgs := s.Snapshot()
	if msgs[0].Content[0].(protocol.Text).Text != "earlier we looked" || msgs[1].Content[0].(protocol.Text).Text != Ack || msgs[2].Content[0].(protocol.Text).Text != "next" {
		t.Fatalf("%+v", msgs)
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
	s.Parent = "p-1"
	md := s.Markdown()
	for _, want := range []string{"# Probe\n", "- model: m-1\n", "- forked from: p-1\n", "- usage: 30 in, 7 out, 2 cache read, 1 cache write, 3 reasoning\n", "## User\n\nlook\n\n[image image/png, 3 bytes]", "## Assistant\n\n> hmm\n\n> [redacted thinking]\n\ncalling\n\n**Call c1** `lookup`\n\n```json\n{\"q\":\"x\"}\n```", "## Tool results\n\n**Result c1** `lookup` (error)\n\nfound\n", "## Assistant\n\ndone\n"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in\n%s", want, md)
		}
	}
	if !strings.HasPrefix(New().Markdown(), "# 20") {
		t.Fatal("untitled sessions use the id")
	}
}

func TestMarshalRoundTripAndMemoryStore(t *testing.T) {
	s := sample()
	s.SetTitle("t")
	s.Label("k", "v")
	data, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID || !got.Created.Equal(s.Created) || !got.Updated.Equal(s.Updated) || got.Title != "t" || got.Model != "m-1" || got.Labels["k"] != "v" || got.Usage != s.Usage || got.Last != s.Last || !reflect.DeepEqual(got.Messages, s.Messages) {
		t.Fatalf("round trip\n%+v\n%+v", got.Info(), s.Info())
	}
	h, n, err := UnmarshalHeader(data)
	if err != nil || h.ID != s.ID || n != 4 || h.Usage != s.Usage {
		t.Fatalf("%v %+v %d", err, h, n)
	}
	if _, _, err := UnmarshalHeader([]byte("{")); err == nil {
		t.Fatal("bad header accepted")
	}
	if _, err := Unmarshal([]byte(`{"messages":[{"role":"user","content":[{"type":"video"}]}]}`)); err == nil {
		t.Fatal("unknown part accepted")
	}
	if _, err := Unmarshal([]byte(`{"messages":[{"role":"tool","content":[{"type":"tool_result","content":[{"type":"video"}]}]}]}`)); err == nil {
		t.Fatal("nested unknown part accepted")
	}
	if _, err := Unmarshal([]byte("junk")); err == nil {
		t.Fatal("junk accepted")
	}
	raw, err := MarshalMessage(s.Messages[1])
	if err != nil {
		t.Fatal(err)
	}
	m, err := UnmarshalMessage(raw)
	if err != nil || !reflect.DeepEqual(m, s.Messages[1]) {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := UnmarshalMessage([]byte("junk")); err == nil {
		t.Fatal("junk message accepted")
	}
	type alien struct{ protocol.Part }
	if _, err := MarshalMessage(protocol.Message{Content: []protocol.Part{alien{}}}); err == nil {
		t.Fatal("unknown part marshalled")
	}
	rebuilt := FromHeader(s.Header(), s.Snapshot())
	if rebuilt.ID != s.ID || rebuilt.Len() != 4 {
		t.Fatal("from header")
	}
	var store Memory
	if list, err := store.List(); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	older := New()
	older.ID = "older"
	older.Updated = s.Updated.Add(-time.Hour)
	store.Save(older)
	loaded, err := store.Load(s.ID)
	if err != nil || loaded.Len() != 4 {
		t.Fatalf("%v", err)
	}
	list, err := store.List()
	if err != nil || len(list) != 2 || list[0].ID != s.ID || list[1].ID != "older" {
		t.Fatalf("%+v %v", list, err)
	}
	if err := store.Delete("older"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("older"); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := store.Load("older"); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := store.Save(&Session{ID: "bad/id"}); err == nil {
		t.Fatal("bad id saved")
	}
	var _ Store = &store
}
