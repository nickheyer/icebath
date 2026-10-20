package jsonfile

import (
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

func sample() *session.Session {
	s := session.New()
	s.SetTitle("probe")
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "look"}, protocol.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}}})
	s.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.Thinking{Text: "hmm", Signature: "sig", Origin: protocol.AnthropicMessages}, protocol.ToolCall{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}}, Usage: protocol.Usage{Input: 10, Output: 5}})
	s.AddResults("root", []protocol.ToolResult{{CallID: "c1", Name: "lookup", Content: []protocol.Part{protocol.Text{Text: "found"}}, IsError: true}})
	s.Log("root", "approval", map[string]string{"tool": "lookup"})
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := Store{Dir: filepath.Join(t.TempDir(), "nested", "sessions")}
	if ids, err := st.List(ctx); err != nil || ids != nil {
		t.Fatalf("empty list %v %v", ids, err)
	}
	if ok, err := st.Exists(ctx, "nothing"); ok || err != nil {
		t.Fatalf("exists on empty store %v %v", ok, err)
	}
	s := sample()
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.Exists(ctx, s.ID()); !ok {
		t.Fatal("saved session must exist")
	}
	got, err := st.Load(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != s.ID() || !got.Created().Equal(s.Created()) || got.Title() != "probe" || got.Usage() != s.Usage() || got.Last() != s.Last() || !reflect.DeepEqual(got.Snapshot(), s.Snapshot()) || len(got.Records()) != 1 {
		t.Fatalf("round trip\n%+v\n%+v", got.Snapshot(), s.Snapshot())
	}
	if got.Entries()[1].Agent != "root" || got.Entries()[1].Usage.Input != 10 {
		t.Fatal("entry metadata lost")
	}
	older := session.FromHeader(session.Header{ID: "20000101-000000-0000000000000000", Updated: s.Updated().Add(-time.Hour)}, nil, nil)
	if err := st.Save(ctx, older); err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(ctx)
	if err != nil || len(infos) != 2 || infos[0].ID != s.ID() || infos[1].ID != older.ID() || infos[0].Messages != 3 || infos[0].Records != 1 || infos[0].Title != "probe" || infos[0].Usage.Input != 10 {
		t.Fatalf("%+v %v", infos, err)
	}
	os.WriteFile(filepath.Join(st.Dir, "stray.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(st.Dir, "dir.json"), 0o755)
	if infos, err := st.List(ctx); err != nil || len(infos) != 2 {
		t.Fatalf("strays must be ignored: %+v %v", infos, err)
	}
	if _, err := st.Load(ctx, "../escape"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := st.Load(ctx, "missing"); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := st.Save(ctx, session.FromHeader(session.Header{ID: "bad/id"}, nil, nil)); err == nil {
		t.Fatal("bad id saved")
	}
	if err := st.Delete(ctx, older.ID()); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, older.ID()); err != session.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := st.Delete(ctx, "bad/id"); err == nil {
		t.Fatal("bad id deleted")
	}
	if ok, _ := st.Exists(ctx, "bad/id"); ok {
		t.Fatal("bad id exists")
	}
	entries, _ := os.ReadDir(st.Dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(entries) != 4 || strings.Contains(strings.Join(names, " "), ".tmp") {
		t.Fatalf("unexpected directory contents: %v", names)
	}
	os.WriteFile(filepath.Join(st.Dir, "corrupt.json"), []byte("{"), 0o644)
	if _, err := st.Load(ctx, "corrupt"); err == nil {
		t.Fatal("corrupt file loaded")
	}
	infos, err = st.List(ctx)
	if err != nil || len(infos) != 2 {
		t.Fatalf("a corrupt file must still be listed: %+v %v", infos, err)
	}
	var corrupt session.Info
	for _, in := range infos {
		if in.ID == "corrupt" {
			corrupt = in
		}
	}
	if corrupt.Error == "" {
		t.Fatalf("corrupt file must carry its error: %+v", infos)
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

func TestListReadsOnlyTheHeader(t *testing.T) {
	ctx := context.Background()
	st := Store{Dir: t.TempDir()}
	s := session.New()
	s.SetTitle("big")
	big := strings.Repeat("x", 1<<20)
	for range 8 {
		s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: big}}})
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID())
	data, _ := os.ReadFile(path)
	broken := append([]byte(nil), data...)
	copy(broken[len(broken)-20:], []byte(`"entries":[{"role"}}}`))
	os.WriteFile(path, broken, 0o600)
	infos, err := st.List(ctx)
	if err != nil || len(infos) != 1 || infos[0].Title != "big" || infos[0].Messages != 8 || infos[0].Error != "" {
		t.Fatalf("header must be readable without the body: %+v %v", infos, err)
	}
	if _, err := st.Load(ctx, s.ID()); err == nil {
		t.Fatal("the damaged body must fail to load")
	}
}

func TestSaveSerializesConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	st := Store{Dir: t.TempDir()}
	s := sample()
	done := make(chan error, 16)
	for i := range 16 {
		go func(i int) {
			c := session.FromHeader(s.Header(), s.Entries(), s.Records())
			c.SetTitle(strings.Repeat("t", i+1))
			done <- st.Save(ctx, c)
		}(i)
	}
	for range 16 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Load(ctx, s.ID())
	if err != nil || got.Len() != 3 {
		t.Fatalf("%v", err)
	}
	entries, _ := os.ReadDir(st.Dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
