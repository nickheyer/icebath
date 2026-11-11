package gatewayrun

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// One recorded adapter send
type sentCall struct {
	chatID   string
	content  string
	metadata map[string]any
}

// Telegram adapter that records every send and always succeeds
type restartTestAdapter struct {
	*gateway.BasePlatformAdapter
	mu        sync.Mutex
	sent      []string
	sentCalls []sentCall
}

func (a *restartTestAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *restartTestAdapter) Disconnect(context.Context) error            { return nil }
func (a *restartTestAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, content)
	a.sentCalls = append(a.sentCalls, sentCall{chatID, content, metadata})
	return gateway.SendResult{Success: true, MessageID: "1"}, nil
}
func (a *restartTestAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Recorded sends in order
func (a *restartTestAdapter) calls() []sentCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sentCall(nil), a.sentCalls...)
}

// Enabled Telegram adapter with a token
func newRestartTestAdapter() *restartTestAdapter {
	a := &restartTestAdapter{}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "***"
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, a)
	return a
}

// Bare running runner with one Telegram platform served by the test adapter
func makeRestartRunner(t *testing.T) (*GatewayRunner, *restartTestAdapter) {
	t.Helper()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(context.Background())
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "***"
	r.config.Platforms[gateway.PlatformTelegram] = cfg
	r.running = true
	r.shutdownEvent = &pythread.Event{}
	r.sessions = map[string]*SessionState{}
	r.voiceMode = pydict.New[string, string]()
	r.backgroundTasks = map[*gatewayTask]struct{}{}
	adapter := newRestartTestAdapter()
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	return r, adapter
}
