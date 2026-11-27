package app

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/sessions/export"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const sessionsContext = "agent.sessions"

// Sessions each list of the screen reads, at most
const listed = 200

var sessionsKeys = []keys.Action{
	{ID: "agent.sessions.beside", Context: sessionsContext, Label: "open beside", Doc: "Open the session in a new chat beside this one", Default: []string{"alt+b"}},
	{ID: "agent.sessions.space", Context: sessionsContext, Label: "new space", Doc: "Open the session in a new space", Default: []string{"alt+c"}},
	{ID: "agent.sessions.pin", Context: sessionsContext, Label: "pin", Doc: "Pin the session, or unpin it", Default: []string{"alt+p"}},
	{ID: "agent.sessions.archive", Context: sessionsContext, Label: "archive", Doc: "Archive the session, or bring it back", Default: []string{"alt+a"}},
	{ID: "agent.sessions.archived", Context: sessionsContext, Label: "show archived", Doc: "Show archived sessions too, or hide them", Default: []string{"alt+v"}},
	{ID: "agent.sessions.export", Context: sessionsContext, Label: "save to file", Doc: "Save the session to a file", Default: []string{"alt+e"}},
	{ID: "agent.sessions.prune", Context: sessionsContext, Label: "delete old", Doc: "Delete sessions you have not used for a while", Default: []string{"alt+x"}},
	{ID: "agent.sessions.new", Context: sessionsContext, Label: "new chat", Doc: "Open a new chat in place of the sessions", Default: []string{"alt+n"}},
	{ID: "agent.sessions.more", Context: sessionsContext, Label: "more", Doc: "Show everything you can do with the session and the list", Default: []string{"ctrl+o"}},
}

// Session the screen lists, with what it is doing when the server holds it open
type listing struct {
	session *sessionsv1.Session
	live    *agentv1.LiveSession
}

// Screen of every session, open on the server or stored
type browser struct {
	desk *desk
	// Chat the screen was opened from, which opening a session shows it in
	from *chat
	hub  *cards.Hub
	list *cards.List
	rows []listing
	// Archived sessions are listed too
	archived bool
	loaded   bool
	err      error
}

// What a session is called in a note or a question: its title, else the start of its id
func (b *browser) name(row listing) string {
	return label(row.session.GetId(), row.session.GetTitle())
}

// What a session is called in its row: its title, and for one that has none yet the word for that, as the start of an id names nothing a
// person knows. Its first message stands beside it and its id under the list
func (b *browser) titled(row listing) string {
	if title := row.session.GetTitle(); title != "" {
		return title
	}
	return render.Dim("Untitled", b.desk.kit.Env.Theme)
}

// Columns of the list: the name, what the session is doing, the first message, how many messages and when it was last used
var sessionColumns = []render.Column{{Title: "Session"}, {}, {Title: "First message", Flex: 1, Whole: true}, {Title: "Messages", Right: true},
	{Title: "Last used", Right: true}}

