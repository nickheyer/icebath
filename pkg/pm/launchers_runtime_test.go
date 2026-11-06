package pm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestLaunchersRuntimeCommandPinsTheHomeBeforeTheSubcommand(t *testing.T) {
	_, root := lnFakeInstall(t)
	binary := filepath.Join(root, "icebath")
	got := InstallRuntimeCommand(root, []string{"gateway", "run"}, LaunchCommandOptions{})
	if strings.Join(got, "\x00") != strings.Join([]string{binary, PinDefaultHomeFlag, "gateway", "run"}, "\x00") {
		t.Fatalf("default home command %q", got)
	}
	got = InstallRuntimeCommand(root, []string{"run"}, LaunchCommandOptions{Subcommand: []string{"acp"}, Home: "/srv/home", Binary: "/opt/other"})
	if strings.Join(got, "\x00") != strings.Join([]string{"/opt/other", PinDefaultHomeFlag + "=/srv/home", "acp", "run"}, "\x00") {
		t.Fatalf("literal home command %q", got)
	}
	installed := InstallationCommand(root, []string{"gateway", "run"}, LaunchCommandOptions{Subcommand: []string{"cron"}})
	if strings.Join(installed, "\x00") != strings.Join([]string{binary, "cron", "gateway", "run"}, "\x00") {
		t.Fatalf("installation command %q", installed)
	}
	os.Remove(binary)
	self, _ := os.Executable()
	if fallback := InstallationCommand(root, nil, LaunchCommandOptions{}); len(fallback) != 2 || fallback[0] != self || fallback[1] != PinDefaultHomeFlag {
		t.Fatalf("binaryless installation command %q", fallback)
	}
}

func TestLaunchersPrintRuntimeCommandEmitsTheJSONArgv(t *testing.T) {
	_, root := lnFakeInstall(t)
	binary := filepath.Join(root, "icebath")
	for _, c := range []struct {
		argv []string
		want []string
	}{
		{[]string{"--print-runtime-command"}, []string{binary, PinDefaultHomeFlag}},
		{[]string{"--print-runtime-command", "--", "chat", "-q", "hi"}, []string{binary, PinDefaultHomeFlag, "chat", "-q", "hi"}},
		{[]string{"--print-runtime-command", "--module", "acp", "--", "--flag"}, []string{binary, PinDefaultHomeFlag, "acp", "--flag"}},
	} {
		var stdout, stderr strings.Builder
		if code := LaunchersMain(context.Background(), root, c.argv, &stdout, &stderr); code != 0 {
			t.Fatalf("%q exited %d %q", c.argv, code, stderr.String())
		}
		parsed, err := pyjson.Loads(stdout.String())
		list, _ := parsed.([]any)
		if err != nil || len(list) != len(c.want) {
			t.Fatalf("%q printed %q", c.argv, stdout.String())
		}
		for i, part := range c.want {
			if list[i] != part {
				t.Fatalf("%q printed %q", c.argv, stdout.String())
			}
		}
	}
	var stdout, stderr strings.Builder
	if code := LaunchersMain(context.Background(), root, []string{"--print-runtime-command", "--bogus"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "unrecognized arguments: --bogus") {
		t.Fatalf("bad option exited %d %q", code, stderr.String())
	}
}

func TestLaunchersMainPublishesOrNamesWhyItCannot(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	out := filepath.Join(userHome, ".local", "bin")
	var stdout, stderr strings.Builder
	if code := LaunchersMain(context.Background(), root, []string{out}, &stdout, &stderr); code != 0 {
		t.Fatalf("publication exited %d %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(entryPoints) {
		t.Fatalf("published %q", lines)
	}
	for _, line := range lines {
		if _, err := os.Lstat(line); err != nil || filepath.Dir(line) != out {
			t.Fatalf("published path %s %v", line, err)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := LaunchersMain(context.Background(), root, nil, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "the following arguments are required: out_dir") {
		t.Fatalf("missing out_dir exited %d %q", code, stderr.String())
	}
	os.Remove(filepath.Join(root, "icebath"))
	stderr.Reset()
	if code := LaunchersMain(context.Background(), root, []string{out}, &stdout, &stderr); code != 1 ||
		stderr.String() != "icebath: store interpreter is missing; finish pm install before publishing launchers\n" {
		t.Fatalf("binaryless publication exited %d %q", code, stderr.String())
	}
}

func TestLaunchersMainReportsAFailedPublication(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	blocker := filepath.Join(userHome, "blocked")
	os.WriteFile(blocker, []byte("file"), 0o644)
	var stdout, stderr strings.Builder
	code := LaunchersMain(context.Background(), root, []string{filepath.Join(blocker, "bin")}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("blocked publication exited %d %q %q", code, stdout.String(), stderr.String())
	}
}

func TestLaunchersMainParsesLikeArgparse(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	var stdout, stderr strings.Builder
	if code := LaunchersMain(context.Background(), root, []string{"--print-runtime-command", "--mod", "acp", "--", "x"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), `"acp", "x"]`) {
		t.Fatalf("abbreviated module exited %d %q %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := LaunchersMain(context.Background(), root, []string{"--bogus"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "the following arguments are required: out_dir") {
		t.Fatalf("option without out_dir exited %d %q", code, stderr.String())
	}
	stderr.Reset()
	out := filepath.Join(userHome, "bin")
	if code := LaunchersMain(context.Background(), root, []string{out, "--bogus", "extra"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "unrecognized arguments: --bogus extra") {
		t.Fatalf("extras exited %d %q", code, stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("rejected arguments still created %s", out)
	}
}
