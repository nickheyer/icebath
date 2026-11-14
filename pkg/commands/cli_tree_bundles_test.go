package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Runs one icebath command line over a temp home with a bundles dir, the line reader replaced when given
func runBundlesMain(t *testing.T, dir string, argv []string, lineInput func(string) (string, error)) (int, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", dir)
	t.Setenv("ICEBATH_BUNDLES_DIR", filepath.Join(dir, "skill-bundles"))
	t.Setenv("COLUMNS", "400")
	old := newBundlesCLI
	t.Cleanup(func() { newBundlesCLI = old })
	newBundlesCLI = func(ctx context.Context, out io.Writer) *skills.BundlesCLI {
		x := old(ctx, out)
		x.LineInput = lineInput
		if lineInput == nil {
			x.LineInput = func(prompt string) (string, error) {
				t.Fatalf("unexpected prompt %q", prompt)
				return "", nil
			}
		}
		return x
	}
	var out bytes.Buffer
	c := NewCLI()
	c.Root.SetArgs(argv)
	c.Root.SetOut(&out)
	c.Root.SetErr(&out)
	err := c.Root.ExecuteContext(home.WithOverride(context.Background(), dir))
	var exit *ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exit):
		return exit.Code, out.String()
	}
	t.Fatalf("%v: %v\n%s", argv, err, out.String())
	return 0, ""
}

func TestCLIBundlesCreateRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if code, out := runBundlesMain(t, dir, []string{"bundles", "create", "dup", "--skill", "s1"}, nil); code != 0 || !strings.Contains(out, "Created bundle:") {
		t.Fatalf("%d %q", code, out)
	}
	code, out := runBundlesMain(t, dir, []string{"bundles", "create", "dup", "--skill", "s2"}, nil)
	low := strings.ToLower(out)
	if code != 1 || !(strings.Contains(low, "already exists") || strings.Contains(low, "--force")) {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCLIBundlesCreateRequiresSkills(t *testing.T) {
	code, out := runBundlesMain(t, t.TempDir(), []string{"bundles", "create", "empty"}, func(string) (string, error) { return "", cliui.ErrEOF })
	if code != 1 || !strings.HasSuffix(out, "\nCancelled.\n") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCLIBundlesDispatchesEverySubcommand(t *testing.T) {
	dir := t.TempDir()
	bundles := filepath.Join(dir, "skill-bundles")
	if code, out := runBundlesMain(t, dir, []string{"bundles"}, nil); code != 0 || !strings.HasPrefix(out, "No bundles installed yet. Create one with:\n  icebath bundles create <name> --skill skill1 --skill skill2\n") {
		t.Fatalf("%d %q", code, out)
	}
	argv := []string{"bundles", "create", "Deep Dive", "-s", "a", "--skill", "b", "-d", "Two skills", "-i", "Go slow", "-f"}
	if code, out := runBundlesMain(t, dir, argv, nil); code != 0 || out != "Created bundle: "+filepath.Join(bundles, "deep-dive.yaml")+"\n  Invoke with: /deep-dive  (loads 2 skills)\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out := runBundlesMain(t, dir, []string{"bundles", "list"}, nil); code != 0 || !strings.Contains(out, "Skill Bundles (1)") || !strings.Contains(out, "/deep-dive") {
		t.Fatalf("%d %q", code, out)
	}
	want := "/deep-dive  Deep Dive\n  Two skills\n  File: " + filepath.Join(bundles, "deep-dive.yaml") + "\n  Skills (2):\n    - a\n    - b\n  Instruction:\n    Go slow\n"
	if code, out := runBundlesMain(t, dir, []string{"bundles", "show", "deep dive"}, nil); code != 0 || out != want {
		t.Fatalf("%d %q", code, out)
	}
	if code, out := runBundlesMain(t, dir, []string{"bundles", "reload"}, nil); code != 0 || out != "No changes. 1 bundle(s) loaded.\n" {
		t.Fatalf("registration scan did not prime the reload baseline %d %q", code, out)
	}
	if code, out := runBundlesMain(t, dir, []string{"bundles", "delete", "deep-dive"}, nil); code != 0 || out != "Deleted bundle: "+filepath.Join(bundles, "deep-dive.yaml")+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out := runBundlesMain(t, dir, []string{"bundles", "show", "deep-dive"}, nil); code != 1 || out != "Bundle 'deep-dive' not found.\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out := runBundlesMain(t, dir, []string{"bundles", "delete", "deep-dive"}, nil); code != 1 || out != "No bundle at "+filepath.Join(bundles, "deep-dive.yaml")+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	if _, err := os.Stat(bundles); err != nil {
		t.Fatal(err)
	}
}

