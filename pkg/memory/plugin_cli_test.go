package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/home"
)

type fakeProviderCLIRun struct {
	name, dir, module, desc string
	handler                 bool
	argv                    []string
}

// Swaps the plugin host CLI seams for fakes recording every run
func fakeProviderCLIHost(t *testing.T, registerCLI, handler bool, describeErr error, rc int, dispatched bool) *[]fakeProviderCLIRun {
	runs := &[]fakeProviderCLIRun{}
	oldDescribe, oldRun := describeProviderCLI, runProviderCLI
	t.Cleanup(func() { describeProviderCLI, runProviderCLI = oldDescribe, oldRun })
	describeProviderCLI = func(ctx context.Context, name, dir, module string) (bool, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module})
		return registerCLI, handler, describeErr
	}
	runProviderCLI = func(ctx context.Context, name, dir, module, desc string, withHandler bool, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool, error) {
		*runs = append(*runs, fakeProviderCLIRun{name: name, dir: dir, module: module, desc: desc, handler: withHandler, argv: argv})
		io.WriteString(stdout, "ran "+strings.Join(argv, " "))
		return rc, dispatched, nil
	}
	return runs
}

// Writes a user provider with a cli.py and plugin.yaml and makes it the active provider
func cliProviderHome(t *testing.T, name, manifest string) (context.Context, string) {
	ctx, h := discHome(t)
	dir := discProvider(t, filepath.Join(h, "plugins"), name)
	discWrite(t, filepath.Join(dir, "cli.py"), "def register_cli(subparser):\n    subparser.add_argument('--test')\n\ndef "+name+"_command(args):\n    pass\n")
	if manifest != "" {
		discWrite(t, filepath.Join(dir, "plugin.yaml"), manifest)
	}
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: "+name+"\n")
	return ctx, dir
}

func TestDiscoversActivePluginWithRegisterCLI(t *testing.T) {
	ctx, dir := cliProviderHome(t, "testplugin", "name: testplugin\ndescription: A test plugin\n")
	other := discProvider(t, filepath.Dir(dir), "otherplugin")
	discWrite(t, filepath.Join(other, "cli.py"), "def register_cli(subparser):\n    subparser.add_argument('--other')\n")
	runs := fakeProviderCLIHost(t, true, true, nil, 0, true)
	cmds := DiscoverPluginCLICommands(ctx, nil)
	if len(cmds) != 1 || cmds[0].Name != "testplugin" || cmds[0].Help != "A test plugin" || cmds[0].Description != "A test plugin" ||
		cmds[0].Plugin != "testplugin" || cmds[0].Setup == nil || cmds[0].Handler == nil {
		t.Fatalf("commands %+v", cmds)
	}
	if len(*runs) != 1 || (*runs)[0].name != "testplugin" || (*runs)[0].dir != dir || (*runs)[0].module != providerDirModuleName(dir, "testplugin") {
		t.Fatalf("describe calls %+v", *runs)
	}
	root := &cobra.Command{Use: "icebath"}
	cmd := &cobra.Command{Use: "testplugin", Short: cmds[0].Help}
	root.AddCommand(cmd)
	cmds[0].Setup(cmd)
	cmd.RunE = cmds[0].Handler
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"testplugin", "--test", "x", "-h"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	last := (*runs)[len(*runs)-1]
	if out.String() != "ran --test x -h" || !last.handler || last.desc != "A test plugin" || strings.Join(last.argv, " ") != "--test x -h" {
		t.Fatalf("run %+v output %q", last, out.String())
	}
}

func TestReturnsNothingWhenNoActiveProvider(t *testing.T) {
	ctx, dir := cliProviderHome(t, "testplugin", "")
	discWrite(t, filepath.Join(filepath.Dir(filepath.Dir(dir)), "config.yaml"), "memory: {}\n")
	runs := fakeProviderCLIHost(t, true, true, nil, 0, true)
	if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 || len(*runs) != 0 {
		t.Fatalf("commands %+v calls %+v", cmds, *runs)
	}
}

