package app

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	anthropiccredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// Ways to sign an Anthropic account in besides an API key
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	settings := configv1connect.NewConfigServiceClient(conn)
	with := func(open func(l *login) tea.Cmd) func(*tui.Kit, providerapp.Target) tea.Cmd {
		return func(kit *tui.Kit, target providerapp.Target) tea.Cmd {
			return open(&login{kit: kit, target: target, settings: settings})
		}
	}
	return []providerapp.Connector{{
		Spec: anthropicconfig.Name,
		Ways: []providerapp.Way{
			{Title: "Claude subscription", Open: with((*login).subscription)},
			{Title: "Setup token from claude setup-token", Open: with((*login).token)},
		},
	}}
}

// Sign in of one account
type login struct {
	kit      *tui.Kit
	target   providerapp.Target
	settings configv1connect.ConfigServiceClient
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
	hub := l.kit.Hub("anthropic.login", cards.NewPage(l.kit.Env, cards.PageSpec{
		Title:  "Claude subscription",
		Key:    "anthropic.login.subscription",
		Lines:  func(int) []string { return out.lines() },
		Follow: true,
	}))
	hub.OnClose(func() tea.Cmd {
		cancel()
		return nil
	})
	prompt := func(label string) (string, bool) {
		answer := make(chan string, 1)
		l.kit.Apply(func() tea.Cmd {
			if ctx.Err() != nil {
				return nil
			}
			return l.kit.Ask(hub, "Claude subscription", cards.Field{Label: strings.TrimRight(strings.TrimSpace(label), ":")}, "Continue", func(text string) tea.Cmd {
				answer <- text
				return nil
			})
		})
		select {
		case text := <-answer:
			return text, true
		case <-ctx.Done():
			return "", false
		}
	}
	return tea.Batch(l.kit.WM.ShowOverlay(hub), tui.Async(func() func() tea.Cmd {
		state := anthropiccredentials.RunIcebathOAuthLoginTo(anthropiccredentials.PoolEnv{Ctx: ctx}, out, prompt, true)
		token, _ := state["access_token"].(string)
		refresh, _ := state["refresh_token"].(string)
		expires, _ := state["expires_at_ms"].(float64)
		left := ctx.Err() != nil
		var err error
		if token != "" {
			err = l.save(token, refresh, int64(expires))
		}
		return func() tea.Cmd {
			cancel()
			switch {
			case err != nil:
				return hub.Fail(tui.Reason(err))
			case left:
				return nil
			case token == "":
				return hub.Fail("Sign-in didn't finish. Close this and try again.")
			}
			return tea.Batch(l.kit.WM.DismissCard(hub), l.kit.WM.Toast("Connected "+l.target.Account.GetTitle(), wm.Success, wm.DefaultToastTTL))
		}
	}))
}

func (l *login) token() tea.Cmd {
	var hub *cards.Hub
	hub = l.kit.Hub("anthropic.login", cards.NewForm(l.kit.Env, cards.FormSpec{
		Title:  "Setup token",
		Intro:  []string{"Run claude setup-token, then paste the token it prints."},
		Fields: []cards.Field{{ID: "token", Label: "Token", Kind: cards.FieldSecret, Required: true}},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tui.Call(hub, func() (struct{}, error) {
				return struct{}{}, l.save(strings.TrimSpace(values["token"]), "", 0)
			}, func(struct{}) tea.Cmd {
				return tea.Batch(l.kit.WM.DismissCard(hub), l.kit.WM.Toast("Connected "+l.target.Account.GetTitle(), wm.Success, wm.DefaultToastTTL))
			})
		},
	}))
	return l.kit.WM.ShowOverlay(hub)
}

func (l *login) save(token, refresh string, expires int64) error {
	values := [][2]string{{anthropicconfig.Token, strconv.Quote(token)}, {anthropicconfig.RefreshToken, "null"}, {anthropicconfig.TokenExpires, "null"}}
	if refresh != "" {
		values[1][1], values[2][1] = strconv.Quote(refresh), strconv.FormatInt(expires, 10)
	}
	for _, value := range values {
		if _, err := l.settings.Set(l.kit.Env.Ctx, &configv1.SetRequest{Profile: l.target.Profile, Key: l.target.Key(value[0]), Value: value[1]}); err != nil {
			return err
		}
	}
	return nil
}
