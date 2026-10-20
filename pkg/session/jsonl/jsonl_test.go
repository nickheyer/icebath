package jsonl

import (
	"bytes"
	"context"
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

var ctx = context.Background()

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

func user(text string) protocol.Message {
	return protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: text}}}
}

func TestAppendOnlyGrowth(t *testing.T) {
	st := &Store{Dir: filepath.Join(t.TempDir(), "logs")}
	s := session.New()
	s.Append(user("one"))
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID())
	if kinds(lines(t, path)) != "header,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message" {
		t.Fatal("unchanged session must not grow the log")
	}
	s.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "t", Args: json.RawMessage(`{}`)}}, Usage: protocol.Usage{Input: 3}})
	s.Log("root", "approval", map[string]string{"tool": "t"})
	s.AddResults("root", []protocol.ToolResult{{CallID: "c", Name: "t", Content: []protocol.Part{protocol.Text{Text: "r"}}}})
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message,message,record,header" {
		t.Fatal(kinds(lines(t, path)))
	}
	s.SetTitle("named")
	st.Save(ctx, s)
	if kinds(lines(t, path)) != "header,message,message,message,record,header,header" {
		t.Fatal("metadata changes append a header")
	}
	got, err := st.Load(ctx, s.ID())
	if err != nil || got.Title() != "named" || got.Usage().Input != 3 || got.Last().Input != 3 || !reflect.DeepEqual(got.Snapshot(), s.Snapshot()) || !got.Created().Equal(s.Created()) {
		t.Fatalf("%v %+v", err, got)
	}
	if recs := got.Records(); len(recs) != 1 || recs[0].Kind != "approval" || recs[0].Seq != 2 || got.Entries()[2].Seq != 3 {
		t.Fatalf("records %+v entries %+v", recs, got.Entries())
	}
	fresh := &Store{Dir: st.Dir}
	got, _ = fresh.Load(ctx, s.ID())
	got.Append(user("more"))
	if err := fresh.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message,message,record,header,header,message,header" {
		t.Fatalf("a loaded session must keep appending: %s", kinds(lines(t, path)))
	}
	again, _ := fresh.Load(ctx, s.ID())
	if again.Len() != 4 || again.Snapshot()[3].Content[0].(protocol.Text).Text != "more" || again.Entries()[3].Seq != 4 {
		t.Fatal("append after load")
	}
}

