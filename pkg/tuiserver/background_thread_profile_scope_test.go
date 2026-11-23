package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Home and secret a callback saw, the error text when the secret read failed closed
type scopeSeen struct{ home, key string }

func observeScope(ctx context.Context) scopeSeen {
	value, _, err := provider.GetSecret(ctx, "TITLE_KEY")
	if err != nil {
		value = err.Error()
	}
	return scopeSeen{home.Dir(ctx), value}
}

// Memory provider recording the scope of the session end commit and of the close
type scopeRecorder struct {
	memory.BaseProvider
	mu             sync.Mutex
	commit, closed *scopeSeen
	done           chan struct{}
	once           sync.Once
}

func (p *scopeRecorder) Name() string                                          { return "scope-recorder" }
func (p *scopeRecorder) Available(context.Context) bool                        { return true }
func (p *scopeRecorder) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (p *scopeRecorder) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (p *scopeRecorder) OnSessionEnd(ctx context.Context, _ []map[string]any) error {
	seen := observeScope(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commit == nil {
		p.commit = &seen
	}
	return nil
}
func (p *scopeRecorder) Shutdown(ctx context.Context) error {
	seen := observeScope(ctx)
	p.mu.Lock()
	if p.closed == nil {
		p.closed = &seen
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}
