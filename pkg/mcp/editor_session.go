package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Indexes whose named connection under the editor scope has a live session, ascending
func EditorConnected(scope string, names map[int]string) []int {
	ctx := tools.WithSessionToolScope(context.Background(), scope)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	var out []int
	for i, name := range names {
		s, ok := mcpServers[serverKey(ctx, name, "", true)]
		if !ok || s == nil {
			continue
		}
		s.mu.Lock()
		live := s.session != nil
		s.mu.Unlock()
		if live {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Connects raw server configs in the bound tool scope without interpolating them, returning every tool name
func RegisterRawServers(ctx context.Context, env *tools.Env, configs map[string]map[string]any) ([]string, error) {
	servers := make(map[string]*Rendered, len(configs))
	for name, cfg := range configs {
		servers[name] = &Rendered{Config: cfg, strs: map[string]renderRecord{}}
	}
	return Connect(ctx, env, servers)
}

// Relisted tools of one held server, nil when it has no session
func relistHeld(ctx context.Context, s *serverTask) ([]*Tool, error) {
	if err := s.rpc.Lock(ctx); err != nil {
		return nil, err
	}
	defer s.rpc.Unlock()
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session == nil {
		return nil, nil
	}
	return session.ListTools(ctx, nil)
}

// Whether a held server's live tools list differs from its discovered one, an unanswered list counting as unchanged
func EditorToolsDrifted(scope string, names []string, timeout time.Duration) bool {
	scoped := tools.WithSessionToolScope(context.Background(), scope)
	mcpMu.Lock()
	var held []*serverTask
	for _, name := range names {
		if s, ok := mcpServers[serverKey(scoped, name, "", true)]; ok && s != nil {
			held = append(held, s)
		}
	}
	mcpMu.Unlock()
	type answer struct {
		listed []*Tool
		err    error
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	answers := make([]chan answer, len(held))
	for i, s := range held {
		answers[i] = make(chan answer, 1)
		go func(s *serverTask, out chan answer) {
			listed, err := relistHeld(ctx, s)
			out <- answer{listed, err}
		}(s, answers[i])
	}
	drifted := false
	for i, s := range held {
		var got answer
		select {
		case got = <-answers[i]:
		case <-ctx.Done():
			got = answer{err: ctx.Err()}
		}
		if got.err != nil {
			mcpLogger().Warn(fmt.Sprintf("Editor MCP server %s: tools/list for the drift check failed: %s", pyRepr(s.name), excStr(got.err)))
			continue
		}
		if got.listed == nil {
			continue
		}
		s.mu.Lock()
		discovered := s.tools
		s.mu.Unlock()
		now, _ := json.Marshal(got.listed)
		before, _ := json.Marshal(discovered)
		if string(now) != string(before) {
			drifted = true
		}
	}
	return drifted
}

// Closes every connection the editor scope owns and deregisters its tools
func DisconnectScope(ctx context.Context, scope string) {
	ctx = tools.WithSessionToolScope(ctx, scope)
	owned := mcpRegistryScope(ctx)
	ShutdownMCPServers(ctx, &owned, nil, shutdownTimeout)
}
