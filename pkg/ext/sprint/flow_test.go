package sprint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Request as the fake endpoint reads it
type wireRequest struct {
	Tools    []struct{ Name string } `json:"tools"`
	Messages []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	} `json:"messages"`
}

// Model standing in for every role, answering each request with the tool call its stage waits for
type fakeModel struct {
	mu    sync.Mutex
	calls []string
}

var ticketOf = regexp.MustCompile(`(?:Your ticket|Handoff on) (T\d+)`)

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tools := map[string]bool{}
	for _, t := range req.Tools {
		tools[t.Name] = true
	}
	last := req.Messages[len(req.Messages)-1]
	var text string
	afterTool := false
	for _, part := range last.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(part, &block)
		if block.Type == "tool_result" {
			afterTool = true
		}
		text += block.Text
	}
	var all string
	for _, m := range req.Messages {
		for _, part := range m.Content {
			var block struct{ Text string }
			json.Unmarshal(part, &block)
			all += block.Text + "\n"
		}
	}
	ticket := ""
	if m := ticketOf.FindAllStringSubmatch(all, -1); len(m) > 0 {
		ticket = m[len(m)-1][1]
	}
	name, args := "", ""
	switch {
	case afterTool:
	case strings.Contains(text, "Write the report"):
	case tools[ToolBrief]:
		name, args = ToolBrief, `{"objective":"make the thing","acceptance":["it works"]}`
	case tools[ToolPlan]:
		name, args = ToolPlan, `{"title":"Thing","summary":"two steps","tickets":[{"id":"T1","title":"first","description":"do first","files":["a.go"]},{"id":"T2","title":"second","description":"do second","files":["b.go"],"depends_on":["T1"]}]}`
	case tools[ToolHandoff]:
		name, args = ToolHandoff, fmt.Sprintf(`{"ticket":%q,"status":"done","summary":"built %s","tested":"go test"}`, ticket, ticket)
	case tools[ToolVerdict]:
		name, args = ToolVerdict, fmt.Sprintf(`{"ticket":%q,"verdict":"accepted"}`, ticket)
	}
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, data any) {
		raw, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": "fake", "usage": map[string]int{"input_tokens": 120, "output_tokens": 0}}})
	stop := "end_turn"
	if name != "" {
		stop = "tool_use"
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call_1", "name": name, "input": map[string]any{}}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
	} else {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "# Shipped\nall of it\n# Not shipped\nnothing\n# How to verify\ngo test\n# Left to do\nnothing"}})
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]int{"output_tokens": 30}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// Calls the fake model answered, oldest first
func (f *fakeModel) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// App whose only profile talks to the fake model, with the sprint tools and kinds in place
func fakeApp(t *testing.T, model *fakeModel) *harness.App {
	t.Helper()
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	path, err := profiles.UserPath()
	if err != nil {
		t.Fatal(err)
	}
	doc := profiles.Document{Default: "fake", Profiles: []profiles.Profile{{Name: "fake", Template: "anthropic", BaseURL: srv.URL, Models: []string{"fake"}, Key: profiles.Source{Kind: profiles.SourceStatic, Value: "k"}}}}
	if err := profiles.WriteFile(path, doc); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app, err := harness.Build(ctx, harness.Config{Profile: "fake", Mode: "bypassPermissions", MaxTokens: 4096}, func(any) {}, harness.BuildOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(ctx) })
	if err := app.Tools.Add(sprintTools(app)...); err != nil {
		t.Fatal(err)
	}
	return app
}

// Waits for a flow to end within the deadline, telling what the sprint was doing when it did not
func waitFlow(t *testing.T, f *harness.Flow) {
	t.Helper()
	select {
	case <-f.Done():
	case <-time.After(30 * time.Second):
		snap, _ := Lookup(f.ID)
		for _, m := range snap.Messages {
			t.Logf("%s %s -> %s %s: %s", m.Kind, m.From, m.To, m.Ticket, m.Text)
		}
		for _, m := range snap.Members {
			if m.Live != nil {
				t.Logf("%s session %d entries", m.Name, m.Live.Session.Len())
				for _, e := range m.Live.Session.Entries() {
					t.Logf("   %s: %.200s", e.Role, protocol.Response{Content: e.Content}.Text())
				}
			}
		}
		t.Fatalf("the sprint did not end in time, stage %s status %q paused %q", snap.Stage, snap.Status, snap.Paused)
	}
}

// Waits until no run goes on and every scheduled save has been written
func settle(t *testing.T, app *harness.App) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for app.Control.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("runs still going long after the sprint ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	app.Saver.Wait()
}

