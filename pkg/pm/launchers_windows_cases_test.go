//go:build windows

package pm

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Records User PATH registrations instead of writing the registry, answering added
func recordUserPathWrites(t *testing.T) *[]string {
	t.Helper()
	var registered []string
	prev := registerUserPath
	registerUserPath = func(entry string) (string, error) {
		registered = append(registered, entry)
		return "added", nil
	}
	t.Cleanup(func() { registerUserPath = prev })
	return &registered
}

// Replaces the environment a new console would see for one case
func fakeConsoleEnvironment(t *testing.T, environ map[string]string, known bool) {
	t.Helper()
	prev := newConsoleEnvironment
	newConsoleEnvironment = func() (map[string]string, bool) { return environ, known }
	t.Cleanup(func() { newConsoleEnvironment = prev })
}

// The _checkout helper, a git tree with a self update stamp and its own install binary
func fakeCheckout(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(path, "install-stamp.json"), `{"updateMechanism": "self"}`, 0o644)
	mustWrite(t, filepath.Join(path, installBinaryName()), "MZ fake install binary\n", 0o755)
	return resolvePath(path)
}

// The _windows_home helper, the default data root under a temp LOCALAPPDATA that new consoles resolve
func windowsHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	local := filepath.Join(tmp, "local")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv(home.EnvHome, "")
	os.Unsetenv(home.EnvHome)
	t.Setenv(EnvRuntimeDir, "")
	os.Unsetenv(EnvRuntimeDir)
	fakeConsoleEnvironment(t, map[string]string{"LOCALAPPDATA": local}, true)
	return home.Root()
}

// The distlib exe shape, a loader prefix then the interpreter shebang then a zip holding the launcher script naming root
func writeDistlibExe(t *testing.T, target, interpreter, root string) {
	t.Helper()
	var payload bytes.Buffer
	zw := zip.NewWriter(&payload)
	w, err := zw.Create("__main__.py")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "import sys\nsys.path.insert(0, %s)\n", pyrepr.StrRepr(root))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte("MZ"), make([]byte, 64)...)
	data = append(data, []byte("#!"+interpreter+" -I\r\n")...)
	data = append(data, payload.Bytes()...)
	mustWrite(t, target, string(data), 0o755)
}

// The _command helper, the command a console runs with PATHEXT resolving exe before cmd
func consoleCommand(h string) string {
	exe := filepath.Join(h, "bin", "icebath.exe")
	if isRegularFile(exe) {
		return exe
	}
	return filepath.Join(h, "bin", "icebath.cmd")
}

func dirBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		out[e.Name()] = mustRead(t, filepath.Join(dir, e.Name()))
	}
	return out
}

// test_post_update_expose_cli.py test_windows_converges_on_the_installer_user_bin
func TestWindowsConvergesOnTheInstallerUserBin(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			isolateHome(t)
			var calls []bool
			prev := exposeUserBin
			exposeUserBin = func(ctx context.Context, root string, c bool) ExposeResult {
				calls = append(calls, c)
				return ExposeResult{OK: true, Written: []string{}}
			}
			t.Cleanup(func() { exposeUserBin = prev })
			t.Setenv(EnvInstallRoot, t.TempDir())
			got, err := ExposeCLI(context.Background(), "", create)
			if err != nil || !reflect.DeepEqual(got, ExposeResult{OK: true, Written: []string{}}) || !reflect.DeepEqual(calls, []bool{create}) {
				t.Fatalf("result %+v err %v calls %v", got, err, calls)
			}
		})
	}
}

// test_post_update_expose_cli.py test_sandbox_home_keeps_its_launchers_off_the_user_path
func TestSandboxHomeKeepsItsLaunchersOffTheUserPath(t *testing.T) {
	box := t.TempDir()
	registered := recordUserPathWrites(t)
	t.Setenv(home.EnvHome, filepath.Join(box, "icebath"))
	t.Setenv("LOCALAPPDATA", filepath.Join(box, "local"))
	t.Setenv("USERPROFILE", filepath.Join(box, "user"))
	result := exposeWindowsUserBin(context.Background(), fakeCheckout(t, filepath.Join(box, "checkout")), true)
	if !result.OK || result.Skipped == "" || !reflect.DeepEqual(result.Written, []string{"icebath.cmd", "icebath-acp.cmd"}) || len(*registered) != 0 {
		t.Fatalf("result %+v registered %v", result, *registered)
	}
}

