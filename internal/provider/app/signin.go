package app

import (
	"bytes"
	"context"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/credentials"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/browser"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of a sign-in in progress
const signinContext = "providers.signin"

var signinKeys = []keys.Action{
	{ID: "providers.signin.open", Context: signinContext, Label: "open link", Doc: "open the sign in link in the browser of this computer", Default: []string{"o"}},
	{ID: "providers.signin.copy", Context: signinContext, Label: "copy link", Doc: "copy the sign in link", Default: []string{"c"}},
	{ID: "providers.signin.code", Context: signinContext, Label: "copy code", Doc: "copy the code to type on the sign in page", Default: []string{"y"}},
}

// Connector of the sign-ins a plugin adds for one kind, each run on a page of its own: what it says, the link to open and the code to
// type there with keys to open and copy them, what it asks in a form, and the fields it gives kept in the account
func SignIns(conn *connect.Client, spec string, ways ...credentials.SignIn) Connector {
	settings := configv1connect.NewConfigServiceClient(conn)
	connector := Connector{Spec: spec}
	for _, way := range ways {
		connector.Ways = append(connector.Ways, Way{Title: way.Title, Open: func(kit *tui.Kit, target Target) tea.Cmd {
			return (&signing{kit: kit, settings: settings, target: target, way: way}).run()
		}})
	}
	return connector
}

// One sign-in on its page
type signing struct {
	kit      *tui.Kit
	settings configv1connect.ConfigServiceClient
	target   Target
	way      credentials.SignIn
	mu       sync.Mutex
	text     bytes.Buffer
	link     string
	code     string
}

func (s *signing) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.Write(data)
}

// What the page shows: the link to open and the code, then the last of what the sign-in said
func (s *signing) lines(int) []string {
	t := s.kit.Env.Theme
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	switch {
	case s.link != "" && s.code != "":
		out = append(out, "Open this:", render.Warm(s.link, t), "", "Then type this code:", render.Heading(s.code, t), "")
	case s.link != "":
		out = append(out, "Open this:", render.Warm(s.link, t), "")
	}
	said := strings.Split(strings.TrimRight(s.text.String(), "\n"), "\n")
	if len(out) > 0 && len(said) > 8 {
		said = said[len(said)-8:]
	}
	for _, line := range said {
		if len(out) > 0 {
			line = render.Dim(line, t)
		}
		out = append(out, line)
	}
	return out
}

// Opens the page and runs the sign-in behind it; closing the page stops the sign-in
func (s *signing) run() tea.Cmd {
	ctx, cancel := context.WithCancel(s.kit.Env.Ctx)
	var hub *cards.Hub
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title:   s.way.Title,
		Key:     "providers.signin." + s.way.Spec,
		Lines:   s.lines,
		Follow:  true,
		Context: signinContext,
		Hints: func(*cards.Page) []wm.Hint {
			s.mu.Lock()
			defer s.mu.Unlock()
			var hints []wm.Hint
			if s.link != "" {
				hints = append(hints, wm.Hint{Action: "providers.signin.open"}, wm.Hint{Action: "providers.signin.copy"})
			}
			if s.code != "" {
				hints = append(hints, wm.Hint{Action: "providers.signin.code"})
			}
			return hints
		},
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"providers.signin.open": func(*cards.Page) tea.Cmd { return s.open(hub) },
			"providers.signin.copy": func(*cards.Page) tea.Cmd { return s.copy(hub, s.shown(true), "link") },
			"providers.signin.code": func(*cards.Page) tea.Cmd { return s.copy(hub, s.shown(false), "code") },
		},
	})
	hub = s.kit.Hub("providers.signin", page)
	hub.OnClose(func() tea.Cmd {
		cancel()
		return nil
	})
	prompter := &credentials.Prompter{
		Ctx: ctx,
		Out: s,
		Ask: func(label string, secret bool) (string, bool) { return s.ask(ctx, hub, label, secret) },
		Show: func(link, code string) {
			s.mu.Lock()
			s.link, s.code = link, code
			s.mu.Unlock()
			s.kit.Apply(func() tea.Cmd { return nil })
		},
		Open:     func(link string) error { return browser.Open(ctx, link) },
		Browser:  true,
		Options:  map[string]string{},
		Settings: s.values(ctx),
	}
	return tea.Batch(s.kit.WM.ShowOverlay(hub), tui.Async(func() func() tea.Cmd {
		signed, err := s.way.Run(prompter)
		left := ctx.Err() != nil
		if err == nil {
			err = s.save(ctx, signed)
		}
		return func() tea.Cmd {
			cancel()
			switch {
			case left:
				return nil
			case err != nil:
				return hub.Fail(strings.Join(credentials.FailureLines(err, "", "/providers"), " "))
			}
			return tea.Batch(s.kit.WM.DismissCard(hub), s.kit.WM.Toast("Connected "+s.target.Account.GetTitle(), wm.Success, wm.DefaultToastTTL))
		}
	}))
}

