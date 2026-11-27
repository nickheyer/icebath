package app

import (
	"bytes"
	"context"
	"errors"
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
	{ID: "providers.signin.open", Context: signinContext, Label: "open link", Doc: "Open the sign-in link in the browser on this computer", Default: []string{"o"}},
	{ID: "providers.signin.copy", Context: signinContext, Label: "copy link", Doc: "Copy the sign-in link", Default: []string{"c"}},
	{ID: "providers.signin.code", Context: signinContext, Label: "copy code", Doc: "Copy the code to type on the sign-in page", Default: []string{"y"}},
}

// Lines of what a sign-in says that a page shows under the link and the code
const saidLines = 8

// Connector of the sign-ins a plugin adds for one kind, each run on a page of its own: what it says, the link to open and the code to
// type there with keys to open and copy them, what it asks in a form, and the fields it gives kept in the account
func SignIns(conn *connect.Client, spec string, ways ...credentials.SignIn) Connector {
	settings := configv1connect.NewConfigServiceClient(conn)
	connector := Connector{Spec: spec}
	for _, way := range ways {
		connector.Ways = append(connector.Ways, Way{Title: way.Title, Kind: way.Type, Open: func(kit *tui.Kit, target Target) tea.Cmd {
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
	// Why the sign-in stopped, empty while it runs
	failed string
}

func (s *signing) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.Write(data)
}

// What the page shows: what to do, the link to open and the code to type there, then the last of what the sign-in said that the link and
// the code do not repeat, and what the page waits for
func (s *signing) lines(w int) []string {
	t := s.kit.Env.Theme
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.target.Provider.GetDisplayName()
	var out []string
	switch {
	case s.link != "" && s.code != "":
		out = append(out, "Open this address in your browser and sign in to "+name+":", render.Warm(s.link, t), "", "When the page asks for a code, type this one:",
			render.Heading(s.code, t))
	case s.link != "":
		out = append(out, "Open this address in your browser and sign in to "+name+":", render.Warm(s.link, t))
	}
	var said []string
	for _, line := range strings.Split(strings.TrimRight(s.text.String(), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "", trimmed == "To continue:":
		case s.link != "" && strings.Contains(line, s.link), s.code != "" && strings.Contains(line, s.code):
		default:
			said = append(said, line)
		}
	}
	if len(said) > saidLines {
		said = said[len(said)-saidLines:]
	}
	if len(said) > 0 && len(out) > 0 {
		out = append(out, "")
	}
	for _, line := range said {
		out = append(out, strings.Split(render.Wrap(render.Dim(line, t), max(w, 8)), "\n")...)
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	switch {
	case s.failed != "":
		out = append(out, render.Bad(s.failed, t))
	case s.link == "" && len(said) == 0:
		out = append(out, render.Dim("Starting the sign-in to "+name+"…", t))
	default:
		out = append(out, render.Dim("Waiting for you to finish signing in…", t))
	}
	return out
}

// Opens the page and runs the sign-in behind it; closing the page stops the sign-in. The page shows in the dialog the sign-in was started
// from, which then says how it ended and reads the provider again, and in a dialog of its own when it was started from none
func (s *signing) run() tea.Cmd {
	ctx, cancel := context.WithCancel(s.kit.Env.Ctx)
	hub := s.target.Hub
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title:   s.way.Title,
		Key:     "providers.signin." + s.way.Spec,
		Lines:   s.lines,
		Follow:  true,
		Context: signinContext,
		Closed: func(*cards.Page) tea.Cmd {
			cancel()
			return nil
		},
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
			"providers.signin.copy": func(*cards.Page) tea.Cmd { return s.copy(hub, s.shown(true), "the sign-in address") },
			"providers.signin.code": func(*cards.Page) tea.Cmd { return s.copy(hub, s.shown(false), "the code") },
		},
	})
	show := func() tea.Cmd { return hub.Push(page) }
	if hub == nil {
		own := s.kit.Hub("providers.signin", page)
		hub, show = own, func() tea.Cmd { return s.kit.WM.ShowOverlay(own) }
	}
	shared := s.target.Hub != nil
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
	was := connected(s.target.Provider.GetCredentialOrigin())
	return tea.Batch(show(), tui.Async(func() func() tea.Cmd {
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
				reason := failure(err, s.target.Provider.GetDisplayName())
				s.mu.Lock()
				s.failed = reason
				s.mu.Unlock()
				return hub.Fail(reason)
			}
			if shared {
				var read tea.Cmd
				if s.target.Done != nil {
					read = s.target.Done()
				}
				return tea.Batch(hub.Close(page), hub.Done(connectedNote(s.target, was, false)), read)
			}
			return tea.Batch(s.kit.WM.DismissCard(hub), s.kit.WM.Toast(connectedNote(s.target, was, true), wm.Success, wm.DefaultToastTTL))
		}
	}))
}

// Why a sign-in failed and what to do next, in words a person can act on
func failure(err error, name string) string {
	switch {
	case credentials.Cancelled(err):
		return "The sign-in stopped before it finished. Close this and connect again to retry."
	case credentials.Unreachable(err):
		return "icebath could not reach " + name + ". Check your internet connection or proxy, then connect again."
	}
	reason := strings.TrimSuffix(strings.TrimSpace(tui.Reason(err)), ".")
	if refused, ok := errors.AsType[*credentials.AuthError](err); ok {
		reason = strings.TrimSuffix(strings.TrimSpace(refused.Message), ".")
		if refused.Code == credentials.Expired {
			return reason + ". Close this and connect again for a new code."
		}
	}
	// The reason is a sentence of its own
	if reason != "" {
		reason = " " + strings.ToUpper(reason[:1]) + reason[1:] + "."
	}
	return "The sign-in failed." + reason + " Close this and try again, or pick another way to connect."
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
				return hub.Fail("The browser did not open. " + s.kit.Env.Keys.Display("providers.signin.copy") + " copies the address so you can open it yourself.")
			}
			return hub.Done("Opened the sign-in address in your browser.")
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
				return hub.Fail("Could not copy " + what + ". Select it on the screen and copy it yourself.")
			}
			return hub.Done("Copied " + what + ".")
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

// Keeps the fields the sign-in gave in the account, made now when it was waiting for the sign-in, clearing what the account last ran into
func (s *signing) save(ctx context.Context, signed credentials.Signed) error {
	if s.target.Account == nil && !s.target.Titled && signed.Identity != "" {
		s.target.Title = signed.Identity
	}
	target, err := s.target.Ready(ctx)
	if err != nil {
		return err
	}
	s.target = target
	return Store(ctx, s.settings, target, signed.Fields)
}

// Keeps fields in the account of the target, made first when it was waiting, each as YAML, nil putting its default back, and clears the
// limits the account ran into
func Store(ctx context.Context, settings configv1connect.ConfigServiceClient, target Target, fields map[string]any) error {
	target, err := target.Ready(ctx)
	if err != nil {
		return err
	}
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

// Note once an account connected: the provider ready to use when it was not before, else the account connected. Closed when the screen
// that connected it has closed already
func connectedNote(target Target, was, closed bool) string {
	name := target.Provider.GetDisplayName()
	switch {
	case was:
		return target.Label() + " of " + name + " is connected."
	case closed:
		return name + " is connected. Send a message to start."
	}
	return name + " is connected. Close this and send a message to start."
}