func (b *browser) open(filter string) tea.Cmd {
	kit := b.desk.kit
	t := kit.Env.Theme
	// The screen's watch of the store and its look at the open sessions end as it closes
	ctx, stop := context.WithCancel(kit.Env.Ctx)
	b.list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Sessions",
		Glyph:   t.Glyphs.Sessions,
		Key:     "sessions",
		Context: cards.HubContext,
		Shared:  []string{sessionsContext},
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search sessions",
		Columns: sessionColumns,
		Yield:   []int{3, 4},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, row := range b.rows {
				cells := []string{b.titled(row), render.Dim(b.state(row), t), render.Dim(strings.Join(strings.Fields(row.session.GetPreview()), " "), t), "",
					render.Dim(when(row.session), t)}
				if n := row.session.GetMessages(); n > 0 {
					cells[3] = render.Dim(strconv.FormatInt(n, 10), t)
				}
				if row.session.GetArchived() {
					cells[0] = render.Dim(cells[0], t)
				}
				rows[i] = cells
			}
			return rows
		},
		Words: func(i int) string {
			return b.rows[i].session.GetId() + " " + b.rows[i].session.GetDirectory() + " " + b.rows[i].session.GetModel()
		},
		Detail: func(i int) []string { return b.detail(b.rows[i]) },
		Status: func() string {
			if b.archived {
				return "Archived sessions shown"
			}
			return ""
		},
		Empty: func() string {
			switch {
			case b.err != nil:
				return "Your sessions could not be read. " + tui.Reason(b.err) + " " + kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !b.loaded:
				return "Reading your sessions…"
			}
			return "No sessions yet. " + kit.Env.Keys.Display("agent.sessions.new") + " opens a new chat."
		},
		Free: map[string]bool{"hub.refresh": true, "agent.sessions.new": true, "agent.sessions.archived": true, "agent.sessions.prune": true,
			"agent.sessions.more": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "agent.sessions.new"}, b.archivedHint(), {Action: "agent.sessions.prune"}, {Action: "agent.sessions.more"}}
			}
			return b.hints(b.rows[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":                 func(_ *cards.List, i int) tea.Cmd { return b.use(b.rows[i].session) },
			"agent.sessions.new":      func(*cards.List, int) tea.Cmd { return b.fresh() },
			"hub.rename":              func(_ *cards.List, i int) tea.Cmd { return b.rename(b.rows[i]) },
			"hub.edit":                func(_ *cards.List, i int) tea.Cmd { return b.rename(b.rows[i]) },
			"hub.remove":              func(_ *cards.List, i int) tea.Cmd { return b.remove(b.rows[i]) },
			"hub.refresh":             func(*cards.List, int) tea.Cmd { return b.load() },
			"agent.sessions.beside":   func(_ *cards.List, i int) tea.Cmd { return b.beside(b.rows[i].session) },
			"agent.sessions.space":    func(_ *cards.List, i int) tea.Cmd { return b.space(b.rows[i].session) },
			"agent.sessions.pin":      func(_ *cards.List, i int) tea.Cmd { return b.pin(b.rows[i]) },
			"agent.sessions.archive":  func(_ *cards.List, i int) tea.Cmd { return b.archive(b.rows[i]) },
			"agent.sessions.archived": func(*cards.List, int) tea.Cmd { return b.showArchived() },
			"agent.sessions.export":   func(_ *cards.List, i int) tea.Cmd { return b.export(b.rows[i]) },
			"agent.sessions.prune":    func(*cards.List, int) tea.Cmd { return b.prune() },
			"agent.sessions.more": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(b.rows) {
					return b.more(nil)
				}
				return b.more(&b.rows[i])
			},
		},
	})
	b.list.ToggleDetail()
	if filter != "" {
		b.list.Filter(filter)
	}
	b.hub = kit.Hub("sessions", b.list)
	b.hub.OnClose(func() tea.Cmd {
		stop()
		return nil
	})
	return tea.Batch(kit.WM.ShowOverlay(b.hub), b.load(), b.watch(ctx), b.tick(ctx))
}

// Button that shows the archived sessions too, or hides them again
func (b *browser) archivedHint() wm.Hint {
	if b.archived {
		return wm.Hint{Action: "agent.sessions.archived", Label: "hide archived"}
	}
	return wm.Hint{Action: "agent.sessions.archived"}
}

// Buttons for a session, most used first: open it, open it beside this chat, rename it, close or delete it, and everything else behind more.
// A session a chat shows is neither opened beside nor closed nor deleted from here, so pinning takes a place of those
func (b *browser) hints(row listing) []wm.Hint {
	shown := b.desk.shows(row.session.GetId()) != nil
	hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
	if shown {
		hints[0].Label = "go to chat"
	} else {
		hints = append(hints, wm.Hint{Action: "agent.sessions.beside"})
	}
	hints = append(hints, wm.Hint{Action: "hub.rename"})
	switch {
	case shown:
		hints = append(hints, b.pinHint(row))
	case row.live != nil:
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "close"})
	default:
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "delete"})
	}
	return append(hints, wm.Hint{Action: "agent.sessions.more"})
}

