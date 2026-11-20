package arena

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Messages a test environment handed the program, and how many of them reached the card
type inbox struct {
	mu    sync.Mutex
	msgs  []tea.Msg
	taken int
}

// Inbox of every test environment
var inboxes = map[*cards.Env]*inbox{}

// Keymap of the tests, the defaults with the arena keys
func testKeys() *keys.Map {
	km := keys.Default()
	RegisterKeys(km)
	return km
}

// Environment of a card under test, everything sent to the program kept in its inbox
func testEnv() *cards.Env {
	in := &inbox{}
	env := &cards.Env{Theme: theme.Blackwall(), Keys: testKeys()}
	env.Send = func(m tea.Msg) {
		in.mu.Lock()
		in.msgs = append(in.msgs, m)
		in.mu.Unlock()
	}
	inboxes[env] = in
	return env
}

// Operations a card under test asked the harness for, by name
type probeHarness struct {
	calls []string
	texts []string
}

func (h *probeHarness) Interrupt(s *Snapshot, m Member) tea.Cmd {
	h.calls = append(h.calls, "interrupt:"+m.Name)
	return nil
}

func (h *probeHarness) Steer(s *Snapshot, m Member, text string) tea.Cmd {
	h.calls, h.texts = append(h.calls, "steer:"+m.Name), append(h.texts, text)
	return nil
}

func (h *probeHarness) Open(s *Snapshot, m Member) tea.Cmd {
	h.calls = append(h.calls, "open:"+m.Name)
	return nil
}

func (h *probeHarness) Pause(paused bool) tea.Cmd {
	if paused {
		h.calls = append(h.calls, "pause")
	} else {
		h.calls = append(h.calls, "resume")
	}
	return nil
}

func (h *probeHarness) Stop(s *Snapshot) tea.Cmd {
	h.calls = append(h.calls, "stop")
	return nil
}

func (h *probeHarness) Retry() tea.Cmd {
	h.calls = append(h.calls, "retry")
	return nil
}

// Journal the cards under test read
var probeJournal = NewJournal()

// Arena card over the probe journal, its worker not started so tests step it by hand
func newArena(env *cards.Env) wm.Card { return New(env, probeJournal, &probeHarness{}, Default(), nil) }

// Reports of a turn with todos, an architect over two builders and a reviewer, one builder done
func probeReports(session string, start time.Time) []Report {
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	todos := []Todo{{ID: "1", Content: "map the parser", Status: "completed"}, {ID: "2", Content: "split the lexer", Status: "in_progress"}, {ID: "3", Content: "ship it", Status: "pending"}}
	arch := Agent{ID: "a1", Goal: "split the lexer into a scanner and a token stream", Status: "running", Model: "m1", ToolCount: 3, Started: at(2),
		Tools: Tools{Current: "delegate_task", Since: at(3), Counts: map[string]int{"read_file": 2, "delegate_task": 1}}}
	b1 := Agent{ID: "b1", Parent: "a1", Depth: 1, Goal: "write the scanner", Status: "running", ToolCount: 6, Started: at(3),
		Tools: Tools{Current: "patch", Since: at(5), Counts: map[string]int{"read_file": 2, "patch": 3, "terminal": 1}}, Thinking: []string{"the scanner needs a lookahead"}}
	b2 := Agent{ID: "b2", Parent: "a1", Depth: 1, Goal: "write the token stream", Status: "completed", ToolCount: 4, Started: at(3), Duration: 2 * time.Second,
		Tools: Tools{Counts: map[string]int{"write_file": 2, "search_files": 2}}, Summary: "token stream written", Usage: Usage{Input: 12000, Output: 3000, Cost: 0.04},
		FilesWritten: []string{"lexer/stream.go"}}
	rev := Agent{ID: "r1", Goal: "review the lexer split for regressions", Status: "running", ToolCount: 1, Started: at(4), Tools: Tools{Current: "read_file", Since: at(5), Counts: map[string]int{"read_file": 1}}}
	base := Report{Session: session, Busy: true, Prompt: "split the lexer", Model: "director-model", Usage: Usage{Input: 1000, Output: 200, Cost: 0.01}, ContextMax: 100000}
	r0 := base
	r0.At = at(0)
	r1 := base
	r1.At, r1.Tools, r1.Todos, r1.Context = at(1), Tools{Current: "todo_list", Since: at(1), Counts: map[string]int{"todo_list": 1}}, todos[:2], 20000
	r2 := r1
	r2.At, r2.Tools = at(2), Tools{Current: "delegate_task", Since: at(2), Counts: map[string]int{"todo_list": 1, "delegate_task": 1}}
	r2.Agents = []Agent{arch}
	b2running := b2
	b2running.Status, b2running.Summary, b2running.Usage = "running", "", Usage{}
	r3 := r2
	r3.At, r3.Agents = at(3), []Agent{arch, b1, b2running}
	r4 := r3
	r4.At, r4.Agents = at(5), []Agent{arch, b1, b2, rev}
	r4.Usage = Usage{Input: 5000, Output: 900, Cost: 0.05}
	return []Report{r0, r1, r2, r3, r4}
}

