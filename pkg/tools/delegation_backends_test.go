package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type rebuildAgentDouble struct {
	tools          []map[string]any
	backend        string
	backendSet     bool
	origin         string
	depth          int
	sessionID      string
	refreshes      int
	cacheBreaks    []string
	refreshReplace bool
}

func (a *rebuildAgentDouble) Tools() []map[string]any { return a.tools }
func (a *rebuildAgentDouble) DelegationBackend() (string, bool) {
	return a.backend, a.backendSet
}
func (a *rebuildAgentDouble) SetDelegationBackend(b string) { a.backend, a.backendSet = b, true }
func (a *rebuildAgentDouble) TurnOrigin() string            { return a.origin }
func (a *rebuildAgentDouble) DelegateDepth() int            { return a.depth }
func (a *rebuildAgentDouble) SessionID() string             { return a.sessionID }
func (a *rebuildAgentDouble) SessionEnv() *Env              { return &Env{} }

func installBackendHooks(t *testing.T) {
	t.Helper()
	saved := DelegationBackendHooks
	t.Cleanup(func() { DelegationBackendHooks = saved })
	DelegationBackendHooks.ListProviders = func(context.Context) []DelegationProvider {
		r := testDelegationRegistry
		r.mu.Lock()
		defer r.mu.Unlock()
		var out []DelegationProvider
		for _, p := range r.registered {
			out = append(out, p)
		}
		return out
	}
	DelegationBackendHooks.RefreshAgentMCPTools = func(_ context.Context, a DelegationRebuildAgent, quiet, contentAware bool) {
		d := a.(*rebuildAgentDouble)
		d.refreshes++
		if !quiet || !contentAware {
			t.Fatal("refresh is not quiet and content aware")
		}
		if d.refreshReplace {
			d.tools = append([]map[string]any{}, d.tools...)
		}
	}
	DelegationBackendHooks.RecordCacheBreak = func(a DelegationRebuildAgent, reason string) {
		d := a.(*rebuildAgentDouble)
		d.cacheBreaks = append(d.cacheBreaks, reason)
	}
}

// A switch persists the backend, and a now switch rebuilds only the asking session once at its next turn
func TestSetGlobalBackendPersistsAndQueuesTheNowRebuild(t *testing.T) {
	installBackendHooks(t)
	registerTestDelegationProvider(t, newOptionsDelegationProvider())
	ctx := delegationHome(t, "delegation:\n  model: m\n")
	if err := SetGlobalBackend(ctx, " Probe-Options ", "", false); err != nil {
		t.Fatal(err)
	}
	if got := SavedDelegationBackend(ctx); got != "probe-options" {
		t.Fatalf("saved %q", got)
	}
	if cfg := LoadDelegationConfig(ctx); cfg["model"] != "m" {
		t.Fatalf("section lost keys %v", cfg)
	}
	var be *DelegationBackendError
	if err := SetGlobalBackend(ctx, "default", "", true); !errors.As(err, &be) || be.Kind != "ValueError" || !strings.Contains(be.Message, "apply_now needs the command's session_key") {
		t.Fatal(err)
	}
	if err := SetGlobalBackend(ctx, "missing", "", false); !errors.As(err, &be) || !strings.HasPrefix(be.Message, "Delegation backend 'missing' is not installed") {
		t.Fatal(err)
	}
	if err := SetGlobalBackend(ctx, "default", "sess-1", true); err != nil {
		t.Fatal(err)
	}
	fork := &rebuildAgentDouble{sessionID: "sess-1", origin: "background_review"}
	child := &rebuildAgentDouble{sessionID: "sess-1", depth: 1}
	if ApplyRequestedToolRebuild(ctx, fork) || ApplyRequestedToolRebuild(ctx, child) || fork.refreshes+child.refreshes != 0 {
		t.Fatal("a fork or subagent took the session request")
	}
	agent := &rebuildAgentDouble{sessionID: "sess-1", tools: []map[string]any{{"function": map[string]any{"name": "delegate_task"}}}, refreshReplace: true}
	if !ApplyRequestedToolRebuild(ctx, agent) || !reflect.DeepEqual(agent.cacheBreaks, []string{"toolset_change"}) {
		t.Fatalf("rebuild %+v", agent)
	}
	if ApplyRequestedToolRebuild(ctx, agent) || agent.refreshes != 1 {
		t.Fatal("the request was taken twice")
	}
	if err := SetGlobalBackend(ctx, "default", "sess-1", true); err != nil {
		t.Fatal(err)
	}
	same := &rebuildAgentDouble{sessionID: "sess-1"}
	if ApplyRequestedToolRebuild(ctx, same) || same.refreshes != 1 || len(same.cacheBreaks) != 0 {
		t.Fatal("an unchanged tool list reported a rebuild")
	}
}

// A managed install never has its config written by a backend switch
func TestAGlobalBackendSwitchIsRefusedOnAManagedInstall(t *testing.T) {
	ctx := delegationHome(t, "delegation:\n  backend: probe-options\n")
	path := filepath.Join(home.Dir(ctx), "config.yaml")
	before, _ := os.ReadFile(path)
	t.Setenv("ICEBATH_MANAGED", "nixos")
	var be *DelegationBackendError
	if err := SetGlobalBackend(ctx, "default", "", false); !errors.As(err, &be) || be.Kind != "PermissionError" || !strings.Contains(be.Message, "managed by nixos") {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("config.yaml was written")
	}
}

// Backends list default first then registered names sorted, and the pin follows the carried definition
func TestListBackendsAndAdoptBuiltBackend(t *testing.T) {
	installBackendHooks(t)
	registerTestDelegationProvider(t, newOptionsDelegationProvider())
	got := ListDelegationBackends(context.Background())
	if len(got) != 2 || got[0].Name != "default" || !got[0].Available || got[1].Name != "probe-options" || got[1].Label != "probe-options" {
		t.Fatalf("%+v", got)
	}
	if rows := (DelegationBackends{}).ListBackends(context.Background()); rows[1]["available"] != true || rows[1]["reason"] != "" {
		t.Fatalf("%v", rows)
	}
	def := map[string]any{"function": map[string]any{"name": "delegate_task", "description": "d"}}
	a := &rebuildAgentDouble{tools: []map[string]any{def}}
	if PinnedBackend(nil) != "" || PinnedBackend(a) != "" {
		t.Fatal("unbuilt agent has a pin")
	}
	AdoptBuiltBackend(a, []map[string]any{{"function": map[string]any{"name": "delegate_task", "description": "other"}}}, "probe-options")
	if PinnedBackend(a) != "" {
		t.Fatal("a kept older definition adopted the new backend")
	}
	AdoptBuiltBackend(a, []map[string]any{{"function": map[string]any{"name": "delegate_task", "description": "d"}}}, "probe-options")
	if PinnedBackend(a) != "probe-options" {
		t.Fatal(PinnedBackend(a))
	}
}
