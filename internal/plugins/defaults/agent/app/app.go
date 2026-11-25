package app

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	agents := agentv1connect.NewAgentServiceClient(conn)
	return []tui.Screen{
		{Title: "chat", Open: func(context.Context) (tui.Screen, error) {
			return (&chat{sessions: sessions, agents: agents, created: make(chan struct{})}).screen("chat"), nil
		}},
		{Title: "sessions", Screens: func(ctx context.Context) ([]tui.Screen, error) {
			response, err := sessions.List(ctx, &sessionsv1.ListRequest{})
			if err != nil {
				return nil, err
			}
			list := make([]tui.Screen, len(response.GetSessions()))
			for i, session := range response.GetSessions() {
				c := &chat{sessions: sessions, agents: agents, id: session.GetId(), created: make(chan struct{})}
				close(c.created)
				list[i] = c.screen(cmp.Or(session.GetTitle(), "untitled"))
			}
			return list, nil
		}},
	}
}

type chat struct {
	sessions sessionsv1connect.SessionServiceClient
	agents   agentv1connect.AgentServiceClient
	created  chan struct{}

	mu sync.Mutex
	id string
}

func (c *chat) screen(title string) tui.Screen {
	return tui.Screen{Title: title, Watch: c.watch, Send: c.send, Stop: c.stop}
}

func (c *chat) session(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id != "" {
		return c.id, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	response, err := c.sessions.Create(ctx, &sessionsv1.CreateRequest{Directory: directory})
	if err != nil {
		return "", err
	}
	c.id = response.GetSession().GetId()
	close(c.created)
	return c.id, nil
}

func (c *chat) watch(ctx context.Context, show func([]string)) error {
	show(nil)
	select {
	case <-c.created:
	case <-ctx.Done():
		return nil
	}
	id, err := c.session(ctx)
	if err != nil {
		return err
	}
	stream, err := c.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: id})
	if err != nil {
		return err
	}
	defer stream.Close()
	var t transcript
	for {
		response, err := stream.Receive()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		show(t.apply(response))
	}
}

func (c *chat) send(ctx context.Context, text string) error {
	id, err := c.session(ctx)
	if err != nil {
		return err
	}
	_, err = c.agents.Send(ctx, &agentv1.SendRequest{SessionId: id, Text: text})
	return err
}

func (c *chat) stop(ctx context.Context) error {
	select {
	case <-c.created:
	default:
		return nil
	}
	id, err := c.session(ctx)
	if err != nil {
		return err
	}
	_, err = c.agents.Stop(ctx, &agentv1.StopRequest{SessionId: id})
	return err
}
