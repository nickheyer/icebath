package sessions

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sqlite/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

func TestStore(t *testing.T) {
	cfg := config.New(t.TempDir())
	host := sessions.New(cfg)
	sqliteconfig.Register(cfg)
	Register(&host.Registry, cfg)

	if _, err := host.Store("missing"); err == nil {
		t.Fatal("Store(\"missing\") resolved, want an error")
	}
	store, err := host.Store(cfg.GetString(sessionsconfig.Store))
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if got, want := cfg.GetString(sqliteconfig.Path), filepath.Join(cfg.Dir(), "sessions.db"); got != want {
		t.Fatalf("database path = %q, want %q", got, want)
	}

	first, err := store.Create("/work/first")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	second, err := store.Create("/work/second")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if first.GetId() == "" || first.GetId() == second.GetId() || first.GetDirectory() != "/work/first" || first.GetTitle() != "" || first.GetCreated() == nil {
		t.Fatalf("Create() = %v", first)
	}

	messages := []*sessionsv1.Message{
		{Role: sessionsv1.Role_ROLE_USER, Text: "hello"},
		{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: "hi", Reasoning: "greet", ToolCalls: []*sessionsv1.ToolCall{{Id: "c1", Name: "terminal", Arguments: `{"command":"ls"}`}}, Carriers: map[string][]byte{"wire": []byte("raw")}},
		{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: "c1", Text: "out", Failed: true},
		{Role: sessionsv1.Role_ROLE_USER, Text: "again"},
	}
	time.Sleep(2 * time.Millisecond)
	for _, message := range messages {
		if err := store.Append(first.GetId(), message); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	read, err := store.Messages(first.GetId())
	if err != nil || len(read) != 4 {
		t.Fatalf("Messages() = %d messages, %v, want 4", len(read), err)
	}
	if got := read[1]; got.GetReasoning() != "greet" || got.GetToolCalls()[0].GetArguments() != `{"command":"ls"}` || string(got.GetCarriers()["wire"]) != "raw" || !read[2].GetFailed() {
		t.Fatalf("Messages() did not round trip: %v", read)
	}
	if other, _ := store.Messages(second.GetId()); len(other) != 0 {
		t.Fatalf("Messages() of the other session = %d, want 0", len(other))
	}

	list, err := store.List()
	if err != nil || len(list) != 2 || list[0].GetId() != first.GetId() {
		t.Fatalf("List() = %v, %v, want the session written to last first", list, err)
	}
	if err := store.Rename(first.GetId(), "renamed"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if got, _ := store.Get(first.GetId()); got.GetTitle() != "renamed" {
		t.Fatalf("Get() title = %q, want renamed", got.GetTitle())
	}
	if err := store.Truncate(first.GetId(), 1); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	if read, _ := store.Messages(first.GetId()); len(read) != 1 || read[0].GetText() != "hello" {
		t.Fatalf("Messages() after Truncate(1) = %v, want only the first", read)
	}

	if err := store.Delete(second.GetId()); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	for name, err := range map[string]error{
		"Get":    func() error { _, err := store.Get(second.GetId()); return err }(),
		"Rename": store.Rename(second.GetId(), "x"),
		"Delete": store.Delete(second.GetId()),
		"Append": store.Append(second.GetId(), messages[0]),
	} {
		if !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("%s() of a deleted session error = %v, want ErrNotFound", name, err)
		}
	}

	if err := host.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, err := store.Get(first.GetId()); err != nil || got.GetTitle() != "renamed" {
		t.Fatalf("Get() after Close() = %v, %v, want the store to reopen its file", got, err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}
