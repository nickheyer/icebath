package commands

import (
	"slices"
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
