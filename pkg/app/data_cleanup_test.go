package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Temp machine whose home holds data beside the runtime witnesses a data wipe must keep
type dataLayoutFixture struct {
	home      string
	source    string
	userdata  string
	witnesses []string
	data      []string
}

// Builds the layout under root, routing the user home, desktop data, runtime store and project root into it
func dataLayoutAt(t *testing.T, root string) dataLayoutFixture {
	t.Helper()
	user := filepath.Join(root, "user")
	h := filepath.Join(user, "icebath-data")
	source := filepath.Join(h, "workspace", "custom-source")
	store := filepath.Join(h, "machine", "tool-store")
	t.Setenv("HOME", user)
	t.Setenv("USERPROFILE", user)
	t.Setenv("XDG_CONFIG_HOME", user)
	t.Setenv("APPDATA", user)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(pm.EnvRuntimeDir, store)
	swap(t, &uninstallProjectRoot, func() string { return source })
	f := dataLayoutFixture{home: h, source: source, userdata: desktopUserdataDir()}
	f.witnesses = []string{filepath.Join(source, "hermes_cli", "__init__.py"), filepath.Join(store, "python", "python.exe"),
		filepath.Join(h, "installs", "other-install", "facts.json"), filepath.Join(h, "bin", "icebath.cmd"),
		filepath.Join(h, "profiles", "sibling", "config.yaml"), filepath.Join(h, "cache", "partials", ".locks", "other-profile-transfer")}
	for _, p := range f.witnesses {
		uninstallWrite(t, p, "keep")
	}
	f.data = []string{filepath.Join(h, "config.yaml"), filepath.Join(h, "sessions", "session.json"), filepath.Join(h, "workspace", "notes.txt"),
		filepath.Join(h, ".env"), filepath.Join(h, "logs", "file")}
	for _, p := range f.data {
		text := "user data"
		if ext := filepath.Ext(p); ext == ".yaml" || ext == ".json" {
			text = "{}"
		}
		uninstallWrite(t, p, text)
	}
	return f
}

func dataLayout(t *testing.T) dataLayoutFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dataLayoutAt(t, root)
}

func (f dataLayoutFixture) witnessesKept(t *testing.T, from int) {
	t.Helper()
	for _, p := range f.witnesses[from:] {
		if text, err := os.ReadFile(p); err != nil || string(text) != "keep" {
			t.Fatalf("witness %s lost %v", p, err)
		}
	}
}

func (f dataLayoutFixture) dataExists(t *testing.T, want bool) {
	t.Helper()
	for _, p := range f.data {
		if uninstallExists(p) != want {
			t.Fatalf("%s exists %v want %v", p, !want, want)
		}
	}
}

// Feeds the prompt answers through the app console
func dataConsoleInput(t *testing.T, input string) {
	t.Helper()
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(input), Out: io.Discard}, io.Discard))
}

func TestDataOnlyPreservesRuntimeAndSiblingHomes(t *testing.T) {
	for _, mode := range []string{"confirmed", "cancel", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := dataLayout(t)
			ctx := context.Background()
			generation := filepath.Join(pm.InstallStateDir(ctx, f.source), "environments", "selected")
			uninstallWrite(t, filepath.Join(generation, "pyvenv.cfg"), "include-system-site-packages = false")
			uninstallWrite(t, pm.EnvironmentRuntimeFactsPath(ctx, f.source),
				fmt.Sprintf(`{"packages": {"venv": {"environment": %q}}}`, generation))
			dataConsoleInput(t, "no\n")
			ns := map[string]any{"yes": mode != "cancel", "dry_run": mode == "dry-run"}
			if _, err := RunDataUninstall(ctx, ns, io.Discard); err != nil {
				t.Fatal(err)
			}
			if selected, err := pm.SelectedVenv(ctx, f.source); err != nil || selected != generation {
				t.Fatalf("selected %q %v", selected, err)
			}
			f.witnessesKept(t, 0)
			f.dataExists(t, mode != "confirmed")
			if st, err := os.Stat(f.home); err != nil || !st.IsDir() {
				t.Fatal("home removed")
			}
		})
	}
}