func TestPluginCLIDiscoveryRefusalsAndFailures(t *testing.T) {
	t.Run("no cli.py", func(t *testing.T) {
		ctx, dir := cliProviderHome(t, "nocli", "")
		if err := os.Remove(filepath.Join(dir, "cli.py")); err != nil {
			t.Fatal(err)
		}
		runs := fakeProviderCLIHost(t, true, true, nil, 0, true)
		if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 || len(*runs) != 0 {
			t.Fatalf("commands %+v calls %+v", cmds, *runs)
		}
	})
	t.Run("host isolation refuses", func(t *testing.T) {
		ctx, dir := cliProviderHome(t, "isolated", "")
		discWrite(t, filepath.Join(filepath.Dir(filepath.Dir(dir)), "config.yaml"), "memory:\n  provider: isolated\nplugins:\n  isolation: host\n")
		runs := fakeProviderCLIHost(t, true, true, nil, 0, true)
		if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 || len(*runs) != 0 {
			t.Fatalf("commands %+v calls %+v", cmds, *runs)
		}
	})
	t.Run("no register_cli", func(t *testing.T) {
		ctx, _ := cliProviderHome(t, "noreg", "")
		fakeProviderCLIHost(t, false, true, nil, 0, true)
		if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 {
			t.Fatalf("commands %+v", cmds)
		}
	})
	t.Run("import failure", func(t *testing.T) {
		ctx, _ := cliProviderHome(t, "broken", "")
		fakeProviderCLIHost(t, true, true, errors.New("SyntaxError: invalid syntax"), 0, true)
		if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 {
			t.Fatalf("commands %+v", cmds)
		}
	})
	t.Run("no handler and generic help", func(t *testing.T) {
		ctx, _ := cliProviderHome(t, "nohandler", "")
		runs := fakeProviderCLIHost(t, true, false, nil, 3, true)
		cmds := DiscoverPluginCLICommands(ctx, nil)
		if len(cmds) != 1 || cmds[0].Handler != nil || cmds[0].Help != "Manage nohandler memory plugin" || cmds[0].Description != "" {
			t.Fatalf("commands %+v", cmds)
		}
		root := &cobra.Command{Use: "icebath"}
		cmd := &cobra.Command{Use: "nohandler"}
		root.AddCommand(cmd)
		cmds[0].Setup(cmd)
		root.SetOut(io.Discard)
		root.SetArgs([]string{"nohandler", "status"})
		var exit *PluginCLIExit
		if err := root.Execute(); !errors.As(err, &exit) || exit.ExitCode() != 3 || (*runs)[len(*runs)-1].handler {
			t.Fatalf("error %v runs %+v", err, *runs)
		}
	})
	t.Run("undispatched prints the top level help", func(t *testing.T) {
		ctx, _ := cliProviderHome(t, "nofunc", "")
		fakeProviderCLIHost(t, true, false, nil, 0, false)
		cmds := DiscoverPluginCLICommands(ctx, nil)
		root := &cobra.Command{Use: "icebath", Long: "top level help text"}
		cmd := &cobra.Command{Use: "nofunc"}
		root.AddCommand(cmd)
		cmds[0].Setup(cmd)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"nofunc"})
		if err := root.Execute(); err != nil || !strings.Contains(out.String(), "top level help text") {
			t.Fatalf("error %v output %q", err, out.String())
		}
	})
}

// Swaps the plugin host schema seam for one answering per path and counting loads
func fakeSchemaHost(t *testing.T, answer func(path string) (any, error)) map[string]int {
	calls := map[string]int{}
	old := providerConfigSchema
	t.Cleanup(func() { providerConfigSchema = old })
	providerConfigSchema = func(ctx context.Context, path string) (any, error) {
		calls[path]++
		return answer(path)
	}
	return calls
}

func TestUserProviderConfigSchemaRunsInThePluginHost(t *testing.T) {
	isolateRegistry(t)
	root := t.TempDir()
	dirs := map[string]string{}
	for _, label := range []string{"A", "B"} {
		h := filepath.Join(root, label)
		dirs[label] = discProvider(t, filepath.Join(h, "plugins"), "custom")
		discWrite(t, filepath.Join(dirs[label], "config_schema.py"), "CONFIG_SCHEMA = None\n")
	}
	calls := fakeSchemaHost(t, func(path string) (any, error) {
		label := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
		return map[string]any{"name": "custom", "label": label, "fields": []any{map[string]any{"key": "api_key", "label": "Key", "kind": "secret"}}}, nil
	})
	t.Setenv("PYTHONPATH", "")
	for _, label := range []string{"A", "B", "A"} {
		ctx := home.WithOverride(context.Background(), filepath.Join(root, label))
		s := GetProviderConfigSchema(ctx, nil, "custom")
		if s == nil || s.Label != label || len(s.Fields) != 1 || s.Fields[0].Key != "api_key" {
			t.Fatalf("profile %s schema %+v", label, s)
		}
	}
	pathA := filepath.Join(dirs["A"], "config_schema.py")
	if calls[pathA] != 1 || calls[filepath.Join(dirs["B"], "config_schema.py")] != 1 {
		t.Fatalf("schema loads %v", calls)
	}
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if s := GetProviderConfigSchema(home.WithOverride(context.Background(), filepath.Join(root, "A")), nil, "custom"); s != nil {
		t.Fatalf("schema served after its file went away %+v", s)
	}
}

func TestUserProviderConfigSchemaFailuresAndEmptyRecords(t *testing.T) {
	isolateRegistry(t)
	ctx, h := discHome(t)
	dir := discProvider(t, filepath.Join(h, "plugins"), "brokenmem")
	answer := []any{errors.New("SyntaxError: invalid syntax"), nil, []any{"not", "a", "mapping"}, map[string]any{"name": "brokenmem", "label": "Broken"}}
	step := 0
	fakeSchemaHost(t, func(path string) (any, error) {
		a := answer[step]
		step++
		if err, ok := a.(error); ok {
			return nil, err
		}
		return a, nil
	})
	if s := GetProviderConfigSchema(ctx, nil, "brokenmem"); s != nil {
		t.Fatalf("schema without config_schema.py %+v", s)
	}
	if step != 0 {
		t.Fatal("the host ran a missing schema file")
	}
	discWrite(t, filepath.Join(dir, "config_schema.py"), "this is not python(")
	for i := 0; i < 3; i++ {
		if s := GetProviderConfigSchema(ctx, nil, "brokenmem"); s != nil {
			t.Fatalf("load %d answered %+v", i, s)
		}
	}
	if s := GetProviderConfigSchema(ctx, nil, "brokenmem"); s == nil || s.Label != "Broken" || step != 4 {
		t.Fatalf("recovered schema %+v after %d loads", s, step)
	}
}