// Button that pins the session, or unpins it
func (b *browser) pinHint(row listing) wm.Hint {
	if row.session.GetPinned() {
		return wm.Hint{Action: "agent.sessions.pin", Label: "unpin"}
	}
	return wm.Hint{Action: "agent.sessions.pin"}
}

// Everything the screen does, with the highlighted session and with the list, in a view of its own, each row with the key that does it
// straight from the list
func (b *browser) more(row *listing) tea.Cmd {
	var deeds []deed
	title := "Sessions"
	if row != nil {
		r := *row
		session := r.session
		shown := b.desk.shows(session.GetId()) != nil
		title = b.name(r)
		const own = "This session"
		open := deed{"Open in this chat", own, "hub.use", "open", func() tea.Cmd { return b.use(session) }}
		if shown {
			open.title, open.verb = "Go to the chat that shows it", "go to chat"
		}
		deeds = append(deeds, open)
		if !shown {
			deeds = append(deeds,
				deed{"Open in a new chat beside this one", own, "agent.sessions.beside", "open", func() tea.Cmd { return b.beside(session) }},
				deed{"Open in a new space", own, "agent.sessions.space", "open", func() tea.Cmd { return b.space(session) }})
		}
		deeds = append(deeds, deed{"Rename", own, "hub.rename", "rename", func() tea.Cmd { return b.rename(r) }})
		pin := deed{"Pin to the top of the list", own, "agent.sessions.pin", "pin", func() tea.Cmd { return b.pin(r) }}
		if session.GetPinned() {
			pin.title, pin.verb = "Unpin", "unpin"
		}
		archive := deed{"Archive", own, "agent.sessions.archive", "archive", func() tea.Cmd { return b.archive(r) }}
		if session.GetArchived() {
			archive.title, archive.verb = "Bring back from the archive", "bring back"
		}
		deeds = append(deeds, pin, archive, deed{"Save to a file", own, "agent.sessions.export", "save", func() tea.Cmd { return b.export(r) }})
		switch {
		case shown:
		case r.live != nil:
			deeds = append(deeds, deed{"Close, keeping it in your sessions", own, "hub.remove", "close", func() tea.Cmd { return b.remove(r) }})
		default:
			deeds = append(deeds, deed{"Delete", own, "hub.remove", "delete", func() tea.Cmd { return b.remove(r) }})
		}
	}
	const all = "All sessions"
	shows := deed{"Show archived sessions too", all, "agent.sessions.archived", "show", b.showArchived}
	if b.archived {
		shows.title, shows.verb = "Hide archived sessions", "hide"
	}
	deeds = append(deeds, deed{"Open a new chat", all, "agent.sessions.new", "open", b.fresh}, shows,
		deed{"Delete sessions you have not used for a while", all, "agent.sessions.prune", "choose", b.prune},
		deed{"Read the list again", all, "hub.refresh", "read again", b.load})
	return showDeeds(b.desk.kit, b.hub, title, "Everything you can do here. The key beside each one does it straight from the list.", deeds)
}

// Opens a new chat in place of the screen
func (b *browser) fresh() tea.Cmd { return b.leave(b.desk.kit.WM.OpenHere(b.desk.start(nil, ""))) }

// Lists the archived sessions too, or hides them again
func (b *browser) showArchived() tea.Cmd {
	b.archived = !b.archived
	return b.load()
}

// Shows the session the id names in the window the screen was opened from, else lists the sessions the words match
func (b *browser) resume(id string) tea.Cmd {
	kit := b.desk.kit
	return tui.Async(func() func() tea.Cmd {
		response, err := b.desk.sessions.Resolve(kit.Env.Ctx, &sessionsv1.ResolveRequest{Ref: id, Tip: true})
		return func() tea.Cmd {
			if err != nil {
				return b.open(id)
			}
			return tea.Batch(b.from.switchTo(response.GetSession()), kit.WM.Focus(b.from))
		}
	})
}

