package app

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	{ID: "agent.sessions.beside", Context: sessionsContext, Label: "beside", Doc: "open the session in a new window beside this one", Default: []string{"alt+b"}},
	{ID: "agent.sessions.space", Context: sessionsContext, Label: "new space", Doc: "open the session in a new space", Default: []string{"alt+c"}},
	{ID: "agent.sessions.pin", Context: sessionsContext, Label: "pin", Doc: "pin the session, or unpin it", Default: []string{"alt+p"}},
	{ID: "agent.sessions.archive", Context: sessionsContext, Label: "archive", Doc: "archive the session, or bring it back from the archive", Default: []string{"alt+a"}},
	{ID: "agent.sessions.archived", Context: sessionsContext, Label: "archived", Doc: "show archived sessions too, or hide them", Default: []string{"alt+v"}},
	{ID: "agent.sessions.export", Context: sessionsContext, Label: "export", Doc: "save the session to a file", Default: []string{"alt+e"}},
	{ID: "agent.sessions.prune", Context: sessionsContext, Label: "prune", Doc: "delete stored sessions untouched for a while", Default: []string{"alt+x"}},
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

func (b *browser) name(row listing) string {
	return label(row.session.GetId(), row.session.GetTitle())
}

func (b *browser) open(filter string) tea.Cmd {
	kit := b.desk.kit
	glyphs := kit.Env.Theme.Glyphs
	// The screen's watch of the store and its look at the open sessions end as it closes
	ctx, stop := context.WithCancel(kit.Env.Ctx)
	b.list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Sessions",
		Glyph:   glyphs.Sessions,
		Key:     "sessions",
		Context: cards.HubContext,
		Shared:  []string{sessionsContext},
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search sessions",
		Columns: []render.Column{{}, {Title: "Session", Max: 36}, {Title: "Started with", Flex: 1}, {Title: "Msgs", Right: true}, {Title: "When", Right: true}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, row := range b.rows {
				cells := []string{b.mark(row), b.name(row), strings.Join(strings.Fields(row.session.GetPreview()), " "), "", when(row.session)}
				if n := row.session.GetMessages(); n > 0 {
					cells[3] = strconv.FormatInt(n, 10)
				}
				if row.session.GetArchived() {
					for j := range cells {
						cells[j] = render.Dim(cells[j], kit.Env.Theme)
					}
				}
				rows[i] = cells
			}
			return rows
		},
		Detail: func(i int) []string { return b.detail(b.rows[i]) },
		Status: func() string {
			if b.archived {
				return "showing archived sessions too"
			}
			return fmt.Sprintf("%s here  %s open  %s working  %s running  %s pinned", glyphs.Live, glyphs.Marked, glyphs.Active, glyphs.Hollow, glyphs.Flag)
		},
		Empty: func() string {
			return tui.Waiting(b.loaded, b.err, "No sessions yet. "+kit.WM.Keys().Display("hub.add")+" starts one beside this window.")
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true, "agent.sessions.archived": true, "agent.sessions.prune": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "new"}, {Action: "agent.sessions.archived"}, {Action: "agent.sessions.prune"}}
			}
			remove := wm.Hint{Action: "hub.remove", Label: "delete"}
			if b.rows[i].live != nil {
				remove.Label = "close"
			}
			pin := wm.Hint{Action: "agent.sessions.pin"}
			if b.rows[i].session.GetPinned() {
				pin.Label = "unpin"
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "agent.sessions.beside"}, remove, {Action: "hub.rename"}, pin,
				{Action: "agent.sessions.archive"}, {Action: "agent.sessions.export"}, {Action: "hub.add", Label: "new"}, {Action: "agent.sessions.archived"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":                 func(_ *cards.List, i int) tea.Cmd { return b.use(b.rows[i].session) },
			"hub.add":                 func(*cards.List, int) tea.Cmd { return b.leave(kit.WM.OpenHere(b.desk.start(nil, ""))) },
			"hub.rename":              func(_ *cards.List, i int) tea.Cmd { return b.rename(b.rows[i]) },
			"hub.edit":                func(_ *cards.List, i int) tea.Cmd { return b.rename(b.rows[i]) },
			"hub.remove":              func(l *cards.List, i int) tea.Cmd { return b.remove(l, b.rows[i]) },
			"hub.refresh":             func(*cards.List, int) tea.Cmd { return b.load() },
			"agent.sessions.beside":   func(_ *cards.List, i int) tea.Cmd { return b.beside(b.rows[i].session) },
			"agent.sessions.space":    func(_ *cards.List, i int) tea.Cmd { return b.space(b.rows[i].session) },
			"agent.sessions.pin":      func(_ *cards.List, i int) tea.Cmd { return b.pin(b.rows[i]) },
			"agent.sessions.archive":  func(_ *cards.List, i int) tea.Cmd { return b.archive(b.rows[i]) },
			"agent.sessions.archived": func(*cards.List, int) tea.Cmd { b.archived = !b.archived; return b.load() },
			"agent.sessions.export":   func(_ *cards.List, i int) tea.Cmd { return b.export(b.rows[i]) },
			"agent.sessions.prune":    func(l *cards.List, _ int) tea.Cmd { return b.prune(l) },
		},
	})
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

