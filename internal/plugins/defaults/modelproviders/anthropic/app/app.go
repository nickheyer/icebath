package app

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	anthropiccredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct {
	config *config.Config
}

func Register(registry *app.Registry, cfg *config.Config) {
	registry.Register(screens{config: cfg})
}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "anthropic.login",
		Title: "Sign in to Anthropic",
		Short: "Claude Pro/Max subscription sign in, or a setup token",
		Open: func(kit *tui.Kit) tea.Cmd {
			return (&login{kit: kit, config: s.config, settings: settings}).open()
		},
	}}
}

type method struct {
	title string
	short string
	run   func() tea.Cmd
}

type login struct {
	kit      *tui.Kit
	config   *config.Config
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	methods  []method
}

func (l *login) open() tea.Cmd {
	l.methods = []method{
		{"Claude Pro/Max subscription", "Authorize in the browser and paste the code it shows", l.subscription},
		{"Setup token", "Paste a token made by claude setup-token", l.token},
	}
	list := cards.NewList(l.kit.Env, cards.ListSpec{
		Title:   "Sign in to Anthropic",
		Key:     "anthropic.login",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Method"}, {Title: "What it does", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(l.methods))
			for i, entry := range l.methods {
				rows[i] = []string{entry.title, entry.short}
			}
			return rows
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "sign in"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return l.methods[i].run() },
		},
	})
	l.hub = l.kit.Hub("anthropic.login", list)
	return l.kit.WM.ShowOverlay(l.hub)
}

type output struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (o *output) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(data)
}

func (o *output) lines() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Split(strings.TrimRight(o.text.String(), "\n"), "\n")
}

func (l *login) subscription() tea.Cmd {
	out := new(output)
	ctx, cancel := context.WithCancel(l.kit.Env.Ctx)
	page := cards.NewPage(l.kit.Env, cards.PageSpec{
		Title:  "Claude Pro/Max subscription",
		Key:    "anthropic.login.subscription",
		Lines:  func(int) []string { return out.lines() },
		Follow: true,
	})
	prompt := func(label string) (string, bool) {
		answer := make(chan string, 1)
		l.kit.Apply(func() tea.Cmd {
			return l.hub.Push(cards.NewForm(l.kit.Env, cards.FormSpec{
				Title:  "Claude Pro/Max subscription",
				Intro:  out.lines(),
				Fields: []cards.Field{{ID: "answer", Label: strings.TrimRight(strings.TrimSpace(label), ":"), Required: true}},
				Submit: "Continue",
				Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
					answer <- values["answer"]
					return l.hub.Pop()
				},
				Cancel: func(*cards.Form) tea.Cmd {
					cancel()
					return l.hub.Pop()
				},
			}))
		})
		select {
		case text := <-answer:
			return text, true
		case <-ctx.Done():
			return "", false
		}
	}
	return tea.Batch(l.hub.Push(page), tui.Async(func() func() tea.Cmd {
		state := anthropiccredentials.RunIcebathOAuthLoginTo(anthropiccredentials.PoolEnv{Config: l.config, Ctx: ctx}, out, prompt, true)
		token, _ := state["access_token"].(string)
		var err error
		if token != "" {
			err = l.save(token)
		}
		return func() tea.Cmd {
			cancel()
			switch {
			case err != nil:
				return l.hub.Fail(err.Error())
			case token == "":
				return l.hub.Fail("Sign in did not return a credential")
			}
			return tea.Batch(l.hub.PopTo(l.hub.Root()), l.hub.Done("Signed in to Anthropic"))
		}
	}))
}

func (l *login) token() tea.Cmd {
	return l.hub.Push(cards.NewForm(l.kit.Env, cards.FormSpec{
		Title:  "Setup token",
		Intro:  []string{"Run claude setup-token, follow the browser prompts, then paste the token it prints (sk-ant-oat-...)."},
		Fields: []cards.Field{{ID: "token", Label: "Setup token", Kind: cards.FieldSecret, Required: true}},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			token := strings.TrimSpace(values["token"])
			return tui.Async(func() func() tea.Cmd {
				err := l.save(token)
				return func() tea.Cmd {
					if err != nil {
						return l.hub.Fail(err.Error())
					}
					return tea.Batch(l.hub.Pop(), l.hub.Done("Setup token saved"))
				}
			})
		},
		Cancel: func(*cards.Form) tea.Cmd { return l.hub.Pop() },
	}))
}

func (l *login) save(token string) error {
	_, err := l.settings.Set(l.kit.Env.Ctx, &configv1.SetRequest{Key: anthropicconfig.Token, Value: strconv.Quote(token)})
	return err
}
