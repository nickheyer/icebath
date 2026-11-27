package app

import (
	"context"
	"errors"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/browser"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// How often an open sign in page looks whether it was closed
const watchEvery = 2 * time.Second

// Opens the link in the browser of this computer, telling the hub when it could not
func open(kit *tui.Kit, hub *cards.Hub, link string) tea.Cmd {
	if link == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		err := browser.Open(kit.Env.Ctx, link)
		return func() tea.Cmd {
			if err != nil {
				return hub.Fail("The browser didn't open: copy the link and open it yourself")
			}
			return nil
		}
	})
}

// Copies the link, telling the hub how that went
func copyLink(kit *tui.Kit, hub *cards.Hub, link string) tea.Cmd {
	if link == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		ok := clip.WriteText(kit.Env.Ctx, link)
		return func() tea.Cmd {
			if !ok {
				return hub.Fail("Couldn't copy the link")
			}
			return hub.Done("Copied the link")
		}
	})
}

// Asks the address the browser ended on, for a sign in whose browser could not reach icebath
func paste(kit *tui.Kit, hub *cards.Hub, service mcpv1connect.McpServiceClient, id string) tea.Cmd {
	if id == "" {
		return nil
	}
	field := cards.Field{Label: "Address", Placeholder: "http://127.0.0.1:…/callback?code=…", Help: "Where the browser ended after you approved"}
	return kit.Ask(hub, "Paste the address", field, "Send", func(text string) tea.Cmd {
		return tui.Call(hub, func() (*mcpv1.CallbackResponse, error) {
			return service.Callback(kit.Env.Ctx, &mcpv1.CallbackRequest{Id: id, Pasted: text})
		}, func(*mcpv1.CallbackResponse) tea.Cmd { return hub.Done("Sent, finishing the sign in…") })
	})
}

// Sign in running on a page of a hub
type signing struct {
	kit     *tui.Kit
	hub     *cards.Hub
	service mcpv1connect.McpServiceClient
	page    *cards.Page
	name    string
	id      string
	link    string
	code    string
	ended   bool
	cancel  context.CancelFunc
}

// Signs in to a server on a page pushed on the hub: the link opened in this computer's browser, copied, or the address the browser ended on
// pasted, until the server approves it or it fails; done gets the last event, closing the page stops it
func signin(kit *tui.Kit, hub *cards.Hub, service mcpv1connect.McpServiceClient, request *mcpv1.LoginRequest, done func(event *mcpv1.LoginEvent) tea.Cmd) tea.Cmd {
	ctx, cancel := context.WithCancel(kit.Env.Ctx)
	s := &signing{kit: kit, hub: hub, service: service, name: request.GetName(), cancel: cancel}
	t := kit.Env.Theme
	s.page = cards.NewPage(kit.Env, cards.PageSpec{
		Title:   "Sign in to " + s.name,
		Key:     Screen + ":signin:" + s.name,
		Context: signinContext,
		Lines: func(int) []string {
			out := []string{render.Heading("Sign in to "+s.name, t), ""}
			if s.link == "" {
				return append(out, render.Dim("Starting the sign in…", t))
			}
			if s.code != "" {
				out = append(out, "Open this link and enter the code "+render.Good(s.code, t)+":")
			} else {
				out = append(out, "Approve icebath in the browser:")
			}
			return append(out, render.Warm(s.link, t), "", render.Dim("Waiting for the server to approve. If the browser can't reach icebath, paste "+
				"the address it ended on", t))
		},
		Hints: func(*cards.Page) []wm.Hint {
			hints := []wm.Hint{{Action: "mcp.signin.open"}, {Action: "mcp.signin.copy"}}
			if s.code == "" {
				hints = append(hints, wm.Hint{Action: "mcp.signin.paste"})
			}
			return hints
		},
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"mcp.signin.open":  func(*cards.Page) tea.Cmd { return open(kit, hub, s.link) },
			"mcp.signin.copy":  func(*cards.Page) tea.Cmd { return copyLink(kit, hub, s.link) },
			"mcp.signin.paste": func(*cards.Page) tea.Cmd { return paste(kit, hub, service, s.id) },
		},
	})
	go s.run(ctx, request, done)
	go s.watch(ctx)
	return hub.Push(s.page)
}

// Reads the sign in's events: the link shown and opened, the outcome handed to done
func (s *signing) run(ctx context.Context, request *mcpv1.LoginRequest, done func(event *mcpv1.LoginEvent) tea.Cmd) {
	defer s.cancel()
	finish := func(event *mcpv1.LoginEvent) {
		s.kit.Apply(func() tea.Cmd {
			if s.ended {
				return nil
			}
			s.ended = true
			return tea.Batch(s.hub.Close(s.page), done(event))
		})
	}
	stream, err := s.service.Login(ctx, request)
	if err != nil {
		finish(&mcpv1.LoginEvent{Done: true, Error: tui.Reason(err)})
		return
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			if ctx.Err() == nil {
				finish(&mcpv1.LoginEvent{Done: true, Error: tui.Reason(err)})
			}
			return
		case event.GetDone():
			finish(event)
			return
		}
		s.kit.Apply(func() tea.Cmd {
			first := s.link == ""
			s.id, s.link, s.code = event.GetId(), event.GetLink(), event.GetCode()
			if first && !s.ended {
				return open(s.kit, s.hub, s.link)
			}
			return nil
		})
	}
}

// Stops the sign in once its page is no longer shown
func (s *signing) watch(ctx context.Context) {
	ticker := time.NewTicker(watchEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.kit.Apply(func() tea.Cmd {
			if s.ended || s.hub.Holds(s.page) && s.hub.Shown() {
				return nil
			}
			s.ended = true
			s.cancel()
			if s.id == "" {
				return nil
			}
			id := s.id
			return tui.Async(func() func() tea.Cmd {
				s.service.CancelLogin(context.WithoutCancel(s.kit.Env.Ctx), &mcpv1.CancelLoginRequest{Id: id})
				return func() tea.Cmd { return s.hub.Done("Stopped the sign in to " + s.name) }
			})
		})
	}
}
