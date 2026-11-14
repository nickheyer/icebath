package install

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLauncherResolutionIsInstallationBound(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if got := ResolveInstalledCommand(context.Background(), root); got != "" {
		t.Fatalf("resolved %q with nothing installed", got)
	}
	binary := InstallBinaryPath(root)
	os.WriteFile(binary, []byte("bin"), 0o755)
	if got := ResolveInstalledCommand(context.Background(), root); got != binary {
		t.Fatalf("got %q want the install binary", got)
	}
	name := "icebath"
	if runtime.GOOS == "windows" {
		name = "icebath.cmd"
	}
	launcher := filepath.Join(root, ".icebath", "bin", name)
	os.MkdirAll(filepath.Dir(launcher), 0o755)
	os.WriteFile(launcher, []byte("new"), 0o755)
	if got := ResolveInstalledCommand(context.Background(), root); got != launcher {
		t.Fatalf("got %q want the checkout launcher", got)
	}
}

func TestMissingFileIsNotLocked(t *testing.T) {
	if IsLocked("/nonexistent/does/not/exist/xyz") {
		t.Fatal("missing file reported locked")
	}
}

func TestUpdateChildEnvNamesOurPidForTheLockHandoff(t *testing.T) {
	env := strings.Join(UpdateChildEnv("/x/icebath-agent"), "\n")
	if !strings.Contains(env, "ICEBATH_UPDATE_HANDOFF_PID="+itoa(os.Getpid())) {
		t.Fatalf("handoff pid missing from %q", env)
	}
	if !strings.Contains(env, "ICEBATH_INSTALL_ROOT=/x/icebath-agent") {
		t.Fatalf("install root missing from %q", env)
	}
}

func TestLockProbePathsIncludeDesktopAppPayload(t *testing.T) {
	root := "/x/icebath-agent"
	probes := InstallLockProbePaths(root)
	if len(probes) == 0 {
		t.Fatal("no probes")
	}
	for _, p := range probes {
		if !strings.HasPrefix(p, filepath.Join(root, "apps/desktop/release")) {
			t.Fatalf("%s is not a replaced application output", p)
		}
	}
}

func TestLockProbePathsCoverArm64LinuxBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux layout")
	}
	probes := strings.Join(InstallLockProbePaths("/x/icebath-agent"), "\n")
	for _, dir := range []string{"linux-unpacked", "linux-arm64-unpacked"} {
		if !strings.Contains(probes, filepath.Join("/x/icebath-agent/apps/desktop/release", dir, "icebath")) {
			t.Fatalf("%s payload must be probed", dir)
		}
	}
}

func TestLockedPathsIgnoresMissingPayloads(t *testing.T) {
	if got := LockedPaths(InstallLockProbePaths("/nonexistent/icebath-agent")); len(got) != 0 {
		t.Fatalf("locked %v", got)
	}
}

func TestConcurrentUpdateMessageShowsTheChildsRefusalBlock(t *testing.T) {
	tail := strings.Split("→ Fetching updates...\n✓ Updated to 6b2c23ae42\n✗ Another Icebath update is already running (started 3m 42s ago, process 65285).\n\n  Wait for it to finish, then run `icebath update` again.", "\n")
	want := "✗ Another Icebath update is already running (started 3m 42s ago, process 65285).\n\n  Wait for it to finish, then run `icebath update` again."
	if got := ConcurrentUpdateMessage(tail); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestConcurrentUpdateMessageFallsBackWithoutARefusalBlock(t *testing.T) {
	generic := "Icebath is still running. Close all Icebath windows and try the update again."
	if ConcurrentUpdateMessage(nil) != generic || ConcurrentUpdateMessage([]string{"→ Fetching updates..."}) != generic {
		t.Fatal("generic fallback expected")
	}
}

func TestUpdateTargetsTheCheckedChannelBeforeAnyBranch(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--update", "--branch", "bb/test"}, "--branch bb/test"},
		{[]string{"--update", "--branch=main"}, "--branch main"},
		{[]string{"--update", "--channel", "stable", "--target-app", "/A.app"}, "--channel stable"},
		{[]string{"--update", "--branch", "main", "--channel=stable"}, "--channel stable"},
	}
	for _, c := range cases {
		if got := strings.Join(UpdateTargetFromArgs(c.args), " "); got != c.want {
			t.Fatalf("%v: got %q want %q", c.args, got, c.want)
		}
	}
	if UpdateTargetFromArgs([]string{"--update"})[0] != "--branch" {
		t.Fatal("bare update must target a branch")
	}
}

func TestUpdateManifestLeadsWithHandoffAndGatesInstall(t *testing.T) {
	base := UpdateStages(true, false)
	if base[0].Name != "handoff" {
		t.Fatal("the lock wait must surface as the first visible step")
	}
	names := map[string]bool{}
	for _, s := range base {
		names[s.Name] = true
	}
	if !names["update"] || !names["desktop"] || names["install"] {
		t.Fatalf("stages %v", names)
	}
	withInstall := UpdateStages(true, true)
	if withInstall[len(withInstall)-1].Name != "install" || len(withInstall) != len(base)+1 {
		t.Fatal("include_install adds exactly one final stage")
	}
	if len(UpdateStages(false, false)) != 2 {
		t.Fatal("no desktop refresh without an installed desktop")
	}
}

