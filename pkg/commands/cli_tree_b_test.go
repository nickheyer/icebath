package commands

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Parses argv through the full CLI and returns the namespace the path's handler received
func bNamespace(t *testing.T, path string, argv []string) (map[string]any, error) {
	t.Helper()
	c := NewCLI()
	var ns map[string]any
	c.Bind(path, func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	err := c.Execute(argv)
	return ns, err
}

func TestApprovalsParserUsesTheArgparseDests(t *testing.T) {
	ns, err := bNamespace(t, "approvals test", []string{"approvals", "test", "--env-type", "docker", "--", "rm", "-rf", "./build"})
	if err != nil {
		t.Fatal(err)
	}
	words, _ := ns["command_words"].([]string)
	if !slices.Equal(words, []string{"rm", "-rf", "./build"}) || ns["env_type"] != "docker" || ns["command"] != "approvals" || ns["json"] != false {
		t.Fatalf("test namespace %v", ns)
	}
	ns, err = bNamespace(t, "approvals suggest", []string{"approvals", "suggest", "--apply", "1,3", "--days", "30"})
	if err != nil {
		t.Fatal(err)
	}
	if ns["apply_indices"] != "1,3" || ns["days"] != 30 || ns["min_count"] != 2 || ns["limit"] != 20 || ns["db"] != nil {
		t.Fatalf("suggest namespace %v", ns)
	}
	if _, present := ns["apply"]; present {
		t.Fatal("the dashed flag name leaked beside its dest")
	}
	c := NewCLI()
	if use := c.find("approvals test").Use; use != "test [-- command...]" {
		t.Fatalf("usage %q", use)
	}
}

func TestBackupKeepRefusesNegativeAndNonDecimalValues(t *testing.T) {
	for _, bad := range []string{"-1", "x", "1.5", ""} {
		_, err := bNamespace(t, "backup", []string{"backup", "--keep", bad})
		if err == nil || err.Error() != "argument -k/--keep: must be a non-negative integer (0 keeps everything)" {
			t.Fatalf("keep %q: %v", bad, err)
		}
	}
	ns, err := bNamespace(t, "backup", []string{"backup", "-k", "0"})
	if err != nil || ns["keep"] != 0 {
		t.Fatalf("keep 0: %v %v", ns["keep"], err)
	}
	ns, err = bNamespace(t, "backup", []string{"backup", "--quick", "--label", "pre-update"})
	if err != nil || ns["keep"] != 3 || ns["quick"] != true || ns["label"] != "pre-update" || ns["output"] != nil {
		t.Fatalf("defaults %v %v", ns, err)
	}
}

func TestSmallSubcommandParsersMatchTheirHermesShapes(t *testing.T) {
	c := NewCLI()
	for path, flags := range map[string][]string{
		"import":            {"force"},
		"security audit":    {"json", "fail-on", "skip-venv", "skip-plugins", "skip-mcp"},
		"migrate xai":       {"apply", "no-backup"},
		"migrate relay":     {"all-profiles", "no-validate"},
		"checkpoints prune": {"retention-days", "max-size-mb", "keep-orphans", "force"},
	} {
		cmd := c.find(path)
		if cmd == nil {
			t.Fatalf("%s missing", path)
		}
		for _, name := range flags {
			if cmd.Flags().Lookup(name) == nil {
				t.Fatalf("%s lacks --%s", path, name)
			}
		}
	}
	ns, err := bNamespace(t, "import", []string{"import", "home.zip", "-f"})
	if err != nil || ns["zipfile"] != "home.zip" || ns["force"] != true {
		t.Fatalf("import %v %v", ns, err)
	}
	if _, err := bNamespace(t, "security audit", []string{"security", "audit", "--fail-on", "severe"}); err == nil {
		t.Fatal("fail-on accepts only its choices")
	}
	for _, src := range ImportSources {
		if c.find("migrate "+src.Name) == nil {
			t.Fatalf("migrate %s missing", src.Name)
		}
	}
}

// import takes the zipfile positional and the force flag of the hermes import parser
func TestImportParserTakesTheZipfileAndForce(t *testing.T) {
	ns, err := bNamespace(t, "import", []string{"import", "backup.zip"})
	if err != nil || ns["zipfile"] != "backup.zip" || ns["force"] != false {
		t.Fatalf("defaults %v %v", ns, err)
	}
	for _, flag := range []string{"-f", "--force"} {
		ns, err = bNamespace(t, "import", []string{"import", flag, "backup.zip"})
		if err != nil || ns["zipfile"] != "backup.zip" || ns["force"] != true {
			t.Fatalf("%s %v %v", flag, ns, err)
		}
	}
	if _, err := bNamespace(t, "import", []string{"import"}); err == nil || !strings.Contains(err.Error(), "zipfile") {
		t.Fatalf("missing zipfile: %v", err)
	}
}

// verify reads its defaults as the hermes parser does, port and phase None, both timeouts floats
func TestVerifyParserBuildsTheHermesDefaults(t *testing.T) {
	ns, err := bNamespace(t, "verify", []string{"verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ns["path"] != nil || ns["port"] != nil || ns["phase"] != nil || ns["timeout"] != 600.0 || ns["ready_timeout"] != 60.0 ||
		ns["detect_only"] != false || ns["save"] != false || ns["skip_start"] != false || ns["json"] != false {
		t.Fatalf("defaults %v", ns)
	}
	ns, err = bNamespace(t, "verify", []string{"verify", "proj", "--phase", "build", "--phase", "test", "--port", "0", "--timeout", "5", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	phases, _ := ns["phase"].([]string)
	if ns["path"] != "proj" || ns["port"] != 0 || !slices.Equal(phases, []string{"build", "test"}) || ns["timeout"] != 5.0 || ns["json"] != true {
		t.Fatalf("given %v", ns)
	}
	if _, err := bNamespace(t, "verify", []string{"verify", "--phase", "deploy"}); err == nil {
		t.Fatal("phase accepts only its choices")
	}
	c := NewCLI()
	cmd := c.find("verify")
	if cmd.Short != "Detect a project's run recipe and smoke-test it" || cmd.Flags().Lookup("timeout").Usage != "Per-phase timeout in seconds (default: 600)" ||
		cmd.Flags().Lookup("ready-timeout").Usage != "Readiness poll timeout in seconds (default: 60)" {
		t.Fatalf("help %q %q", cmd.Short, cmd.Flags().Lookup("timeout").Usage)
	}
}

// hooks attaches list, test, revoke and doctor with the hermes aliases and test options
func TestHooksParserAttachesEveryAction(t *testing.T) {
	c := NewCLI()
	for path, aliases := range map[string][]string{"hooks list": {"ls"}, "hooks test": nil, "hooks revoke": {"remove", "rm"}, "hooks doctor": nil} {
		cmd := c.find(path)
		if cmd == nil || !slices.Equal(cmd.Aliases, aliases) {
			t.Fatalf("%s %v", path, cmd)
		}
	}
	ns, err := bNamespace(t, "hooks test", []string{"hooks", "test", "pre_tool_call", "--for-tool", "terminal", "--payload-file", "p.json"})
	if err != nil || ns["event"] != "pre_tool_call" || ns["for_tool"] != "terminal" || ns["payload_file"] != "p.json" {
		t.Fatalf("test %v %v", ns, err)
	}
	ns, err = bNamespace(t, "hooks revoke", []string{"hooks", "rm", "/bin/hook.sh"})
	if err != nil || ns["command"] != "/bin/hook.sh" {
		t.Fatalf("revoke %v %v", ns, err)
	}
	if !strings.Contains(c.find("hooks").Long, "~/.icebath/shell-hooks-allowlist.json") {
		t.Fatal("hooks description")
	}
}
