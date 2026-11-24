package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/mcp"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func rpcCode(response map[string]any) int {
	failure, _ := response["error"].(map[string]any)
	switch code := failure["code"].(type) {
	case float64:
		return int(code)
	case int:
		return code
	case int64:
		return int(code)
	}
	return 0
}

// OAuth attempt of the fake MCP backend, pending until the case approves it
type fakeMCPAttempt struct {
	mu     sync.Mutex
	url    string
	status string
}

func (a *fakeMCPAttempt) AuthURL() string            { return a.url }
func (a *fakeMCPAttempt) Detail() string             { return "" }
func (a *fakeMCPAttempt) Flow() *mcp.ClientOAuthFlow { return nil }

// Approved attempts carry a discovery error so no server is dialed
func (a *fakeMCPAttempt) Poll() mcp.AttemptPoll {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "approved" {
		return mcp.AttemptPoll{Status: a.status, DiscoveryError: "tools are listed once the server is reachable"}
	}
	return mcp.AttemptPoll{Status: a.status}
}

// MCP backend whose OAuth starts are recorded by server name, a start failing with the reason set for its name
type fakeMCPBackend struct {
	mu       sync.Mutex
	base     string
	starts   []string
	failures map[string]string
	attempts map[string]*fakeMCPAttempt
}

func (b *fakeMCPBackend) RequiredEnv(context.Context, string) ([]*jsonutil.Object, error) {
	return nil, nil
}
func (b *fakeMCPBackend) InstallsWithOAuth(context.Context, string) (bool, error) { return true, nil }
func (b *fakeMCPBackend) Install(context.Context, string, *jsonutil.Object) ([]string, error) {
	return nil, errors.New("install is not driven by these cases")
}
func (b *fakeMCPBackend) Enable(context.Context, string) error { return nil }
func (b *fakeMCPBackend) StartInstallOAuth(ctx context.Context, name string, _ *jsonutil.Object) (mcp.ConnectorAttempt, error) {
	return b.StartOAuth(ctx, name)
}

// Link numbered by the server's start count, or the failure set for the server
func (b *fakeMCPBackend) StartOAuth(_ context.Context, name string) (mcp.ConnectorAttempt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts = append(b.starts, name)
	if reason := b.failures[name]; reason != "" {
		return nil, errors.New(reason)
	}
	count := 0
	for _, started := range b.starts {
		if started == name {
			count++
		}
	}
	attempt := &fakeMCPAttempt{url: fmt.Sprintf("%s%s/%d", b.base, name, count), status: "pending"}
	b.attempts[name] = attempt
	return attempt, nil
}

// Server names of every OAuth start so far
func (b *fakeMCPBackend) started() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.starts)
}

// Sets or clears the failure of the next starts of one server
func (b *fakeMCPBackend) fail(name, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[name] = reason
}

// Approves the server's open attempt
func (b *fakeMCPBackend) setConnected(name string) {
	b.mu.Lock()
	attempt := b.attempts[name]
	b.mu.Unlock()
	attempt.mu.Lock()
	attempt.status = "approved"
	attempt.mu.Unlock()
}
