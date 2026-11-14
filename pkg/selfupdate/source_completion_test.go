package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Hidden first argument that routes a re-executed test binary to a source completion stand in
const scChildEntry = "__source_completion_test_child__"

// Git repository with one tagged release commit, its home sandboxed so the update marker stays inside the test
func scRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath"))
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = []string{"HOME=" + tmp, "PATH=" + os.Getenv("PATH")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Icebath Test")
	git("config", "user.email", "icebath@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("commit", "-qm", "release")
	git("tag", "v0.21.4")
	return root
}

// Replaces the launcher publish, the product build and the maintenance with stand ins
func scDependencies(t *testing.T, build func(context.Context, string, bool) error, maintenance func(context.Context, PostUpdateMaintenance) (bool, error)) {
	t.Helper()
	prevPublish, prevBuild, prevMaint := publishLaunchers, sourceCompletionBuildProducts, sourceCompletionMaintenance
	publishLaunchers = func(context.Context, string) error { return nil }
	sourceCompletionBuildProducts = build
	sourceCompletionMaintenance = maintenance
	t.Cleanup(func() {
		publishLaunchers, sourceCompletionBuildProducts, sourceCompletionMaintenance = prevPublish, prevBuild, prevMaint
	})
}

func scNoBuild(context.Context, string, bool) error { return nil }

// Completion options writing to buffers, with the followups slice when given
func scOptions(owed *[][2]string) SourceCompletion {
	return SourceCompletion{AssumeYes: true, Followups: owed, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
}

func scStampExists(root string) bool {
	info, err := os.Stat(filepath.Join(root, "install-stamp.json"))
	return err == nil && info.Mode().IsRegular()
}

func TestSuccessfulSourceCompletionWritesCheckoutIdentity(t *testing.T) {
	root := scRepo(t)
	stampedEarly := false
	scDependencies(t, scNoBuild, func(context.Context, PostUpdateMaintenance) (bool, error) {
		stampedEarly = scStampExists(root)
		return true, nil
	})
	ok, err := CompleteSourceCheckout(context.Background(), root, scOptions(nil))
	if err != nil || !ok || stampedEarly || !scStampExists(root) {
		t.Fatalf("ok %v err %v stamped early %v stamp %v", ok, err, stampedEarly, scStampExists(root))
	}
}

func TestFailedSourceCompletionDoesNotPublishIdentity(t *testing.T) {
	root := scRepo(t)
	scDependencies(t, func(context.Context, string, bool) error { return errors.New("web UI build: npm exited 1") },
		func(context.Context, PostUpdateMaintenance) (bool, error) { return true, nil })
	var owed [][2]string
	ok, err := CompleteSourceCheckout(context.Background(), root, scOptions(&owed))
	if err != nil || ok || len(owed) != 1 || owed[0][0] != "build" || scStampExists(root) {
		t.Fatalf("ok %v err %v owed %v stamp %v", ok, err, owed, scStampExists(root))
	}
}

func TestUnsafeSqliteWithholdsSuccessButNotTheFinishedTail(t *testing.T) {
	root := scRepo(t)
	scDependencies(t, scNoBuild, func(context.Context, PostUpdateMaintenance) (bool, error) { return false, nil })
	owed := [][2]string{}
	ok, err := CompleteSourceCheckout(context.Background(), root, scOptions(&owed))
	if err != nil || ok || len(owed) != 0 || !scStampExists(root) {
		t.Fatalf("ok %v err %v owed %v stamp %v", ok, err, owed, scStampExists(root))
	}
}

func TestCompletedMaintenanceSurvivesStampIoError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath"))
	scDependencies(t, scNoBuild, func(context.Context, PostUpdateMaintenance) (bool, error) { return true, nil })
	prev := sourceCompletionWriteStamp
	sourceCompletionWriteStamp = func(string, bool) (*pyjson.Object, error) {
		return nil, &pm.PyError{Class: "OSError", Message: "readonly"}
	}
	t.Cleanup(func() { sourceCompletionWriteStamp = prev })
	var stderr bytes.Buffer
	ok, err := CompleteSourceCheckout(context.Background(), tmp, SourceCompletion{AssumeYes: true, Stdout: &bytes.Buffer{}, Stderr: &stderr})
	if err != nil || !ok || !strings.Contains(stderr.String(), "completed, but the install stamp") {
		t.Fatalf("ok %v err %v stderr %q", ok, err, stderr.String())
	}
}

func TestCompletionHoldsTheUpdateLockDuringTheTail(t *testing.T) {
	root := scRepo(t)
	marker := updaterecord.UpdateMarkerPath()
	var holder *updaterecord.UpdateHolder
	scDependencies(t, func(ctx context.Context, _ string, _ bool) error {
		holder = updaterecord.ReadLiveUpdate(ctx, marker, "")
		return nil
	}, func(context.Context, PostUpdateMaintenance) (bool, error) { return true, nil })
	ok, err := CompleteSourceCheckout(context.Background(), root, scOptions(nil))
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("the tail must hold the shared lock while it builds: %+v", holder)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a completed tail must release the lock for the next launch: %v", err)
	}
}

