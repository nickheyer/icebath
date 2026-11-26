package app

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1/slashv1connect"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of past sessions
const sessionsScreen = "sessions"

type desk struct {
	kit       *tui.Kit
	sessions  sessionsv1connect.SessionServiceClient
	agents    agentv1connect.AgentServiceClient
	slash     slashv1connect.SlashServiceClient
	providers providerv1connect.ProviderServiceClient
	settings  configv1connect.ConfigServiceClient
	tools     toolsv1connect.ToolServiceClient
	chats     []*chat
	opened    int
}

func Register(registry *app.Registry) {
	registry.Register(new(desk))
}

func (d *desk) Screens(conn *connect.Client) []tui.Screen {
	d.sessions = sessionsv1connect.NewSessionServiceClient(conn)
	d.agents = agentv1connect.NewAgentServiceClient(conn)
	d.slash = slashv1connect.NewSlashServiceClient(conn)
	d.providers = providerv1connect.NewProviderServiceClient(conn)
	d.settings = configv1connect.NewConfigServiceClient(conn)
	d.tools = toolsv1connect.NewToolServiceClient(conn)
	return []tui.Screen{
		{
			ID:      "chat",
			Title:   "New chat",
			Short:   "Open a new chat, on the profile in use or the one named",
			Slash:   []string{"new"},
			Usage:   "[@profile]",
			Keys:    []string{"alt+n"},
			Actions: approvalKeys,
			Where:   map[string]string{approvalContext: "approval prompt"},
			Startup: true,
			Suggest: true,
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, _ := profile.Split(argument)
				return kit.WM.OpenHere(d.at(kit).start(nil, name))
			},
		},
		{
			ID:      sessionsScreen,
			Title:   "Sessions",
			Short:   "Open, rename or delete past sessions",
			Slash:   []string{"sessions"},
			Keys:    []string{"alt+s"},
			Suggest: true,
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				return (&history{desk: d.at(kit)}).open()
			},
		},
		{
			ID:    "model",
			Title: "Model",
			Short: "Change the model for this chat",
			Keys:  []string{"alt+m"},
			Ready: func(kit *tui.Kit) bool {
				_, focused := kit.Focused().(*chat)
				return focused
			},
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				if target, ok := kit.Focused().(*chat); ok {
					return target.pick()
				}
				return nil
			},
		},
	}
}

func (d *desk) at(kit *tui.Kit) *desk {
	if d.kit == nil {
		d.kit = kit
		kit.Report(d.report)
	}
	return d
}

// Opens a chat of the session, or of a session to be made on the profile
func (d *desk) start(session *sessionsv1.Session, profile string) *chat {
	d.opened++
	c := newChat(d, "chat:"+strconv.Itoa(d.opened), session, profile)
	d.chats = append(d.chats, c)
	return c
}

func (d *desk) closed(c *chat) {
	d.chats = slices.DeleteFunc(d.chats, func(open *chat) bool { return open == c })
}

// Opens the session in a window of its own, or focuses the window that already shows it
func (d *desk) open(session *sessionsv1.Session) tea.Cmd {
	for _, c := range d.chats {
		if c.id == session.GetId() {
			return d.kit.WM.Focus(c)
		}
	}
	return d.kit.WM.OpenHere(d.start(session, ""))
}

func (d *desk) report(status *wm.Status) {
	for _, c := range d.chats {
		space := d.kit.WM.WorkspaceOf(c)
		if space == nil {
			continue
		}
		signal := wm.Idle
		switch {
		case c.log.busy:
			signal = wm.Working
		case c.unseen:
			signal = wm.Fresh
		}
		if lane := status.Lanes[space]; signal > lane.Signal {
			lane.Signal = signal
			status.Lanes[space] = lane
		}
	}
}

type history struct {
	desk   *desk
	hub    *cards.Hub
	rows   []*sessionsv1.Session
	loaded bool
	err    error
}

func (h *history) open() tea.Cmd {
	kit := h.desk.kit
	leave := func(cmd tea.Cmd) tea.Cmd { return tea.Batch(kit.WM.DismissCard(h.hub), cmd) }
	list := cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Sessions",
		Glyph:   kit.Env.Theme.Glyphs.Session,
		Key:     "sessions",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search sessions",
		Columns: []render.Column{{Title: "Session", Max: 60}, {Title: "Profile"}, {Title: "Directory", Flex: 1}, {Title: "Last used"}},
		Yield:   []int{2},
		Rows: func() [][]string {
			home, _ := os.UserHomeDir()
			rows := make([][]string, len(h.rows))
			for i, session := range h.rows {
				directory := session.GetDirectory()
				if rest, inside := strings.CutPrefix(directory, home+string(filepath.Separator)); inside && home != "" {
					directory = "~" + string(filepath.Separator) + rest
				}
				rows[i] = []string{cmp.Or(session.GetTitle(), "Untitled"), session.GetProfile(), directory, session.GetUpdated().AsTime().Local().Format("2006-01-02 15:04")}
			}
			return rows
		},
		Empty: func() string { return tui.Waiting(h.loaded, h.err, "No sessions yet") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.rename"}, {Action: "hub.remove", Label: "delete"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return leave(h.desk.open(h.rows[i])) },
			"hub.rename": func(_ *cards.List, i int) tea.Cmd {
				session := h.rows[i]
				return kit.Ask(h.hub, "Rename session", cards.Field{Label: "Title", Value: session.GetTitle()}, "Rename", func(title string) tea.Cmd {
					return tui.Call(h.hub, func() (*sessionsv1.RenameResponse, error) {
						return h.desk.sessions.Rename(kit.Env.Ctx, &sessionsv1.RenameRequest{Id: session.GetId(), Title: title})
					}, func(*sessionsv1.RenameResponse) tea.Cmd {
						for _, c := range h.desk.chats {
							if c.id == session.GetId() {
								c.title = title
							}
						}
						return h.load()
					})
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				session := h.rows[i]
				l.Confirm("Delete "+cmp.Or(session.GetTitle(), "this session")+"?", func() tea.Cmd {
					return tui.Call(h.hub, func() (*sessionsv1.DeleteResponse, error) {
						return h.desk.sessions.Delete(kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: session.GetId()})
					}, func(*sessionsv1.DeleteResponse) tea.Cmd { return h.load() })
				})
				return nil
			},
		},
	})
	h.hub = kit.Hub("sessions", list)
	return tea.Batch(kit.WM.ShowOverlay(h.hub), h.load())
}

func (h *history) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := h.desk.sessions.List(h.desk.kit.Env.Ctx, &sessionsv1.ListRequest{})
		return func() tea.Cmd {
			h.rows, h.err, h.loaded = response.GetSessions(), err, true
			return nil
		}
	})
}