// test_post_update_expose_cli.py test_the_home_new_consoles_use_registers_its_bin
func TestTheHomeNewConsolesUseRegistersItsBin(t *testing.T) {
	for _, parts := range [][]string{nil, {"icebath"}, {"icebath", "profiles", "work"}} {
		t.Run(strings.Join(parts, "_"), func(t *testing.T) {
			tmp := t.TempDir()
			local := filepath.Join(tmp, "local")
			t.Setenv("LOCALAPPDATA", local)
			if parts == nil {
				t.Setenv(home.EnvHome, "")
				os.Unsetenv(home.EnvHome)
			} else {
				t.Setenv(home.EnvHome, filepath.Join(append([]string{local}, parts...)...))
			}
			fakeConsoleEnvironment(t, map[string]string{"LOCALAPPDATA": local}, true)
			registered := recordUserPathWrites(t)
			result := exposeWindowsUserBin(context.Background(), fakeCheckout(t, filepath.Join(tmp, "checkout")), true)
			if result.Path != "added" || !reflect.DeepEqual(*registered, []string{filepath.Join(local, "icebath", "bin")}) {
				t.Fatalf("result %+v registered %v", result, *registered)
			}
		})
	}
}

// test_post_update_expose_cli.py test_inherited_home_reference_expands_from_the_console_environment
func TestInheritedHomeReferenceExpandsFromTheConsoleEnvironment(t *testing.T) {
	for _, tc := range []struct{ storedHome, override string }{{`%LOCALAPPDATA%\icebath-data`, "LOCALAPPDATA"}, {`~\icebath-data`, "USERPROFILE"}} {
		t.Run(tc.override, func(t *testing.T) {
			tmp := t.TempDir()
			real := filepath.Join(tmp, "real")
			console := map[string]string{"LOCALAPPDATA": filepath.Join(real, "local"), "USERPROFILE": filepath.Join(real, "user"), home.EnvHome: tc.storedHome}
			fakeConsoleEnvironment(t, console, true)
			t.Setenv("LOCALAPPDATA", console["LOCALAPPDATA"])
			t.Setenv("USERPROFILE", console["USERPROFILE"])
			t.Setenv(home.EnvHome, tc.storedHome)
			t.Setenv(tc.override, filepath.Join(tmp, "box", strings.ToLower(tc.override)))
			registered := recordUserPathWrites(t)
			result := exposeWindowsUserBin(context.Background(), fakeCheckout(t, filepath.Join(tmp, "checkout")), true)
			if result.Skipped == "" || len(*registered) != 0 {
				t.Fatalf("result %+v registered %v", result, *registered)
			}
		})
	}
}

// test_post_update_expose_cli.py test_unknown_console_environment_leaves_the_user_path
func TestUnknownConsoleEnvironmentLeavesTheUserPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "local"))
	t.Setenv(home.EnvHome, "")
	os.Unsetenv(home.EnvHome)
	fakeConsoleEnvironment(t, nil, false)
	registered := recordUserPathWrites(t)
	if result := exposeWindowsUserBin(context.Background(), fakeCheckout(t, filepath.Join(tmp, "checkout")), true); result.Skipped == "" || len(*registered) != 0 {
		t.Fatalf("result %+v registered %v", result, *registered)
	}
}

// test_post_update_expose_cli.py test_new_console_environment_ignores_this_process
func TestNewConsoleEnvironmentIgnoresThisProcess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "local"))
	t.Setenv(home.EnvHome, filepath.Join(tmp, "icebath"))
	t.Setenv("ICEBATH_TEST_ONLY_IN_THIS_PROCESS", "1")
	environ, ok := consoleEnvironment()
	if !ok {
		t.Fatal("the console environment is unreadable")
	}
	if _, leaked := environ["ICEBATH_TEST_ONLY_IN_THIS_PROCESS"]; leaked || environ["LOCALAPPDATA"] == "" || environ["LOCALAPPDATA"] == filepath.Join(tmp, "local") || environ[home.EnvHome] == filepath.Join(tmp, "icebath") {
		t.Fatalf("environ %v", environ)
	}
}

