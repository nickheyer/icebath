package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Chat completions requests built for a wire that sends a translation of them, as the native Gemini wire does, and the chunks of its replies
// put back together as a chat completions stream's are: the request shaped by the hooks of its kind, the reply read the same way
type Builder struct {
	r *registrant
}

func NewBuilder(registry *transport.Registry, cfg *config.Config, providers *provider.Registry, m *models.Models) *Builder {
	return &Builder{r: newRegistrant(registry, cfg, providers, m)}
}

// One request built for a route
type Built struct {
	c *client
	p *prepared
}

func (b *Builder) Build(ctx context.Context, route provider.Route, request *provider.Request) (*Built, error) {
	c := &client{r: b.r, route: route}
	p, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	return &Built{c: c, p: p}, nil
}

// Body as the chat completions API would take it
func (b *Built) Body() map[string]any { return b.p.body }

func (b *Built) Limits() transport.Limits { return b.p.limits }

func (b *Built) Refused() transport.Refused { return b.p.refused }

func (b *Built) Tokens() int { return b.p.tokens }

// Settings of the request's provider
func (b *Built) Settings() config.View { return b.p.settings }

// Reasoning the request asks, its kind's default filled in
func (b *Built) Ask() provider.Ask { return b.p.call.Ask }

// Reply of a whole chat completion payload
func (b *Built) Normalize(payload map[string]any, header http.Header) (*provider.Response, error) {
	return b.c.normalize(b.p, payload, header)
}

// Reply put together from chat completion chunks as they arrive, its deltas sent on as a stream's are
type Chunks struct {
	s *stream
}

func (b *Built) Chunks(fn provider.StreamFunc, header http.Header, status int) *Chunks {
	return &Chunks{s: newStream(b.c, b.p, fn, &http.Response{Header: header, StatusCode: status})}
}

// Folds one chunk into the reply, true once the reply must stop
func (c *Chunks) Feed(chunk map[string]any) (bool, error) {
	if err := c.s.emit(provider.EventActivity, "receiving the reply"); err != nil {
		return true, Unstopped(err)
	}
	stop, err := c.s.chunk(chunk)
	return stop, Unstopped(err)
}

// Whether something of the reply reached the person
func (c *Chunks) Delivered() bool { return c.s.delivered }

// Reply once the chunks ended
func (c *Chunks) Finish() (*provider.Response, error) {
	response, err := c.s.finish()
	return response, Unstopped(err)
}

// Error the consumer of a stream's events returned, as it was, any other error as it is
func Unstopped(err error) error {
	if stop, ok := errors.AsType[*stopped](err); ok {
		return stop.err
	}
	return err
}
