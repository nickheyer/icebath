package buzz

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestCursorStoreRoundTripAndForeignFiles(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	s := &CursorStore{SelfPubkey: "pk", RelayURL: "wss://r"}
	s.Watch("b", &ChannelCursor{LastTS: 5, Seen: []string{"e1", "e2"}})
	s.Watch("a", &ChannelCursor{ChatType: "dm"})
	s.SaveCursors(ctx)
	raw, err := os.ReadFile(CursorPath(ctx))
	want := `{"identity": "pk", "relay": "wss://r", "channels": {"b": {"chat_type": "group", "last_ts": 5, "seen": ["e1", "e2"]}, "a": {"chat_type": "dm", "last_ts": 0, "seen": []}}}`
	if err != nil || string(raw) != want {
		t.Fatalf("%s %v", raw, err)
	}
	other := &CursorStore{SelfPubkey: "pk", RelayURL: "wss://r"}
	if err := other.LoadCursors(ctx); err != nil || other.RestoredCursors["b"].LastTS != 5 || other.RestoredCursors["b"].ChatType != "group" || len(other.RestoredCursors["a"].Seen) != 0 {
		t.Fatalf("%+v %v", other.RestoredCursors, err)
	}
	foreign := &CursorStore{SelfPubkey: "pk2", RelayURL: "wss://r"}
	if foreign.LoadCursors(ctx); len(foreign.RestoredCursors) != 0 {
		t.Fatal("foreign identity restored")
	}
	seen := make([]string, 0, 510)
	for i := 0; i < 510; i++ {
		seen = append(seen, `"x`+strings.Repeat("y", i%3)+`"`)
	}
	body := "\ufeff" + `{"identity": "pk", "relay": "wss://r", "channels": {"c": {"last_ts": "12", "seen": [` + strings.Join(seen, ",") + `, 7], "chat_type": 3}, "d": {"last_ts": "x"}, "e": [], "f": {"last_ts": 2.9, "seen": "no"}}}`
	os.WriteFile(CursorPath(ctx), []byte(body), 0o600)
	if err := other.LoadCursors(ctx); err != nil {
		t.Fatal(err)
	}
	c := other.RestoredCursors["c"]
	if len(c.Seen) != 500 || c.Seen[499] != "7" || c.LastTS != 12 || c.ChatType != "3" || other.RestoredCursors["f"].LastTS != 2 || len(other.RestoredCursors) != 2 {
		t.Fatalf("%+v", other.RestoredCursors)
	}
	os.WriteFile(CursorPath(ctx), []byte(`{"identity": "pk", "relay": "wss://r", "channels": {"g": {"last_ts": Infinity}}}`), 0o600)
	if err := other.LoadCursors(ctx); err == nil {
		t.Fatal("infinite last_ts did not raise")
	}
	os.WriteFile(CursorPath(ctx), []byte(`not json`), 0o600)
	if err := other.LoadCursors(ctx); err != nil || len(other.RestoredCursors) != 0 {
		t.Fatal(err)
	}
	os.Remove(CursorPath(ctx))
	if err := other.LoadCursors(ctx); err != nil || len(other.RestoredCursors) != 0 {
		t.Fatal(err)
	}
}

func TestCursorWriterSkipsStaleSnapshotsAndMarks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	s := &CursorStore{SelfPubkey: "pk", RelayURL: "wss://r"}
	state := &ChannelCursor{LastTS: 1}
	s.Watch("a", state)
	older := s.CursorWriter(ctx)
	state.LastTS = 2
	newer := s.CursorWriter(ctx)
	newer()
	older()
	raw, _ := os.ReadFile(CursorPath(ctx))
	if !strings.Contains(string(raw), `"last_ts": 2`) {
		t.Fatalf("older write landed after the newer one: %s", raw)
	}
	if m := CursorMarkOf(state); m != (CursorMark{LastTS: 2}) {
		t.Fatalf("%+v", m)
	}
	state.Seen = []string{"a", "b"}
	if m := CursorMarkOf(state); m != (CursorMark{LastTS: 2, Count: 2, Last: "b", HasLast: true}) {
		t.Fatalf("%+v", m)
	}
}
