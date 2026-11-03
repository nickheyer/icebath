package sprint

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Text of a tool result
func resultText(res tool.Result) string { return protocol.Response{Content: res.Content}.Text() }

func TestAddress(t *testing.T) {
	roster := []string{"Director", "Architect", "Reviewer", "Builder 1", "Builder 2"}
	names, person, err := address(roster, "Builder 1", "builder 2")
	if err != nil || person || len(names) != 1 || names[0] != "Builder 2" {
		t.Fatalf("by name %v %v %v", names, person, err)
	}
	if names, _, err = address(roster, "Builder 1", "ARCHITECT"); err != nil || names[0] != "Architect" {
		t.Fatalf("upper case name %v %v", names, err)
	}
	if _, _, err := address(roster, "Builder 1", "BUILDER 1"); err == nil || !strings.Contains(err.Error(), "is you") {
		t.Fatalf("self %v", err)
	}
	_, _, err = address(roster, "Builder 1", "Builder 9")
	if err == nil {
		t.Fatal("stranger taken")
	}
	for _, want := range append(roster, toTeam, Person) {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal of a stranger does not list %q: %v", want, err)
		}
	}
	names, person, err = address(roster, "Builder 1", "Team")
	if err != nil || person || strings.Join(names, ",") != "Director,Architect,Reviewer,Builder 2" {
		t.Fatalf("team %v %v %v", names, person, err)
	}
	if names, person, err = address(roster, "Builder 1", "You"); err != nil || !person || names != nil {
		t.Fatalf("person %v %v %v", names, person, err)
	}
	if _, _, err := address([]string{"Director"}, "Director", toTeam); err == nil {
		t.Fatal("team of one taken")
	}
}

func TestRoster(t *testing.T) {
	s := newSprint("sprint-20", "x", time.Now(), nil)
	s.putMember(&Member{Role: RoleBuilder, Name: "Ada"})
	s.putMember(&Member{Role: RoleDirector, Name: "Director"})
	cfg := Default()
	if got := strings.Join(s.roster(cfg), ","); got != "Ada,Director,Architect,Builder 1,Reviewer" {
		t.Fatalf("roster before the plan %s", got)
	}
	s.setPlan(Plan{Title: "p", Summary: "s"}, []*Ticket{{ID: "T1"}, {ID: "T2"}, {ID: "T3"}, {ID: "T4"}})
	if got := strings.Join(s.roster(cfg), ","); got != "Ada,Director,Architect,Builder 1,Builder 2,Builder 3,Reviewer" {
		t.Fatalf("roster with four tickets %s", got)
	}
	cfg.Team.Parallel = false
	if got := strings.Join(s.roster(cfg), ","); got != "Ada,Director,Architect,Builder 1,Reviewer" {
		t.Fatalf("roster in series %s", got)
	}
}

func TestMessageRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	for args, want := range map[string]string{
		`{"to":"Builder 2","text":"   "}`: "text is required",
		`{"to":" ","text":"hi"}`:          "to is required",
		`{"to":`:                          "unexpected",
	} {
		res, err := messageTool{}.Run(ctx, json.RawMessage(args))
		if err != nil || !res.IsError || !strings.Contains(resultText(res), want) {
			t.Fatalf("%s: %v %q", args, err, resultText(res))
		}
	}
}

// Sprint in its build on the fake app with each member on an open session, registered for its tools
func chatSprint(t *testing.T, app *harness.App, id string, members ...Member) (*Sprint, []*Member) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, k := range Default().Kinds(app.Primary.Profile.Name) {
		if err := app.PutKind(k); err != nil {
			t.Fatal(err)
		}
	}
	s := newSprint(id, "x", time.Now(), app.Announce)
	s.setPlan(Plan{Title: "p", Summary: "s"}, []*Ticket{{ID: "T1", Title: "parser", Description: "d", Files: []string{"parse.go"}, Status: TicketActive}})
	s.enter(StageBuild, "building")
	s.drive(ctx)
	var out []*Member
	for _, want := range members {
		live, err := app.CreateKind(ctx, KindName(want.Role))
		if err != nil {
			t.Fatal(err)
		}
		m := want
		m.Joined = time.Now()
		s.bind(app, &m, live)
		out = append(out, &m)
	}
	register(s)
	unregister(t, s)
	return s, out
}

