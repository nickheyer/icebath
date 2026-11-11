package plugins

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type paAdapter struct {
	platform  string
	connected bool
	reacted   []string
	renamed   []string
	setOK     bool
	setErr    error
	renameOK  bool
	client    DiscordActionClient
}

func (a *paAdapter) PlatformValue() string { return a.platform }
func (a *paAdapter) IsConnected() bool     { return a.connected }
func (a *paAdapter) SetReaction(_ context.Context, chat, msg, emoji string) (bool, error) {
	a.reacted = append(a.reacted, chat+"/"+msg+"/"+emoji)
	return a.setOK, a.setErr
}
func (a *paAdapter) RenameDMTopic(_ context.Context, chat string, thread *big.Int, title string) error {
	a.renamed = append(a.renamed, chat+"/"+thread.String()+"/"+title)
	return nil
}
func (a *paAdapter) RenameThread(_ context.Context, thread, title string) (bool, error) {
	a.renamed = append(a.renamed, thread+"/"+title)
	return a.renameOK, nil
}

type paDiscordAdapter struct{ *paAdapter }

func (a paDiscordAdapter) DiscordClient() DiscordActionClient { return a.client }

type paClient struct{ log *[]string }
type paChannel struct{ log *[]string }
type paMessage struct{ log *[]string }

func (c paClient) GetChannel(id *big.Int) DiscordActionChannel {
	*c.log = append(*c.log, "get "+id.String())
	return nil
}
func (c paClient) FetchChannel(_ context.Context, id *big.Int) (DiscordActionChannel, error) {
	*c.log = append(*c.log, "fetch_channel "+id.String())
	return paChannel(c), nil
}
func (c paChannel) FetchMessage(_ context.Context, id *big.Int) (DiscordActionMessage, error) {
	*c.log = append(*c.log, "fetch_message "+id.String())
	return paMessage(c), nil
}
func (m paMessage) AddReaction(_ context.Context, emoji string) error {
	*m.log = append(*m.log, "react "+emoji)
	return nil
}

type paRunner struct{ adapters map[string]any }

func (r paRunner) ParsePlatform(v string) (string, error) {
	switch v {
	case "telegram", "discord", "slack":
		return v, nil
	}
	return "", fmt.Errorf("%q is not a valid Platform", v)
}

// Default profile registry lookup of the fake runner, the runner.adapters.get the case reads
func paAdapterByValue(runner any, value string) (any, bool) {
	r, ok := runner.(paRunner)
	if !ok {
		return nil, false
	}
	return r.adapters[value], true
}

// Runner with the fail closed profile ladder, the default profile reading the default adapters
type paMultiplexRunner struct {
	paRunner
	profiles map[string]map[string]any
}

func (r paMultiplexRunner) AuthorizationAdapter(platform, profile string) any {
	if profile == "default" {
		return r.adapters[platform]
	}
	if a, ok := r.profiles[profile][platform]; ok {
		return a
	}
	return nil
}

// Home whose config grants or denies the capability, with the runner installed as the live gateway
func paSetup(t *testing.T, cfg string, runner PlatformActionRunner) context.Context {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	h := filepath.Join(root, ".icebath")
	memWrite(t, filepath.Join(h, "config.yaml"), cfg)
	previous, previousLookup := installedServices(), RunnerAdapterByValue
	InstallServices(&Services{GatewayRunnerRef: func() PlatformActionRunner { return runner }})
	RunnerAdapterByValue = paAdapterByValue
	t.Cleanup(func() { InstallServices(previous); RunnerAdapterByValue = previousLookup })
	return home.WithOverride(context.Background(), h)
}

const paGranted = "plugins:\n  entries:\n    p:\n      granted_capabilities: [gateway.platform_actions]\n"

func telegramPA() *paAdapter { return &paAdapter{platform: "telegram", connected: true, setOK: true} }

