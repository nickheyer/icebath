package googlemeet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Runs the test only where the plugin installs, the platforms marker of the upstream case
func requireMeetPlatform(t *testing.T) {
	t.Helper()
	if s := runtime.GOOS; s != "linux" && s != "darwin" {
		t.Skipf("google_meet install runs on linux and macos, this host is %s", s)
	}
}

// Swaps the CLI output for a buffer until the test ends
func captureCLI(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := cliOut
	cliOut = &buf
	t.Cleanup(func() { cliOut = prev })
	return &buf
}

func TestMeetJoinHandlerMissingURLReturnsError(t *testing.T) {
	isolateHome(t)
	var out map[string]any
	if err := json.Unmarshal([]byte(handleMeetJoin(context.Background(), &tools.Call{Args: map[string]any{}})), &out); err != nil {
		t.Fatal(err)
	}
	if out["success"] != false || out["error"] == nil || out["error"] == "" {
		t.Fatal(out)
	}
}

func TestOnSessionEndNoopWhenNothingActive(t *testing.T) {
	isolateHome(t)
	prev := sessionEndStop
	t.Cleanup(func() { sessionEndStop = prev })
	called := false
	sessionEndStop = func(context.Context, string) (*jsonutil.Object, error) {
		called = true
		return jsonutil.NewObject(), nil
	}
	if res, err := onSessionEnd(context.Background(), nil); res != nil || err != nil {
		t.Fatal(res, err)
	}
	if called {
		t.Fatal("stop called with no active meeting")
	}
}

func TestOnSessionEndStopsLiveBot(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	_ = writeActive(ctx, jsonutil.ObjectOf("pid", 22222, "meeting_id", "a-b-c", "out_dir", filepath.Join(t.TempDir(), "a-b-c"), "url", "https://meet.google.com/a-b-c", "started_at", 0))
	prevStop, prevAlive := sessionEndStop, pidAlive
	t.Cleanup(func() { sessionEndStop, pidAlive = prevStop, prevAlive })
	pidAlive = func(int) bool { return true }
	var reasons []string
	sessionEndStop = func(_ context.Context, reason string) (*jsonutil.Object, error) {
		reasons = append(reasons, reason)
		return nil, errors.New("signal failed")
	}
	if res, err := onSessionEnd(ctx, map[string]any{"session_id": "s1"}); res != nil || err != nil {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(reasons, []string{"session ended"}) {
		t.Fatal(reasons)
	}
}

func TestCLIRegisterIncludesNodeSubcommand(t *testing.T) {
	root := &cobra.Command{Use: "icebath meet", SilenceUsage: true, SilenceErrors: true}
	registerMeetCLI(root)
	prev := nodeDispatch
	t.Cleanup(func() { nodeDispatch = prev })
	var ns map[string]any
	nodeDispatch = func(got map[string]any) int {
		ns = got
		return 0
	}
	root.SetArgs([]string{"node", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if ns["meet_command"] != "node" || ns["node_cmd"] != "list" {
		t.Fatal(ns)
	}
}

func TestCmdInstallUsesDeclaredDependenciesAndPMChromium(t *testing.T) {
	requireMeetPlatform(t)
	out := captureCLI(t)
	pe, pr := pmEnsure, installRun
	t.Cleanup(func() { pmEnsure, installRun = pe, pr })
	var calls []any
	pmEnsure = func(_ context.Context, name string, o pm.EnsureOptions) (*pm.Runner, error) {
		calls = append(calls, name, o)
		return &pm.Runner{Name: name}, nil
	}
	installRun = func(argv []string) (int, error) {
		t.Fatalf("direct installer subprocess %v", argv)
		return 0, nil
	}
	if code := cmdInstall(context.Background(), false, true); code != 0 {
		t.Fatal(code, out.String())
	}
	want := []any{"chromium", pm.EnsureOptions{Explicit: true}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%#v", calls)
	}
	if !strings.Contains(out.String(), "icebath meet setup") {
		t.Fatal(out.String())
	}
}

func TestCmdInstallReportsDependencyFailure(t *testing.T) {
	requireMeetPlatform(t)
	out := captureCLI(t)
	pe := pmEnsure
	t.Cleanup(func() { pmEnsure = pe })
	pmEnsure = func(context.Context, string, pm.EnsureOptions) (*pm.Runner, error) {
		return nil, errors.New("dependency resolution failed")
	}
	if code := cmdInstall(context.Background(), false, true); code != 1 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "dependency resolution failed") {
		t.Fatal(out.String())
	}
}

// Loads the enabled plugin and finds its tools, CLI and hook with no skill listed, the guide only shipped beside it
func TestRegisterListsNoPluginSkill(t *testing.T) {
	requireMeetPlatform(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n  - google_meet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(context.Background(), true)
	var row map[string]any
	for _, r := range m.ListPlugins() {
		if r["key"] == "google_meet" {
			row = r
		}
	}
	if row == nil || row["enabled"] != true || row["error"] != nil || row["tools"] != len(meetTools) || row["hooks"] != 1 {
		t.Fatalf("%v", row)
	}
	if m.CLICommands()["meet"] == nil || !m.HasHook("on_session_end") {
		t.Fatal("meet CLI or on_session_end hook missing")
	}
	if got := m.ListPluginSkills("google_meet"); len(got) != 0 {
		t.Fatal(got)
	}
	if got := m.ListPluginSkillMetadata(); len(got) != 0 {
		t.Fatal(got)
	}
	guide, err := os.ReadFile("SKILL.md")
	if err != nil || !strings.HasPrefix(string(guide), "---\nname: google_meet\n") {
		t.Fatal(err, len(guide))
	}
	if strings.Contains(strings.ToLower(string(guide)), "hermes") {
		t.Fatal("upstream name left in the usage guide")
	}
}
