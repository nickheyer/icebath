package app

import (
	"cmp"
	"strconv"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1/slashv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct {
	chats int
}

func Register(registry *app.Registry) {
	registry.Register(new(screens))
}

func (s *screens) Screens(conn *connect.Client) []tui.Screen {
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	agents := agentv1connect.NewAgentServiceClient(conn)
	commands := slashv1connect.NewSlashServiceClient(conn)
	providers := providerv1connect.NewProviderServiceClient(conn)
	var start func(kit *tui.Kit, session *sessionsv1.Session) *chat
	start = func(kit *tui.Kit, session *sessionsv1.Session) *chat {
		s.chats++
		c := newChat(kit, sessions, agents, commands, "chat:"+strconv.Itoa(s.chats), session)
		c.pick = func() tea.Cmd { return (&picker{kit: kit, providers: providers, chat: c}).open() }
		c.open = func(session *sessionsv1.Session) tea.Cmd { return kit.WM.OpenHere(start(kit, session)) }
		return c
	}
	return []tui.Screen{
		{
			ID:       "chat",
			Title:    "New chat",
			Short:    "Start a session with the agent in this directory",
			Startup:  true,
			Commands: []string{"session.beside", "session.new"},
			Open: func(kit *tui.Kit) tea.Cmd {
				return kit.WM.OpenHere(start(kit, nil))
			},
		},
		{
			ID:       "model",
			Title:    "Model",
			Short:    "Switch the provider and model of the session in focus",
			Commands: []string{"model.switch"},
			Open: func(kit *tui.Kit) tea.Cmd {
				target, ok := kit.Focused().(*chat)
				if !ok {
					return kit.WM.Toast("Focus a chat to switch its model", wm.Warn, wm.DefaultToastTTL)
				}
				return target.pick()
			},
		},
		{
			ID:    "sessions",
			Title: "Sessions",
			Short: "Open, rename or delete an earlier session",
			Open: func(kit *tui.Kit) tea.Cmd {
				return (&history{kit: kit, sessions: sessions, chat: start}).open()
			},
		},
	}
}

type history struct {
	kit      *tui.Kit
	sessions sessionsv1connect.SessionServiceClient
	chat     func(kit *tui.Kit, session *sessionsv1.Session) *chat
	hub      *cards.Hub
	rows     []*sessionsv1.Session
	loaded   bool
	err      error
}

func (h *history) open() tea.Cmd {
	list := cards.NewList(h.kit.Env, cards.ListSpec{
		Title:   "Sessions",
		Glyph:   h.kit.Env.Theme.Glyphs.Session,
		Key:     "sessions",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Session", Flex: 1}, {Title: "Directory", Flex: 1}, {Title: "Updated"}},
		Yield:   []int{1},
		Rows: func() [][]string {
			rows := make([][]string, len(h.rows))
			for i, session := range h.rows {
				rows[i] = []string{cmp.Or(session.GetTitle(), "untitled"), session.GetDirectory(), session.GetUpdated().AsTime().Local().Format("2006-01-02 15:04")}
			}
			return rows
		},
		Empty: func() string {
			switch {
			case h.err != nil:
				return h.err.Error()
			case !h.loaded:
				return "Loading…"
			}
			return "No sessions yet"
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use"}, {Action: "hub.rename"}, {Action: "hub.remove"}, {Action: "hub.refresh"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(h.kit.WM.DismissCard(h.hub), h.kit.WM.OpenHere(h.chat(h.kit, h.rows[i])))
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd { return h.rename(h.rows[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				session := h.rows[i]
				l.Confirm("Delete "+cmp.Or(session.GetTitle(), "this session")+"?", func() tea.Cmd { return h.remove(session) })
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return h.load() },
		},
	})
	h.hub = h.kit.Hub("sessions", list)
	return tea.Batch(h.kit.WM.ShowOverlay(h.hub), h.load())
}

func (h *history) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := h.sessions.List(h.kit.Env.Ctx, &sessionsv1.ListRequest{})
		return func() tea.Cmd {
			h.rows, h.err, h.loaded = response.GetSessions(), err, true
			return nil
		}
	})
}

func (h *history) remove(session *sessionsv1.Session) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := h.sessions.Delete(h.kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: session.GetId()})
		return func() tea.Cmd {
			if err != nil {
				return h.hub.Fail(err.Error())
			}
			return tea.Batch(h.hub.Done("Session deleted"), h.load())
		}
	})
}

func (h *history) rename(session *sessionsv1.Session) tea.Cmd {
	return h.hub.Push(cards.NewForm(h.kit.Env, cards.FormSpec{
		Title:  "Rename session",
		Fields: []cards.Field{{ID: "title", Label: "Title", Value: session.GetTitle(), Required: true}},
		Submit: "Rename",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tui.Async(func() func() tea.Cmd {
				_, err := h.sessions.Rename(h.kit.Env.Ctx, &sessionsv1.RenameRequest{Id: session.GetId(), Title: values["title"]})
				return func() tea.Cmd {
					if err != nil {
						return h.hub.Fail(err.Error())
					}
					return tea.Batch(h.hub.Pop(), h.hub.Done("Session renamed"), h.load())
				}
			})
		},
		Cancel: func(*cards.Form) tea.Cmd { return h.hub.Pop() },
	}))
}