func TestHistoryChangesRewriteTheLog(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	for i := 0; i < 3; i++ {
		s.Append(user("u"))
		s.AddResponse("", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "a"}}})
		s.Log("", "turn", i)
	}
	st.Save(ctx, s)
	path, _ := st.Path(s.ID())
	if len(lines(t, path)) != 10 {
		t.Fatal(len(lines(t, path)))
	}
	s.Compact("summary", 2)
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,record,record,record,message,message,message,message" {
		t.Fatalf("rewrite must interleave by sequence: %s", kinds(lines(t, path)))
	}
	got, _ := st.Load(ctx, s.ID())
	if got.Len() != 4 || got.Snapshot()[0].Content[0].(protocol.Text).Text != "summary" || len(got.Records()) != 3 {
		t.Fatal("compacted log")
	}
	s.Truncate(1)
	st.Save(ctx, s)
	if kinds(lines(t, path)) != "header,record,record,record,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	os.Remove(path)
	s.Append(user("again"))
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,record,record,record,message,message" {
		t.Fatal("a missing log must be rebuilt")
	}
	for _, e := range must(os.ReadDir(st.Dir)) {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestCrashTolerantLoadLeavesTheFileAloneUntilSave(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	s.Append(user("kept"))
	st.Save(ctx, s)
	path, _ := st.Path(s.ID())
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"message","message":{"role":"user","content":[{"type":"text","te`)
	f.Close()
	before, _ := os.ReadFile(path)
	got, err := st.Load(ctx, s.ID())
	if err != nil || got.Len() != 1 {
		t.Fatalf("truncated final line must be dropped: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("load must not write to the log")
	}
	got.Append(user("after"))
	if err := st.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if kinds(lines(t, path)) != "header,message,message" {
		t.Fatalf("save after a partial line must rewrite cleanly: %s", kinds(lines(t, path)))
	}
	if again, err := st.Load(ctx, s.ID()); err != nil || again.Len() != 2 || again.Snapshot()[1].Content[0].(protocol.Text).Text != "after" {
		t.Fatalf("append after repair: %v", err)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"dangling"}]}}`)
	f.Close()
	if got, err := st.Load(ctx, s.ID()); err != nil || got.Len() != 2 {
		t.Fatalf("a complete line without its newline never finished writing: %v", err)
	}
	for name, content := range map[string]string{
		"noheader":  `{"type":"message","message":{"role":"user","content":[]}}` + "\n",
		"badline":   "{\n",
		"badtype":   `{"type":"weird"}` + "\n",
		"nobody":    `{"type":"header"}` + "\n",
		"badpart":   `{"type":"header","header":{"id":"x"}}` + "\n" + `{"type":"message","message":{"role":"user","content":[{"type":"video"}]}}` + "\n",
		"badrecord": `{"type":"header","header":{"id":"x"}}` + "\n" + `{"type":"record","record":{"seq":1}}` + "\n",
	} {
		os.WriteFile(filepath.Join(st.Dir, name+".jsonl"), []byte(content), 0o600)
		if _, err := st.Load(ctx, name); err == nil {
			t.Fatalf("%s loaded", name)
		}
		os.Remove(filepath.Join(st.Dir, name+".jsonl"))
	}
}

func TestForeignWriterTriggersRewrite(t *testing.T) {
	dir := t.TempDir()
	a := &Store{Dir: dir}
	b := &Store{Dir: dir}
	s := session.New()
	s.Append(user("one"))
	a.Save(ctx, s)
	other, _ := b.Load(ctx, s.ID())
	other.Append(user("from b"))
	b.Save(ctx, other)
	s.Append(user("from a"))
	if err := a.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Load(ctx, s.ID())
	if got.Len() != 2 || got.Snapshot()[1].Content[0].(protocol.Text).Text != "from a" {
		t.Fatalf("the saving store's view must win over a foreign append: %+v", got.Snapshot())
	}
	path, _ := a.Path(s.ID())
	if kinds(lines(t, path)) != "header,message,message" {
		t.Fatal(kinds(lines(t, path)))
	}
}

func TestHeaderLinesAreCapped(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	s.Append(user("x"))
	st.Save(ctx, s)
	path, _ := st.Path(s.ID())
	for i := 0; i < MaxHeaders+5; i++ {
		s.SetTitle(strings.Repeat("t", i+1))
		if err := st.Save(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	n := strings.Count(kinds(lines(t, path)), "header")
	if n > MaxHeaders+1 {
		t.Fatalf("%d header lines", n)
	}
	got, _ := st.Load(ctx, s.ID())
	if got.Title() != strings.Repeat("t", MaxHeaders+5) {
		t.Fatal("latest header must win after a compaction rewrite")
	}
}

func TestListReadsTheTailHeader(t *testing.T) {
	st := &Store{Dir: filepath.Join(t.TempDir(), "missing")}
	if infos, err := st.List(ctx); err != nil || infos != nil {
		t.Fatalf("%v %v", infos, err)
	}
	s := session.New()
	s.SetTitle("new")
	big := strings.Repeat("y", 200<<10)
	for range 3 {
		s.Append(user(big))
	}
	st.Save(ctx, s)
	s.SetTitle("renamed")
	st.Save(ctx, s)
	older := session.FromHeader(session.Header{ID: "older", Updated: s.Updated().Add(-time.Hour)}, nil, nil)
	st.Save(ctx, older)
	os.WriteFile(filepath.Join(st.Dir, "stray.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(st.Dir, "corrupt.jsonl"), []byte("{\n"), 0o600)
	infos, err := st.List(ctx)
	if err != nil || len(infos) != 3 {
		t.Fatalf("%+v %v", infos, err)
	}
	byID := map[string]session.Info{}
	for _, in := range infos {
		byID[in.ID] = in
	}
	if in := byID[s.ID()]; in.Title != "renamed" || in.Messages != 3 || in.Error != "" {
		t.Fatalf("tail header %+v", in)
	}
	if byID["corrupt"].Error == "" || byID["older"].Error != "" {
		t.Fatalf("corrupt logs must be listed with their error: %+v", infos)
	}
	if infos[0].ID != s.ID() {
		t.Fatalf("newest first %+v", infos)
	}
	if ok, _ := st.Exists(ctx, "older"); !ok {
		t.Fatal("exists")
	}
	if err := st.Delete(ctx, "older"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "older"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if _, err := st.Load(ctx, "older"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if _, err := st.Load(ctx, "../x"); err == nil || st.Delete(ctx, "../x") == nil || st.Save(ctx, session.FromHeader(session.Header{ID: "a/b"}, nil, nil)) == nil {
		t.Fatal("bad ids accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if st.Save(cancelled, s) == nil || st.Delete(cancelled, s.ID()) == nil {
		t.Fatal("cancelled context accepted")
	}
	if _, err := st.Load(cancelled, s.ID()); err == nil {
		t.Fatal("cancelled load")
	}
	if _, err := st.List(cancelled); err == nil {
		t.Fatal("cancelled list")
	}
	if _, err := st.Exists(cancelled, s.ID()); err == nil {
		t.Fatal("cancelled exists")
	}
	var _ session.Store = st
	var _ session.Pather = st
}

func TestEditsBehindTheTailRewriteTheLog(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := session.New()
	for _, text := range []string{"one", "two", "three"} {
		s.Append(user(text))
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.Truncate(2)
	s.Append(user("redone"))
	s.Append(user("four"))
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID())
	if kinds(lines(t, path)) != "header,message,message,message,message" {
		t.Fatal(kinds(lines(t, path)))
	}
	got, err := st.Load(ctx, s.ID())
	if err != nil || got.Len() != 4 || got.Snapshot()[2].Content[0].(protocol.Text).Text != "redone" || got.Snapshot()[3].Content[0].(protocol.Text).Text != "four" {
		t.Fatalf("an edit behind the tail must reach the log: %v %+v", err, got)
	}
}
