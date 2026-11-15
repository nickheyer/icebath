//go:build !windows

package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestBackgroundTaskUnderMultiplexRunsTerminalWithTheServedProfilesSecrets(t *testing.T) {
	launch := filepath.Join(t.TempDir(), "launch")
	served := filepath.Join(launch, "profiles", "beta")
	record := filepath.Join(t.TempDir(), "terminal.out")
	probe := `{ [ "$SKILL_TOKEN" = skill-secret ] && echo skill=served || echo skill=other; ` +
		`echo "launch=${LAUNCH_ONLY_TOKEN:-MISSING}"; echo "home=$ICEBATH_HOME"; } > '` + record + `'`
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
	t.Setenv("SKILL_TOKEN", "launch-skill")
	if err := fakellm.WriteHome(served, server.BaseURL(), "", "", 0); err != nil {
		t.Fatal(err)
	}
	appendFileLines(t, filepath.Join(served, ".env"), "SKILL_TOKEN=skill-secret\n")
	if err := fakellm.WriteHome(launch, server.BaseURL(), "", "", 0); err != nil {
		t.Fatal(err)
	}
	appendFileLines(t, filepath.Join(launch, ".env"), "LAUNCH_ONLY_TOKEN=launch-secret\n")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
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
	profile := "beta"
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001", ChatType: "group", Profile: &profile}
	if err := r.runBackgroundTask(ctx, "probe the terminal env", source, "bg-scope", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		var toolResults []any
		for _, request := range server.MainRequests() {
			messages, _ := request["messages"].([]any)
			for _, rawMessage := range messages {
				if message, _ := rawMessage.(map[string]any); message["role"] == "tool" {
					toolResults = append(toolResults, message["content"])
				}
			}
		}
		t.Fatalf("the background task's terminal command never ran: %v, tool results %v", err, toolResults)
	}
	want := []string{"skill=served", "launch=MISSING", "home=" + served}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("background terminal saw %q, want %q", got, want)
	}
}
