package gateway

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSystemdProbesSelectScopeAndReadUnitState(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	pinServiceSeams(t, "linux", 1000, filepath.Join(tmp, "etc"))
	var calls []string
	pinSystemdHost(t, "linux", false, false, false, true, func(argv []string) (CommandResult, error) {
		calls = append(calls, strings.Join(argv, " "))
		switch {
		case slices.Contains(argv, "is-active"):
			if slices.Contains(argv, "--user") {
				return CommandResult{Stdout: "inactive\n", ReturnCode: 3}, nil
			}
			return CommandResult{Stdout: "active\n"}, nil
		case slices.Contains(argv, "show"):
			return CommandResult{Stdout: "MainPID=812\nActiveState= active \nEnvironment=ICEBATH_HOME=/srv/pinned LANG=C\n"}, nil
		}
		return CommandResult{}, nil
	})
	if scope, running, err := ProbeSystemdServiceRunning(ctx, false); err != nil || scope || running {
		t.Fatalf("nothing installed %v %v %v", scope, running, err)
	}
	sysUnit, _ := GetSystemdUnitPath(ctx, true)
	writeUnit(t, sysUnit, "[Service]\n")
	if scope, running, err := ProbeSystemdServiceRunning(ctx, false); err != nil || !scope || !running {
		t.Fatalf("system only unit %v %v %v", scope, running, err)
	}
	userUnit, _ := GetSystemdUnitPath(ctx, false)
	writeUnit(t, userUnit, "[Service]\n")
	if scope, running, err := ProbeSystemdServiceRunning(ctx, false); err != nil || scope || running {
		t.Fatalf("user unit wins %v %v %v", scope, running, err)
	}
	if scopes, err := GetInstalledSystemdScopes(ctx); err != nil || !slices.Equal(scopes, []string{"user", "system"}) {
		t.Fatalf("scopes %v %v", scopes, err)
	}
	if pid, ok, err := SystemdMainPID(ctx, true); err != nil || !ok || pid != 812 {
		t.Fatalf("main pid %d %v %v", pid, ok, err)
	}
	props, _ := ReadSystemdUnitProperties(ctx, false, "ActiveState")
	if props["ActiveState"] != "active" || !strings.Contains(calls[len(calls)-1], "--property ActiveState") {
		t.Fatalf("properties %v %v", props, calls)
	}
	if err := SyncHermesHomeFromSystemdUnit(ctx, false); err != nil || os.Getenv("ICEBATH_HOME") != filepath.Join(tmp, "home") {
		t.Fatal("user scope must not adopt a unit home")
	}
	if err := SyncHermesHomeFromSystemdUnit(ctx, true); err != nil || os.Getenv("ICEBATH_HOME") != "/srv/pinned" {
		t.Fatalf("show environment adoption %q %v", os.Getenv("ICEBATH_HOME"), err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	writeUnit(t, sysUnit, "[Service]\nEnvironment=\"ICEBATH_HOME=/srv/from-unit\"\n")
	if err := SyncHermesHomeFromSystemdUnit(ctx, true); err != nil || os.Getenv("ICEBATH_HOME") != "/srv/from-unit" {
		t.Fatalf("unit file adoption %q %v", os.Getenv("ICEBATH_HOME"), err)
	}
	pinSystemdHost(t, "linux", false, false, false, true, func(argv []string) (CommandResult, error) { return CommandResult{}, ErrCommandNotFound })
	if active, err := SystemdUnitIsActive(ctx, true); err != nil || active {
		t.Fatalf("missing systemctl reads inactive %v %v", active, err)
	}
	if props, err := ReadSystemdUnitProperties(ctx, true); err != nil || len(props) != 0 {
		t.Fatalf("missing systemctl reads no properties %v %v", props, err)
	}
}

func TestLaunchdProbeNeedsThePlistAndASupervisedPID(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("HOME", tmp)
	pinServiceSeams(t, "darwin", 501, "")
	serviceLookupAccount = func() (*user.User, error) { return &user.User{HomeDir: tmp}, nil }
	pid := "\t\"PID\" = 4410;"
	pinSystemdHost(t, "darwin", false, false, false, true, func(argv []string) (CommandResult, error) {
		if argv[1] == "list" {
			return CommandResult{Stdout: "{\n" + pid + "\n};"}, nil
		}
		return CommandResult{ReturnCode: 1}, nil
	})
	if ok, err := ProbeLaunchdServiceRunning(ctx); err != nil || ok {
		t.Fatalf("no plist %v %v", ok, err)
	}
	plist, _ := GetLaunchdPlistPath(ctx)
	writeUnit(t, plist, "<plist/>")
	if ok, err := ProbeLaunchdServiceRunning(ctx); err != nil || !ok {
		t.Fatalf("supervised %v %v", ok, err)
	}
	pid = "\t\"LastExitStatus\" = 0;"
	if ok, err := ProbeLaunchdServiceRunning(ctx); err != nil || ok {
		t.Fatalf("registered without a process %v %v", ok, err)
	}
}
