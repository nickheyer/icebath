package commands

import (
	"bytes"
	"strings"
	"testing"
)

func TestArgparseHelpPause(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("COLUMNS", "100")
	c := NewCLI()
	var out bytes.Buffer
	c.Root.SetOut(&out)
	c.Root.SetArgs([]string{"pause", "--help"})
	if err := c.Root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "usage: icebath pause [-h] [--reason REASON]\n\n" +
		"Engage the global emergency stop. Halts NEW work only — cron dispatch, kanban dispatch, and new\n" +
		"gateway turns — until `icebath resume`. Interactive CLI, TUI and Desktop turns are gateway turns,\n" +
		"so they are held too: each gets \"Icebath is paused\" with the reason. Turns already running finish;\n" +
		"in-flight work is never killed.\n\n" +
		"options:\n" +
		"  -h, --help       show this help message and exit\n" +
		"  --reason REASON  Optional reason stored in the sentinel and shown to users\n"
	if out.String() != want {
		t.Fatalf("pause help\n%s\nwant\n%s", out.String(), want)
	}
}

func TestArgparseHelpSubparsersAndWrappedUsage(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("COLUMNS", "60")
	c := NewCLI()
	moa := c.find("moa")
	got := argparseHelp(moa)
	want := "usage: icebath moa [-h]\n                   {list,ls,configure,config,delete,rm} ...\n\n" +
		"Configure the provider/model set used by /moa <prompt>.\n\n" +
		"positional arguments:\n" +
		"  {list,ls,configure,config,delete,rm}\n" +
		"    list (ls)           Show current MoA model slots\n" +
		"    configure (config)  Interactively pick MoA models\n" +
		"    delete (rm)         Delete a MoA preset\n\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n"
	if got != want {
		t.Fatalf("moa help\n%s\nwant\n%s", got, want)
	}
	usage := argparseUsage(c.find("chat"))
	lines := strings.Split(strings.TrimSuffix(usage, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "usage: icebath chat [-h] [-q QUERY | --query-file PATH]") || len(lines) < 4 {
		t.Fatalf("chat usage %q", usage)
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, strings.Repeat(" ", len("usage: icebath chat "))) || len(line) > 58 {
			t.Fatalf("chat usage continuation %q", line)
		}
	}
}

func TestArgparseTableMatchesCobraTree(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := NewCLI()
	for path, p := range argparseTree {
		cmd := c.Root
		if path != "" {
			cmd = c.find(path)
		}
		if cmd == nil {
			t.Errorf("parser %q has no cobra command", path)
			continue
		}
		for _, a := range p.Acts {
			if _, ok := apOptionHelp(cmd, a); a.Kind == 'f' && !ok {
				t.Errorf("parser %q option %v has no cobra flag", path, a.Opts)
			}
		}
	}
}
