package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/spf13/cobra"
)

func TestMemoryStatusAndSetupAreBound(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profiles", "work")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: honcho\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	c := NewCLI()
	for _, path := range []string{"memory", "memory setup", "memory status"} {
		for _, unbound := range c.Unbound() {
			if unbound == path {
				t.Fatalf("%s is unbound", path)
			}
		}
	}
	for _, run := range []struct {
		sub  string
		args []string
	}{{"status", nil}, {"", nil}, {"setup", []string{"honcho"}}} {
		out := &bytes.Buffer{}
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(out)
		cmd.SetErr(out)
		if err := runMemorySetup(cmd, run.sub, run.args); err != nil {
			t.Fatalf("%q: %v", run.sub, err)
		}
		if !strings.Contains(out.String(), "icebath -p work plugins install honcho") {
			t.Fatalf("%q output lacks the profile install command\n%s", run.sub, out.String())
		}
	}
}

func TestCuratorInsightsAndMemoryOffResetAreBoundThroughTheTree(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	skill := filepath.Join(h, "skills", "coding-standards")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: coding-standards\ndescription: x\n---\n# coding-standards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: honcho\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "memories", "USER.md"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	c := NewCLI()
	unbound := strings.Join(c.Unbound(), "\n")
	for _, path := range append([]string{"insights", "memory off", "memory reset"}, func() []string {
		var out []string
		for _, sub := range curatorSubcommands {
			out = append(out, "curator "+sub)
		}
		return out
	}()...) {
		if strings.Contains("\n"+unbound+"\n", "\n"+path+"\n") {
			t.Fatalf("%s is unbound", path)
		}
	}
	run := func(argv ...string) (string, error) {
		out := &bytes.Buffer{}
		cli := NewCLI()
		cli.Root.SetOut(out)
		cli.Root.SetErr(out)
		cli.Root.SetIn(strings.NewReader(""))
		err := cli.Execute(argv)
		return out.String(), err
	}
	if out, err := run("curator", "lock", "coding-standards"); err != nil || !strings.Contains(out, "locked 'coding-standards'") {
		t.Fatalf("curator lock: %v\n%s", err, out)
	}
	if out, err := run("curator", "status"); err != nil || !strings.Contains(out, "locked (1): coding-standards") {
		t.Fatalf("curator status: %v\n%s", err, out)
	}
	var exit *ExitError
	if out, err := run("curator", "lock", "no-such-skill"); !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out, "no skill named 'no-such-skill'") {
		t.Fatalf("curator lock refusal: %v\n%s", err, out)
	}
	if out, err := run("curator", "prune", "--days", "0", "--dry-run"); !errors.As(err, &exit) || exit.Code != 2 || !strings.Contains(out, "curator: --days must be >= 1 (got 0)") {
		t.Fatalf("curator prune: %v\n%s", err, out)
	}
	if out, err := run("insights", "--days", "7"); err != nil || !strings.Contains(out, "No session data yet.") {
		t.Fatalf("insights: %v\n%s", err, out)
	}
	if out, err := run("memory", "off"); err != nil || !strings.Contains(out, "Memory provider: built-in only") {
		t.Fatalf("memory off: %v\n%s", err, out)
	}
	config.ResetCache()
	if tree, err := config.Load(h, nil); err != nil || config.AsMap(tree["memory"])["provider"] != "" {
		t.Fatalf("provider after off %v %v", tree, err)
	}
	if out, err := run("memory", "reset", "--target", "user", "--yes"); err != nil || !strings.Contains(out, "Deleted USER.md (user profile)") {
		t.Fatalf("memory reset: %v\n%s", err, out)
	}
}

func TestMemorySubparsersParseThroughTheTree(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profiles", "work")
	for name, body := range map[string]string{"config.yaml": "memory:\n  provider: honcho\n", "memories/MEMORY.md": "m", "memories/USER.md": "u"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(h, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	run := func(argv ...string) string {
		out := &bytes.Buffer{}
		cli := NewCLI()
		cli.Root.SetOut(out)
		cli.Root.SetErr(out)
		cli.Root.SetIn(strings.NewReader(""))
		if err := cli.Execute(argv); err != nil {
			out.WriteString("\nerror: " + err.Error())
		}
		return out.String()
	}
	for _, argv := range [][]string{{"memory", "setup", "honcho"}, {"memory", "status"}, {"memory"}} {
		if out := run(argv...); !strings.Contains(out, "icebath -p work plugins install honcho") || strings.Contains(out, "\nerror: ") {
			t.Fatalf("%v\n%s", argv, out)
		}
	}
	if out := run("memory", "setup", "honcho", "extra"); !strings.Contains(out, "unrecognized arguments: extra") {
		t.Fatalf("second setup positional accepted\n%s", out)
	}
	if out := run("memory", "reset", "--target", "bogus", "--yes"); !strings.Contains(out, "argument --target: invalid choice: 'bogus' (choose from 'all', 'memory', 'user')") {
		t.Fatalf("reset target choice\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h, "memories", "USER.md")); err != nil {
		t.Fatalf("rejected reset touched USER.md: %v", err)
	}
	t.Setenv("COLUMNS", "200")
	for argv, want := range map[string][]string{
		"memory --help":       {"Only one external provider can be active at a time.", "Interactive provider selection and configuration", "Show current memory provider config", "Disable external provider (built-in only)", "Erase all built-in memory (MEMORY.md and USER.md)"},
		"memory setup --help": {"Provider to configure directly (e.g. honcho), skipping the picker"},
		"memory reset --help": {"Which store to reset: 'all' (default), 'memory', or 'user'", "Skip confirmation prompt"},
	} {
		out := run(strings.Fields(argv)...)
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Fatalf("%s lacks %q\n%s", argv, w, out)
			}
		}
	}
	if out := run("memory", "reset", "-y"); !strings.Contains(out, "Deleted MEMORY.md") || !strings.Contains(out, "Deleted USER.md (user profile)") {
		t.Fatalf("default reset target\n%s", out)
	}
}