// Live process that is not an ancestor of this test, a stand in updater
func scForeignPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], scChildEntry, "sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestCompletionRefusesToStackOnALiveUpdate(t *testing.T) {
	root := scRepo(t)
	marker := updaterecord.UpdateMarkerPath()
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("%d\n%d\n", scForeignPid(t), time.Now().Unix())
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	scDependencies(t, scNoBuild, func(context.Context, PostUpdateMaintenance) (bool, error) { return true, nil })
	_, err := CompleteSourceCheckout(context.Background(), root, scOptions(nil))
	if err == nil || !strings.Contains(err.Error(), "an update is still running") {
		t.Fatalf("err %v", err)
	}
}

func TestCompletionRunsUnderAParentsClaimWithoutReleasingIt(t *testing.T) {
	root := scRepo(t)
	marker := updaterecord.UpdateMarkerPath()
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], scChildEntry, "parent", marker, root)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if err != nil || lines[len(lines)-1] != "true" {
		t.Fatalf("%v %s%s", err, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the parent still owns its claim after our tail: %v", err)
	}
}

func TestACompletionFromAnotherHomeNeverBuildsAnOwnedCheckout(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, filepath.Join(tmp, "other-home"))
	entered := 0
	prev := sourceCompletionLocked
	sourceCompletionLocked = func(context.Context, string, SourceCompletion) (bool, error) {
		entered++
		return true, nil
	}
	t.Cleanup(func() { sourceCompletionLocked = prev })
	holder := exec.Command(os.Args[0], scChildEntry, "holder", root, filepath.Join(tmp, "owner-marker"))
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		holder.Process.Kill()
		holder.Wait()
	})
	line := make([]byte, 5)
	if _, err := io.ReadFull(out, line); err != nil || string(line) != "held\n" {
		t.Fatalf("holder said %q %v", line, err)
	}
	_, err = CompleteSourceCheckout(context.Background(), root, scOptions(nil))
	if err == nil || !strings.Contains(err.Error(), "an update is still running") {
		t.Fatalf("err %v", err)
	}
	if entered != 0 {
		t.Fatal("the completion body ran while another update owned the checkout")
	}
}

