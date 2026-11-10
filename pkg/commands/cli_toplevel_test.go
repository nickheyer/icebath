package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/home"
)

// Namespace the chat handler receives for argv, whether the command or the root dispatched it
func chatNamespace(t *testing.T, argv []string) (map[string]any, error) {
	t.Helper()
	c := NewCLI()
	var ns map[string]any
	c.Bind("chat", func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	err := c.Execute(argv)
	return ns, err
}

func TestChatVerboseArgChatWithoutVerboseLeavesAttributeUnset(t *testing.T) {
	ns, err := chatNamespace(t, []string{"chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := ns["verbose"]; present {
		t.Fatalf("verbose present in %v", ns)
	}
}

func TestAcceptHooksOnAgentSubparsersAcceptedAtEveryPosition(t *testing.T) {
	for _, argv := range [][]string{
		{"--accept-hooks", "gateway", "run", "--help"},
		{"gateway", "--accept-hooks", "run", "--help"},
		{"gateway", "run", "--accept-hooks", "--help"},
		{"--accept-hooks", "cron", "tick", "--help"},
		{"cron", "--accept-hooks", "tick", "--help"},
		{"cron", "tick", "--accept-hooks", "--help"},
		{"cron", "run", "--accept-hooks", "dummy-id", "--help"},
		{"acp", "--accept-hooks", "--help"},
	} {
		c := NewCLI()
		var out, errOut bytes.Buffer
		c.Root.SetOut(&out)
		c.Root.SetErr(&errOut)
		code, msg := ExitCode(c.Execute(argv))
		stderr := errOut.String() + msg
		if code != 0 {
			t.Fatalf("argv=%q returned %d\nstderr: %s", argv, code, stderr)
		}
		if strings.Contains(stderr, "unrecognized arguments") {
			t.Fatalf("argv=%q stderr: %s", argv, stderr)
		}
	}
}

func TestChatSubparserInheritedValueFlagsAllThreeFlagsBeforeChat(t *testing.T) {
	ns, err := chatNamespace(t, []string{"-t", "web", "-m", "anthropic/claude-sonnet-4", "--provider", "openrouter", "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if ns["toolsets"] != "web" || ns["model"] != "anthropic/claude-sonnet-4" || ns["provider"] != "openrouter" {
		t.Fatalf("namespace %v", ns)
	}
}

func TestChatSubparserInheritedValueFlagsChatSubparserInheritedValueFlagsUseSuppress(t *testing.T) {
	c := NewCLI()
	topLevel := map[string]bool{}
	c.Root.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && c.Root.PersistentFlags().Lookup(f.Name) == nil {
			topLevel[flagDest(f)] = true
		}
	})
	var offenders []string
	c.find("chat").LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" || !topLevel[flagDest(f)] {
			return
		}
		if f.Annotations["suppress"] == nil {
			offenders = append(offenders, actionName(f)+" dest="+flagDest(f)+" default="+f.DefValue)
		}
	})
	if len(offenders) > 0 {
		t.Fatalf("Chat subparser redeclares these top-level flags without default=argparse.SUPPRESS; they will silently clobber the top-level value when used as `icebath <flag> <value> chat`:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestCheckpointsParsesAtTheTopLevelAndSurvivesChat(t *testing.T) {
	for _, argv := range [][]string{{"--tui", "--checkpoints"}, {"--checkpoints", "chat"}, {"chat", "--tui", "--checkpoints"}} {
		ns, err := chatNamespace(t, argv)
		if err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
		if ns["checkpoints"] != true {
			t.Fatalf("%q: checkpoints = %#v", argv, ns["checkpoints"])
		}
	}
}

func TestChatParserAcceptsQueryFile(t *testing.T) {
	ns, err := chatNamespace(t, []string{"chat", "--query-file", "/tmp/x.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if ns["query_file"] != "/tmp/x.txt" || ns["query"] != nil {
		t.Fatalf("namespace %v", ns)
	}
}

func TestQueryAndQueryFileMutuallyExclusive(t *testing.T) {
	f := filepath.Join(t.TempDir(), "dm.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	ns, err := chatNamespace(t, []string{"chat", "-q", "x", "--query-file", f})
	code, msg := ExitCode(err)
	if code != 2 || ns != nil {
		t.Fatalf("exit %d namespace %v", code, ns)
	}
	if !strings.HasSuffix(msg, "icebath chat: error: argument --query-file: not allowed with argument -q/--query") || !strings.HasPrefix(msg, "usage: icebath chat ") {
		t.Fatalf("stderr %q", msg)
	}
}

// User home at tmp, the home variable unset, and a sticky coder profile, the fallback warning latch rearmed
func stickyProfileHome(t *testing.T) (string, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, "")
	os.Unsetenv(home.EnvHome)
	root := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "active_profile"), []byte("coder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	t.Cleanup(home.CaptureProfileFallbackWarning(stderr))
	return root, stderr
}

func TestBuildingTheParserBeforeTheProfileOverrideStaysSilent(t *testing.T) {
	_, stderr := stickyProfileHome(t)
	NewCLI()
	if strings.Contains(stderr.String(), "ICEBATH_HOME fallback") {
		t.Fatalf("stderr %q", stderr.String())
	}
	home.Dir(context.Background())
	if !strings.Contains(stderr.String(), "ICEBATH_HOME fallback") {
		t.Fatal("the warning latch was spent before the parser was built")
	}
}

func TestHelpTextNamesTheProfileConfigOnceTheProcessIsReHomed(t *testing.T) {
	root, _ := stickyProfileHome(t)
	t.Setenv(home.EnvHome, filepath.Join(root, "profiles", "coder"))
	if got := cfgPathForHelp(); got != "~/.icebath/profiles/coder/config.yaml" {
		t.Fatalf("cfg path %q", got)
	}
}

// Required and optional value option strings of the root, read from the flags themselves
func parserValueFlags(c *CLI) (map[string]bool, map[string]bool) {
	required, optional := map[string]bool{}, map[string]bool{}
	c.Root.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" || c.Root.PersistentFlags().Lookup(f.Name) != nil || f.NoOptDefVal == "true" {
			return
		}
		target := required
		if f.NoOptDefVal != "" {
			target = optional
		}
		for _, name := range strings.Split(actionName(f), "/") {
			target[name] = true
		}
	})
	return required, optional
}