// What a session is doing in a few words: shown in the chat the screen came from or in another, waiting for an answer, working, or kept
// open by the server; then whether it is pinned or archived. Nothing for one that is only stored
func (b *browser) state(row listing) string {
	var words []string
	switch shown := b.desk.shows(row.session.GetId()); {
	case shown != nil && shown == b.from:
		words = append(words, "in this chat")
	case shown != nil:
		words = append(words, "in another chat")
	case row.live != nil && row.live.GetWaiting():
		words = append(words, "waiting for you")
	case row.live != nil && row.live.GetBusy():
		words = append(words, "working")
	case row.live != nil:
		words = append(words, "open")
	}
	if row.session.GetPinned() {
		words = append(words, "pinned")
	}
	if row.session.GetArchived() {
		words = append(words, "archived")
	}
	return strings.Join(words, ", ")
}

// When a session was last active: the time today, else the day
func when(s *sessionsv1.Session) string {
	at := s.GetUpdated()
	if at == nil {
		at = s.GetCreated()
	}
	if at == nil {
		return ""
	}
	return sent(at.AsTime())
}

// Where a session was started, in words: this app, the command line, a scheduled job, a subagent or the chat app of that name
func origin(source string) string {
	switch source {
	case "":
		return ""
	case "tui":
		return "this app"
	case "cli":
		return "the command line"
	case "cron":
		return "a scheduled job"
	case "subagent":
		return "a subagent"
	}
	return strings.ToUpper(source[:1]) + source[1:]
}