func TestDataOnlyReportsAFailedRemoval(t *testing.T) {
	f := dataLayout(t)
	swap(t, &dataRmtree, func(p string) error {
		if p == filepath.Join(f.home, "sessions") {
			return &os.PathError{Op: "rmdir", Path: p, Err: os.ErrPermission}
		}
		return pyshutil.Rmtree(p)
	})
	var out bytes.Buffer
	code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true, "dry_run": false}, &out)
	if err != nil || code == 0 {
		t.Fatalf("exit %d %v", code, err)
	}
	if strings.Contains(out.String(), "Icebath data removed") || !strings.Contains(out.String(), filepath.Join(f.home, "sessions")) {
		t.Fatalf("output %q", out.String())
	}
	f.witnessesKept(t, 0)
}

func TestNamedHomeDoesNotEraseSiblingsOrDesktopData(t *testing.T) {
	f := dataLayout(t)
	active := filepath.Join(f.home, "profiles", "active")
	config := filepath.Join(active, "config.yaml")
	uninstallWrite(t, config, "model: selected")
	uninstallWrite(t, filepath.Join(f.userdata, "preferences.json"), "keep")
	t.Setenv("ICEBATH_HOME", active)
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	if uninstallExists(config) {
		t.Fatal("active profile config kept")
	}
	f.witnessesKept(t, 0)
	if text, _ := os.ReadFile(filepath.Join(f.userdata, "preferences.json")); string(text) != "keep" {
		t.Fatal("desktop data erased by a named profile")
	}
}