// Source completion stand ins a re-executed test binary runs, returning its exit status
func scChild(mode string, args []string) int {
	switch mode {
	case "holder":
		ok, err := updaterecord.NewUpdateLock(args[1], args[0], true).Acquire(context.Background())
		if err != nil || !ok {
			fmt.Fprintln(os.Stderr, ok, err)
			return 1
		}
		fmt.Println("held")
		time.Sleep(120 * time.Second)
		return 0
	case "sleep":
		time.Sleep(120 * time.Second)
		return 0
	case "parent":
		body := fmt.Sprintf("%d\n%d\n", os.Getpid(), time.Now().Unix())
		if err := os.WriteFile(args[0], []byte(body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cmd := exec.Command(os.Args[0], scChildEntry, "tail", args[1])
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			return 1
		}
		return 0
	case "tail":
		publishLaunchers = func(context.Context, string) error { return nil }
		sourceCompletionBuildProducts = scNoBuild
		sourceCompletionMaintenance = func(context.Context, PostUpdateMaintenance) (bool, error) { return true, nil }
		ok, err := CompleteSourceCheckout(context.Background(), args[0], SourceCompletion{AssumeYes: true, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		data, _ := json.Marshal(ok)
		fmt.Println(string(data))
		return 0
	}
	return 2
}

// Windows checkout with pm's staged git in its store and the system PATH alone, returning the root and the git
func epgWindows(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	store := filepath.Join(tmp, "tools")
	staged := filepath.Join(store, "git-2.53.0+3-win32-x64", "cmd", "git.exe")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "checkout", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevPlatform, prevStore, prevLocate, prevEnsure := exposePMGitPlatform, exposePMGitStoreRoot, exposePMGitLocate, exposePMGitEnsure
	exposePMGitPlatform = "windows"
	exposePMGitStoreRoot = func(context.Context) (string, error) { return store, nil }
	t.Cleanup(func() {
		exposePMGitPlatform, exposePMGitStoreRoot, exposePMGitLocate, exposePMGitEnsure = prevPlatform, prevStore, prevLocate, prevEnsure
	})
	t.Setenv("PATH", `C:\Windows\System32`)
	return tmp, staged
}

func TestPmGitIsAcquiredAndRecordedWhenWindowsHasNoGitOfItsOwn(t *testing.T) {
	for _, gitOnPath := range []string{"none", "installer-staged"} {
		t.Run(gitOnPath, func(t *testing.T) {
			root, staged := epgWindows(t)
			storePath := `C:\store\git-2.53.0+3-win32-x64\cmd;C:\store\git-2.53.0+3-win32-x64\usr\bin;C:\Windows\System32`
			var calls []string
			exposePMGitEnsure = func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
				calls = append(calls, name+" explicit="+strconv.FormatBool(opts.Explicit))
				return &pm.Runner{Name: name, Env: map[string]string{"Path": storePath}}, nil
			}
			exposePMGitLocate = func(string) []string {
				if gitOnPath == "installer-staged" {
					return []string{staged}
				}
				return nil
			}
			if err := ExposePMGit(context.Background(), filepath.Join(root, "checkout")); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0] != "git explicit=true" || os.Getenv("PATH") != storePath {
				t.Fatalf("calls %v PATH %q", calls, os.Getenv("PATH"))
			}
		})
	}
}

func TestOwnGitAndGitLessZipInstallsAreLeftAlone(t *testing.T) {
	for _, install := range []string{"own-git", "git-less-zip"} {
		t.Run(install, func(t *testing.T) {
			root, _ := epgWindows(t)
			var ownGit []string
			if install == "own-git" {
				ownGit = []string{filepath.Join(root, "Git", "cmd", "git.exe")}
			} else if err := os.Remove(filepath.Join(root, "checkout", ".git")); err != nil {
				t.Fatal(err)
			}
			exposePMGitLocate = func(string) []string { return ownGit }
			exposePMGitEnsure = func(context.Context, string, pm.EnsureOptions) (*pm.Runner, error) {
				t.Fatal("acquired PM git")
				return nil, nil
			}
			if err := ExposePMGit(context.Background(), filepath.Join(root, "checkout")); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("PATH") != `C:\Windows\System32` {
				t.Fatalf("PATH %q", os.Getenv("PATH"))
			}
		})
	}
}

// Entry refuses a tree without the checkout marker and parses its flags with argparse errors
func TestSourceCompletionMainArgumentsAndCheckoutCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := SourceCompletionMain(context.Background(), []string{"--desktop"}, &stdout, &stderr); code != 2 ||
		!strings.HasSuffix(stderr.String(), "icebath source-completion: error: the following arguments are required: --source\n") {
		t.Fatalf("missing source %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := SourceCompletionMain(context.Background(), []string{"--sou", "x", "--bogus"}, &stdout, &stderr); code != 2 ||
		!strings.HasSuffix(stderr.String(), "error: unrecognized arguments: --bogus\n") {
		t.Fatalf("unknown %d %q", code, stderr.String())
	}
	stderr.Reset()
	dir := t.TempDir()
	if code := SourceCompletionMain(context.Background(), []string{"--source", dir, "--finish"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "is not a Icebath source checkout") {
		t.Fatalf("not a checkout %d %q", code, stderr.String())
	}
	stdout.Reset()
	if code := SourceCompletionMain(context.Background(), []string{"-h"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "--finish-update") || !strings.HasPrefix(stdout.String(), "usage: icebath source-completion") {
		t.Fatalf("help %d %q", code, stdout.String())
	}
}

// Bootstrap re-enters the checkout binary with the passthrough flags and the prepared marker
func TestSourceCompletionBootstrapReentersTheCheckoutBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "selfupdate"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sourceCompletionMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	prevResolve, prevCall := resolveStorePython, sourceCompletionCall
	t.Cleanup(func() { resolveStorePython, sourceCompletionCall = prevResolve, prevCall })
	resolveStorePython = func(string) string { return "/opt/icebath/icebath" }
	var got []string
	sourceCompletionCall = func(_ context.Context, command []string, cwd string, _ []string, _ []*os.File, _, _ io.Writer) (int, error) {
		got = append([]string{cwd}, command...)
		return 3, nil
	}
	var stdout, stderr bytes.Buffer
	code := SourceCompletionMain(context.Background(), []string{"--source", dir, "--desktop", "--interactive", "--finish-update"}, &stdout, &stderr)
	want := []string{dir, "/opt/icebath/icebath", SourceCompletionEntry, "--source", dir, "--desktop", "--finish-update", sourceCompletionPrepared}
	if code != 3 || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("code %d command %q %s", code, got, stderr.String())
	}
}