func TestHelperSetsMatchTopLevelParser(t *testing.T) {
	c := NewCLI()
	required, optional := c.TopLevelValueFlagSets()
	parserRequired, parserOptional := parserValueFlags(c)
	var missingRequired, missingOptional []string
	for name := range parserRequired {
		if !required[name] {
			missingRequired = append(missingRequired, name)
		}
	}
	for name := range parserOptional {
		if !optional[name] {
			missingOptional = append(missingOptional, name)
		}
	}
	if len(missingRequired) > 0 {
		t.Fatalf("value flags missing from required set: %v (they will be misread as positionals and force eager plugin discovery)", missingRequired)
	}
	if len(missingOptional) > 0 {
		t.Fatalf("optional-value flags missing: %v", missingOptional)
	}
	wantRequired := map[string]bool{"-z": true, "--oneshot": true, "-m": true, "--model": true, "--provider": true, "--reasoning": true,
		"-t": true, "--toolsets": true, "-r": true, "--resume": true, "-s": true, "--skills": true, "--usage-file": true, "--in": true}
	if !reflect.DeepEqual(required, wantRequired) || !reflect.DeepEqual(optional, map[string]bool{"-c": true, "--continue": true}) {
		t.Fatalf("required %v optional %v", required, optional)
	}
}

func TestFirstPositionalSkipsReasoningValue(t *testing.T) {
	c := NewCLI()
	argv := []string{"--reasoning", "high", "chat", "hello"}
	if got := c.FirstPositional(argv); got != "chat" {
		t.Fatalf("first positional %q", got)
	}
	if c.PluginCLIDiscoveryNeeded(argv) {
		t.Fatal("plugin discovery needed for chat")
	}
}

func TestReasoningFormsDoNotShadowTheSubcommand(t *testing.T) {
	c := NewCLI()
	for _, argv := range [][]string{
		{"--reasoning", "ultra", "--provider", "openai", "-z", "prompt here"},
		{"-m", "x", "--reasoning=low", "chat"},
	} {
		if first := c.FirstPositional(argv); first == "high" || first == "ultra" || first == "low" {
			t.Fatalf("a reasoning level leaked through as the positional: %q", first)
		}
	}
}

// Unknown options at the root or a group are argparse extras unless an invalid subcommand is reported first
func TestUnknownOptionsBeforeTheCommandAreUnrecognized(t *testing.T) {
	for argv, want := range map[string]string{
		"--bogus":                   "unrecognized arguments: --bogus",
		"--yolo --bogus chat":       "unrecognized arguments: --bogus",
		"mcp --bogus list":          "unrecognized arguments: --bogus",
		"-m x --bogus=1 gateway -X": "unrecognized arguments: --bogus=1 -X",
	} {
		ns, err := chatNamespace(t, strings.Fields(argv))
		code, msg := ExitCode(err)
		if code != 2 || ns != nil || !strings.HasSuffix(msg, ": error: "+want) {
			t.Fatalf("%q: exit %d %q", argv, code, msg)
		}
	}
	code, msg := ExitCode(NewCLI().Execute([]string{"mcp", "serve", "--tools"}))
	if code != 2 || !strings.HasPrefix(msg, "icebath mcp: 'serve' is not a `icebath mcp` command.") {
		t.Fatalf("serve: exit %d %q", code, msg)
	}
}

// Entries that parse their own arguments receive them untouched
func TestRawArgumentEntriesReceiveTheirArguments(t *testing.T) {
	for _, path := range []string{"source-completion", "venv-sync"} {
		c := NewCLI()
		var got []string
		c.Bind(path, func(cmd *cobra.Command, args []string) error {
			got = args
			return nil
		})
		argv := []string{path, "--source", "/x", "--finish-update"}
		if err := c.Execute(argv); err != nil || strings.Join(got, " ") != "--source /x --finish-update" {
			t.Fatalf("%s: %v %q", path, err, got)
		}
	}
}

func TestGmiMainFlowChatParserAcceptsGmiProvider(t *testing.T) {
	ns, err := chatNamespace(t, []string{"chat", "--provider", "gmi"})
	if err != nil || ns["provider"] != "gmi" {
		t.Fatalf("namespace %v err %v", ns, err)
	}
}
