package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/pkg/browser"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/strs"
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

// Opens the page as the next step of the job that started it and runs the sign-in behind it; going back from the page stops the sign-in.
// Once it signs in, what it gave is kept and the job goes on as the target says
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
	if s.target.Next != nil {
		show = func() tea.Cmd { return s.target.Next(page) }
	}
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
	return tea.Batch(show(), tui.Async(func() func() tea.Cmd {
		signed, err := s.way.Run(prompter)
		// A sign-in the person left is dropped. One that finished while they stayed is kept to the end even if they leave meanwhile, so
		// neither a provider nor an account is left half made
		left := ctx.Err() != nil
		var account *providerv1.Account
		if err == nil && !left {
			account, err = s.target.Keep(context.WithoutCancel(ctx), signed.Fields, signed.Identity)
		}
		return func() tea.Cmd {
			cancel()
			switch {
			case err == nil && !left:
				return s.target.Done(account)
			case left:
				// The page is told it closed when its dialog leaves the screen too, and a menu shows it again as it was left
				s.mu.Lock()
				s.failed = "The sign-in stopped when this page closed. Go back and try again."
				s.mu.Unlock()
				return nil
			}
			reason := failure(err, s.target.Provider.GetDisplayName())
			s.mu.Lock()
			s.failed = reason
			s.mu.Unlock()
			return hub.Fail(reason)
		}
	}))
}

// Why a sign-in failed and what to do next, in words a person can act on
func failure(err error, name string) string {
	switch {
	case credentials.Cancelled(err):
		return "The sign-in stopped before it finished. Go back and try again."
	case credentials.Unreachable(err):
		return "icebath could not reach " + name + ". Check your internet connection or proxy, then go back and try again."
	}
	reason := strings.TrimSuffix(strings.TrimSpace(tui.Reason(err)), ".")
	if refused, ok := errors.AsType[*credentials.AuthError](err); ok {
		reason = strings.TrimSuffix(strings.TrimSpace(refused.Message), ".")
		if refused.Code == credentials.Expired {
			return reason + ". Go back and try again for a new code."
		}
	}
	// The reason is a sentence of its own
	if reason != "" {
		reason = " " + strs.Capital(reason) + "."
	}
	return "The sign-in failed." + reason + " Go back and try again, or pick another way to connect."
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
	// The answer, or nil once the form closed with none given, which nothing can answer any more
	answer := make(chan *string, 1)
	field := cards.Field{Label: strings.TrimRight(strings.TrimSpace(label), ":")}
	if secret {
		field.Kind = cards.FieldSecret
	}
	s.kit.Apply(func() tea.Cmd {
		if ctx.Err() != nil {
			return nil
		}
		return s.kit.AskOr(hub, s.way.Title, field, "Continue", func(text string) tea.Cmd {
			answer <- &text
			return nil
		}, func() tea.Cmd {
			answer <- nil
			return nil
		})
	})
	select {
	case text := <-answer:
		if text == nil {
			return "", false
		}
		return *text, true
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

// Settings of the provider by their names below it, as the profile has them; none for a provider not added yet
func (s *signing) values(ctx context.Context) map[string]string {
	values := map[string]string{}
	if s.target.Provider.GetSection() == "" {
		return values
	}
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
