package gatewayrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const configLoadersConfig = "platform_toolsets: {cli: []}\n" +
	"model: {provider: custom, default: m, base_url: \"http://127.0.0.1:9/v1\"}\n" +
	"provider_routing: {only: [Fresh], sort: price}\n"

var errAgentBuilt = errors.New("agent built")

type configLoadersOwner struct {
	ctx       context.Context
	runner    *GatewayRunner
	authority *gateway.SessionAuthority
	owner     *gateway.AuthorityConnection
	sid       string
	mu        sync.Mutex
	built     *agent.AIAgentOptions
}

func (o *configLoadersOwner) dispatch(t *testing.T, id, method string, params *pyjson.Object) *pyjson.Object {
	t.Helper()
	response, err := o.owner.Dispatch(o.ctx, pyjson.ObjectOf("id", id, "method", method, "params", params))
	if err != nil {
		t.Fatal(err)
	}
	result, ok := response.Get("result")
	if !ok {
		t.Fatalf("%s answered %s", method, picksDump(response))
	}
	out, _ := result.(*pyjson.Object)
	return out
}

// Real runner whose daemon start routing differs from its profile's, one cli session created, agents recorded
func newConfigLoadersOwner(t *testing.T) *configLoadersOwner {
	t.Helper()
	installContractPrefillLoaders(t)
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(configLoadersConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &configLoadersOwner{ctx: context.Background()}
	prev := newAIAgent
	newAIAgent = func(ctx context.Context, opts agent.AIAgentOptions) (*agent.AIAgent, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.built = &opts
		return nil, errAgentBuilt
	}
	t.Cleanup(func() { newAIAgent = prev })
	prevBudget := gateway.NormalizeRunBudgetSeconds
	gateway.NormalizeRunBudgetSeconds = agent.NormalizeRunBudgetSeconds
	t.Cleanup(func() { gateway.NormalizeRunBudgetSeconds = prevBudget })
	runner, err := NewGatewayRunner(o.ctx, gateway.DefaultGatewayConfig(o.ctx))
	if err != nil {
		t.Fatal(err)
	}
	runner.providerRouting = map[string]any{"only": []any{"DaemonStart"}}
	authority, err := gateway.InitializeSessionAuthority(o.ctx, runner, "default", "owner", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := gateway.NewAuthorityConnection(authority, picksTransport{}, map[string]any{"user_id": "owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = owner.Close(o.ctx)
		runner.sessionStore.CloseAllDBHandles()
	})
	o.runner, o.authority, o.owner = runner, authority, owner
	created := o.dispatch(t, "c", "session.create", pyjson.ObjectOf("request_id", "bg", "source", "cli", "cwd", home, "toolsets", []any{}))
	sid, _ := created.Get("session_id")
	o.sid, _ = sid.(string)
	return o
}

// Routing keywords the recorded agent was built with match the session's own provider routing
func (o *configLoadersOwner) assertFreshRouting(t *testing.T) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.built == nil {
		t.Fatalf("no agent was built")
	}
	if !reflect.DeepEqual(o.built.ProvidersAllowed, []any{"Fresh"}) || o.built.ProviderSort != "price" {
		t.Fatalf("agent routing %#v %#v", o.built.ProvidersAllowed, o.built.ProviderSort)
	}
}

func TestInProcessTurnRoutesWithTheSessionsOwnProviderRouting(t *testing.T) {
	o := newConfigLoadersOwner(t)
	live, ok := o.authority.Live(o.sid)
	if !ok {
		t.Fatalf("no live session %s", o.sid)
	}
	turn := &TurnRunner{runner: o.runner, turnCtx: o.ctx, ctx: &TurnContext{source: live.Source, sessionKey: live.Route,
		message: "hi", userConfig: map[string]any{}, enabledToolsets: []string{}}}
	if _, err := turn.runSyncScoped(o.ctx); !errors.Is(err, errAgentBuilt) {
		t.Fatalf("turn answered %v", err)
	}
	o.assertFreshRouting(t)
}

func TestBgSideAgentUsesTheSessionsProviderRouting(t *testing.T) {
	o := newConfigLoadersOwner(t)
	o.dispatch(t, "r", "session.resume", pyjson.ObjectOf("session_id", o.sid))
	reply := o.dispatch(t, "b", "prompt.background", pyjson.ObjectOf("session_id", o.sid, "text", "BG"))
	taskID, _ := reply.Get("task_id")
	if text, _ := taskID.(string); !strings.HasPrefix(text, "bg_") {
		t.Fatalf("background reply %s", picksDump(reply))
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, _, sides := o.authority.UnserveStopCounts(); sides == 0 {
			break
		}
	}
	o.assertFreshRouting(t)
}