// Lines under the list about the highlighted session: how it started, what it is doing, then its model, profile, origin and folder, its
// size, when it was used and what it cost, and last the id that /resume takes
func (b *browser) detail(row listing) []string {
	t := b.desk.kit.Env.Theme
	s := row.session
	var lines []string
	if preview := strings.Join(strings.Fields(s.GetPreview()), " "); preview != "" {
		lines = append(lines, "Started with: "+preview)
	}
	state := "Kept in your sessions."
	switch shown := b.desk.shows(s.GetId()); {
	case shown != nil && shown == b.from:
		state = "Open in the chat you came from."
	case shown != nil:
		state = "Open in another chat of this window."
	case row.live != nil && row.live.GetWaiting():
		state = "Open in the background and waiting for your answer."
	case row.live != nil && row.live.GetBusy():
		state = "Open in the background and working on a reply."
	case row.live != nil:
		state = "Open in the background. " + b.desk.kit.Env.Keys.Display("hub.remove") + " closes it and keeps it in your sessions."
	}
	if s.GetPinned() {
		state += " Pinned to the top of the list."
	}
	switch {
	case s.GetAutoArchived():
		state += " Archived because it was not used for a while."
	case s.GetArchived():
		state += " Archived."
	}
	lines = append(lines, state)
	var facts []string
	if model := strings.TrimPrefix(s.GetProvider()+"/"+s.GetModel(), "/"); s.GetModel() != "" {
		facts = append(facts, "Model: "+model)
	}
	if s.GetProfile() != "" {
		facts = append(facts, "Profile: "+s.GetProfile())
	}
	if from := origin(s.GetSource()); from != "" {
		facts = append(facts, "Started from "+from)
	}
	if s.GetDirectory() != "" {
		facts = append(facts, "Folder: "+tilde(s.GetDirectory()))
	}
	if s.GetGitBranch() != "" {
		facts = append(facts, "Git branch: "+s.GetGitBranch())
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	size := []string{render.Count(int(s.GetMessages()), "message")}
	if n := s.GetToolCalls(); n > 0 {
		size = append(size, render.Count(int(n), "tool call"))
	}
	if at := activity(s); at != nil {
		size = append(size, "Last used "+at.AsTime().Local().Format("Jan 2 15:04"))
	}
	if cost := cmp.Or(s.GetUsage().GetActualCost(), s.GetUsage().GetEstimatedCost()); cost > 0 {
		size = append(size, "Cost "+money(cost))
	}
	if s.GetYolo() {
		size = append(size, "Approvals skipped")
	}
	lines = append(lines, render.Dim(strings.Join(size, " · "), t), render.Dim("Id: "+s.GetId()+", which /resume takes", t))
	return lines
}

// Path with the home folder written as ~
func tilde(path string) string {
	home, _ := os.UserHomeDir()
	if rest, inside := strings.CutPrefix(path, home+string(filepath.Separator)); inside && home != "" {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

func (b *browser) leave(cmd tea.Cmd) tea.Cmd {
	return tea.Batch(b.desk.kit.WM.DismissCard(b.hub), cmd)
}

// Reads the sessions the server holds open and the stored ones, open first then by when they were active, the cursor kept on its session
func (b *browser) load() tea.Cmd {
	kit := b.desk.kit
	archived := b.archived
	// Sessions the windows here show, read on the app's loop rather than while the lists load
	var showing []string
	for _, c := range b.desk.chats {
		if c.id != "" {
			showing = append(showing, c.id)
		}
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := kit.Env.Ctx
		live, liveErr := b.desk.agents.Live(ctx, &agentv1.LiveRequest{})
		stored, err := b.desk.sessions.List(ctx, &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Limit: listed, IncludeArchived: archived}})
		rows := []listing{}
		seen := map[string]bool{}
		open := map[string]*agentv1.LiveSession{}
		for _, session := range live.GetSessions() {
			open[session.GetSessionId()] = session
		}
		for _, session := range stored.GetSessions() {
			seen[session.GetId()] = true
			rows = append(rows, listing{session: session, live: open[session.GetId()]})
		}
		// Open sessions and those a window shows that the stored list left out, empty ones among them
		var missing []string
		for id := range open {
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		for _, id := range showing {
			if !seen[id] && open[id] == nil {
				missing = append(missing, id)
			}
		}
		for _, id := range missing {
			if got, err := b.desk.sessions.Get(ctx, &sessionsv1.GetRequest{Id: id}); err == nil {
				seen[id] = true
				rows = append(rows, listing{session: got.GetSession(), live: open[id]})
			}
		}
		return func() tea.Cmd {
			selected := ""
			if i := b.list.Index(); i >= 0 && i < len(b.rows) {
				selected = b.rows[i].session.GetId()
			} else if !b.loaded && b.from != nil {
				selected = b.from.id
			}
			slices.SortStableFunc(rows, func(x, y listing) int {
				return cmp.Or(compareTrue(!x.session.GetArchived(), !y.session.GetArchived()), compareTrue(x.session.GetPinned(), y.session.GetPinned()),
					compareTrue(x.live != nil, y.live != nil), -compareTime(activity(x.session), activity(y.session)))
			})
			b.rows, b.loaded = rows, true
			switch {
			case liveErr != nil:
				b.err = liveErr
			case err != nil && len(rows) == 0:
				b.err = err
			default:
				b.err = nil
			}
			b.list.Refresh()
			for i, row := range b.rows {
				if row.session.GetId() == selected {
					b.list.Select(i)
				}
			}
			switch {
			case err != nil && len(rows) > 0:
				return b.hub.Fail("Your stored sessions could not be read, so only the open ones are listed. " + tui.Reason(err))
			case liveErr != nil && len(rows) > 0:
				return b.hub.Fail("Which sessions are open could not be read. " + tui.Reason(liveErr))
			}
			return nil
		}
	})
}

// Orders true before false
func compareTrue(x, y bool) int {
	switch {
	case x && !y:
		return -1
	case !x && y:
		return 1
	}
	return 0
}

func activity(s *sessionsv1.Session) *timestamppb.Timestamp {
	if s.GetUpdated() != nil {
		return s.GetUpdated()
	}
	return s.GetCreated()
}

func compareTime(x, y *timestamppb.Timestamp) int {
	return x.AsTime().Compare(y.AsTime())
}

// Reloads whenever the store says its sessions changed, until the screen closes
func (b *browser) watch(ctx context.Context) tea.Cmd {
	kit := b.desk.kit
	return func() tea.Msg {
		stream, err := b.desk.sessions.Watch(ctx, &sessionsv1.WatchRequest{})
		if err != nil {
			return nil
		}
		defer stream.Close()
		for {
			if _, err := stream.Receive(); err != nil {
				return nil
			}
			kit.Apply(func() tea.Cmd {
				if ctx.Err() != nil {
					return nil
				}
				return b.load()
			})
		}
	}
}

// Reloads on a beat, often while a session works, so what the open sessions do stays current
func (b *browser) tick(ctx context.Context) tea.Cmd {
	wait := pollIdle
	if slices.ContainsFunc(b.rows, func(row listing) bool { return row.live.GetBusy() }) {
		wait = pollBusy
	}
	return tui.Async(func() func() tea.Cmd {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		return func() tea.Cmd {
			if ctx.Err() != nil {
				return nil
			}
			return tea.Batch(b.load(), b.tick(ctx))
		}
	})
}

// Shows the session in the window the screen was opened from, or focuses the window that shows it; in a new window when none was
func (b *browser) use(session *sessionsv1.Session) tea.Cmd {
	kit := b.desk.kit
	if shown := b.desk.shows(session.GetId()); shown != nil {
		return b.leave(kit.WM.Focus(shown))
	}
	if b.from != nil && kit.WM.Has(b.from.Key()) {
		return b.leave(tea.Batch(b.from.switchTo(session), kit.WM.Focus(b.from)))
	}
	return b.leave(b.desk.open(session))
}

func (b *browser) beside(session *sessionsv1.Session) tea.Cmd {
	kit := b.desk.kit
	if shown := b.desk.shows(session.GetId()); shown != nil {
		return b.leave(kit.WM.Focus(shown))
	}
	return b.leave(kit.WM.OpenHere(b.desk.start(session, "")))
}

// Opens the session in a new space of its own
func (b *browser) space(session *sessionsv1.Session) tea.Cmd {
	kit := b.desk.kit
	if shown := b.desk.shows(session.GetId()); shown != nil {
		return b.leave(kit.WM.Focus(shown))
	}
	c := b.desk.start(session, "")
	space, cmd := kit.WM.AddWorkspace(c.Key(), c)
	return b.leave(tea.Batch(cmd, kit.WM.Switch(kit.WM.IndexOf(space)), kit.WM.Focus(c)))
}

func (b *browser) rename(row listing) tea.Cmd {
	kit := b.desk.kit
	name := b.name(row)
	session := row.session
	field := cards.Field{Label: "Title", Value: session.GetTitle(), Help: "The name the session is listed by and its chat shows."}
	return kit.Ask(b.hub, "Rename "+name, field, "Rename", func(title string) tea.Cmd {
		if title == "" || title == session.GetTitle() {
			return nil
		}
		b.hub.Busy("Renaming " + name + "…")
		return tui.Call(b.hub, func() (*sessionsv1.RenameResponse, error) {
			return b.desk.sessions.Rename(kit.Env.Ctx, &sessionsv1.RenameRequest{Id: session.GetId(), Title: title})
		}, func(response *sessionsv1.RenameResponse) tea.Cmd {
			stored := cmp.Or(response.GetTitle(), title)
			if c := b.desk.shows(session.GetId()); c != nil {
				c.title = stored
			}
			return tea.Batch(b.hub.Done(name+" is now called "+stored+"."), b.load())
		})
	})
}

func (b *browser) pin(row listing) tea.Cmd {
	kit := b.desk.kit
	pinned := !row.session.GetPinned()
	return tui.Call(b.hub, func() (*sessionsv1.MarkResponse, error) {
		return b.desk.sessions.Mark(kit.Env.Ctx, &sessionsv1.MarkRequest{Id: row.session.GetId(), Pinned: &pinned})
	}, func(*sessionsv1.MarkResponse) tea.Cmd {
		done := b.name(row) + " is now pinned to the top."
		if !pinned {
			done = b.name(row) + " is no longer pinned."
		}
		return tea.Batch(b.hub.Done(done), b.load())
	})
}

func (b *browser) archive(row listing) tea.Cmd {
	kit := b.desk.kit
	archived := !row.session.GetArchived()
	if archived && b.desk.shows(row.session.GetId()) != nil {
		return b.hub.Fail(b.name(row) + " is open in a chat. Close that chat first, then archive the session.")
	}
	return tui.Call(b.hub, func() (*sessionsv1.MarkResponse, error) {
		return b.desk.sessions.Mark(kit.Env.Ctx, &sessionsv1.MarkRequest{Id: row.session.GetId(), Archived: &archived})
	}, func(*sessionsv1.MarkResponse) tea.Cmd {
		done := b.name(row) + " is now archived."
		if !b.archived {
			done += " " + kit.Env.Keys.Display("agent.sessions.archived") + " shows archived sessions."
		}
		if !archived {
			done = b.name(row) + " is back from the archive."
		}
		return tea.Batch(b.hub.Done(done), b.load())
	})
}

// Closes a session the server holds open, which stays stored, or deletes a stored one, after a yes; never one a chat shows
func (b *browser) remove(row listing) tea.Cmd {
	kit := b.desk.kit
	name, id := b.name(row), row.session.GetId()
	if b.desk.shows(id) != nil {
		return b.hub.Fail(name + " is open in a chat. Close that chat first.")
	}
	if row.live != nil {
		b.list.Confirm("Close "+name+"? It stops running in the background and stays in your sessions.", func() tea.Cmd {
			b.hub.Busy("Closing " + name + "…")
			return tui.Call(b.hub, func() (*agentv1.ShutResponse, error) {
				return b.desk.agents.Shut(kit.Env.Ctx, &agentv1.ShutRequest{SessionId: id})
			}, func(*agentv1.ShutResponse) tea.Cmd { return tea.Batch(b.hub.Done(name+" is now closed."), b.load()) })
		})
		return nil
	}
	b.list.Confirm("Delete "+name+"? Its messages are gone for good.", func() tea.Cmd {
		b.hub.Busy("Deleting " + name + "…")
		return tui.Call(b.hub, func() (*sessionsv1.DeleteResponse, error) {
			return b.desk.sessions.Delete(kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: id})
		}, func(*sessionsv1.DeleteResponse) tea.Cmd { return tea.Batch(b.hub.Done("Deleted "+name+"."), b.load()) })
	})
	return nil
}

