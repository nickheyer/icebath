package app

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

func TestCLIModuleArgsMapOntoMainKeywords(t *testing.T) {
	var errOut bytes.Buffer
	o, _, done := parseCLIModuleArgs([]string{"hello", "--max_turns", "5", "-w", "--list-toolsets", "--skills", "a,b", "--skills", "c", "--api_key=k", "--oneshot=False"}, &errOut)
	if done {
		t.Fatalf("parse stopped, stderr %q", errOut.String())
	}
	if o.Query != "hello" || o.MaxTurns == nil || *o.MaxTurns != 5 || !o.W || !o.ListToolsets || o.APIKey != "k" || o.Oneshot || o.OutputFormat != "text" {
		t.Fatalf("options %+v", o)
	}
	if !reflect.DeepEqual(o.Skills, []string{"a,b", "c"}) {
		t.Fatalf("skills %v", o.Skills)
	}
	o, _, done = parseCLIModuleArgs([]string{"--query", "first", "second", "--gateway"}, &errOut)
	if done || o.Query != "first" || o.Q != "second" || !o.Gateway || o.MaxTurns != nil {
		t.Fatalf("positional fills the first unset parameter, got %+v", o)
	}
	if _, code, done := parseCLIModuleArgs([]string{"one", "two", "three"}, &errOut); !done || code != 2 {
		t.Fatalf("a word the bool parameter cannot take exits 2, got %d %v", code, done)
	}
	errOut.Reset()
	if _, code, done := parseCLIModuleArgs([]string{"--bogus"}, &errOut); !done || code != 2 || !strings.Contains(errOut.String(), "bogus") {
		t.Fatalf("unknown flag exits 2, got %d %v %q", code, done, errOut.String())
	}
	if _, code, done := parseCLIModuleArgs([]string{"--help"}, &errOut); !done || code != 0 {
		t.Fatalf("help exits 0, got %d %v", code, done)
	}
}

func TestCLIModuleEntryShorthandQueryRunsTheSingleQuery(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetProcessState(t)
	out, errOut := captureStreams(t)
	client := &scriptedClient{script: []any{completeFrame("hi there")}, result: map[string]any{"final_response": "hi there", "api_calls": 1, "completed": true}}
	installScriptedGateway(t, client)
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "test/model", "provider": "openai", "base_url": "http://127.0.0.1:9/v1"},
		"agent": map[string]any{"max_turns": 5}, "display": map[string]any{}, "skills": map[string]any{"auto_load": []any{}}})
	code := cliModuleMain(context.Background(), []string{"-q", "say hi", "--quiet", "--model", "test/model", "--provider", "openai",
		"--api-key", "test-key-1234567890", "--base_url", "http://127.0.0.1:9/v1", "--max-turns", "5"}, errOut)
	if code != 0 {
		t.Fatalf("exit code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if submits := client.callsOf("prompt.submit"); len(submits) != 1 || submits[0]["text"] != "say hi" {
		t.Fatalf("prompt.submit calls %v", submits)
	}
}

func TestPlatformsReportNamesTheGatewayRunCommand(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	lines := strings.Join(platformsLines(context.Background()), "\n")
	if !strings.Contains(lines, "    icebath gateway run") || strings.Contains(lines, "python") || strings.Contains(lines, CLIModuleEntry) {
		t.Fatalf("platforms report %q", lines)
	}
}

func TestGatewayKeywordAnnouncesTheGatewayAndReportsItsStartFailure(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(gateway.EnvStartupWatchdog, "0")
	t.Setenv("TERMINAL_CWD", ".")
	t.Setenv("HOME", "")
	t.Setenv("ICEBATH_EXEC_ASK", "")
	previous := gatewayrun.RestoreUserProxyEnv
	gatewayrun.RestoreUserProxyEnv = selfupdate.RestoreUserProxyEnv
	t.Cleanup(func() { gatewayrun.RestoreUserProxyEnv = previous })
	resetProcessState(t)
	out, errOut := captureStreams(t)
	code := cliModuleMain(context.Background(), []string{"--gateway"}, errOut)
	if code != 1 {
		t.Fatalf("exit code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if out.String() != i18n.T(context.Background(), "cli.gateway.starting", "", nil)+"\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if !strings.Contains(errOut.String(), "Traceback (most recent call last):\n") || !strings.Contains(errOut.String(), "$HOME is not defined") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestListToolsetsKeywordShowsTheBannerAndToolsetsWithoutATurn(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetProcessState(t)
	out, errOut := captureStreams(t)
	client := &scriptedClient{}
	installScriptedGateway(t, client)
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "test/model", "provider": "openai", "base_url": "http://127.0.0.1:9/v1"},
		"agent": map[string]any{"max_turns": 5}, "display": map[string]any{}, "skills": map[string]any{"auto_load": []any{}}, "worktree": true})
	code := cliModuleMain(context.Background(), []string{"--list_toolsets", "-q", "ignored", "--model", "test/model", "--provider", "openai",
		"--api-key", "test-key-1234567890", "--base-url", "http://127.0.0.1:9/v1"}, errOut)
	if code != 0 {
		t.Fatalf("exit code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Available Toolsets") {
		t.Fatalf("stdout %q", out.String())
	}
	if submits := client.callsOf("prompt.submit"); len(submits) != 0 {
		t.Fatalf("a listing ran a turn %v", submits)
	}
	activeWorktreeMu.Lock()
	worktree := activeWorktree
	activeWorktreeMu.Unlock()
	if worktree != nil {
		t.Fatalf("a listing created worktree %v", worktree)
	}
}
