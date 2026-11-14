package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Temp home bound as the process home with the named skills written under it
func slashExecHome(t *testing.T, skillNames ...string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv(home.EnvHome, h)
	for _, name := range skillNames {
		dir := filepath.Join(h, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + name + " skill.\n---\n# " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestEveryExecuteKeyResolvesToAnExecutor(t *testing.T) {
	var migrated, unresolved []string
	for i := range Builtin {
		d := &Builtin[i]
		if MetaOf(d.Name).Execute == "" {
			continue
		}
		migrated = append(migrated, d.Name)
		if ResolveExecutor(d) == nil {
			unresolved = append(unresolved, d.Name)
		}
	}
	if len(migrated) == 0 {
		t.Fatal("no command names an executor")
	}
	if len(unresolved) > 0 {
		t.Fatalf("CommandDef.execute names no EXECUTORS entry: %v", unresolved)
	}
}

// The gateway commands listing names a skill a builtin shadows and still lists the others
func TestExecCommandsListsTheSkillCollisionNote(t *testing.T) {
	slashExecHome(t, "handoff", "tidy-notes")
	reply, err := execCommands(context.Background(), CommandContext{Options: map[string]any{"page_size": 500}})
	note := "slash command /handoff unavailable — name taken by built-in; use /skill handoff"
	if err != nil || !strings.Contains(reply.Text, "⚠ "+note) || !strings.Contains(reply.Text, "`/tidy-notes`") || reply.Format != "markdown" {
		t.Fatalf("reply %q %v", reply.Text, err)
	}
	if strings.Contains(strings.ReplaceAll(reply.Text, note, ""), "`/handoff` —") {
		t.Fatal("the shadowed skill is listed as a command")
	}
}

// Pages clamp into range with navigation, a bad page prints usage and a restricted caller sees no skills
func TestExecCommandsPaginatesAndRestricts(t *testing.T) {
	slashExecHome(t, "tidy-notes")
	ctx := context.Background()
	reply, _ := execCommands(ctx, CommandContext{Args: "x"})
	if reply.Text != "Usage: `/commands [page]`" {
		t.Fatalf("usage %q", reply.Text)
	}
	reply, _ = execCommands(ctx, CommandContext{Args: "999", Options: map[string]any{"page_size": "5"}})
	lines := strings.Split(reply.Text, "\n")
	if !strings.HasPrefix(lines[0], "📚 **Commands** (") || !strings.HasSuffix(reply.Text, "_(Requested page 999 was out of range, showing page "+strings.TrimSuffix(strings.SplitN(lines[0], "/", 2)[1], ")")+".)_") {
		t.Fatalf("out of range %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "← prev") || strings.Contains(reply.Text, "next →") {
		t.Fatalf("last page navigation %q", reply.Text)
	}
	reply, _ = execCommands(ctx, CommandContext{Args: "-3", Options: map[string]any{"page_size": nil, "allowed_commands": []string{"help"}}})
	if !strings.Contains(reply.Text, "(1 total, page 1/1)") || !strings.Contains(reply.Text, "`/help") || strings.Contains(reply.Text, "tidy-notes") ||
		!strings.HasSuffix(reply.Text, "_(Requested page -3 was out of range, showing page 1.)_") {
		t.Fatalf("restricted %q", reply.Text)
	}
}

// The help body lists skill commands only for an unrestricted caller
func TestExecHelpListsSkillsForUnrestrictedCallers(t *testing.T) {
	slashExecHome(t, "tidy-notes")
	ctx := context.Background()
	reply, _ := execHelp(ctx, CommandContext{})
	if !strings.HasPrefix(reply.Text, "📖 **Icebath Commands**\n") || !strings.Contains(reply.Text, "⚡ **Skill Commands** (1 active):") ||
		!strings.Contains(reply.Text, "`/tidy-notes` — tidy-notes skill.") {
		t.Fatalf("help %q", reply.Text)
	}
	reply, _ = execHelp(ctx, CommandContext{Options: map[string]any{"allowed_commands": []any{"help"}}})
	if strings.Contains(reply.Text, "Skill Commands") || strings.Count(reply.Text, "`/") != 1 {
		t.Fatalf("restricted help %q", reply.Text)
	}
}

// The profile reply takes the per source values a multiplexed gateway passes
func TestExecProfileUsesGatewayOptionsBeforeTheProcess(t *testing.T) {
	h := slashExecHome(t)
	ctx := context.Background()
	reply, _ := execProfile(ctx, CommandContext{Options: map[string]any{"profile_name": " coder ", "home_display": "~/x"}})
	if reply.Text != "Profile: coder\nHome: ~/x" || reply.Data["profile"] != "coder" || reply.Data["home"] != "~/x" {
		t.Fatalf("options %q %v", reply.Text, reply.Data)
	}
	reply, _ = execProfile(ctx, CommandContext{})
	if reply.Data["profile"] != "default" || reply.Data["home"] != home.Display(h) {
		t.Fatalf("process %q %v", reply.Text, reply.Data)
	}
}

// Bundles reply names the directory when none are installed
func TestExecBundlesWithoutBundlesNamesTheDirectory(t *testing.T) {
	h := slashExecHome(t)
	reply, err := execBundles(context.Background(), CommandContext{})
	if err != nil || !strings.HasPrefix(reply.Text, "No skill bundles installed.") || !strings.HasSuffix(reply.Text, "Directory: "+home.BundlesDir(h)) || reply.Data["dir"] != home.BundlesDir(h) {
		t.Fatalf("bundles %q %v", reply.Text, err)
	}
}

// Execute runs the named command's executor and refuses a command without one
func TestExecuteCommandRunsTheRegistryExecutor(t *testing.T) {
	prev := BannerVersionLabel
	BannerVersionLabel = func(context.Context) string { return "Icebath Agent v1" }
	t.Cleanup(func() { BannerVersionLabel = prev })
	ctx := context.Background()
	reply, err := ExecuteCommand(ctx, "v", CommandContext{Surface: "gateway"})
	if err != nil || reply.Text != "Icebath Agent v1" || reply.Format != "plain" {
		t.Fatalf("version %q %v", reply.Text, err)
	}
	var missing *NoExecutorError
	for _, name := range []string{"model", "no-such-command"} {
		if _, err := ExecuteCommand(ctx, name, CommandContext{}); !errors.As(err, &missing) || err.Error() != "no registry-owned executor for /"+name {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