// Form that saves the session to a file, by the rules /save follows
func (b *browser) export(row listing) tea.Cmd {
	kit := b.desk.kit
	name, id := b.name(row), row.session.GetId()
	folder := "the saved sessions folder"
	if path, err := export.Folder(savedFolder); err == nil {
		folder = tilde(path)
	}
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title: "Save to a file",
		Intro: []string{"Writes " + name + " to a file you can keep or share."},
		Fields: []cards.Field{
			{ID: "format", Label: "Format", Kind: cards.FieldChoice, Value: "md", Choices: []cards.Choice{
				{Value: "md", Label: "Markdown", Meaning: "The messages as text you can read"},
				{Value: "json", Label: "JSON", Meaning: "Every message with all its details, for other programs"},
				{Value: "html", Label: "HTML", Meaning: "One page to open in a browser"},
			}},
			{ID: "file", Label: "File", Placeholder: "Named by icebath",
				Help: "A name alone goes in the folder of the session.\nLeave empty and icebath names the file and puts it in " + folder + "."},
			{ID: "redact", Label: "Hide secrets", Kind: cards.FieldBool, Value: "true", Help: "Replaces API keys and other secrets in the file with a mark."},
		},
		Submit: "Save",
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			b.hub.Busy("Saving " + name + " to a file…")
			return tui.Call(b.hub, func() (string, error) {
				return b.desk.save(kit.Env.Ctx, id, values["format"], values["file"], values["redact"] == "true")
			}, func(path string) tea.Cmd {
				return tea.Batch(b.hub.Close(form), b.hub.Done("Saved "+name+" to "+tilde(path)+"."))
			})
		},
	})
	return b.hub.Push(form)
}