func TestDirectoryReplacedWithALinkDoesNotExpandRemoval(t *testing.T) {
	f := dataLayout(t)
	tmp := filepath.Dir(filepath.Dir(f.home))
	original := filepath.Join(f.home, "workspace")
	foreign := filepath.Join(tmp, "foreign")
	witness := filepath.Join(foreign, "notes.txt")
	uninstallWrite(t, witness, "not Icebath data")
	probe := filepath.Join(tmp, "symlink-probe")
	if err := os.Symlink(foreign, probe); err != nil {
		t.Skipf("native symlink creation unavailable: %v", err)
	}
	os.Remove(probe)
	plan, err := planDataRemoval(context.Background(), f.home, f.source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, filepath.Join(f.home, "workspace-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, original); err != nil {
		t.Fatal(err)
	}
	if _, failures := removeData(plan); len(failures) == 0 {
		t.Fatal("removal followed the swapped link")
	}
	if text, _ := os.ReadFile(witness); string(text) != "not Icebath data" {
		t.Fatal("foreign data removed")
	}
}

func TestDataOnlyPreservesTheContainingBundledApplication(t *testing.T) {
	f := dataLayout(t)
	app := filepath.Join(f.home, "installed-app")
	payload := filepath.Join(app, "resources", bundlePayloadDirName)
	project := filepath.Join(payload, "repo")
	uninstallWrite(t, filepath.Join(project, "install-stamp.json"), `{"payload": "bundled"}`)
	shell := filepath.Join(app, "Icebath.exe")
	uninstallWrite(t, shell, "retained app bytes")
	os.MkdirAll(filepath.Join(payload, "venv"), 0o755)
	manifest := filepath.Join(payload, "manifest.json")
	uninstallWrite(t, manifest, `{"repo": "repo", "venv": "venv"}`)
	swap(t, &uninstallProjectRoot, func() string { return project })
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	if text, _ := os.ReadFile(shell); string(text) != "retained app bytes" || !uninstallExists(manifest) {
		t.Fatal("bundled app removed")
	}
	f.dataExists(t, false)
}

func TestDataOnlyWorksFromASelfContainedRuntimeWithoutAnApp(t *testing.T) {
	f := dataLayout(t)
	pkg := filepath.Join(filepath.Dir(f.home), "usr", "lib", "icebath-agent")
	project := filepath.Join(pkg, "app")
	uninstallWrite(t, filepath.Join(project, "hermes_cli", "__init__.py"), "")
	uninstallWrite(t, filepath.Join(project, "install-stamp.json"),
		`{"payload": "runtime", "distribution": "apt-termux", "updateMechanism": "external"}`)
	os.MkdirAll(filepath.Join(pkg, "venv"), 0o755)
	swap(t, &uninstallProjectRoot, func() string { return project })
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	f.dataExists(t, false)
	if !uninstallExists(filepath.Join(project, "install-stamp.json")) || !uninstallExists(filepath.Join(pkg, "venv")) {
		t.Fatal("runtime removed")
	}
}

// Parses an uninstall command line through the real parser and dispatches it like the CLI handler
func dataCLIUninstall(t *testing.T, argv []string) (int, error) {
	t.Helper()
	c := commands.NewCLI()
	var ns map[string]any
	c.Bind("uninstall", func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute(argv); err != nil {
		return 0, err
	}
	return CmdUninstall(context.Background(), ns, io.Discard, func(string) { t.Fatal("data-only dispatch reached the terminal gate") })
}

func TestDataEntrypointsPreserveRuntime(t *testing.T) {
	for _, entry := range []string{"cli", "module", "cli-dry-run"} {
		t.Run(entry, func(t *testing.T) {
			f := dataLayout(t)
			noCodeRemoval := func(context.Context, map[string]any, io.Writer) (int, error) {
				t.Fatal("data-only dispatch reached code removal")
				return 0, nil
			}
			swap(t, &uninstallRunAgentMode, noCodeRemoval)
			swap(t, &uninstallRunGUIMode, noCodeRemoval)
			if entry == "module" {
				if code := UninstallModuleMain(context.Background(), []string{"--mode", "data"}, io.Discard, io.Discard); code != 0 {
					t.Fatalf("module exit %d", code)
				}
			} else {
				flag := "--yes"
				if entry == "cli-dry-run" {
					flag = "--dry-run"
				}
				if _, err := dataCLIUninstall(t, []string{"uninstall", "--data", flag}); err != nil {
					t.Fatal(err)
				}
			}
			f.witnessesKept(t, 0)
			f.dataExists(t, entry == "cli-dry-run")
		})
	}
}

func TestConflictingDataModeIsRejectedBeforeDispatch(t *testing.T) {
	for _, other := range []string{"--full", "--gui"} {
		t.Run(other, func(t *testing.T) {
			_, err := dataCLIUninstall(t, []string{"uninstall", "--data", other, "--yes"})
			if code, _ := commands.ExitCode(err); code != 2 {
				t.Fatalf("exit %d error %v", code, err)
			}
		})
	}
}

func TestModuleSubprocessRemovesOnlyTheDisposableHome(t *testing.T) {
	f := dataLayout(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Dir(filepath.Dir(f.home))
	env := []string{"ICEBATH_HOME=" + f.home, pm.EnvRuntimeDir + "=" + filepath.Join(f.home, "machine", "tool-store"),
		"HOME=" + tmp, "USERPROFILE=" + tmp, "APPDATA=" + tmp, "LOCALAPPDATA=" + tmp}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "PATH", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "TEMP", "TMP":
			env = append(env, kv)
		}
	}
	cmd := exec.Command(exe, UninstallModuleEntry, "--mode", "data")
	cmd.Dir, cmd.Env = tmp, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f.witnessesKept(t, 1)
	f.dataExists(t, false)
	if uninstallExists(filepath.Join(f.home, "skills")) || uninstallExists(filepath.Join(f.home, "SOUL.md")) {
		t.Fatal("the run seeded the home it removed")
	}
}

func TestModuleUsesTheDesktopInstancesExplicitDataPath(t *testing.T) {
	f := dataLayout(t)
	tmp := filepath.Dir(filepath.Dir(f.home))
	ordinary := filepath.Join(tmp, "user", "desktop-data")
	variant := filepath.Join(tmp, "variant-desktop-data")
	for _, dir := range []string{ordinary, variant} {
		uninstallWrite(t, filepath.Join(dir, "preferences.json"), "{}")
	}
	if code := UninstallModuleMain(context.Background(), []string{"--mode", "data", "--desktop-userdata", variant}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if uninstallExists(variant) {
		t.Fatal("explicit desktop data kept")
	}
	if text, _ := os.ReadFile(filepath.Join(ordinary, "preferences.json")); string(text) != "{}" {
		t.Fatal("ordinary desktop data removed")
	}
}

func TestUnconfirmedRemovalNeverContactsTheGateway(t *testing.T) {
	for _, mode := range []string{"confirmed", "cancel", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := dataLayout(t)
			var calls []string
			swap(t, &holdersIdentifyGateway, func(target string) *pyjson.Object {
				calls = append(calls, target)
				return pyjson.ObjectOf("pid", int64(42), "hermes_home", f.home, "supervisor", "systemd")
			})
			dataConsoleInput(t, "no\n")
			ns := map[string]any{"yes": mode != "cancel", "dry_run": mode == "dry-run"}
			code, err := RunDataUninstall(context.Background(), ns, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "confirmed" {
				if code == 0 || len(calls) != 1 || calls[0] != f.home {
					t.Fatalf("exit %d calls %v", code, calls)
				}
			} else if len(calls) != 0 {
				t.Fatalf("calls %v", calls)
			}
			f.dataExists(t, true)
		})
	}
}

func TestLiveChatLeaseBlocksDataDeletion(t *testing.T) {
	f := dataLayout(t)
	lease, refusal, err := activesessions.TryAcquire(context.Background(), "fixture-session", "cli", map[string]any{}, nil, f.home, true)
	if err != nil || lease == nil || refusal != nil {
		t.Fatalf("lease %v %v %v", lease, refusal, err)
	}
	defer activesessions.ReleaseActiveSession(lease)
	if code, _ := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code == 0 {
		t.Fatal("live chat lease did not block")
	}
	f.dataExists(t, true)
}

// Short machine root under the system temp dir so the control socket path fits sun_path
func dataShortLayout(t *testing.T) dataLayoutFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "icu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	return dataLayoutAt(t, root)
}

