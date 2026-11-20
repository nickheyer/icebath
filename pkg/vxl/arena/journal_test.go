package arena

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Newest snapshot of the only turn of a session
func newestView(t *testing.T, j *Journal, session string) (*Snapshot, map[string]liveState) {
	t.Helper()
	ids := j.Sprints(session)
	if len(ids) == 0 {
		t.Fatal("no turn recorded")
	}
	snap, live, ok := j.View(ids[len(ids)-1], time.Time{})
	if !ok {
		t.Fatal("the newest turn has no view")
	}
	return snap, live
}

// Kinds and parties of the messages, one line each
func messageLines(s *Snapshot) []string {
	var out []string
	for _, m := range s.Messages {
		out = append(out, string(m.Kind)+" "+m.From+">"+m.To+" "+m.Ticket)
	}
	return out
}

// A turn with todos, an architect over two builders and a reviewer maps onto the crew and the board
func TestJournalMapsTurn(t *testing.T) {
	j := NewJournal()
	start := time.Unix(1_700_000_000, 0)
	reports := probeReports("s1", start)
	j.Observe(reports[0])
	snap, live := newestView(t, j, "s1")
	if !snap.Running() || snap.Stage != StageBrief || snap.Turn != 1 || snap.Title != "split the lexer" || len(snap.Members) != 1 || snap.Members[0].Role != RoleDirector {
		t.Fatalf("turn open as %+v", snap)
	}
	if !live["Kay"].running {
		t.Fatal("the director does not run while the turn does")
	}
	j.Observe(reports[1])
	snap, live = newestView(t, j, "s1")
	if snap.Stage != StagePlan {
		t.Fatalf("stage %q once the director planned", snap.Stage)
	}
	if t1, ok := snap.Ticket("T1"); !ok || t1.Status != TicketDone || t1.Kind != KindTodo || t1.Builder != "Kay" {
		t.Fatalf("first todo as %+v", t1)
	}
	if t2, _ := snap.Ticket("T2"); t2.Status != TicketActive {
		t.Fatalf("todo in progress as %q", t2.Status)
	}
	k := live["Kay"]
	if k.tool != "todo_list" || k.tally.calls != 1 || k.heat != 0.2 || k.contextLimit != 100000 {
		t.Fatalf("director live %+v", k)
	}
	for _, r := range reports[2:] {
		j.Observe(r)
	}
	snap, live = newestView(t, j, "s1")
	roles := map[string]Role{}
	for _, m := range snap.Members {
		roles[m.Name] = m.Role
	}
	if roles["Ada"] != RoleArchitect || roles["Linus"] != RoleBuilder || roles["Grace"] != RoleBuilder || roles["Ken"] != RoleReviewer {
		t.Fatalf("roles %v", roles)
	}
	ada, _ := snap.Member("Ada")
	if ada.ID != "a1" || ada.Parent != "Kay" || ada.Ticket != "D1" {
		t.Fatalf("architect %+v", ada)
	}
	if d1, _ := snap.Ticket("D1"); !slices.Equal(d1.DependsOn, []string{"D2", "D3"}) || d1.Status != TicketActive {
		t.Fatalf("architect ticket %+v", d1)
	}
	if d3, _ := snap.Ticket("D3"); d3.Status != TicketReview {
		t.Fatalf("finished result under a working architect is %q", d3.Status)
	}
	msgs := messageLines(snap)
	for _, want := range []string{"assign Kay>Ada D1", "assign Ada>Linus D2", "assign Ada>Grace D3", "assign Kay>Ken D4", "handoff Grace>Ada D3"} {
		if !slices.Contains(msgs, want) {
			t.Fatalf("no %q among %v", want, msgs)
		}
	}
	if l := live["Linus"]; l.tool != "patch" || l.tally.edits != 3 || l.tally.reads != 2 || l.tally.shells != 1 || !l.running {
		t.Fatalf("builder live %+v", l)
	}
	if g := snap.Usage["Grace"]; g.Cost != 0.04 || g.Output != 3000 {
		t.Fatalf("finished builder spend %+v", g)
	}
	if k := snap.Usage["Kay"]; k.Input != 4000 || k.Output != 700 {
		t.Fatalf("director spend of the turn %+v", k)
	}
	grace, _ := snap.Member("Grace")
	if grace.Summary != "token stream written" || !slices.Equal(grace.FilesWritten, []string{"lexer/stream.go"}) {
		t.Fatalf("finished builder detail %+v", grace)
	}
}

