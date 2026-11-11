package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Installs the core slash command lookup pkg/commands registers at init in the icebath binary
func registerCoreCommands(t *testing.T) {
	skills.RegisterCoreCommandResolver(func(name string) bool { return name == "status" })
	t.Cleanup(func() { skills.RegisterCoreCommandResolver(nil) })
}

func TestExpandSkillInvocationForReplayRoundTripsTheProjection(t *testing.T) {
	registerCoreCommands(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	skillDir := filepath.Join(h, "skills", "worktree-kickoff")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: worktree-kickoff\ndescription: Spin up a worktree\n---\n\n# kickoff\n\nSPIN UP A WORKTREE, never the primary checkout.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	expanded := tuigateway.ExpandSkillInvocationForReplay(context.Background(), "/worktree-kickoff fix it", "task-1")
	if !strings.Contains(expanded, "SPIN UP A WORKTREE") {
		t.Fatalf("not expanded %q", expanded)
	}
	if got := tuigateway.SkillScaffoldProjection(expanded); got != "/worktree-kickoff fix it" {
		t.Fatalf("projection %q", got)
	}
}

func TestExpandSkillInvocationForReplayLeavesOrdinaryTextAlone(t *testing.T) {
	registerCoreCommands(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if got := tuigateway.ExpandSkillInvocationForReplay(ctx, "just words", "t"); got != "just words" {
		t.Fatalf("got %q", got)
	}
	if got := tuigateway.ExpandSkillInvocationForReplay(ctx, "/status", "t"); got != "/status" {
		t.Fatalf("got %q", got)
	}
}
