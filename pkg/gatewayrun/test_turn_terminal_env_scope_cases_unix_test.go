//go:build !windows

package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Appends lines to a file, creating it when missing
func appendFileLines(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayTurnUnderMultiplexRunsTerminalWithTheServedProfilesSecrets(t *testing.T) {
	launch := filepath.Join(t.TempDir(), "launch")
	served := filepath.Join(launch, "profiles", "beta")
	record := filepath.Join(t.TempDir(), "terminal.out")
	probe := `{ [ "$SVC_TOKEN" = served-secret ] && echo svc=served || echo svc=other; ` +
		`[ "$SKILL_TOKEN" = skill-secret ] && echo skill=served || echo skill=other; ` +
		`echo "launch=${LAUNCH_ONLY_TOKEN:-MISSING}"; echo "key=${OPENROUTER_API_KEY:-}"; echo "home=$ICEBATH_HOME"; } > '` + record + `'`
	server, err := fakellm.New([]any{fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": probe}}, fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	if err := os.MkdirAll(filepath.Join(launch, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-"+strings.Repeat("f", 48))
	t.Setenv("SKILL_TOKEN", "launch-skill")
	t.Setenv("SVC_TOKEN", "")
	os.Unsetenv("SVC_TOKEN")
	if err := fakellm.WriteHome(served, server.BaseURL(), "", "terminal:\n  env_passthrough: [SVC_TOKEN]\n", 0); err != nil {
		t.Fatal(err)
	}
	appendFileLines(t, filepath.Join(served, ".env"), "SVC_TOKEN=served-secret\nSKILL_TOKEN=skill-secret\nGATEWAY_ALLOW_ALL_USERS=true\n")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	if err := fakellm.WriteHome(launch, server.BaseURL(), "", "", 0); err != nil {
		t.Fatal(err)
	}
	appendFileLines(t, filepath.Join(launch, ".env"), "LAUNCH_ONLY_TOKEN=launch-secret\n")
	for _, k := range []string{"TELEGRAM_ALLOWED_USERS", "GATEWAY_ALLOWED_USERS", "TELEGRAM_ALLOW_ALL_USERS"} {
		t.Setenv(k, "")
	}
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	adapter := newCleanupCaptureAdapter()
	r := makeTurnRunner(t, adapter)
	r.config = gateway.DefaultGatewayConfig(ctx)
	on := true
	r.config.MultiplexProfiles = &on
	r.config.Platforms[gateway.PlatformTelegram] = &gateway.PlatformConfig{Enabled: true, Token: "***"}
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(launch, "sessions"), r.config, nil)
	t.Cleanup(r.sessionStore.CloseAllDBHandles)
	r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	r.sessions = map[string]*SessionState{}
	r.agentCache, r.agentCacheLock = pydict.New[string, agentCacheEntry](), &sync.Mutex{}
	event := dispatchEvent("probe the terminal env", false)
	profile := "beta"
	event.Source.Profile = &profile
	reply, err := r.makeProfileMessageHandler(ctx, profile).fn(ctx, event)
	for _, key := range r.agentCache.Keys() {
		if a := r.cachedAgentFor(key, true); a != nil {
			t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
		}
	}
	if err != nil || reply != "done" {
		t.Fatalf("turn reply %#v err %v", reply, err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		var toolResults []any
		for _, request := range server.MainRequests() {
			messages, _ := request["messages"].([]any)
			for _, raw := range messages {
				if message, _ := raw.(map[string]any); message["role"] == "tool" {
					toolResults = append(toolResults, message["content"])
				}
			}
		}
		t.Fatalf("the turn's terminal command never ran: %v, tool results %v", err, toolResults)
	}
	want := []string{"svc=served", "skill=served", "launch=MISSING", "key=", "home=" + served}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("terminal saw %q, want %q", got, want)
	}
}