// Reviewed results are accepted once their parent stops, failures and requests are told, the turn ends reporting
func TestJournalSettlesTurn(t *testing.T) {
	j := NewJournal()
	start := time.Unix(1_700_000_000, 0)
	reports := probeReports("s1", start)
	for _, r := range reports {
		j.Observe(r)
	}
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	last := reports[len(reports)-1]
	agents := slices.Clone(last.Agents)
	agents[0].Status, agents[0].Summary = "completed", "lexer split"
	r := last
	r.At, r.Agents = at(6), agents
	j.Observe(r)
	snap, _ := newestView(t, j, "s1")
	if d3, _ := snap.Ticket("D3"); d3.Status != TicketDone {
		t.Fatalf("result of the builder stays %q once the architect finished", d3.Status)
	}
	if d1, _ := snap.Ticket("D1"); d1.Status != TicketReview {
		t.Fatalf("architect result under the open turn is %q", d1.Status)
	}
	if !slices.Contains(messageLines(snap), "verdict Ada>Grace D3") || !slices.Contains(messageLines(snap), "handoff Ada>Kay D1") {
		t.Fatalf("messages %v", messageLines(snap))
	}
	agents = slices.Clone(agents)
	agents[1].Status = "failed"
	r.At, r.Agents, r.Approval = at(7), agents, "run rm -rf build"
	j.Observe(r)
	snap, live := newestView(t, j, "s1")
	if d2, _ := snap.Ticket("D2"); d2.Status != TicketFailed || !live["Linus"].failed {
		t.Fatalf("failed builder ticket %q live %+v", d2.Status, live["Linus"])
	}
	if snap.Waiting != "approve run rm -rf build" || !live["Kay"].approval {
		t.Fatalf("waiting %q", snap.Waiting)
	}
	if t2, _ := snap.Ticket("T2"); t2.Status != TicketBlocked {
		t.Fatalf("todo in progress while an approval waits is %q", t2.Status)
	}
	msgs := messageLines(snap)
	for _, want := range []string{"verdict Ada>Linus D2", "failure Linus> D2", "question Kay>you "} {
		if !slices.Contains(msgs, want) {
			t.Fatalf("no %q among %v", want, msgs)
		}
	}
	r.At, r.Approval = at(8), ""
	j.Observe(r)
	snap, _ = newestView(t, j, "s1")
	if snap.Waiting != "" || !slices.Contains(messageLines(snap), "answer you>Kay ") {
		t.Fatalf("answered request still waits %q", snap.Waiting)
	}
	r.At, r.Busy, r.Outcome = at(9), false, OutcomeDone
	j.Observe(r)
	snap, _ = newestView(t, j, "s1")
	if snap.Stage != StageReport || snap.Outcome != OutcomeDone {
		t.Fatalf("ended turn at stage %q outcome %q", snap.Stage, snap.Outcome)
	}
	if d1, _ := snap.Ticket("D1"); d1.Status != TicketDone {
		t.Fatalf("architect result stays %q after the turn", d1.Status)
	}
	if !snap.Running() {
		t.Fatal("the turn stopped while its reviewer still runs")
	}
	agents = slices.Clone(agents)
	agents[3].Status = "completed"
	r.At, r.Agents = at(10), agents
	j.Observe(r)
	snap, _ = newestView(t, j, "s1")
	if snap.Running() || !snap.Ended.Equal(at(10)) {
		t.Fatalf("turn running %v ended %v once every agent finished", snap.Running(), snap.Ended)
	}
	if d4, _ := snap.Ticket("D4"); d4.Status != TicketDone {
		t.Fatalf("reviewer result after the turn is %q", d4.Status)
	}
	if past, _, _ := j.View(snap.Sprint, at(1.5)); past.Stage != StagePlan || len(past.Members) != 1 || len(past.Tickets) != 2 {
		t.Fatalf("turn at 1.5s replays as stage %q with %d members and %d tickets", past.Stage, len(past.Members), len(past.Tickets))
	}
}