func TestManualGatewayDrainsOverRealControlTransportBeforeDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix control socket transport")
	}
	f := dataShortLayout(t)
	child := exec.Command("sleep", "120")
	child.Dir = f.home
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	ln, err := net.Listen("unix", filepath.Join(f.home, "gateway.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var paused sync.WaitGroup
	paused.Add(1)
	pausedOnce := sync.OnceFunc(paused.Done)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if strings.Contains(line, `"pause-for-update"`) {
				pausedOnce()
				child.Process.Signal(os.Interrupt)
				child.Wait()
				fmt.Fprintf(conn, `{"ok": true, "result": {"pid": %d, "pausing": true, "drain_timeout": 0}}`+"\n", child.Process.Pid)
				conn.Close()
				ln.Close()
				return
			}
			start, _ := pyjson.Dumps(gateway.GetProcessStartTime(child.Process.Pid))
			home, _ := pyjson.Dumps(f.home)
			fmt.Fprintf(conn, `{"ok": true, "result": {"pid": %d, "start_time": %s, "hermes_home": %s, "supervisor": "manual"}}`+"\n",
				child.Process.Pid, start, home)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	var out bytes.Buffer
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, &out); code != 0 || err != nil {
		t.Fatalf("exit %d %v\n%s", code, err, out.String())
	}
	paused.Wait()
	<-done
	if child.ProcessState == nil {
		t.Fatal("gateway child still running")
	}
	f.dataExists(t, false)
}

func TestBackendInitialProfileIsNotItsWriteScope(t *testing.T) {
	for _, profile := range []string{"", "sibling"} {
		t.Run(fmt.Sprintf("%q", profile), func(t *testing.T) {
			f := dataLayout(t)
			if !tools.RegisterSelf(context.Background(), "serve", uninstallProjectRoot(), map[string]any{"profile": profile}) {
				t.Fatal("register_self refused")
			}
			if code, _ := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code == 0 {
				t.Fatal("live backend did not block")
			}
			f.dataExists(t, true)
		})
	}
}

func TestDetachedCronAttemptMustFinishBeforeDataRemoval(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			f := dataLayout(t)
			ctx := context.Background()
			record, err := cron.CreateExecutionIn(ctx, f.home, "temporary-job", "manual")
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				if _, err := cron.FinishExecutionIn(ctx, f.home, record["id"].(string), true); err != nil {
					t.Fatal(err)
				}
				if code, err := RunDataUninstall(ctx, map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
					t.Fatalf("exit %d %v", code, err)
				}
				f.dataExists(t, false)
				return
			}
			if code, _ := RunDataUninstall(ctx, map[string]any{"yes": true}, io.Discard); code == 0 {
				t.Fatal("running cron attempt did not block")
			}
			f.dataExists(t, true)
		})
	}
}

