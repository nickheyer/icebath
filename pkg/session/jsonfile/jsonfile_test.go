package jsonfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

func sample() *session.Session {
	s := session.New()
	s.SetTitle("probe")
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "look"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}})
	s.AddResponse(protocol.Response{Content: []protocol.Part{protocol.Thinking{Text: "hmm", Signature: "sig"}, protocol.ToolCall{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}}, Usage: protocol.Usage{Input: 10, Output: 5}})
	s.AddResults([]protocol.ToolResult{{CallID: "c1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "found"}}, IsError: true}})
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	st := Store{Dir: filepath.Join(t.TempDir(), "nested", "sessions")}
	if ids, err := st.List(); err != nil || ids != nil {
		t.Fatalf("empty list %v %v", ids, err)
	}
	s := sample()
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID || !got.Created.Equal(s.Created) || got.Title != "probe" || got.Usage != s.Usage || got.Last != s.Last || !reflect.DeepEqual(got.Messages, s.Messages) {
		t.Fatalf("round trip\n%+v\n%+v", got.Messages, s.Messages)
	}
	older := session.New()
	older.ID = "20000101-000000-0000000000000000"
	older.Updated = s.Updated.Add(-time.Hour)
	if err := st.Save(older); err != nil {
		t.Fatal(err)
	}
	infos, err := st.List()
	if err != nil || len(infos) != 2 || infos[0].ID != s.ID || infos[1].ID != older.ID || infos[0].Messages != 3 || infos[0].Title != "probe" || infos[0].Usage.Input != 10 {
		t.Fatalf("%+v %v", infos, err)
	}
	os.WriteFile(filepath.Join(st.Dir, "stray.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(st.Dir, "dir.json"), 0o755)
	if infos, err := st.List(); err != nil || len(infos) != 2 {
		t.Fatalf("strays must be ignored: %+v %v", infos, err)
	}
	if _, err := st.Load("../escape"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := st.Load("missing"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := st.Save(&session.Session{ID: "bad/id"}); err == nil {
		t.Fatal("bad id saved")
	}
	if err := st.Delete(older.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(older.ID); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := st.Delete("bad/id"); err == nil {
		t.Fatal("bad id deleted")
	}
	if entries, _ := os.ReadDir(st.Dir); len(entries) != 3 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	os.WriteFile(filepath.Join(st.Dir, "corrupt.json"), []byte("{"), 0o644)
	if _, err := st.Load("corrupt"); err == nil {
		t.Fatal("corrupt file loaded")
	}
	if _, err := st.List(); err == nil {
		t.Fatal("corrupt file listed")
	}
	var _ session.Store = st
}