// Every turn opens its own sprint, agents stay with their first turn, failures and stops are kept
func TestJournalTurnsAndOutcomes(t *testing.T) {
	j := NewJournal()
	start := time.Unix(1_700_000_000, 0)
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	bg := Agent{ID: "x1", Goal: "index the repository", Status: "running"}
	j.Observe(Report{Session: "s2", At: at(0), Agents: []Agent{bg}})
	snap, _ := newestView(t, j, "s2")
	if snap.Turn != 0 || !snap.Running() || len(snap.Members) != 2 {
		t.Fatalf("agents already running open turn %d running %v with %d members", snap.Turn, snap.Running(), len(snap.Members))
	}
	j.Observe(Report{Session: "s2", At: at(1), Busy: true, Prompt: "fix the bug", Agents: []Agent{bg}})
	fresh := Agent{ID: "y1", Goal: "reproduce the bug", Status: "running"}
	j.Observe(Report{Session: "s2", At: at(2), Busy: true, Prompt: "fix the bug", Agents: []Agent{bg, fresh}})
	ids := j.Sprints("s2")
	if len(ids) != 2 {
		t.Fatalf("%d turns after a prompt", len(ids))
	}
	first, _, _ := j.View(ids[0], time.Time{})
	second, _, _ := j.View(ids[1], time.Time{})
	if _, ok := first.Member("Ada"); !ok || len(first.Members) != 2 || len(second.Members) != 2 || second.Members[1].ID != "y1" {
		t.Fatalf("agents spread as %v and %v", first.Members, second.Members)
	}
	j.Observe(Report{Session: "s2", At: at(3), Busy: false, Outcome: OutcomeFailed, Failure: "provider refused", Agents: []Agent{bg, {ID: "y1", Goal: "reproduce the bug", Status: "interrupted"}}})
	second, _, _ = j.View(ids[1], time.Time{})
	if second.Err == nil || second.Err.Error() != "provider refused" || !second.Restartable() || second.Running() {
		t.Fatalf("failed turn err %v restartable %v", second.Err, second.Restartable())
	}
	if !slices.Contains(messageLines(second), "failure Kay> ") || !slices.Contains(messageLines(second), "verdict Kay>Ada D1") {
		t.Fatalf("messages %v", messageLines(second))
	}
	first, _, _ = j.View(ids[0], time.Time{})
	if !first.Running() || first.Restartable() {
		t.Fatal("the older turn stopped with its agent still running, or claims a retry")
	}
	j.Observe(Report{Session: "s2", At: at(4), Busy: true, Prompt: "again"})
	j.Observe(Report{Session: "s2", At: at(5), Busy: false})
	ids = j.Sprints("s2")
	third, _, _ := j.View(ids[2], time.Time{})
	if !third.Stopped() || third.Err != nil || third.Stage != StageBrief {
		t.Fatalf("turn ended without a final answer as stopped %v err %v stage %q", third.Stopped(), third.Err, third.Stage)
	}
	j.Guide(ids[2], "Kay", "use the cache", at(6))
	third, _, _ = j.View(ids[2], time.Time{})
	if m := third.Messages[len(third.Messages)-1]; m.Kind != MsgVerdict || m.From != Person || !strings.HasPrefix(m.Text, string(VerdictGuidance)) {
		t.Fatalf("guidance recorded as %+v", m)
	}
	if after := third.MessagesAfter(len(third.Messages) - 1); len(after) != 1 || after[0].Seq != len(third.Messages) {
		t.Fatalf("messages after the second to last are %v", after)
	}
	j.Observe(Report{Session: "s2", At: at(7)})
	first, _, _ = j.View(ids[0], time.Time{})
	ada, _ := first.Member("Ada")
	if first.Running() || ada.Status != statusGone || !slices.Contains(messageLines(first), "verdict Kay>Ada D1") {
		t.Fatalf("agent gone from the roster left running %v status %q", first.Running(), ada.Status)
	}
	if d1, _ := first.Ticket("D1"); d1.Status != TicketBlocked {
		t.Fatalf("ticket of an agent gone from the roster is %q", d1.Status)
	}
}

// Tool names of the harness fall into the classes the platform shows, calls without a name counted as other
func TestTallyClassifiesHarnessTools(t *testing.T) {
	got := tallyOf(map[string]int{"read_file": 2, "search_files": 1, "patch": 1, "write_file": 1, "terminal": 3, "web_search": 1, "browser_exec": 1, "delegate_task": 2, "todo_list": 1, "mcp_github_issue": 1}, 16)
	want := toolTally{reads: 3, edits: 2, shells: 3, fetches: 2, agents: 2, calls: 1, others: 3}
	if got != want {
		t.Fatalf("tally %+v want %+v", got, want)
	}
}