// test_post_update_expose_cli.py test_review_tree_leaves_the_installed_commands_alone
func TestReviewTreeLeavesTheInstalledCommandsAlone(t *testing.T) {
	for _, form := range []string{"exe", "cmd"} {
		t.Run(form, func(t *testing.T) {
			h := windowsHome(t)
			registered := recordUserPathWrites(t)
			installed := fakeCheckout(t, filepath.Join(h, "icebath-agent"))
			review := fakeCheckout(t, filepath.Join(h, "reviews", "pr-1", "rebuild-wt"))
			ctx := context.Background()
			if form == "exe" {
				for _, name := range windowsBinLaunchers {
					writeDistlibExe(t, filepath.Join(h, "bin", name+".exe"), filepath.Join(installed, installBinaryName()), installed)
				}
			} else if got := exposeWindowsUserBin(ctx, installed, true); got.Path != "added" {
				t.Fatalf("installed publication %+v", got)
			}
			before := dirBytes(t, filepath.Join(h, "bin"))
			result := exposeWindowsUserBin(ctx, review, true)
			if !reflect.DeepEqual(dirBytes(t, filepath.Join(h, "bin")), before) {
				t.Fatal("the review tree rewrote the installed commands")
			}
			if want := (ExposeResult{OK: true, Skipped: filepath.Join(h, "bin", "icebath."+form) + " starts another checkout"}); !reflect.DeepEqual(result, want) {
				t.Fatalf("result %+v", result)
			}
			if launcherInstall(consoleCommand(h)) != installed {
				t.Fatalf("the console command starts %s", launcherInstall(consoleCommand(h)))
			}
			want := []string{}
			if form == "cmd" {
				want = []string{filepath.Join(h, "bin")}
			}
			if !reflect.DeepEqual(append([]string{}, *registered...), want) {
				t.Fatalf("registered %v", *registered)
			}
		})
	}
}

// test_post_update_expose_cli.py test_review_tree_publishes_where_no_working_command_exists
func TestReviewTreePublishesWhereNoWorkingCommandExists(t *testing.T) {
	h := windowsHome(t)
	recordUserPathWrites(t)
	first := fakeCheckout(t, filepath.Join(h, "reviews", "pr-1", "rebuild-wt"))
	second := fakeCheckout(t, filepath.Join(h, "reviews", "pr-2", "rebuild-wt"))
	ctx := context.Background()
	if got := exposeWindowsUserBin(ctx, first, true); got.Skipped != "" || launcherInstall(consoleCommand(h)) != first {
		t.Fatalf("first %+v command %s", got, launcherInstall(consoleCommand(h)))
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	if got := exposeWindowsUserBin(ctx, second, true); got.Skipped != "" || launcherInstall(consoleCommand(h)) != second {
		t.Fatalf("second %+v command %s", got, launcherInstall(consoleCommand(h)))
	}
}

// test_post_update_expose_cli.py test_installed_checkout_takes_its_commands_back
func TestInstalledCheckoutTakesItsCommandsBack(t *testing.T) {
	h := windowsHome(t)
	recordUserPathWrites(t)
	installed := fakeCheckout(t, filepath.Join(h, "icebath-agent"))
	review := fakeCheckout(t, filepath.Join(h, "reviews", "pr-1", "rebuild-wt"))
	ctx := context.Background()
	exposeWindowsUserBin(ctx, review, true)
	if launcherInstall(consoleCommand(h)) != review {
		t.Fatalf("the console command starts %s", launcherInstall(consoleCommand(h)))
	}
	if got := exposeWindowsUserBin(ctx, installed, true); got.Skipped != "" || launcherInstall(consoleCommand(h)) != installed {
		t.Fatalf("installed %+v command %s", got, launcherInstall(consoleCommand(h)))
	}
}

// test_hermes_home_shared_launchers.py test_dead_exe_never_shadows_kept_cmd
func TestDeadExeNeverShadowsKeptCmd(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "local"))
	t.Setenv("USERPROFILE", tmp)
	t.Setenv(EnvRuntimeDir, "")
	os.Unsetenv(EnvRuntimeDir)
	defaultStore := filepath.Join(tmp, "default", "tools")
	mustWrite(t, filepath.Join(defaultStore, "python-default", "python.exe"), "", 0o755)
	mustWrite(t, filepath.Join(tmp, "temp", "tools", "python-temp", "python.exe"), "", 0o755)
	repo := fakeCheckout(t, filepath.Join(tmp, "checkout"))
	local := checkoutLauncherDir(repo)
	gone := filepath.Join(tmp, "gone", "python.exe")
	writeDistlibExe(t, filepath.Join(local, "icebath.exe"), gone, repo)
	mustWrite(t, filepath.Join(local, "icebath.cmd"), "@echo off\r\n@rem icebath-store \""+defaultStore+"\"\r\n\""+filepath.Join(repo, installBinaryName())+"\" %*\r\n", 0o644)
	if launcherBinary(filepath.Join(local, "icebath.exe")) != gone || launcherStore(filepath.Join(local, "icebath.exe")) != gone {
		t.Fatalf("dead exe reads %q", launcherStore(filepath.Join(local, "icebath.exe")))
	}
	t.Setenv(home.EnvHome, filepath.Join(tmp, "temp"))
	written, err := EnsureInstallLaunchers(context.Background(), repo, local)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(written, filepath.Join(local, "icebath.cmd")) || lexists(filepath.Join(local, "icebath.exe")) {
		t.Fatalf("written %v exe present %v", written, lexists(filepath.Join(local, "icebath.exe")))
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// Copies the running test binary into place as an install binary
func copyTestBinary(t *testing.T, target string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// test_source_launcher_publication.py test_running_source_launcher_can_republish_itself
func TestRunningSourceLauncherCanRepublishItself(t *testing.T) {
	for _, form := range []string{"native", "cmd", "native-with-maker"} {
		t.Run(form, func(t *testing.T) {
			h := windowsHome(t)
			repo := filepath.Join(h, "source repo")
			copyTestBinary(t, filepath.Join(repo, installBinaryName()))
			out := filepath.Join(h, "bin")
			if err := os.MkdirAll(out, 0o777); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			written, err := EnsureInstallLaunchers(ctx, repo, out)
			if err != nil || len(written) != len(windowsBinLaunchers) {
				t.Fatalf("written %v err %v", written, err)
			}
			command := ""
			for _, path := range written {
				if strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) == "icebath" {
					command = path
				}
			}
			if filepath.Ext(command) != ".cmd" {
				t.Fatalf("published form %q", command)
			}
			cmd := exec.Command(command)
			cmd.Env = append(os.Environ(), republishChild+"="+repo)
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "published 2 2") {
				t.Fatalf("republish %q %v", output, err)
			}
			if again, err := EnsureInstallLaunchers(ctx, repo, out); err != nil || len(again) != len(windowsBinLaunchers) {
				t.Fatalf("republish after the run %v %v", again, err)
			}
			if resolveInstallBinary(repo) != filepath.Join(repo, installBinaryName()) {
				t.Fatalf("install binary %q", resolveInstallBinary(repo))
			}
		})
	}
}