// Whole sprint runs through every stage against the fake model, its file and sessions written as it goes
func TestSprintRunsEndToEnd(t *testing.T) {
	model := &fakeModel{}
	app := fakeApp(t, model)
	ctx := context.Background()
	f, err := app.Flows.Start(ctx, workflow(), nil, "make the thing")
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "sprint-1" {
		t.Fatalf("flow id %s", f.ID)
	}
	waitFlow(t, f)
	if f.Err() != nil {
		t.Fatalf("sprint failed: %v", f.Err())
	}
	snap, ok := Lookup(f.ID)
	if !ok {
		t.Fatal("sprint not registered")
	}
	unregister(t, sprintByID(f.ID))
	if snap.Stage != StageDone || snap.Running() || snap.Summary == "" || snap.Brief.Objective != "make the thing" || len(snap.Tickets) != 2 {
		t.Fatalf("snapshot %+v", snap)
	}
	for _, tk := range snap.Tickets {
		if tk.Status != TicketDone || tk.Builder == "" {
			t.Fatalf("ticket %+v", tk)
		}
	}
	if len(snap.Members) != 5 {
		t.Fatalf("%d members, want director, architect, reviewer and two builders for two tickets", len(snap.Members))
	}
	settle(t, app)
	infos, err := app.Store.ListProject(ctx, app.Root)
	if err != nil {
		t.Fatal(err)
	}
	labeled := 0
	for _, in := range infos {
		if in.Labels[sprintLabel] == f.ID && in.Labels[roleLabel] != "" {
			labeled++
		} else {
			t.Logf("unlabeled stored session %s labels %v error %q", in.ID, in.Labels, in.Error)
		}
	}
	if labeled != 5 {
		for _, m := range snap.Members {
			t.Logf("member %s session %s open %v", m.Name, m.SessionID(), m.Live != nil && app.LiveOf(m.Live.Session) != nil)
		}
		t.Fatalf("%d stored sessions labeled with the sprint", labeled)
	}
	dir, err := journalPath(app)
	if err != nil {
		t.Fatal(err)
	}
	r, err := readRecord(filepath.Join(dir, f.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != StageDone || r.Finished.IsZero() || len(r.Members) != 5 || r.Members[0].Session == "" {
		t.Fatalf("record %+v", r)
	}
	calls := strings.Join(model.seen(), " ")
	for _, want := range []string{ToolBrief, ToolPlan, ToolHandoff, ToolVerdict} {
		if !strings.Contains(calls, want) {
			t.Fatalf("the model never answered with %s: %s", want, calls)
		}
	}
	if _, err := Forget(app, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(filepath.Join(dir, f.ID+".json")); err == nil {
		t.Fatal("file kept after forget")
	}
}

// Sprint left mid build by an exited process restarts on its stored sessions and runs to the end
func TestSprintRecoversAndFinishes(t *testing.T) {
	model := &fakeModel{}
	app := fakeApp(t, model)
	ctx := context.Background()
	stored := session.New()
	stored.Label(session.ProjectLabel, app.Root)
	stored.Label("kind", KindName(RoleBuilder))
	stored.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "# Your ticket T1 first\nearlier work"}}})
	stored.AddResponse("", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "working"}}, StopReason: protocol.StopEnd})
	if err := app.Store.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	dir, err := journalPath(app)
	if err != nil {
		t.Fatal(err)
	}
	r := record{ID: "sprint-5", Project: app.Root, Request: "make the thing", Started: time.Now().Add(-time.Hour), Stage: StageBuild, Status: "building",
		Brief: Brief{Objective: "make the thing"}, Plan: Plan{Title: "Thing", Summary: "two steps"},
		Tickets: []Ticket{{ID: "T1", Title: "first", Description: "do first", Status: TicketActive, Builder: "Builder 1"}, {ID: "T2", Title: "second", Description: "do second", Status: TicketTodo, DependsOn: []string{"T1"}}},
		Members: []memberRecord{{Role: RoleBuilder, Name: "Builder 1", Session: stored.ID(), Ticket: "T1", Task: "building T1"}}}
	if err := writeRecord(filepath.Join(dir, "sprint-5.json"), r); err != nil {
		t.Fatal(err)
	}
	if n, err := recoverSprints(app); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	s := sprintByID("sprint-5")
	unregister(t, s)
	if !s.Snapshot().Exited() {
		t.Fatal("recovered sprint not marked interrupted")
	}
	f, err := app.Flows.Restart(ctx, "sprint-5", workflow(), nil, "make the thing")
	if err != nil {
		t.Fatal(err)
	}
	waitFlow(t, f)
	if f.Err() != nil {
		t.Fatalf("restarted sprint failed: %v", f.Err())
	}
	snap := s.Snapshot()
	if snap.Stage != StageDone || snap.Summary == "" {
		t.Fatalf("snapshot after restart %+v", snap)
	}
	m, ok := snap.Member("Builder 1")
	if !ok || m.Live == nil || m.Live.Session.ID() != stored.ID() {
		t.Fatalf("builder not back on its stored session: %+v", m)
	}
	if m.Live.Session.Len() <= 2 {
		t.Fatal("the stored transcript did not grow")
	}
	if calls := strings.Join(model.seen(), " "); strings.Contains(calls, ToolBrief) || strings.Contains(calls, ToolPlan) {
		t.Fatalf("restart at the build stage ran the brief or the plan again: %s", calls)
	}
	next, err := nextID(app)
	if err != nil || next != "sprint-6" {
		t.Fatalf("next id %q %v", next, err)
	}
}