// Link or code shown on the page
func (s *signing) shown(link bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if link {
		return s.link
	}
	return s.code
}

// Asks for a line in a form over the page, waiting until it is sent or the sign-in stops
func (s *signing) ask(ctx context.Context, hub *cards.Hub, label string, secret bool) (string, bool) {
	answer := make(chan string, 1)
	field := cards.Field{Label: strings.TrimRight(strings.TrimSpace(label), ":")}
	if secret {
		field.Kind = cards.FieldSecret
	}
	s.kit.Apply(func() tea.Cmd {
		if ctx.Err() != nil {
			return nil
		}
		return s.kit.Ask(hub, s.way.Title, field, "Continue", func(text string) tea.Cmd {
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

func (s *signing) open(hub *cards.Hub) tea.Cmd {
	link := s.shown(true)
	if link == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		err := browser.Open(s.kit.Env.Ctx, link)
		return func() tea.Cmd {
			if err != nil {
				return hub.Fail("The browser didn't open: copy the link and open it yourself")
			}
			return nil
		}
	})
}

func (s *signing) copy(hub *cards.Hub, text, what string) tea.Cmd {
	if text == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		ok := clip.WriteText(s.kit.Env.Ctx, text)
		return func() tea.Cmd {
			if !ok {
				return hub.Fail("Couldn't copy the " + what)
			}
			return hub.Done("Copied the " + what)
		}
	})
}

// Settings of the provider by their names below it, as the profile has them
func (s *signing) values(ctx context.Context) map[string]string {
	values := map[string]string{}
	listed, err := s.settings.List(ctx, &configv1.ListRequest{Profile: s.target.Profile})
	if err != nil {
		return values
	}
	prefix := s.target.Provider.GetSection() + "."
	for _, setting := range listed.GetSettings() {
		if name, ok := strings.CutPrefix(setting.GetKey(), prefix); ok && !strings.Contains(name, ".") {
			values[name] = setting.GetValue()
		}
	}
	return values
}

// Keeps the fields the sign-in gave in the account, clearing what the account last ran into
func (s *signing) save(ctx context.Context, signed credentials.Signed) error {
	return Store(ctx, s.settings, s.target, signed.Fields)
}

// Keeps fields in the account of the target, each as YAML, nil putting its default back, and clears the limits the account ran into
func Store(ctx context.Context, settings configv1connect.ConfigServiceClient, target Target, fields map[string]any) error {
	fields[providerconfig.Status] = nil
	for field, value := range fields {
		text := "null"
		if value != nil {
			encoded, err := yaml.Marshal(value)
			if err != nil {
				return err
			}
			text = strings.TrimSuffix(string(encoded), "\n")
		}
		if _, err := settings.Set(ctx, &configv1.SetRequest{Profile: target.Profile, Key: target.Key(field), Value: text}); err != nil {
			return err
		}
	}
	return nil
}