// test_platform_actions.py TestGateDefaultOff, all five cases
func TestPlatformActionsGateDefaultOff(t *testing.T) {
	a := telegramPA()
	runner := paRunner{map[string]any{"telegram": a}}
	pa := &PlatformActions{pluginID: "p"}
	for cfg, wantOK := range map[string]bool{
		"{}\n": false, "plugins:\n  entries:\n    p:\n      allow_platform_actions: true\n": true, paGranted: true, "plugins: [unclosed\n": false,
	} {
		ctx := paSetup(t, cfg, runner)
		r := pa.AddReaction(ctx, "telegram", "123", "456", "\U0001F44D")
		if r["ok"] != wantOK || !wantOK && (r["error"] != "capability_not_granted" ||
			r["detail"] != "plugin 'p' lacks the 'gateway.platform_actions' capability (grant via consent flow or plugins.entries.p.allow_platform_actions)") {
			t.Fatalf("%q = %v", cfg, r)
		}
	}
}

// test_platform_actions.py TestStructuredErrors, all cases
func TestPlatformActionsStructuredErrors(t *testing.T) {
	pa := &PlatformActions{pluginID: "p"}
	ctx := paSetup(t, paGranted, nil)
	if r := pa.AddReaction(ctx, "telegram", "1", "2", "x"); r["error"] != "gateway_unavailable" {
		t.Fatalf("no runner %v", r)
	}
	a := telegramPA()
	ctx = paSetup(t, paGranted, paRunner{map[string]any{"telegram": a}})
	cases := map[string]map[string]any{
		"unknown_platform":       pa.AddReaction(ctx, "myspace", "1", "2", "x"),
		"adapter_not_registered": pa.AddReaction(ctx, "discord", "1", "2", "x"),
	}
	for code, r := range cases {
		if r["error"] != code {
			t.Fatalf("%s = %v", code, r)
		}
	}
	for _, bad := range []any{"", "   ", nil, 123} {
		if r := pa.AddReaction(ctx, "telegram", "1", "2", bad); r["error"] != "invalid_argument" || r["detail"] != "emoji must be a non-empty string" {
			t.Fatalf("bad %v = %v", bad, r)
		}
	}
	a.connected = false
	if r := pa.AddReaction(ctx, "telegram", "1", "2", "x"); r["error"] != "adapter_disconnected" || r["detail"] != "the telegram adapter is not connected" {
		t.Fatalf("disconnected %v", r)
	}
	a.connected, a.setErr = true, errors.New("api down")
	if r := pa.AddReaction(ctx, "telegram", "1", "2", "x"); r["error"] != "action_failed" || !strings.Contains(r["detail"].(string), "api down") {
		t.Fatalf("raising adapter %v", r)
	}
	slack := &paAdapter{platform: "slack", connected: true}
	ctx = paSetup(t, paGranted, paRunner{map[string]any{"slack": slack}})
	if r := pa.AddReaction(ctx, "slack", "1", "2", "x"); r["error"] != "unsupported_platform_action" || r["detail"] != "add_reaction is not implemented for slack" {
		t.Fatalf("unsupported %v", r)
	}
}

// test_platform_actions.py TestVerbRouting, all seven cases
func TestPlatformActionsVerbRouting(t *testing.T) {
	pa := &PlatformActions{pluginID: "p"}
	tg := telegramPA()
	log := []string{}
	dc := paDiscordAdapter{&paAdapter{platform: "discord", connected: true, renameOK: true, client: paClient{&log}}}
	ctx := paSetup(t, paGranted, paRunner{map[string]any{"telegram": tg, "discord": dc}})
	if r := pa.AddReaction(ctx, "Telegram ", "123", "456", "x"); r["ok"] != true || r["action"] != "add_reaction" || tg.reacted[0] != "123/456/x" {
		t.Fatalf("telegram reaction %v %v", r, tg.reacted)
	}
	tg.setOK = false
	if r := pa.AddReaction(ctx, "telegram", "1", "2", "x"); r["error"] != "action_failed" || r["detail"] != "telegram set_message_reaction failed" {
		t.Fatalf("telegram false %v", r)
	}
	if r := pa.SetThreadTitle(ctx, "telegram", "123", "77", "New title"); r["ok"] != true || tg.renamed[0] != "123/77/New title" {
		t.Fatalf("telegram topic %v %v", r, tg.renamed)
	}
	if r := pa.SetThreadTitle(ctx, "telegram", "123", "abc", "t"); r["error"] != "action_failed" || r["detail"] != "invalid literal for int() with base 10: 'abc'" {
		t.Fatalf("telegram bad thread %v", r)
	}
	if r := pa.SetThreadTitle(ctx, "discord", "123", "999", "Renamed"); r["ok"] != true || dc.renamed[0] != "999/Renamed" {
		t.Fatalf("discord rename %v %v", r, dc.renamed)
	}
	dc.renameOK = false
	if r := pa.SetThreadTitle(ctx, "discord", "123", "999", "x"); r["error"] != "action_failed" || r["detail"] != "discord thread rename failed" {
		t.Fatalf("discord false %v", r)
	}
	if r := pa.AddReaction(ctx, "discord", "111", "222", "y"); r["ok"] != true || strings.Join(log, ",") != "get 111,fetch_channel 111,fetch_message 222,react y" {
		t.Fatalf("discord reaction %v %v", r, log)
	}
	if r := pa.AddReaction(ctx, "discord", "abc", "222", "y"); r["error"] != "invalid_argument" || r["detail"] != "discord ids must be numeric" {
		t.Fatalf("discord ids %v", r)
	}
}

