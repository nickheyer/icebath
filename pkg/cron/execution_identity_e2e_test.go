package cron_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cronIdentityProbePlugin = `
import dataclasses, json, os
from pathlib import Path

CTX = None

def _record(**kwargs):
    ident = CTX.current_cron_execution()
    out = Path(os.environ["ICEBATH_HOME"]) / "seen.json"
    out.write_text(json.dumps({"tool": kwargs.get("tool_name"),
                               "identity": dataclasses.asdict(ident) if ident else None}))

def register(ctx):
    global CTX
    CTX = ctx
    ctx.register_hook("pre_tool_call", _record)
`

func writeIdentityFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Collaborators the plugin host reaches while a cron agent run fires a tool hook, wired as the binary wires them
func installIdentityProbeServices(t *testing.T) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	previous := plugins.InstalledServices()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	plugins.InstallServices(&plugins.Services{
		InvokeLifecycleHook:    agent.LifecycleInvokeHook,
		BindProfileSecretScope: provider.BindProfileSecretScope,
		GetSecret:              provider.GetSecret,
		DispatchTool:           tools.DispatchForPlugin,
		HostObject:             func(string, any) (string, bool) { return "", false },
		ProviderRegistries: map[string]plugins.ScopedRegistry{
			"image_gen_provider":            media.ImageGenScopedRegistry(),
			"video_gen_provider":            media.VideoGenScopedRegistry(),
			"tts_provider":                  media.TTSScopedRegistry(),
			"transcription_provider":        media.TranscriptionScopedRegistry(),
			"web_search_provider":           agent.WebSearchScopedRegistry(),
			"browser_provider":              browser.BrowserProviderRegistry.Scoped(),
			"browser_page_backend":          browser.BrowserPageBackendRegistry.Scoped(),
			"terminal_environment_provider": agent.TerminalEnvScopedRegistry(),
			"secret_source":                 provider.SecretSourceScopedRegistry(),
			"delegation_provider":           agent.DelegationScopedRegistry(),
			"login_backend":                 browser.LoginBackendScopedRegistry(),
		},
		CurrentCronExecution: func(ctx context.Context) any {
			if exec := cron.CurrentCronExecution(ctx); exec != nil {
				return exec
			}
			return nil
		},
	})
	t.Cleanup(func() { plugins.InstallServices(previous) })
}

func TestToolHookInATickedJobSeesItsExecutionAndNoneOutside(t *testing.T) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ICEBATH_") || strings.HasPrefix(name, "_ICEBATH_") || strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_TOKEN") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	home := filepath.Join(t.TempDir(), "home")
	pluginDir := filepath.Join(home, "plugins", "cron-identity-probe")
	writeIdentityFile(t, filepath.Join(pluginDir, "plugin.yaml"), "name: cron-identity-probe\nversion: 0.0.1\n")
	writeIdentityFile(t, filepath.Join(pluginDir, "__init__.py"), cronIdentityProbePlugin)
	note := filepath.Join(home, "note.txt")
	srv, err := fakellm.New([]any{fakellm.ToolCall{Name: "read_file", Args: map[string]any{"path": note}}, fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if err := fakellm.WriteHome(home, srv.BaseURL(), "", "plugins:\n  enabled: [cron-identity-probe]\n", observability.OfferVersion); err != nil {
		t.Fatal(err)
	}
	writeIdentityFile(t, note, "hello\n")
	nextRun := time.Now().Add(-time.Minute).Format("2006-01-02T15:04:05.000000-07:00")
	jobs, err := json.Marshal(map[string]any{"jobs": []any{map[string]any{
		"id": "probe", "name": "identity probe", "prompt": "read the note",
		"schedule":    map[string]any{"kind": "interval", "minutes": 240},
		"next_run_at": nextRun, "enabled": true, "state": "scheduled", "deliver": "local",
		"repeat": map[string]any{"times": nil, "completed": 0}}}})
	if err != nil {
		t.Fatal(err)
	}
	writeIdentityFile(t, filepath.Join(home, "cron", "jobs.json"), string(jobs))
	t.Setenv("ICEBATH_HOME", home)
	installIdentityProbeServices(t)
	cron.InstallCronOwner(t)
	cron.UseRealTickSeams(t)
	cron.UseRealDelivery(t)
	ctx := context.Background()
	if _, err := cron.TickSyncForHome(ctx, home); err != nil {
		t.Fatal(err)
	}
	probe := plugins.NewContext(nil, plugins.GetPluginManager(ctx))
	outside, err := probe.CurrentCronExecution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	executions, err := cron.ListExecutionsForHome(ctx, home, "probe")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "seen.json"))
	if err != nil {
		var transcript any
		if main := srv.MainRequests(); len(main) > 0 {
			transcript = main[len(main)-1]["messages"]
		}
		t.Fatalf("the plugin hook never recorded a tool call: %v, provider saw %d main requests ending in %v, executions %v", err, len(srv.MainRequests()), transcript, executions)
	}
	var seen struct {
		Tool     string         `json:"tool"`
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 {
		t.Fatalf("executions %v", executions)
	}
	execution := executions[0]
	if seen.Tool != "read_file" {
		t.Fatalf("tool %q", seen.Tool)
	}
	ident := seen.Identity
	if ident == nil {
		t.Fatal("pre_tool_call fired inside a cron run saw no cron identity")
	}
	if len(ident) != 7 || ident["job_id"] != "probe" || ident["job_name"] != "identity probe" || ident["execution_id"] != execution["id"] {
		t.Fatalf("identity %v execution %v", ident, execution)
	}
	if ident["source"] != "builtin" || execution["source"] != "builtin" {
		t.Fatalf("source %v %v", ident["source"], execution["source"])
	}
	if ident["scheduled_instant"] == nil || ident["scheduled_instant"] != execution["scheduled_instant"] {
		t.Fatalf("scheduled instant %v %v", ident["scheduled_instant"], execution["scheduled_instant"])
	}
	if ident["started_at"] != execution["started_at"] {
		t.Fatalf("started at %v %v", ident["started_at"], execution["started_at"])
	}
	if ident["profile"] != probe.ProfileName(ctx) {
		t.Fatalf("profile %v want %q", ident["profile"], probe.ProfileName(ctx))
	}
	if outside != nil {
		t.Fatalf("identity outside the fire %v", outside)
	}
}
