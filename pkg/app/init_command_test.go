package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool env with its session context store under a temp home for the init prompt cases
func initTestEnv(t *testing.T) *tools.Env {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &tools.Env{Values: &sync.Map{}}
}

// Pointer to a session key literal
func keyOf(s string) *string { return &s }

func TestIncludesExtraNotesVerbatim(t *testing.T) {
	notes := "focus on the test setup, and mention the flaky e2e suite"
	if prompt := buildInitPrompt("/tmp/proj", nil, notes); !strings.Contains(prompt, notes) {
		t.Fatal("notes missing")
	}
}

func TestReadsExistingAgentsMd(t *testing.T) {
	env := initTestEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Existing\n\nRun `tox -e py311`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if prompt := buildInitPromptForCwd(env, dir, "", nil); !strings.Contains(prompt, "Run `tox -e py311`.") {
		t.Fatal("existing content missing")
	}
}

func TestPassesExtraThrough(t *testing.T) {
	env := initTestEnv(t)
	if prompt := buildInitPromptForCwd(env, t.TempDir(), "keep it short", nil); !strings.Contains(prompt, "keep it short") {
		t.Fatal("extra missing")
	}
}

func TestSessionCwdRecordWinsOverProcessCwd(t *testing.T) {
	env := initTestEnv(t)
	tmp := t.TempDir()
	workspace, launch := filepath.Join(tmp, "proj"), filepath.Join(tmp, "home")
	for _, d := range []string{workspace, launch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(launch)
	tools.RecordSessionCwd(env, "desktop:123", workspace)
	t.Cleanup(func() { tools.ClearSessionCwd(env, "desktop:123") })
	prompt := buildInitPromptForCwd(env, "", "", keyOf("desktop:123"))
	if !strings.Contains(prompt, "for the project at: "+workspace) || strings.Contains(prompt, launch) {
		t.Fatalf("prompt %q", prompt[:200])
	}
}

func TestStaleRecordFallsBackToProcessCwd(t *testing.T) {
	env := initTestEnv(t)
	tmp := t.TempDir()
	launch := filepath.Join(tmp, "home")
	if err := os.MkdirAll(launch, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	tools.RecordSessionCwd(env, "desktop:123", filepath.Join(tmp, "deleted-project"))
	t.Cleanup(func() { tools.ClearSessionCwd(env, "desktop:123") })
	if prompt := buildInitPromptForCwd(env, "", "", keyOf("desktop:123")); !strings.Contains(prompt, "for the project at: "+launch) {
		t.Fatalf("prompt %q", prompt[:200])
	}
}

func TestExplicitCwdWinsOverSessionRecord(t *testing.T) {
	env := initTestEnv(t)
	tmp := t.TempDir()
	tools.RecordSessionCwd(env, "desktop:123", filepath.Join(tmp, "other"))
	t.Cleanup(func() { tools.ClearSessionCwd(env, "desktop:123") })
	if prompt := buildInitPromptForCwd(env, tmp, "", keyOf("desktop:123")); !strings.Contains(prompt, "for the project at: "+tmp) {
		t.Fatalf("prompt %q", prompt[:200])
	}
}

func TestAmbientSessionKeyUsedWhenNone(t *testing.T) {
	env := initTestEnv(t)
	tmp := t.TempDir()
	workspace, launch := filepath.Join(tmp, "proj"), filepath.Join(tmp, "home")
	for _, d := range []string{workspace, launch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(launch)
	tools.RecordSessionCwd(env, "gateway:999", workspace)
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "gateway:999"})
	t.Cleanup(func() {
		tools.ClearSessionCwd(env, "gateway:999")
		tools.ResetSessionVars(env)
	})
	if prompt := buildInitPromptForCwd(env, "", "", nil); !strings.Contains(prompt, "for the project at: "+workspace) {
		t.Fatalf("prompt %q", prompt[:200])
	}
}

func TestStaleExplicitCwdFallsThroughToTheSessionRecord(t *testing.T) {
	env := initTestEnv(t)
	tmp := t.TempDir()
	workspace, gone := filepath.Join(tmp, "proj"), filepath.Join(tmp, "deleted-project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	tools.RecordSessionCwd(env, "desktop:123", workspace)
	t.Cleanup(func() { tools.ClearSessionCwd(env, "desktop:123") })
	prompt := buildInitPromptForCwd(env, gone, "", keyOf("desktop:123"))
	if !strings.Contains(prompt, "for the project at: "+workspace) || strings.Contains(prompt, gone) {
		t.Fatalf("prompt %q", prompt[:200])
	}
}

func TestInitIsRegisteredAndResolves(t *testing.T) {
	if d := commands.Resolve("init"); d == nil || d.Name != "init" {
		t.Fatalf("resolved %#v", d)
	}
}

func TestInitWorksOnTheGateway(t *testing.T) {
	if !commands.GatewayKnownCommands()["init"] {
		t.Fatal("init not gateway known")
	}
}