func TestParsesOnlyAppTargets(t *testing.T) {
	if got := TargetAppFromArgs([]string{"--update", "--target-app", "/Applications/Icebath.app"}); got != "/Applications/Icebath.app" {
		t.Fatalf("got %q", got)
	}
	if got := TargetAppFromArgs([]string{"--target-app", "/tmp/not-an-app"}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func writeBundleMarker(t *testing.T, dir, contents string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(contents), 0o644)
}

func TestSwapInstallsNewBundleAndCleansUp(t *testing.T) {
	base := t.TempDir()
	target, tmp, old := filepath.Join(base, "Icebath.app"), filepath.Join(base, "Icebath.app.icebath-update-new"), filepath.Join(base, "Icebath.app.icebath-update-old")
	writeBundleMarker(t, target, "OLD")
	writeBundleMarker(t, tmp, "NEW")
	if err := SwapInNewBundle(tmp, target, old); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(target, "marker.txt")); string(data) != "NEW" {
		t.Fatalf("target holds %q", data)
	}
	for _, p := range []string{tmp, old} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s left behind", p)
		}
	}
}

func TestSwapFailureNeverLeavesTargetMissing(t *testing.T) {
	base := t.TempDir()
	target, tmp, old := filepath.Join(base, "Icebath.app"), filepath.Join(base, "Icebath.app.icebath-update-new"), filepath.Join(base, "Icebath.app.icebath-update-old")
	writeBundleMarker(t, target, "OLD")
	writeBundleMarker(t, old, "OCCUPIED")
	if SwapInNewBundle(tmp, target, old) == nil {
		t.Fatal("swap should fail when neither move can complete")
	}
	if data, _ := os.ReadFile(filepath.Join(target, "marker.txt")); string(data) != "OLD" {
		t.Fatal("original app contents must be intact after a failed swap")
	}
}

func TestSwapRollsBackWhenInstallStepFails(t *testing.T) {
	base := t.TempDir()
	target, tmp, old := filepath.Join(base, "Icebath.app"), filepath.Join(base, "Icebath.app.icebath-update-new"), filepath.Join(base, "Icebath.app.icebath-update-old")
	writeBundleMarker(t, target, "OLD")
	if SwapInNewBundle(tmp, target, old) == nil {
		t.Fatal("swap should fail without a staged bundle")
	}
	if data, _ := os.ReadFile(filepath.Join(target, "marker.txt")); string(data) != "OLD" {
		t.Fatal("original must be restored after failed install")
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("backup should be rolled back, not left behind")
	}
}

// Installed command double printing a refusal block then exiting with FAKE_EXIT
func fakeInstalledCommand(t *testing.T, root string) {
	t.Helper()
	os.MkdirAll(root, 0o755)
	script := "#!/bin/sh\necho \"→ Fetching updates...\"\necho \"✗ Another Icebath update is already running (process 4242).\"\necho \"args: $*\"\nexit ${FAKE_EXIT:-0}\n"
	os.WriteFile(InstallBinaryPath(root), []byte(script), 0o755)
}

func TestRunUpdateStreamsTheChildAndCompletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell double")
	}
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("HOME", t.TempDir())
	root := InstallRootOf(homeDir)
	fakeInstalledCommand(t, root)
	t.Setenv("FAKE_EXIT", "0")
	var events []Event
	exited := false
	err := RunUpdate(context.Background(), UpdateHost{Args: []string{"--update", "--channel", "stable"}, Emit: func(e Event) { events = append(events, e) }, Exit: func() { exited = true }})
	if err != nil || !exited {
		t.Fatalf("update err=%v exited=%v", err, exited)
	}
	var seq, logs []string
	for _, e := range events {
		switch e.Type {
		case EventStage:
			seq = append(seq, e.Name+":"+string(e.State))
		case EventLog:
			logs = append(logs, e.Line)
		default:
			seq = append(seq, string(e.Type))
		}
	}
	if got := strings.Join(seq, " "); got != "manifest handoff:running handoff:succeeded update:running update:succeeded complete" {
		t.Fatalf("sequence %s", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "args: update --yes --gateway --channel stable") {
		t.Fatalf("child argv not streamed: %v", logs)
	}
	if _, err := os.Stat(UpdateMarkerPath()); err == nil {
		t.Fatal("marker must be released after the update")
	}
}

func TestRunUpdateSurfacesTheChildsRefusalOnExit2(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell double")
	}
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("HOME", t.TempDir())
	fakeInstalledCommand(t, InstallRootOf(homeDir))
	t.Setenv("FAKE_EXIT", "2")
	var last Event
	err := RunUpdate(context.Background(), UpdateHost{Args: []string{"--update"}, Emit: func(e Event) { last = e }})
	if err == nil || last.Type != EventFailed || !strings.HasPrefix(*last.Error, "✗ Another Icebath update is already running (process 4242).") {
		t.Fatalf("err=%v last=%+v", err, last)
	}
}