// test_platform_actions.py TestMultiplexProfileRouting, a named profile and an unregistered one failing closed
func TestPlatformActionsMultiplexProfileRouting(t *testing.T) {
	pa := &PlatformActions{pluginID: "p"}
	def, teamB := telegramPA(), telegramPA()
	runner := paMultiplexRunner{paRunner{map[string]any{"telegram": def}}, map[string]map[string]any{"team-b": {"telegram": teamB}}}
	ctx := paSetup(t, paGranted, runner)
	profile := filepath.Join(home.Dir(ctx), "profiles", "team-b")
	memWrite(t, filepath.Join(profile, "config.yaml"), paGranted)
	if r := pa.AddReaction(home.WithOverride(ctx, profile), "telegram", "1", "2", "x"); r["ok"] != true || len(teamB.reacted) != 1 || len(def.reacted) != 0 {
		t.Fatalf("team-b %v", r)
	}
	runner.profiles = map[string]map[string]any{}
	ctx = paSetup(t, paGranted, runner)
	if r := pa.AddReaction(home.WithOverride(ctx, profile), "telegram", "1", "2", "x"); r["error"] != "adapter_not_registered" || len(def.reacted) != 0 {
		t.Fatalf("unregistered profile %v", r)
	}
}

// test_platform_actions.py TestPluginContextWiring plus the hosted binding of both verbs
func TestPlatformActionsContextWiringAndHostCall(t *testing.T) {
	c := newContext(&Manifest{Name: "actions-fixture", Key: "actions-fixture", Source: "user"}, NewManager(t.TempDir()))
	facade, _ := c.PlatformActions()
	again, _ := c.PlatformActions()
	pa, ok := facade.(*PlatformActions)
	if !ok || pa.pluginID != "actions-fixture" || again != facade {
		t.Fatalf("facade %#v", facade)
	}
	if _, isMethod, async, err := pa.HostAttr(context.Background(), "set_thread_title"); !isMethod || !async || err != nil {
		t.Fatal("set_thread_title is not a coroutine method")
	}
	if _, _, _, err := pa.HostAttr(context.Background(), "adapter"); err == nil || !strings.Contains(err.Error(), "'PlatformActions' object has no attribute 'adapter'") {
		t.Fatalf("attr err %v", err)
	}
	ctx := paSetup(t, "{}\n", nil)
	r, err := pa.HostCall(ctx, "add_reaction", []any{"telegram", "1"}, map[string]any{"message_id": "2", "emoji": "x"})
	if err != nil || r.(map[string]any)["error"] != "capability_not_granted" {
		t.Fatalf("host call %v %v", r, err)
	}
	if _, err := pa.HostCall(ctx, "add_reaction", []any{"telegram", "1", "2"}, nil); err == nil || !strings.Contains(err.Error(), "PlatformActions.add_reaction()") {
		t.Fatalf("binding err %v", err)
	}
}