// Probe turn folded into the probe journal under a session of the test, the journal following that session
func registerProbeSprint(t *testing.T) *Snapshot {
	t.Helper()
	for _, r := range probeReports(t.Name(), time.Now().Add(-10*time.Second)) {
		probeJournal.Observe(r)
	}
	ids := probeJournal.Sprints(t.Name())
	if len(ids) == 0 {
		t.Fatal("the probe reports opened no turn")
	}
	snap, _, _ := probeJournal.View(ids[len(ids)-1], time.Time{})
	return snap
}

// Paints the card into a fresh buffer of a size
func paintInto(a *arena, w, h int) *uv.Buffer {
	buf := uv.NewBuffer(w, h)
	a.Paint(buf)
	return buf
}

// Hands the card every frame its worker sent since the last delivery, counting them
func (a *arena) deliver() int {
	in := inboxes[a.env]
	in.mu.Lock()
	pending := in.msgs[in.taken:]
	in.taken = len(in.msgs)
	in.mu.Unlock()
	n := 0
	for _, m := range pending {
		if fm, ok := m.(frameMsg); ok && fm.card == wm.Card(a) {
			a.Update(fm)
			n++
		}
	}
	return n
}

// Steps the worker by hand a count of frames at thirty a second, each frame drawn and delivered
func (a *arena) advance(n int) {
	s := a.stage
	for range n {
		s.drain()
		s.sc.update(1.0 / 30)
		s.moveCamera(1.0 / 30)
		s.render(time.Now())
		a.deliver()
	}
}

// Puts the camera of the worker at rest on its target
func (a *arena) settleCamera() {
	a.stage.drain()
	a.stage.rig.snap()
}

