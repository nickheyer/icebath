package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Chat completions server that streams a delta every gap until it sent the count
func slowServer(t testing.TB, deltas int, gap time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < deltas; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m-fast\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", fmt.Sprintf("word%d ", i))
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m-fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Function run on the event loop of the program
type onLoop struct {
	fn   func(*Model) tea.Cmd
	done chan struct{}
}

// Model that runs onLoop messages inside the program and counts card ticks
type looped struct {
	*Model
	ticks *atomic.Int64
}

func (l looped) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if fmt.Sprintf("%T", msg) == "cards.tickMsg" {
		l.ticks.Add(1)
	}
	if o, ok := msg.(onLoop); ok {
		cmd := o.fn(l.Model)
		close(o.done)
		return l, cmd
	}
	_, cmd := l.Model.Update(msg)
	return l, cmd
}

// Runs fn on the event loop and waits for it, failing with every goroutine stack when the loop stalls
func inLoop(t *testing.T, p *tea.Program, fn func(*Model) tea.Cmd) {
	t.Helper()
	o := onLoop{fn: fn, done: make(chan struct{})}
	go p.Send(o)
	select {
	case <-o.done:
	case <-time.After(5 * time.Second):
		pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		t.Fatal("event loop stalled")
	}
}

func TestTwoSessionsStreamAtOnce(t *testing.T) {
	srv := slowServer(t, 150, 10*time.Millisecond)
	cfg, dir := testConfig(t, srv.URL)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var ticks atomic.Int64
	p := tea.NewProgram(looped{Model: m, ticks: &ticks}, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(200, 60), tea.WithoutSignalHandler(), tea.WithFilter(m.Filter))
	m.Attach(p)
	ran := make(chan error, 1)
	go func() { _, err := p.Run(); ran <- err }()
	t.Cleanup(func() {
		p.Quit()
		<-ran
	})
	var ids []string
	inLoop(t, p, func(m *Model) tea.Cmd { return m.newSession() })
	inLoop(t, p, func(m *Model) tea.Cmd {
		for _, d := range m.desks {
			ids = append(ids, d.live.Session.ID())
		}
		return nil
	})
	if len(ids) != 2 {
		t.Fatalf("desks %d", len(ids))
	}
	send := func(id, text string) func(*Model) tea.Cmd {
		return func(*Model) tea.Cmd {
			return func() tea.Msg {
				return cards.SubmitMsg{Session: id, Parts: []protocol.Part{protocol.Text{Text: text}}, Text: text}
			}
		}
	}
	inLoop(t, p, send(ids[0], "tell me a long story"))
	time.Sleep(200 * time.Millisecond)
	inLoop(t, p, send(ids[1], "tell me a story"))
	start := time.Now()
	deadline := start.Add(15 * time.Second)
	for {
		var busy bool
		inLoop(t, p, func(m *Model) tea.Cmd {
			busy = m.app.Control.Busy()
			return nil
		})
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runs never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Two transcript loops at 120ms each, with slack for the tail after the runs end
	limit := int64(time.Since(start)/(120*time.Millisecond))*2 + 10
	if n := ticks.Load(); n > limit {
		t.Fatalf("%d card ticks in %s, two transcript loops allow %d", n, time.Since(start).Round(time.Millisecond), limit)
	}
}