// test_source_launcher_publication.py test_repin_without_distlib_retires_stale_native_launcher
func TestRepinWithoutDistlibRetiresStaleNativeLauncher(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{{"idle", false}, {"running", true}, {"running-store-pip", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := windowsHome(t)
			repo := fakeCheckout(t, filepath.Join(h, "icebath-agent"))
			out := filepath.Join(h, "bin")
			stale := filepath.Join(out, "icebath.exe")
			copyTestBinary(t, stale)
			var process *exec.Cmd
			var stdin io.WriteCloser
			if tc.running {
				process = exec.Command(stale)
				process.Env = append(os.Environ(), sleepChild+"=1")
				pipe, err := process.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				stdin = pipe
				stdout, err := process.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "ready" {
					t.Fatalf("stale launcher printed %q", line)
				}
			}
			result, err := stageLauncher(context.Background(), "icebath", repo, out)
			if err != nil {
				t.Fatal(err)
			}
			if tc.running {
				_ = stdin.Close()
				if err := process.Wait(); err != nil {
					t.Fatalf("the retired launcher did not finish its own run: %v", err)
				}
			}
			if result != filepath.Join(out, "icebath.cmd") || lexists(stale) {
				t.Fatalf("result %q stale present %v", result, lexists(stale))
			}
			if !strings.Contains(mustRead(t, result), filepath.Join(repo, installBinaryName())) {
				t.Fatalf("cmd %q", mustRead(t, result))
			}
			if again, err := stageLauncher(context.Background(), "icebath", repo, out); err != nil || again != result {
				t.Fatal("republication after the run failed")
			}
			leftovers, _ := filepath.Glob(filepath.Join(out, "icebath.exe*.deleteme"))
			if len(leftovers) != 0 {
				t.Fatalf("aside copies left %v", leftovers)
			}
		})
	}
}