// Writes cells as a picture, each cell its glyph mask in its two colors
func writeCellsPNG(t *testing.T, buf *uv.Buffer, mode vxl.CellMode, path string) {
	t.Helper()
	px, py := mode.Pixels()
	img := image.NewRGBA(image.Rect(0, 0, buf.Width()*px, buf.Height()*py))
	rgba := func(c color.Color, fallback color.Color) color.Color {
		if c == nil {
			return fallback
		}
		return c
	}
	for y := range buf.Height() {
		for x := range buf.Width() {
			c := buf.CellAt(x, y)
			if c == nil {
				continue
			}
			mask, ok := vxl.GlyphMask(c.Content)
			if !ok && c.Content != " " && c.Content != "" {
				mask = 0xffffffff
			}
			fg, bg := rgba(c.Style.Fg, color.White), rgba(c.Style.Bg, color.Black)
			for i := range px * py {
				col := bg
				if mask>>i&1 != 0 {
					col = fg
				}
				img.Set(x*px+i%px, y*py+i/px, col)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// Snapshot of a turn on its own, a director, an architect, three builders and a reviewer over five tickets
func probeSnapshot() Snapshot {
	now := time.Now()
	members := []Member{
		{Name: "Kay", Role: RoleDirector, Task: "split the lexer", Status: "running"},
		{Name: "Ada", Role: RoleArchitect, Index: 0, Task: "plan the split", Ticket: "D1", Status: "running", Parent: "Kay"},
		{Name: "Linus", Role: RoleBuilder, Index: 1, Task: "write the scanner", Ticket: "D2", Status: "running", Parent: "Ada"},
		{Name: "Grace", Role: RoleBuilder, Index: 2, Task: "write the token stream", Ticket: "D3", Status: "completed", Parent: "Ada"},
		{Name: "Ken", Role: RoleBuilder, Index: 3, Task: "port the tests", Ticket: "D4", Status: "running", Parent: "Ada"},
		{Name: "Alan", Role: RoleReviewer, Index: 4, Task: "review the split", Ticket: "D5", Status: "running", Parent: "Kay"},
	}
	tickets := []Ticket{
		{ID: "D2", Title: "write the scanner", Status: TicketActive, Builder: "Linus"},
		{ID: "D3", Title: "write the token stream", Status: TicketReview, Builder: "Grace"},
		{ID: "D4", Title: "port the tests", Status: TicketActive, Builder: "Ken", DependsOn: []string{"D2"}},
		{ID: "D1", Title: "plan the split", Status: TicketActive, Builder: "Ada", DependsOn: []string{"D2", "D3", "D4"}},
		{ID: "D5", Title: "review the split", Status: TicketTodo, Builder: "Alan", DependsOn: []string{"D1"}},
	}
	msgs := []Message{
		{Seq: 1, At: now, Kind: MsgAssign, From: "Kay", To: "Ada", Ticket: "D1", Text: "plan the split"},
		{Seq: 2, At: now, Kind: MsgAssign, From: "Ada", To: "Linus", Ticket: "D2", Text: "write the scanner"},
		{Seq: 3, At: now, Kind: MsgHandoff, From: "Grace", To: "Ada", Ticket: "D3", Text: "token stream written"},
	}
	return Snapshot{ID: "probe-1", Sprint: "probe-1", Turn: 1, Title: "split the lexer", Stage: StagePlan, Started: now.Add(-time.Minute), At: now, Members: members, Tickets: tickets, Messages: msgs,
		Usage: map[string]Usage{"Grace": {Input: 12000, Output: 3000, Cost: 0.04}}, Live: true, Newest: true}
}

// What each probe member does
func probeLive() map[string]liveState {
	now := time.Now()
	return map[string]liveState{
		"Kay":   {running: true, tool: "delegate_task", toolAt: now, since: now.Add(-time.Minute), heat: 0.2},
		"Ada":   {running: true, tool: "read_file", toolAt: now, heat: 0.3},
		"Linus": {running: true, tool: "patch", toolAt: now, tally: toolTally{reads: 2, edits: 3}, heat: 0.5},
		"Grace": {tally: toolTally{edits: 2, reads: 2}, heat: 0.4},
		"Ken":   {running: true, tool: "terminal", toolAt: now, tally: toolTally{shells: 2}, heat: 0.6},
		"Alan":  {running: true, tool: "read_file", toolAt: now},
	}
}

// Snapshot of a larger turn of fourteen members over the tickets T1 to T6
func probeSnapshot14() Snapshot {
	now := time.Now()
	names := []string{"Kay", "Ada", "Linus", "Grace", "Ken", "Alan", "Barbara", "Dennis", "Margaret", "Edsger", "Frances", "Donald", "Radia", "Bjarne"}
	var members []Member
	var tickets []Ticket
	for i, name := range names {
		m := Member{Name: name, Role: RoleBuilder, Index: i - 1, Status: "running", Parent: "Kay"}
		switch {
		case i == 0:
			m = Member{Name: name, Role: RoleDirector, Status: "running"}
		case i == 1:
			m.Role = RoleArchitect
		case i == 13:
			m.Role = RoleReviewer
		}
		if i >= 2 && i <= 7 {
			m.Ticket = "T" + string(rune('0'+i-1))
			status := TicketActive
			if i%3 == 0 {
				status = TicketDone
			}
			var deps []string
			if i > 2 {
				deps = []string{"T" + string(rune('0'+i-2))}
			}
			tickets = append(tickets, Ticket{ID: m.Ticket, Title: "part " + m.Ticket, Status: status, Builder: name, DependsOn: deps})
		}
		members = append(members, m)
	}
	return Snapshot{ID: "probe-14", Sprint: "probe-14", Turn: 2, Title: "port the compiler", Stage: StagePlan, Started: now.Add(-2 * time.Minute), At: now, Members: members, Tickets: tickets, Live: true, Newest: true}
}

// What each member of the larger probe does
func probeLive14() map[string]liveState {
	now := time.Now()
	out := map[string]liveState{}
	for i, m := range probeSnapshot14().Members {
		out[m.Name] = liveState{running: true, tool: []string{"read_file", "patch", "terminal", "web_search"}[i%4], toolAt: now, heat: float64(i%5) / 5}
	}
	return out
}