// Mark of what a session is doing: shown here, open in another window, working, or held open by the server; pinned after it
func (b *browser) mark(row listing) string {
	glyphs := b.desk.kit.Env.Theme.Glyphs
	mark := ""
	switch shown := b.desk.shows(row.session.GetId()); {
	case shown != nil && shown == b.from:
		mark = glyphs.Live
	case shown != nil:
		mark = glyphs.Marked
	case row.live != nil && (row.live.GetBusy() || row.live.GetWaiting()):
		mark = glyphs.Active
	case row.live != nil:
		mark = glyphs.Hollow
	}
	if row.session.GetPinned() {
		mark += glyphs.Flag
	}
	return mark
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
	t := at.AsTime().Local()
	if now := time.Now(); t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

func (b *browser) detail(row listing) []string {
	s := row.session
	state := "stored"
	switch {
	case b.desk.shows(s.GetId()) != nil:
		state = "open in a window"
	case row.live != nil && row.live.GetWaiting():
		state = "waiting for your answer"
	case row.live != nil && row.live.GetBusy():
		state = "working"
	case row.live != nil:
		state = "open on the server"
	}
	if s.GetArchived() {
		state += " · archived"
	}
	if s.GetPinned() {
		state += " · pinned"
	}
	lines := []string{b.name(row)}
	if preview := strings.Join(strings.Fields(s.GetPreview()), " "); preview != "" {
		lines = append(lines, preview)
	}
	lines = append(lines, "", "state: "+state)
	if s.GetModel() != "" {
		lines = append(lines, "model "+s.GetModel())
	}
	if s.GetProfile() != "" {
		lines = append(lines, "profile "+s.GetProfile())
	}
	if s.GetSource() != "" {
		lines = append(lines, "started from "+s.GetSource())
	}
	if s.GetDirectory() != "" {
		lines = append(lines, "in "+tilde(s.GetDirectory()))
	}
	return append(lines, fmt.Sprintf("%d messages", s.GetMessages()), "id "+s.GetId())
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
			if err != nil && len(rows) > 0 {
				return b.hub.Fail("Could not load the stored sessions: " + tui.Reason(err))
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
	return kit.Ask(b.hub, "Rename "+name, cards.Field{Label: "Title", Value: session.GetTitle()}, "Rename", func(title string) tea.Cmd {
		if title == "" || title == session.GetTitle() {
			return nil
		}
		b.hub.Busy("Saving " + name + "…")
		return tui.Call(b.hub, func() (*sessionsv1.RenameResponse, error) {
			return b.desk.sessions.Rename(kit.Env.Ctx, &sessionsv1.RenameRequest{Id: session.GetId(), Title: title})
		}, func(response *sessionsv1.RenameResponse) tea.Cmd {
			stored := cmp.Or(response.GetTitle(), title)
			if c := b.desk.shows(session.GetId()); c != nil {
				c.title = stored
			}
			return tea.Batch(b.hub.Done("Renamed it "+stored+"."), b.load())
		})
	})
}

func (b *browser) pin(row listing) tea.Cmd {
	kit := b.desk.kit
	pinned := !row.session.GetPinned()
	return tui.Call(b.hub, func() (*sessionsv1.MarkResponse, error) {
		return b.desk.sessions.Mark(kit.Env.Ctx, &sessionsv1.MarkRequest{Id: row.session.GetId(), Pinned: &pinned})
	}, func(*sessionsv1.MarkResponse) tea.Cmd {
		done := "Pinned " + b.name(row) + "."
		if !pinned {
			done = "Unpinned " + b.name(row) + "."
		}
		return tea.Batch(b.hub.Done(done), b.load())
	})
}

func (b *browser) archive(row listing) tea.Cmd {
	kit := b.desk.kit
	archived := !row.session.GetArchived()
	if archived && b.desk.shows(row.session.GetId()) != nil {
		return b.hub.Fail(b.name(row) + " is open in a window; close the window to archive it.")
	}
	return tui.Call(b.hub, func() (*sessionsv1.MarkResponse, error) {
		return b.desk.sessions.Mark(kit.Env.Ctx, &sessionsv1.MarkRequest{Id: row.session.GetId(), Archived: &archived})
	}, func(*sessionsv1.MarkResponse) tea.Cmd {
		done := "Archived " + b.name(row) + "."
		if !archived {
			done = "Brought " + b.name(row) + " back from the archive."
		}
		return tea.Batch(b.hub.Done(done), b.load())
	})
}

// Closes a session the server holds open, which stays stored, or deletes a stored one; never one a window shows
func (b *browser) remove(l *cards.List, row listing) tea.Cmd {
	kit := b.desk.kit
	name, id := b.name(row), row.session.GetId()
	if b.desk.shows(id) != nil {
		return b.hub.Fail(name + " is open in a window; close the window to close it.")
	}
	if row.live != nil {
		l.Confirm("Close "+name+"? It stays in your stored sessions.", func() tea.Cmd {
			b.hub.Busy("Closing " + name + "…")
			return tui.Call(b.hub, func() (*agentv1.ShutResponse, error) {
				return b.desk.agents.Shut(kit.Env.Ctx, &agentv1.ShutRequest{SessionId: id})
			}, func(*agentv1.ShutResponse) tea.Cmd { return tea.Batch(b.hub.Done("Closed "+name+"."), b.load()) })
		})
		return nil
	}
	l.Confirm("Delete "+name+"?", func() tea.Cmd {
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
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title: "Export " + name,
		Fields: []cards.Field{
			{ID: "format", Label: "Format", Kind: cards.FieldChoice, Value: "md", Choices: []cards.Choice{
				{Value: "md", Label: "Markdown", Meaning: "the conversation as readable text"},
				{Value: "json", Label: "JSON", Meaning: "every message with its details, for tools"},
				{Value: "html", Label: "HTML", Meaning: "a page to open in a browser"},
			}},
			{ID: "file", Label: "File", Placeholder: "a name in the session's folder, empty for the default"},
			{ID: "redact", Label: "Hide secrets", Kind: cards.FieldBool, Value: "true", Help: "Replaces keys and tokens in the file with a mark"},
		},
		Submit: "Export",
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			b.hub.Busy("Exporting " + name + "…")
			return tui.Call(b.hub, func() (string, error) {
				return b.desk.save(kit.Env.Ctx, id, values["format"], values["file"], values["redact"] == "true")
			}, func(path string) tea.Cmd { return tea.Batch(b.hub.Close(form), b.hub.Done("Saved "+path+".")) })
		},
	})
	return b.hub.Push(form)
}

// Form that deletes stored sessions untouched for a number of days, after a yes; sessions a window or the server holds open are kept
func (b *browser) prune(l *cards.List) tea.Cmd {
	kit := b.desk.kit
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title: "Prune old sessions",
		Intro: []string{"Deletes stored sessions you have not touched for a while. Sessions open in a window are kept."},
		Fields: []cards.Field{
			{ID: "days", Label: "Untouched for days", Kind: cards.FieldNumber, Value: "30", Required: true,
				Help: "Sessions with no activity for this many days are deleted", Check: func(value string) string {
					if n, err := strconv.Atoi(strings.TrimSpace(value)); err != nil || n < 0 {
						return "needs a number"
					}
					return ""
				}},
			{ID: "archived", Label: "Archived only", Kind: cards.FieldBool, Value: "false", Help: "Only delete sessions you archived"},
			{ID: "pinned", Label: "Pinned too", Kind: cards.FieldBool, Value: "false"},
		},
		Submit: "Prune",
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			days, _ := strconv.Atoi(strings.TrimSpace(values["days"]))
			// Sessions a window or the server holds open are not ended, which pruning never deletes
			filter := &sessionsv1.Filter{ActiveBefore: timestamppb.New(time.Now().Add(-time.Duration(days) * 24 * time.Hour)),
				IncludePinned: values["pinned"] == "true", OnlyArchived: values["archived"] == "true", IncludeArchived: true}
			return tea.Batch(b.hub.Close(form), tui.Async(func() func() tea.Cmd {
				return func() tea.Cmd {
					l.Confirm(fmt.Sprintf("Delete every stored session untouched for %d days?", days), func() tea.Cmd {
						b.hub.Busy("Pruning sessions…")
						return tui.Call(b.hub, func() (*sessionsv1.PruneResponse, error) {
							return b.desk.sessions.Prune(kit.Env.Ctx, &sessionsv1.PruneRequest{Filter: filter})
						}, func(response *sessionsv1.PruneResponse) tea.Cmd {
							return tea.Batch(b.hub.Done(fmt.Sprintf("Deleted %d sessions.", response.GetDeleted())), b.load())
						})
					})
					return nil
				}
			}))
		},
	})
	return b.hub.Push(form)
}
