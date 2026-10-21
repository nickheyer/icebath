package harness

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

func TestWorkspaceTracksFilesAndDirectory(t *testing.T) {
	w := NewWorkspace("/start")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w.Clock = func() time.Time { return base }
	end := func(name, id string, at time.Duration, data any) agent.Event {
		return agent.Event{Type: agent.EventToolEnd, Agent: "root", Time: base.Add(at), Call: protocol.ToolCall{ID: id, Name: name}, Data: data}
	}
	w.Apply(end("Read", "r1", 0, files.ReadData{Path: "/p/a.go"}))
	w.Apply(end("Write", "w1", time.Second, files.Change{Path: "/p/b.go", Old: "", New: "one", Created: true}))
	w.Apply(end("Edit", "e1", 2*time.Second, files.Change{Path: "/p/b.go", Old: "one", New: "two", Replaced: 1}))
	w.Apply(end("Glob", "g1", 3*time.Second, files.GlobData{}))
	w.Apply(end("Grep", "g2", 3*time.Second, files.GrepData{}))
	w.Apply(end("Bash", "s1", 4*time.Second, shell.Run{Dir: "/p/sub"}))
	w.Apply(end("Bash", "s2", 4*time.Second, shell.Run{}))
	w.Apply(agent.Event{Type: agent.EventToolStart, Data: files.ReadData{Path: "/ignored"}})
	w.Apply(agent.Event{Type: agent.EventToolEnd, Data: files.ReadData{Path: "/p/c.go"}})
	fs := w.Files()
	if len(fs) != 3 || fs[0].Path != "/p/b.go" || fs[1].Path != "/p/a.go" || fs[2].Path != "/p/c.go" {
		t.Fatalf("%+v", fs)
	}
	b := fs[0]
	if b.Writes != 1 || b.Edits != 1 || b.Change == nil || b.Change.New != "two" || b.Original == nil || *b.Original != "" || b.CallID != "e1" || b.Agent != "root" {
		t.Fatalf("%+v", b)
	}
	if changed := w.Changed(); len(changed) != 1 || changed[0].Path != "/p/b.go" {
		t.Fatalf("%+v", changed)
	}
	if got, ok := w.Get("/p/a.go"); !ok || got.Reads != 1 {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := w.Get("/nope"); ok {
		t.Fatal("unknown path found")
	}
	if w.Dir() != "/p/sub" || w.Searches() != 2 {
		t.Fatalf("%q %d", w.Dir(), w.Searches())
	}
	w.SetDir("/elsewhere")
	if w.Dir() != "/elsewhere" {
		t.Fatal("set dir")
	}
}