// Context of a finished run on the member session, as the harness hands it to a tool the member calls
func callFrom(t *testing.T, app *harness.App, m *Member) context.Context {
	t.Helper()
	run, err := app.RunOn(context.Background(), m.Live, "", []protocol.Part{protocol.Text{Text: "stand by"}})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, app, run)
	return harness.WithRun(context.Background(), run.ID)
}

// Waits for a run to end within the deadline
func waitRun(t *testing.T, app *harness.App, run *harness.Run) {
	t.Helper()
	done, ok := app.Control.Done(run.ID)
	if !ok {
		t.Fatalf("no run %s", run.ID)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("run %s did not end in time", run.ID)
	}
}

// Root runs started on the session of a member
func runsOn(app *harness.App, m *Member) []*harness.Run {
	var out []*harness.Run
	for _, r := range app.Control.Runs() {
		if r.Parent == "" && r.Session == m.Live.Session {
			out = append(out, r)
		}
	}
	return out
}

// Texts of the user messages of a session in order
func userTexts(sess *session.Session) []string {
	var out []string
	for _, e := range sess.Entries() {
		if e.Role == protocol.RoleUser {
			out = append(out, protocol.Response{Content: e.Content}.Text())
		}
	}
	return out
}

// Chats of a sprint log
func chats(s *Sprint) []Message {
	var out []Message
	for _, m := range s.Snapshot().Messages {
		if m.Kind == MsgChat {
			out = append(out, m)
		}
	}
	return out
}

// Script that ends a stand by turn and calls the tool a tell line asks for
func scriptTells(extra func(last string, tools map[string]bool) (string, string, bool)) func(string, map[string]bool) (string, string, bool) {
	return func(last string, tools map[string]bool) (string, string, bool) {
		if last == "stand by" {
			return "", "", true
		}
		if to, text, ok := strings.Cut(strings.TrimPrefix(last, "tell "), ": "); ok && strings.HasPrefix(last, "tell ") {
			args, _ := json.Marshal(messageInput{To: to, Text: text})
			return ToolMessage, string(args), true
		}
		if extra != nil {
			return extra(last, tools)
		}
		return "", "", false
	}
}

func TestMessageRefusesWithinASprint(t *testing.T) {
	model := &fakeModel{script: scriptTells(nil)}
	app := fakeApp(t, model)
	s, team := chatSprint(t, app, "sprint-21", Member{Role: RoleBuilder, Name: "Builder 1"}, Member{Role: RoleBuilder, Name: "Builder 2", Index: 1})
	ctx := callFrom(t, app, team[0])
	for to, want := range map[string]string{"Nobody": "Builder 1, Builder 2, Director, Architect, Reviewer", "builder 1": "is you", "Reviewer": "Reviewer has not joined yet"} {
		res, err := messageTool{app}.Run(ctx, json.RawMessage(`{"to":"`+to+`","text":"hello"}`))
		if err != nil || !res.IsError || !strings.Contains(resultText(res), want) {
			t.Fatalf("to %s: %v %q", to, err, resultText(res))
		}
	}
	if n := len(chats(s)); n != 0 {
		t.Fatalf("%d refused messages logged", n)
	}
	if res, _ := (messageTool{app}).Run(context.Background(), json.RawMessage(`{"to":"Builder 2","text":"hello"}`)); !res.IsError {
		t.Fatal("a call from outside the team was taken")
	}
	s.finish(nil)
	res, _ := messageTool{app}.Run(ctx, json.RawMessage(`{"to":"Builder 2","text":"hello"}`))
	if !res.IsError || !strings.Contains(resultText(res), "ended") {
		t.Fatalf("ended sprint %q", resultText(res))
	}
	settle(t, app)
	if n := len(runsOn(app, team[1])); n != 0 {
		t.Fatalf("refused messages started %d runs", n)
	}
}