func TestNamedProfileCannotDeleteDataServedByTheDefaultMultiplexer(t *testing.T) {
	f := dataLayout(t)
	profile := filepath.Join(f.home, "profiles", "active")
	config := filepath.Join(profile, "config.yaml")
	uninstallWrite(t, config, "{}")
	t.Setenv("ICEBATH_HOME", profile)
	swap(t, &holdersIdentifyGateway, func(target string) *pyjson.Object {
		if target == f.home {
			return pyjson.ObjectOf("pid", int64(42), "hermes_home", f.home, "served_profiles", []any{"active"})
		}
		return nil
	})
	swap(t, &holdersPauseGateway, func(string) *pyjson.Object {
		t.Fatal("must not stop other profiles' gateway")
		return nil
	})
	if code, _ := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code == 0 {
		t.Fatal("multiplexed profile wipe was not refused")
	}
	if text, _ := os.ReadFile(config); string(text) != "{}" {
		t.Fatal("profile config removed")
	}
}

func TestDataRemovalRespectsTheCheckpointStoreOwner(t *testing.T) {
	f := dataLayout(t)
	release, err := tools.CheckpointStoreLock(filepath.Join(f.home, "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if code, _ := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code == 0 {
		t.Fatal("checkpoint store owner did not block")
	}
	f.dataExists(t, true)
}

// Error a fixture pause raises once the owner check has passed
type pastOwnerCheck struct{}

func (pastOwnerCheck) Error() string { return "past owner check" }

func TestProfileDataCleanupDrainsOnlyAManualGateway(t *testing.T) {
	manual := func() *pyjson.Object {
		return pyjson.ObjectOf("pid", int64(4321), "supervisor", "none", "launch_supervisor", "manual")
	}
	desktop := manual()
	desktop.Set("launch_supervisor", "desktop")
	cases := []struct {
		identity *pyjson.Object
		refusal  string
	}{{manual(), ""}, {desktop, "desktop gateway still owns"}, {pyjson.ObjectOf("pid", int64(4321), "supervisor", "none"), "unknown gateway still owns"}}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			answer := tc.identity.Copy()
			answer.Set("hermes_home", h)
			answer.Set("start_time", int64(111))
			swap(t, &holdersIdentifyGateway, func(string) *pyjson.Object { return answer })
			reachedPause := false
			swap(t, &holdersPauseGateway, func(string) *pyjson.Object {
				reachedPause = true
				return nil
			})
			swap(t, &holdersProcessStartTime, func(int) any { return int64(111) })
			err = drainManualGateway(h)
			if tc.refusal == "" {
				if !reachedPause {
					t.Fatalf("manual gateway was not drained: %v", err)
				}
				return
			}
			if reachedPause || err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("error %v paused %v", err, reachedPause)
			}
		})
	}
}

func TestRunDataUninstallRemovesElectronUserdata(t *testing.T) {
	tmp := t.TempDir()
	installKindHome(t, tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("APPDATA", tmp)
	userdata := desktopUserdataDir()
	uninstallWrite(t, filepath.Join(userdata, "connection.json"), "{}")
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	if uninstallExists(userdata) {
		t.Fatal("desktop data kept")
	}
}

func TestRunDataUninstallWorksOnSealedTrees(t *testing.T) {
	tmp := t.TempDir()
	h := installKindHome(t, tmp)
	uninstallWrite(t, filepath.Join(h, "icebath-agent", "install-stamp.json"), `{"distribution": "nix", "updateMechanism": "external"}`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "none"))
	if code, err := RunDataUninstall(context.Background(), map[string]any{"yes": true}, io.Discard); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	if uninstallExists(filepath.Join(h, "config.yaml")) || !uninstallExists(filepath.Join(h, "icebath-agent")) {
		t.Fatal("data mode removed code or kept data")
	}
}

// Home with sessions, logs, config, secrets and a canonical agent checkout, the user home beside it
func installKindHome(t *testing.T, tmp string) string {
	t.Helper()
	h := filepath.Join(tmp, ".icebath")
	uninstallWrite(t, filepath.Join(h, "sessions", "s1.json"), "{}")
	os.MkdirAll(filepath.Join(h, "logs"), 0o755)
	uninstallWrite(t, filepath.Join(h, "config.yaml"), "x: 1\n")
	uninstallWrite(t, filepath.Join(h, ".env"), "KEY=secret\n")
	uninstallWrite(t, filepath.Join(h, "icebath-agent", "hermes_cli", "__init__.py"), "")
	t.Setenv("HOME", filepath.Join(tmp, "user"))
	t.Setenv("USERPROFILE", filepath.Join(tmp, "user"))
	t.Setenv("ICEBATH_HOME", h)
	return h
}
