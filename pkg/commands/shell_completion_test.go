package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Minimal command tree mirroring the real icebath structure
func makeCompletionParser() *cobra.Command {
	sub := func(parent *cobra.Command, name, help string, aliases ...string) *cobra.Command {
		cmd := &cobra.Command{Use: name, Short: help, Aliases: aliases, Run: func(*cobra.Command, []string) {}}
		parent.AddCommand(cmd)
		return cmd
	}
	p := &cobra.Command{Use: "icebath"}
	p.Flags().BoolP("version", "V", false, "")
	p.Flags().StringP("profile", "p", "", "Profile name")
	chat := sub(p, "chat", "Interactive chat with the agent")
	chat.Flags().StringP("query", "q", "", "")
	chat.Flags().StringP("model", "m", "", "")
	gw := sub(p, "gateway", "Messaging gateway management")
	sub(gw, "start", "Start service")
	sub(gw, "stop", "Stop service")
	sub(gw, "status", "Show status")
	sub(gw, "run", "Run in foreground", "foreground")
	sess := sub(p, "sessions", "Manage session history")
	sub(sess, "list", "List sessions")
	sub(sess, "delete", "Delete a session")
	prof := sub(p, "profile", "Manage profiles")
	for _, row := range [][2]string{{"list", "List profiles"}, {"use", "Switch to a profile"}, {"create", "Create a new profile"},
		{"delete", "Delete a profile"}, {"show", "Show profile details"}, {"alias", "Set profile alias"},
		{"rename", "Rename a profile"}, {"export", "Export a profile"}} {
		sub(prof, row[0], row[1])
	}
	sub(p, "version", "Show version")
	return p
}

func TestWalkTopLevelSubcommandsExtracted(t *testing.T) {
	tree := walkCompletion(makeCompletionParser())
	got := strings.Join(sortedCompletionNames(tree.Subcommands), " ")
	if got != "chat gateway profile sessions version" {
		t.Fatalf("subcommands %q", got)
	}
}

func TestWalkAliasesNotDuplicated(t *testing.T) {
	gw := walkCompletion(makeCompletionParser()).Subcommands["gateway"].Subcommands
	if _, present := gw["foreground"]; present {
		t.Fatal("alias foreground listed as its own subcommand")
	}
}

func TestGenerateBashContainsCompletionFunctionAndRegister(t *testing.T) {
	out := GenerateBash(makeCompletionParser())
	if !strings.Contains(out, "_icebath_completion()") || !strings.Contains(out, "complete -F _icebath_completion icebath") {
		t.Fatal(out)
	}
}

func TestGenerateBashValidBashSyntax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completion.bash")
	if err := os.WriteFile(path, []byte(GenerateBash(makeCompletionParser())), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestProfileCompletionBashProfileActionsCompleteProfileNames(t *testing.T) {
	inProfileCase, hasProfilesInAction := false, false
	for _, line := range strings.Split(GenerateBash(makeCompletionParser()), "\n") {
		if strings.Contains(line, "profile)") {
			inProfileCase = true
		}
		if inProfileCase && strings.Contains(line, "_icebath_profiles") {
			hasProfilesInAction = true
			break
		}
	}
	if !hasProfilesInAction {
		t.Fatal("profile actions should complete with _icebath_profiles")
	}
}

func TestProfileCompletionFishProfileActionsCompleteNames(t *testing.T) {
	out := GenerateFish(makeCompletionParser())
	if !strings.Contains(out, "__icebath_profiles") {
		t.Fatal(out)
	}
	if count := strings.Count(out, "(__icebath_profiles)"); count < 2 {
		t.Fatalf("Expected >=2 profile completion entries, got %d", count)
	}
}

// Every shell script of the real tree parses, the completion command printing the bash one by default
func TestCompletionCommandPrintsEachShellScript(t *testing.T) {
	c := NewCLI()
	var bash, zsh strings.Builder
	if err := c.Completion("bash", &bash); err != nil || !strings.HasSuffix(bash.String(), "complete -F _icebath_completion icebath\n\n") {
		t.Fatalf("bash %v %q", err, bash.String())
	}
	if err := c.Completion("zsh", &zsh); err != nil || !strings.HasPrefix(zsh.String(), "#compdef icebath\n") {
		t.Fatalf("zsh %v", err)
	}
	path := filepath.Join(t.TempDir(), "completion.bash")
	if err := os.WriteFile(path, []byte(bash.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if !strings.Contains(bash.String(), "        profile)\n            case \"$prev\" in") || strings.Contains(bash.String(), " __complete") {
		t.Fatal("real tree bash script lacks the profile case or lists cobra internals")
	}
}
