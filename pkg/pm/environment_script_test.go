package pm

import (
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Values every dialect must carry through its quoting unchanged, in probe order
var esNasty = []string{
	"PLAIN=value",
	"SPACES=a b  c",
	"QUOTES=it's a \"test\"",
	"SHELL_SYNTAX=$HOME `id` $(id) ; & | > < * ? [x] ~ #",
	"BACKSLASHES=C:\\path\\to\\ \\n \\'",
	"EMPTY=",
	"MULTILINE=first\nsecond",
}

// Script printing each value NUL terminated in the order of the nasty values
func esProbe() string {
	names := make([]string, len(esNasty))
	for i, kv := range esNasty {
		name, _, _ := strings.Cut(kv, "=")
		names[i] = `"$` + name + `"`
	}
	return "printf '%s\\0' " + strings.Join(names, " ")
}

// Shell argv prefix that evaluates a script of the dialect, skipping when fish is not installed
func esShell(t *testing.T, dialect string) []string {
	t.Helper()
	if dialect == "sh" {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Fatal("bash is not on PATH")
		}
		return []string{bash, "-c"}
	}
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	return []string{fish, "--no-config", "-c"}
}

// Script the dialect evaluates, failing the test on an unknown dialect
func esExports(t *testing.T, env []string, dialect string) string {
	t.Helper()
	script, err := ShellExports(env, dialect)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

// Runs a script through the shell and returns its exit status, stdout and stderr
func esRun(t *testing.T, shell []string, script string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(shell[0], append(append([]string{}, shell[1:]...), script)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code, err := exitStatus(cmd.Run())
	if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestARealShellReadsEveryValueBackUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	want := make([]string, len(esNasty))
	for i, kv := range esNasty {
		_, want[i], _ = strings.Cut(kv, "=")
	}
	texts := map[string]string{
		"sh": "export PLAIN=value\nexport SPACES='a b  c'\nexport QUOTES='it'\"'\"'s a \"test\"'\n" +
			"export SHELL_SYNTAX='$HOME `id` $(id) ; & | > < * ? [x] ~ #'\nexport BACKSLASHES='C:\\path\\to\\ \\n \\'\"'\"''\n" +
			"export EMPTY=''\nexport MULTILINE='first\nsecond'",
		"fish": "set -gx PLAIN 'value'\nset -gx SPACES 'a b  c'\nset -gx QUOTES 'it\\'s a \"test\"'\n" +
			"set -gx SHELL_SYNTAX '$HOME `id` $(id) ; & | > < * ? [x] ~ #'\nset -gx BACKSLASHES 'C:\\\\path\\\\to\\\\ \\\\n \\\\\\''\n" +
			"set -gx EMPTY ''\nset -gx MULTILINE 'first\nsecond'",
	}
	for _, dialect := range []string{"sh", "fish"} {
		t.Run(dialect, func(t *testing.T) {
			exports := esExports(t, esNasty, dialect)
			if exports != texts[dialect] {
				t.Fatalf("script %q", exports)
			}
			script := exports + "\n" + esProbe()
			code, stdout, stderr := esRun(t, esShell(t, dialect), script)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			got := strings.Split(stdout, "\x00")
			if !reflect.DeepEqual(got[:len(got)-1], want) {
				t.Fatalf("read back %q", got)
			}
		})
	}
}

func TestFishKeepsPathVariablesAsLists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	script := esExports(t, []string{"PATH=/a b:/c", "PYTHONPATH=/x:/y"}, "fish")
	if script != "set -gx PATH '/a b:/c'\nset -gx PYTHONPATH '/x:/y'" {
		t.Fatalf("script %q", script)
	}
	code, stdout, stderr := esRun(t, esShell(t, "fish"), script+"\nprintf '%s ' (count $PATH) (count $PYTHONPATH)")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := strings.Fields(stdout); !reflect.DeepEqual(got, []string{"2", "2"}) {
		t.Fatalf("counts %q", got)
	}
}

func TestFishReadOnlyVariablesDoNotAbortTheScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	script := esExports(t, []string{"PWD=/elsewhere", "SHLVL=9", "AFTER=reached"}, "fish")
	if script != "set -gx AFTER 'reached'" {
		t.Fatalf("script %q", script)
	}
	code, stdout, stderr := esRun(t, esShell(t, "fish"), script+"\nprintf %s $AFTER")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "reached" {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestNamesNoShellCanAssignAreLeftOut(t *testing.T) {
	for _, dialect := range []string{"sh", "fish"} {
		t.Run(dialect, func(t *testing.T) {
			script := esExports(t, []string{"ProgramFiles(ARM)=x", "1BAD=x", "GOOD=y"}, dialect)
			if !strings.Contains(script, "GOOD") || strings.Contains(script, "ProgramFiles") || strings.Contains(script, "1BAD") {
				t.Fatalf("script %q", script)
			}
		})
	}
}
