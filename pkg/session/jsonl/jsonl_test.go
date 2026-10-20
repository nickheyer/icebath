package jsonl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func kinds(ls []string) string {
	var out []string
	for _, l := range ls {
		var v struct{ Type string }
		json.Unmarshal([]byte(l), &v)
		out = append(out, v.Type)
	}
	return strings.Join(out, ",")
}

func TestAppendOnlyGrowth(t *testing.T) {
	st := &Store{Dir: filepath.Join(t.TempDir(), "logs")}
	s := session.New()
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "one"}}})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID)
	if kinds(lines(t, path)) != "header,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message" {
		t.Fatal("unchanged session must not grow the log")
	}
	s.AddResponse(protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "t", Args: json.RawMessage(`{}`)}}, Usage: protocol.Usage{Input: 3}})
	s.AddResults([]protocol.ToolResult{{CallID: "c", Name: "t", Content: []protocol.Part{protocol.Text{Text: "r"}}}})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message,message,header" {
		t.Fatal(kinds(lines(t, path)))
	}
	s.SetTitle("named")
	st.Save(s)
	if kinds(lines(t, path)) != "header,message,message,message,header,header" {
		t.Fatal("metadata changes append a header")
	}
	got, err := st.Load(s.ID)
	if err != nil || got.Title != "named" || got.Usage.Input != 3 || got.Last.Input != 3 || !reflect.DeepEqual(got.Messages, s.Messages) || !got.Created.Equal(s.Created) {
		t.Fatalf("%v %+v", err, got)
	}
	fresh := &Store{Dir: st.Dir}
	got, _ = fresh.Load(s.ID)
	got.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "more"}}})
	if err := fresh.Save(got); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message,message,header,header,message,header" {
		t.Fatalf("a loaded session must keep appending: %s", kinds(lines(t, path)))
	}
	again, _ := fresh.Load(s.ID)
	if again.Len() != 4 || again.Messages[3].Content[0].(protocol.Text).Text != "more" {
		t.Fatal("append after load")
	}
}

func TestHistoryChangesRewriteTheLog(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	for i := 0; i < 3; i++ {
		s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "u"}}})
		s.AddResponse(protocol.Response{Content: []protocol.Part{protocol.Text{Text: "a"}}})
	}
	st.Save(s)
	path, _ := st.Path(s.ID)
	if len(lines(t, path)) != 7 {
		t.Fatal(len(lines(t, path)))
	}
	s.Compact("summary", 2)
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message,message,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	got, _ := st.Load(s.ID)
	if got.Len() != 4 || got.Messages[0].Content[0].(protocol.Text).Text != "summary" {
		t.Fatal("compacted log")
	}
	s.Truncate(1)
	st.Save(s)
	if kinds(lines(t, path)) != "header,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	os.Remove(path)
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "again"}}})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message" {
		t.Fatal("a missing log must be rebuilt")
	}
	if entries, _ := os.ReadDir(st.Dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestCrashTolerantLoad(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "kept"}}})
	st.Save(s)
	path, _ := st.Path(s.ID)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"message","message":{"role":"user","content":[{"type":"text","te`)
	f.Close()
	before, _ := os.ReadFile(path)
	got, err := st.Load(s.ID)
	if err != nil || got.Len() != 1 {
		t.Fatalf("truncated final line must be dropped: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasSuffix(after, []byte("\n")) || len(after) >= len(before) {
		t.Fatal("load must cut the partial line off the log")
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"dangling"}]}}`)
	f.Close()
	got, err = st.Load(s.ID)
	if err != nil || got.Len() != 1 {
		t.Fatalf("a complete line without its newline never finished writing: %v", err)
	}
	got.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "after"}}})
	if err := st.Save(got); err != nil {
		t.Fatal(err)
	}
	if again, err := st.Load(s.ID); err != nil || again.Len() != 2 || again.Messages[1].Content[0].(protocol.Text).Text != "after" {
		t.Fatalf("append after repair: %v", err)
	}
	for name, content := range map[string]string{
		"noheader": `{"type":"message","message":{"role":"user","content":[]}}` + "\n",
		"badline":  "{\n",
		"badtype":  `{"type":"weird"}` + "\n",
		"nobody":   `{"type":"header"}` + "\n",
		"badpart":  `{"type":"header","header":{"id":"x"}}` + "\n" + `{"type":"message","message":{"role":"user","content":[{"type":"video"}]}}` + "\n",
	} {
		os.WriteFile(filepath.Join(st.Dir, name+".jsonl"), []byte(content), 0o600)
		if _, err := st.Load(name); err == nil {
			t.Fatalf("%s loaded", name)
		}
		os.Remove(filepath.Join(st.Dir, name+".jsonl"))
	}
}

func TestListDeleteAndIDs(t *testing.T) {
	st := &Store{Dir: filepath.Join(t.TempDir(), "missing")}
	if infos, err := st.List(); err != nil || infos != nil {
		t.Fatalf("%v %v", infos, err)
	}
	s := session.New()
	s.SetTitle("new")
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "x"}}})
	st.Save(s)
	older := session.New()
	older.ID = "older"
	older.Updated = s.Updated.Add(-time.Hour)
	st.Save(older)
	os.WriteFile(filepath.Join(st.Dir, "stray.json"), []byte("{}"), 0o600)
	infos, err := st.List()
	if err != nil || len(infos) != 2 || infos[0].ID != s.ID || infos[0].Title != "new" || infos[0].Messages != 1 || infos[1].ID != "older" {
		t.Fatalf("%+v %v", infos, err)
	}
	if err := st.Delete("older"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("older"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if _, err := st.Load("older"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if _, err := st.Load("../x"); err == nil || st.Delete("../x") == nil || st.Save(&session.Session{ID: "a/b"}) == nil {
		t.Fatal("bad ids accepted")
	}
	os.WriteFile(filepath.Join(st.Dir, "corrupt.jsonl"), []byte("{\n"), 0o600)
	if _, err := st.List(); err == nil {
		t.Fatal("corrupt log listed")
	}
	var _ session.Store = st
}

func TestEditsBehindTheTailRewriteTheLog(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	for _, text := range []string{"one", "two", "three"} {
		s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: text}}})
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	s.Messages[0].Content[0] = protocol.Text{Text: "redacted"}
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "four"}}})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID)
	if kinds(lines(t, path)) != "header,message,message,message,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	got, err := st.Load(s.ID)
	if err != nil || got.Len() != 4 || got.Messages[0].Content[0].(protocol.Text).Text != "redacted" || got.Messages[3].Content[0].(protocol.Text).Text != "four" {
		t.Fatalf("an edit behind the tail must reach the log: %v %+v", err, got)
	}
}