func TestMessageStartsARunOnAnIdleMember(t *testing.T) {
	model := &fakeModel{script: scriptTells(nil)}
	app := fakeApp(t, model)
	s, team := chatSprint(t, app, "sprint-22", Member{Role: RoleBuilder, Name: "Builder 1", Ticket: "T1"}, Member{Role: RoleBuilder, Name: "Builder 2", Index: 1})
	run, err := app.RunOn(context.Background(), team[0].Live, "", []protocol.Part{protocol.Text{Text: "tell builder 2: the parser takes an io.Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, app, run)
	settle(t, app)
	got := chats(s)
	if len(got) != 1 || got[0].From != "Builder 1" || got[0].To != "Builder 2" || got[0].Ticket != "T1" || got[0].Text != "the parser takes an io.Reader" {
		t.Fatalf("chat log %+v", got)
	}
	runs := runsOn(app, team[1])
	if len(runs) != 1 || runs[0].Err() != nil {
		t.Fatalf("recipient runs %d", len(runs))
	}
	texts := userTexts(team[1].Live.Session)
	if len(texts) == 0 {
		t.Fatal("the recipient session holds no message")
	}
	for _, want := range []string{"Message from Builder 1 (builder, T1 parser):", "the parser takes an io.Reader", ToolMessage + " to Builder 1"} {
		if !strings.Contains(texts[0], want) {
			t.Fatalf("first message of the recipient lacks %q:\n%s", want, texts[0])
		}
	}
	if result := strings.Join(toolResults(team[0].Live.Session), "\n"); !strings.Contains(result, "Sent to Builder 2") || !strings.Contains(result, "next message") {
		t.Fatalf("sender read %q", result)
	}
}

// Texts of the tool results of a session in order
func toolResults(sess *session.Session) []string {
	var out []string
	for _, e := range sess.Entries() {
		for _, p := range e.Content {
			if r, ok := p.(protocol.ToolResult); ok {
				out = append(out, protocol.Response{Content: r.Content}.Text())
			}
		}
	}
	return out
}

func TestMessageSteersARunningMember(t *testing.T) {
	held, release := make(chan struct{}, 1), make(chan struct{})
	model := &fakeModel{script: scriptTells(func(last string, _ map[string]bool) (string, string, bool) {
		if last != "hold on" {
			return "", "", false
		}
		select {
		case held <- struct{}{}:
		default:
		}
		<-release
		return "", "", true
	})}
	app := fakeApp(t, model)
	s, team := chatSprint(t, app, "sprint-23", Member{Role: RoleBuilder, Name: "Builder 1"}, Member{Role: RoleBuilder, Name: "Builder 2", Index: 1})
	busy, err := app.RunOn(context.Background(), team[1].Live, "", []protocol.Part{protocol.Text{Text: "hold on"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the recipient never reached the model")
	}
	sender, err := app.RunOn(context.Background(), team[0].Live, "", []protocol.Part{protocol.Text{Text: "tell Builder 2: T2 exports Parse"}})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, app, sender)
	if runs := runsOn(app, team[1]); len(runs) != 1 || runs[0] != busy || !busy.Running() {
		t.Fatalf("message to a running member started %d runs", len(runs))
	}
	parts := []protocol.Part{protocol.Text{Text: "again"}}
	_, flowErr := (&harness.Flow{App: app}).Start(context.Background(), team[1].Live, "", parts)
	_, chatErr := startOn(context.Background(), app, team[1].Live, parts)
	if !runGoing(flowErr) || !runGoing(chatErr) || runGoing(nil) {
		t.Fatalf("refusals of a busy session not told apart: %v, %v", flowErr, chatErr)
	}
	close(release)
	waitRun(t, app, busy)
	settle(t, app)
	if runs := runsOn(app, team[1]); len(runs) != 1 {
		t.Fatalf("recipient ended with %d runs", len(runs))
	}
	texts := userTexts(team[1].Live.Session)
	if len(texts) != 2 || texts[0] != "hold on" || !strings.HasPrefix(texts[1], chatHead+"Builder 1") || !strings.Contains(texts[1], "T2 exports Parse") {
		t.Fatalf("recipient messages %q", texts)
	}
	if got := chats(s); len(got) != 1 || got[0].To != "Builder 2" {
		t.Fatalf("chat log %+v", got)
	}
}

func TestMessageToThePersonAndTheTeam(t *testing.T) {
	var mu sync.Mutex
	var notices []harness.NoticeMsg
	app := fakeAppSending(t, &fakeModel{script: scriptTells(nil)}, func(msg any) {
		if n, ok := msg.(harness.NoticeMsg); ok {
			mu.Lock()
			notices = append(notices, n)
			mu.Unlock()
		}
	})
	s, team := chatSprint(t, app, "sprint-24", Member{Role: RoleBuilder, Name: "Builder 1"}, Member{Role: RoleBuilder, Name: "Builder 2", Index: 1}, Member{Role: RoleBuilder, Name: "Builder 3", Index: 2})
	ctx := callFrom(t, app, team[0])
	res, err := messageTool{app}.Run(ctx, json.RawMessage(`{"to":"YOU","text":"which storage do you want"}`))
	if err != nil || res.IsError || !strings.Contains(resultText(res), "person") {
		t.Fatalf("to the person %v %q", err, resultText(res))
	}
	mu.Lock()
	found := false
	for _, n := range notices {
		found = found || n == harness.NoticeMsg{Level: harness.Info, Text: "Builder 1: which storage do you want", Flow: s.ID}
	}
	mu.Unlock()
	if !found {
		t.Fatalf("no toast for the person in %+v", notices)
	}
	res, err = messageTool{app}.Run(ctx, json.RawMessage(`{"to":"team","text":"I renamed Parse to ParseAll"}`))
	if err != nil || res.IsError {
		t.Fatalf("to the team %v %q", err, resultText(res))
	}
	for _, want := range []string{"Builder 2", "Builder 3", "Reviewer has not joined yet"} {
		if !strings.Contains(resultText(res), want) {
			t.Fatalf("team result lacks %q: %s", want, resultText(res))
		}
	}
	settle(t, app)
	got := chats(s)
	if len(got) != 2 || got[0].To != Person || got[1].To != toTeam {
		t.Fatalf("chat log %+v", got)
	}
	for _, m := range team[1:] {
		texts := userTexts(m.Live.Session)
		if len(texts) != 1 || !strings.Contains(texts[0], "Builder 1 (builder, build stage) to the team:") || !strings.Contains(texts[0], "ParseAll") {
			t.Fatalf("%s messages %q", m.Name, texts)
		}
	}
	if texts := userTexts(team[0].Live.Session); len(texts) != 1 {
		t.Fatalf("the sender messaged itself %q", texts)
	}
}

// Runs one exchange on the member inside a flow of the app, handing back what the stage received
func exchangeIn(t *testing.T, app *harness.App, s *Sprint, m *Member, text, toolName string) any {
	t.Helper()
	got := make(chan any, 1)
	w := harness.Workflow{Name: "probe", Run: func(ctx context.Context, f *harness.Flow, _ string) error {
		r := &runner{cfg: Default(), f: f, s: s, deadline: time.Now().Add(time.Hour)}
		v, _, err := r.exchange(ctx, m, text, toolName, time.Minute, nil)
		got <- v
		return err
	}}
	f, err := app.Flows.Start(context.Background(), w, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the exchange did not end in time")
	}
	if f.Err() != nil {
		t.Fatalf("exchange failed: %v", f.Err())
	}
	return <-got
}

func TestExchangeGoesOnAfterAChatTookTheTurn(t *testing.T) {
	held, release := make(chan struct{}, 1), make(chan struct{})
	model := &fakeModel{script: scriptTells(func(last string, _ map[string]bool) (string, string, bool) {
		if !strings.Contains(last, "hold on") {
			return "", "", false
		}
		select {
		case held <- struct{}{}:
		default:
		}
		<-release
		return "", "", true
	})}
	app := fakeApp(t, model)
	s, team := chatSprint(t, app, "sprint-25", Member{Role: RoleBuilder, Name: "Builder 1", Ticket: "T1"}, Member{Role: RoleBuilder, Name: "Builder 2", Index: 1})
	go func() {
		<-held
		run, err := app.RunOn(context.Background(), team[1].Live, "", []protocol.Part{protocol.Text{Text: "tell Builder 1: which name does the parser export"}})
		if err == nil {
			waitRun(t, app, run)
		}
		close(release)
	}()
	v := exchangeIn(t, app, s, team[0], "# Your ticket T1 parser\nhold on", ToolHandoff)
	if h, ok := v.(Handoff); !ok || h.Ticket != "T1" {
		t.Fatalf("stage received %#v", v)
	}
	settle(t, app)
	texts := strings.Join(userTexts(team[0].Live.Session), "\n---\n")
	if !strings.Contains(texts, chatHead+"Builder 2") || !strings.Contains(texts, resume(ToolHandoff)) {
		t.Fatalf("builder messages lack the chat or the resume:\n%s", texts)
	}
	if strings.Contains(texts, reminder(ToolHandoff)) {
		t.Fatalf("a chat that took the turn cost a reminder:\n%s", texts)
	}
}

func TestExchangeWaitsForTheReplyItAskedFor(t *testing.T) {
	model := &fakeModel{script: scriptTells(func(last string, tools map[string]bool) (string, string, bool) {
		switch {
		case strings.HasPrefix(last, "# Handoff on T1"):
			return ToolMessage, `{"to":"Builder 1","text":"which file holds the parser"}`, true
		case strings.HasPrefix(last, chatHead+"Reviewer"):
			time.Sleep(300 * time.Millisecond)
			return ToolMessage, `{"to":"Reviewer","text":"parse.go holds it"}`, true
		case strings.HasPrefix(last, chatHead+"Builder 1") && tools[ToolVerdict]:
			return ToolVerdict, `{"ticket":"T1","verdict":"guidance","notes":"keep it in parse.go"}`, true
		}
		return "", "", false
	})}
	app := fakeApp(t, model)
	s, team := chatSprint(t, app, "sprint-26", Member{Role: RoleReviewer, Name: "Reviewer", Ticket: "T1"}, Member{Role: RoleBuilder, Name: "Builder 1"})
	v := exchangeIn(t, app, s, team[0], "# Handoff on T1 parser from Builder 1\nStatus: question", ToolVerdict)
	if d, ok := v.(Decision); !ok || d.Ticket != "T1" || d.Verdict != VerdictGuidance {
		t.Fatalf("stage received %#v", v)
	}
	settle(t, app)
	if runs := runsOn(app, team[0]); len(runs) != 2 {
		t.Fatalf("reviewer ran %d times, want its stage run and the run the reply started", len(runs))
	}
	texts := strings.Join(userTexts(team[0].Live.Session), "\n---\n")
	if strings.Contains(texts, reminder(ToolVerdict)) || strings.Contains(texts, resume(ToolVerdict)) {
		t.Fatalf("the reviewer was prodded while it waited for the reply:\n%s", texts)
	}
	if got := chats(s); len(got) != 2 || got[0].From != "Reviewer" || got[1].From != "Builder 1" {
		t.Fatalf("chat log %+v", got)
	}
}

func TestSessionMarks(t *testing.T) {
	sess := session.New()
	if lastSeq(sess) != -1 || briefed(sess) {
		t.Fatal("empty session has a mark or a brief")
	}
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "# Your ticket T1"}}})
	mark := lastSeq(sess)
	sess.AddResponse("b", protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c1", Name: ToolMessage, Args: json.RawMessage(`{"to":"Reviewer","text":"q"}`)}}})
	sess.AddResults("b", []protocol.ToolResult{{CallID: "c1", Name: ToolMessage, Content: []protocol.Part{protocol.Text{Text: "Sent"}}}})
	if !asking(sess, mark) || chatted(sess, mark) || carried(sess, mark, "# Your ticket T1") || !carried(sess, -1, "Your ticket") {
		t.Fatal("marks after a question to the reviewer")
	}
	sess.AddResponse("b", protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c2", Name: ToolMessage, Args: json.RawMessage(`{"to":"you","text":"q"}`)}}})
	sess.AddResults("b", []protocol.ToolResult{{CallID: "c2", Name: ToolMessage, Content: []protocol.Part{protocol.Text{Text: "Shown"}}}})
	if !asking(sess, mark) {
		t.Fatal("a later message to the person dropped the open question")
	}
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: chatText(Member{Name: "Reviewer", Role: RoleReviewer}, "T1 parser", false, "answer")}}})
	if asking(sess, mark) || !chatted(sess, mark) || !carried(sess, mark, "answer") {
		t.Fatal("marks after the reply")
	}
	failed := session.New()
	failed.AddResponse("b", protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c3", Name: ToolMessage, Args: json.RawMessage(`{"to":"Nobody","text":"q"}`)}}})
	failed.AddResults("b", []protocol.ToolResult{{CallID: "c3", Name: ToolMessage, IsError: true}})
	if asking(failed, -1) {
		t.Fatal("a refused message waits for a reply")
	}
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: briefText(Brief{Objective: "ship it"})}}})
	if !briefed(sess) {
		t.Fatal("brief not seen")
	}
}

