package app

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Markdown answer of about sixteen hundred words with lists and code fences
var benchAnswer = strings.Repeat(`So here you stand, in front of the **tiling desk**, cards breathing. Somewhere a mission objective is burning in a header and a pump is turning, events arriving as JSON lines for anyone who prefers truth without a terminal.

- the models beyond the wall do not know they are being tiled
- they think they are answering a *person*
- they are answering a layout manager

`+"```go\n"+`for _, e := range ws.entries() {
	moving = m.step(e, dt, focused, hovered) || moving
}
`+"```\n\n", 4)

// Model showing the screenshot desk, two sessions with ten answered turns each beside the sessions, metrics and config panels
func benchModel(b testing.TB) (*Model, []*desk) {
	b.Helper()
	srv, _ := fakeServer(b, false)
	cfg, dir := testConfig(b, srv.URL)
	m, err := New(b.Context(), Options{Config: cfg, Dir: dir, Version: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	feed(m, m.newSession())
	desks := append([]*desk(nil), m.desks...)
	for _, d := range desks {
		for i := 0; i < 10; i++ {
			d.live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "tell me a story"}}})
			d.live.Session.Append(protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Text{Text: benchAnswer}}})
		}
		d.session.Reload()
	}
	feed(m, m.command(nil, "sessions", ""))
	feed(m, m.command(nil, "metrics", ""))
	feed(m, m.command(nil, "config", ""))
	settle(m)
	return m, desks
}

// Runs frames until every card rests in place
func settle(m *Model) {
	for i := 0; i < 240; i++ {
		m.wm.Advance(1.0 / 60)
	}
	m.sync()
	m.View()
}

// One frame of the running program, the clock step, the app sync and the view
func frame(m *Model) {
	m.wm.Advance(1.0 / 60)
	m.sync()
	m.View()
}

// Streams one delta into every session, the way the pump delivers them
func benchDeltas(m *Model, desks []*desk, runs []*harness.Run, words []string, n int) {
	at := time.Now()
	for i, d := range desks {
		_ = d
		text := strings.Join(words[:min(n, len(words))], " ")
		resp := protocol.Response{Model: "m-fast", Content: []protocol.Part{protocol.Text{Text: text}}}
		m.Update(harness.EventMsg{Run: runs[i].ID, Event: agent.Event{Type: agent.EventModel, Time: at, Model: protocol.Event{Type: protocol.EventTextDelta, Delta: " word", Response: resp}}})
	}
}

func BenchmarkFrameIdle(b *testing.B) {
	m, _ := benchModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame(m)
	}
}

func BenchmarkFrameStreamingTwoSessions(b *testing.B) {
	m, desks := benchModel(b)
	words := strings.Fields(benchAnswer)
	var runs []*harness.Run
	for _, d := range desks {
		r, err := m.app.Control.Start(b.Context(), &agent.Agent{Name: "root", Model: stubHold{}, ModelID: "m-fast", Session: d.live.Session}, "", []protocol.Part{protocol.Text{Text: "tell me a story"}})
		if err != nil {
			b.Fatal(err)
		}
		runs = append(runs, r)
		m.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventTurnStart, Time: time.Now()}})
	}
	b.Cleanup(func() {
		for _, r := range runs {
			m.app.Control.Cancel(r.ID)
		}
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDeltas(m, desks, runs, words, (i%600+1)*3)
		frame(m)
	}
}

func BenchmarkFrameResizeWidth(b *testing.B) {
	m, _ := benchModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tea.WindowSizeMsg{Width: 200 + i%2, Height: 60})
		frame(m)
	}
}

func BenchmarkFrameResize(b *testing.B) {
	m, _ := benchModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(tea.WindowSizeMsg{Width: 200, Height: 60 + i%2})
		frame(m)
	}
}

// Model that holds its stream open until the run is cancelled
type stubHold struct{}

func (stubHold) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		<-ctx.Done()
		yield(protocol.Event{}, ctx.Err())
	}
}