// Form that deletes stored sessions not used for a number of days, after a yes; sessions a chat or the server holds open are kept
func (b *browser) prune() tea.Cmd {
	kit := b.desk.kit
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title: "Delete old sessions",
		Intro: []string{"Deletes the sessions you have not used for a while. Sessions that are open are kept."},
		Fields: []cards.Field{
			{ID: "days", Label: "Not used for", Kind: cards.FieldNumber, Value: "30", Required: true,
				Help: "A number of days. Sessions nobody used for this long are deleted.", Check: func(value string) string {
					if n, err := strconv.Atoi(strings.TrimSpace(value)); err != nil || n < 1 {
						return "Enter a whole number of days, such as 30."
					}
					return ""
				}},
			{ID: "archived", Label: "Archived only", Kind: cards.FieldBool, Value: "false", Help: "Delete only sessions you archived."},
			{ID: "pinned", Label: "Pinned too", Kind: cards.FieldBool, Value: "false", Help: "Delete pinned sessions as well. They are kept otherwise."},
		},
		Submit: "Delete",
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			days, _ := strconv.Atoi(strings.TrimSpace(values["days"]))
			archived, pinned := values["archived"] == "true", values["pinned"] == "true"
			// Sessions a chat or the server holds open are not ended, which pruning never deletes
			filter := &sessionsv1.Filter{ActiveBefore: timestamppb.New(time.Now().Add(-time.Duration(days) * 24 * time.Hour)),
				IncludePinned: pinned, OnlyArchived: archived, IncludeArchived: true}
			which := "session"
			if archived {
				which = "archived session"
			}
			if pinned {
				which += ", pinned ones too,"
			}
			question := "Delete every " + which + " not used for " + render.Count(days, "day") + "? They are gone for good."
			return tea.Batch(b.hub.Close(form), tui.Async(func() func() tea.Cmd {
				return func() tea.Cmd {
					b.list.Confirm(question, func() tea.Cmd {
						b.hub.Busy("Deleting old sessions…")
						return tui.Call(b.hub, func() (*sessionsv1.PruneResponse, error) {
							return b.desk.sessions.Prune(kit.Env.Ctx, &sessionsv1.PruneRequest{Filter: filter})
						}, func(response *sessionsv1.PruneResponse) tea.Cmd {
							if response.GetDeleted() == 0 {
								return tea.Batch(b.hub.Done("No session was that old, so nothing was deleted."), b.load())
							}
							return tea.Batch(b.hub.Done("Deleted "+render.Count(int(response.GetDeleted()), "session")+"."), b.load())
						})
					})
					return nil
				}
			}))
		},
	})
	return b.hub.Push(form)
}

// One thing a screen does, as a row of the view that lists all it does: its words, the heading it is listed under, the keymap action whose
// key does it straight from the screen, the word on its button, and what it does
type deed struct {
	title, group, action, verb string
	run                        func() tea.Cmd
}

// View over the hub of everything a screen does, under headings, each row with the key that does it straight from the screen; enter does
// the highlighted one, the view closing first
func showDeeds(kit *tui.Kit, hub *cards.Hub, title, intro string, deeds []deed) tea.Cmd {
	t := kit.Env.Theme
	var view *cards.List
	view = cards.NewList(kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Palette,
		Key:     hub.Key() + ":more",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(deeds) > 8,
		Search:  "Search what you can do",
		Intro:   func() []string { return []string{intro} },
		Columns: []render.Column{{}, {Right: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(deeds))
			for i, d := range deeds {
				rows[i] = []string{d.title, render.Dim(kit.Env.Keys.Display(d.action), t)}
			}
			return rows
		},
		Group: func(i int) string { return deeds[i].group },
		Empty: func() string { return "Nothing to do here." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: deeds[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return tea.Batch(hub.Close(view), deeds[i].run()) },
		},
	})
	return hub.Push(view)
}