func TestChatPrompts(t *testing.T) {
	b := Brief{Objective: "ship it"}
	p := Plan{Title: "Ship", Summary: "all of it"}
	tickets := []Ticket{{ID: "T1", Title: "one", Description: "do one", Files: []string{"a.go", "a_test.go"}, Status: TicketActive, Builder: "Ada"}, {ID: "T2", Title: "two", Description: "do two", Files: []string{"b.go"}, Status: TicketActive, Builder: "Linus"}}
	members := []Member{{Role: RoleDirector, Name: "Fixer"}, {Role: RoleArchitect, Name: "Navigator"}, {Role: RoleReviewer, Name: "Overwatch", Ticket: "T1"}, {Role: RoleBuilder, Name: "Ada", Ticket: "T1"}, {Role: RoleBuilder, Name: "Linus", Ticket: "T2"}, {Role: RoleBuilder, Name: "Grace"}}
	for name, text := range map[string]string{"opening": builderOpening(b, p, tickets, tickets[1], members...), "next": builderNext(tickets[1], tickets, members...)} {
		for _, want := range []string{"## Team, each reached with " + ToolMessage, "Fixer, director, settled the brief", "Navigator, architect, cut the plan", "Overwatch, reviewer, judges every handoff, reviewing T1", "Ada, builder, on T1 one, owns a.go, a_test.go", "Linus, builder, that is you, on T2 two, owns b.go", "Grace, builder, between tickets", "# Your ticket T2 two"} {
			if !strings.Contains(text, want) {
				t.Fatalf("builder %s lacks %q:\n%s", name, want, text)
			}
		}
		if strings.Contains(text, "reviewing T1, owns") {
			t.Fatalf("builder %s gives the reviewer the files of a ticket:\n%s", name, text)
		}
	}
	if strings.Contains(builderOpening(b, p, tickets, tickets[1]), "## Team") {
		t.Fatal("team heading without members")
	}
	for _, role := range Roles {
		text := roleSystem(role, Default().Team)
		for _, want := range []string{ToolMessage + " sends a short message to a member by name, to team for every other member, or to you for the person watching", "settle an interface", "ask the architect about the plan", "exact change", "what you exported", "warn the reviewer", "keep working meanwhile", "never ends your stage"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s prompt lacks %q:\n%s", role, want, text)
			}
		}
	}
	for role, want := range map[Role]string{
		RoleDirector:  "only " + ToolBrief + " does",
		RoleArchitect: "Answer each question with " + ToolMessage + " to whoever asked, from the plan and the code, and change no files",
		RoleBuilder:   "ask its builder for the exact change with " + ToolMessage,
		RoleReviewer:  "answer quickly with guidance. When the handoff leaves the problem unclear, ask the builder for specifics with " + ToolMessage + " before deciding",
	} {
		if text := roleSystem(role, Default().Team); !strings.Contains(text, want) {
			t.Fatalf("%s prompt lacks %q:\n%s", role, want, text)
		}
	}
	chat := chatText(Member{Name: "Ada", Role: RoleBuilder}, "T1 one", false, "Parse now takes a Reader")
	if !strings.HasPrefix(chat, "Message from Ada (builder, T1 one):\n\nParse now takes a Reader") || !strings.Contains(chat, "Answer with "+ToolMessage+" to Ada when it asks something, no answer is needed otherwise") {
		t.Fatalf("chat text:\n%s", chat)
	}
	if sent := chatSent([]string{"Linus"}, []string{"Grace has not joined yet"}); !strings.Contains(sent, "Sent to Linus") || !strings.Contains(sent, "Not delivered: Grace has not joined yet") {
		t.Fatalf("sent text %s", sent)
	}
}