// Flag shape one argparse option maps to
type parserFlag struct{ name, short, kind, def, help string }

// Checks a command's local flags, positional help and help strings against the argparse parser
func assertParser(t *testing.T, c *CLI, path, short, long, positionals string, flags ...parserFlag) {
	t.Helper()
	var cmd *cobra.Command
	if cmd = c.find(path); cmd == nil {
		t.Fatalf("%s missing", path)
	}
	if cmd.Short != short || cmd.Long != long || cmd.Annotations["positionals"] != positionals {
		t.Fatalf("%s help %q %q %q", path, cmd.Short, cmd.Long, cmd.Annotations["positionals"])
	}
	n := 0
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" {
			n++
		}
	})
	if n != len(flags) {
		t.Fatalf("%s has %d flags, parser has %d", path, n, len(flags))
	}
	for _, want := range flags {
		f := cmd.Flags().Lookup(want.name)
		if f == nil || f.Shorthand != want.short || f.Value.Type() != want.kind || f.DefValue != want.def || f.Usage != want.help {
			t.Fatalf("%s --%s is %+v", path, want.name, f)
		}
	}
}

func TestCLIBundlesTreeMatchesBuildBundlesParser(t *testing.T) {
	c := NewCLI()
	assertParser(t, c, "bundles", "Create, list, and manage skill bundles (aliases for multiple skills)",
		"Skill bundles let you load several skills under one slash command. `/<bundle>` from the CLI or gateway loads every referenced skill at once.", "")
	assertParser(t, c, "bundles list", "List installed skill bundles", "", "")
	assertParser(t, c, "bundles show", "Show one bundle's contents", "", "  name   Bundle name")
	assertParser(t, c, "bundles create", "Create a new skill bundle",
		"Create a new bundle. Skills can be passed via --skill (repeat for multiple) or entered interactively when omitted.",
		"  name   Bundle name (becomes the /slash command)",
		parserFlag{"skill", "s", "stringArray", "[]", "Skill name to include (repeat for multiple)"},
		parserFlag{"description", "d", "string", "", "Human-readable description shown in /help and `icebath bundles list`"},
		parserFlag{"instruction", "i", "string", "", "Extra guidance prepended to the loaded skill content"},
		parserFlag{"force", "f", "bool", "false", "Overwrite an existing bundle with the same name"})
	assertParser(t, c, "bundles delete", "Delete a skill bundle", "", "  name   Bundle name")
	assertParser(t, c, "bundles reload", "Re-scan the bundles directory and report changes", "", "")
	for _, path := range c.Unbound() {
		if path == "bundles" || strings.HasPrefix(path, "bundles ") || path == "skills" || strings.HasPrefix(path, "skills ") {
			t.Errorf("%s is unbound", path)
		}
	}
	var help bytes.Buffer
	c.Root.SetArgs([]string{"bundles", "create", "--help"})
	c.Root.SetOut(&help)
	if err := c.Root.Execute(); err != nil || !strings.Contains(help.String(), "\npositional arguments:\n  name                  Bundle name (becomes the /slash command)\n\noptions:\n") {
		t.Fatalf("%v %q", err, help.String())
	}
}
