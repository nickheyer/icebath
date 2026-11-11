package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Hidden first argument that routes a re-executed test binary to a takeover child stand-in
const tkChildEntry = "__update_takeover_test_child__"

var tkSHA = strings.Repeat("c", 40)

func init() {
	if len(os.Args) > 3 && os.Args[1] == tkChildEntry && os.Args[2] == "arm" {
		if err := armFleetObligation(context.Background(), os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func tkWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// Private host state and home so an armed obligation never lands in this machine's own records
func tkHostState(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	lockDir, served := filepath.Join(tmp, "gateway-locks"), filepath.Join(tmp, "home")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", lockDir)
	t.Setenv("ICEBATH_HOME", served)
	return lockDir, served
}

func tkExpectedSHA(t *testing.T) (string, bool) {
	t.Helper()
	record := updaterecord.ReadHostObligation()
	if record == nil {
		return "", false
	}
	sha, _ := record.GetOr("expected_sha", nil).(string)
	return sha, true
}

func TestTakeoverArmsTheCheckoutHeadWhenGitCannotAnswer(t *testing.T) {
	for _, git := range []string{"missing", "failing"} {
		t.Run(git, func(t *testing.T) {
			tkHostState(t)
			tmp := t.TempDir()
			binDir := filepath.Join(tmp, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)
			root := filepath.Join(tmp, "checkout")
			saved := takeoverGitExecutable
			t.Cleanup(func() { takeoverGitExecutable = saved })
			if git == "missing" {
				takeoverGitExecutable = func(context.Context) string { return "git" }
				worktree := filepath.Join(tmp, "repo.git", "worktrees", "checkout")
				tkWrite(t, filepath.Join(worktree, "commondir"), "../..\n", 0o644)
				tkWrite(t, filepath.Join(worktree, "HEAD"), "ref: refs/heads/main\n", 0o644)
				tkWrite(t, filepath.Join(tmp, "repo.git", "packed-refs"),
					"# pack-refs with: peeled fully-peeled sorted\n"+strings.Repeat("d", 40)+" refs/heads/other\n"+tkSHA+" refs/heads/main\n", 0o644)
				tkWrite(t, filepath.Join(root, ".git"), "gitdir: "+worktree+"\n", 0o644)
			} else {
				failingGit := filepath.Join(binDir, "git")
				tkWrite(t, failingGit, "#!/bin/sh\nexit 128\n", 0o755)
				takeoverGitExecutable = func(context.Context) string { return failingGit }
				tkWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
				tkWrite(t, filepath.Join(root, ".git", "refs", "heads", "main"), tkSHA+"\n", 0o644)
			}
			if err := armFleetObligation(context.Background(), root); err != nil {
				t.Fatal(err)
			}
			if sha, _ := tkExpectedSHA(t); sha != tkSHA {
				t.Fatalf("armed %q", sha)
			}
		})
	}
}

func TestTakeoverArmsAnShaLessObligationForAGitLessRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	tkHostState(t)
	tmp := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", tmp)
	root := filepath.Join(tmp, "archive")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := armFleetObligation(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if sha, recorded := tkExpectedSHA(t); !recorded || sha != "" {
		t.Fatalf("recorded %v sha %q", recorded, sha)
	}
}

func TestTakeoverArmsTheHostRecordWithoutApplicationDependencies(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "checkout")
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", root)
	base := []string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}
	run(append(base, "commit", "-q", "--allow-empty", "-m", "init")...)
	head := run(append(base, "rev-parse", "HEAD")...)
	lockDir, served := filepath.Join(tmp, "gateway-locks"), filepath.Join(tmp, "home")
	if err := os.Mkdir(served, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], tkChildEntry, "arm", root)
	child.Env = append(os.Environ(), "ICEBATH_GATEWAY_LOCK_DIR="+lockDir, "ICEBATH_HOME="+served)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	armed, err := filepath.Glob(filepath.Join(lockDir, "host-update-restart-*.json"))
	if err != nil || len(armed) != 1 {
		t.Fatalf("records %v %v", armed, err)
	}
	raw, err := os.ReadFile(armed[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &record); err != nil {
		t.Fatal(err)
	}
	if record["expected_sha"] != head {
		t.Fatalf("armed %v want %s", record["expected_sha"], head)
	}
}

func TestATakeoverDebtAnotherProfileCannotSeeIsNeverSilent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tmp := t.TempDir()
	homes := map[string]string{}
	for _, name := range []string{"coder", "writer"} {
		homes[name] = filepath.Join(tmp, "profiles", name)
		tkWrite(t, filepath.Join(homes[name], "config.yaml"), "{}\n", 0o644)
	}
	lockDir := filepath.Join(tmp, "gateway-locks")
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", lockDir)
	t.Setenv("ICEBATH_HOME", homes["coder"])
	saved := takeoverHeadSHA
	t.Cleanup(func() { takeoverHeadSHA = saved })
	takeoverHeadSHA = func(context.Context, string) string { return strings.Repeat("a", 40) }
	var stderr bytes.Buffer
	t.Cleanup(updaterecord.SetObligationStderr(&stderr))
	if err := os.Chmod(lockDir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := armFleetObligation(context.Background(), tmp)
	_ = os.Chmod(lockDir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(homes["coder"], "fleet_restart_pending")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("no per home marker: %v", err)
	}
	if !strings.Contains(stderr.String(), "icebath gateway restart") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestAHistoricalUpdaterArmsTheHostRecordUnderTheCurrentMutex(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "gateway-locks")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", lockDir)
	sha := strings.Repeat("a", 40)
	if !updaterecord.WriteHostObligation(context.Background(), sha, nil, "", "") {
		t.Fatal("the record was not written")
	}
	if armed, _ := tkExpectedSHA(t); armed != sha {
		t.Fatalf("armed %q", armed)
	}
	if _, err := os.Stat(filepath.Join(lockDir, ".host-update-restart.mutex", "record.lock")); err != nil {
		t.Fatal(err)
	}
}

// Polls the exit code marker while fn writes it and returns every text a poll read
func tkWatch(marker string, fn func()) []string {
	var mu sync.Mutex
	var seen []string
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			if raw, err := os.ReadFile(marker); err == nil {
				mu.Lock()
				seen = append(seen, string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
				mu.Unlock()
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	fn()
	close(stop)
	<-done
	return seen
}

// Receipt writers that record nothing, with the takeover's stderr captured
func tkQuietReceipt(t *testing.T) {
	t.Helper()
	savedStep, savedFinalize, savedStderr := takeoverRecordStep, takeoverFinalize, takeoverStderr
	t.Cleanup(func() { takeoverRecordStep, takeoverFinalize, takeoverStderr = savedStep, savedFinalize, savedStderr })
	takeoverRecordStep = func(context.Context, string, bool, string) {}
	takeoverFinalize = func(context.Context, *int, string) string { return "" }
	takeoverStderr = &bytes.Buffer{}
}

func tkGatewayRequest() *pyjson.Object {
	request := pyjson.NewObject()
	request.Set("gateway_mode", true)
	return request
}

func tkAssertExitCode(t *testing.T, marker string, seen []string, want string) {
	t.Helper()
	for _, text := range seen {
		if text == "" {
			t.Fatal("a poll read an empty exit code")
		}
	}
	raw, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(raw)) != want {
		t.Fatalf("marker %q %v want %s", raw, err, want)
	}
}

func TestFleetWriterNeverExposesAnEmptyExitCode(t *testing.T) {
	served := t.TempDir()
	t.Setenv("ICEBATH_HOME", served)
	marker := filepath.Join(served, ".update_exit_code")
	seen := tkWatch(marker, func() { updaterecord.WriteGatewayUpdateExitCode(context.Background(), true) })
	tkAssertExitCode(t, marker, seen, "0")
}

func TestTakeoverFailureNeverExposesAnEmptyExitCode(t *testing.T) {
	served := t.TempDir()
	t.Setenv("ICEBATH_HOME", served)
	tkQuietReceipt(t)
	marker, result := filepath.Join(served, ".update_exit_code"), filepath.Join(t.TempDir(), "result.json")
	seen := tkWatch(marker, func() {
		if err := recordFailure(context.Background(), tkGatewayRequest(), result, 3, "boom"); err != nil {
			t.Error(err)
		}
	})
	tkAssertExitCode(t, marker, seen, "3")
}

func TestTakeoverOwedFinishNeverExposesAnEmptyExitCode(t *testing.T) {
	served := t.TempDir()
	t.Setenv("ICEBATH_HOME", served)
	tkQuietReceipt(t)
	marker, result := filepath.Join(served, ".update_exit_code"), filepath.Join(t.TempDir(), "result.json")
	seen := tkWatch(marker, func() { recordOwedFinish(context.Background(), tkGatewayRequest(), result, "child missing") })
	tkAssertExitCode(t, marker, seen, "0")
}

func TestTakeoverRecordsWithoutRuntimeStateOrPm(t *testing.T) {
	for _, recorder := range []string{"failure", "owed_finish"} {
		t.Run(recorder, func(t *testing.T) {
			served := t.TempDir()
			t.Setenv("ICEBATH_HOME", served)
			tkQuietReceipt(t)
			result := filepath.Join(t.TempDir(), "result.json")
			want := "0"
			if recorder == "failure" {
				want = "3"
				if err := recordFailure(context.Background(), tkGatewayRequest(), result, 3, "boom"); err != nil {
					t.Fatal(err)
				}
			} else {
				recordOwedFinish(context.Background(), tkGatewayRequest(), result, "child missing")
			}
			if _, err := os.Stat(result); err != nil {
				t.Fatal(err)
			}
			tkAssertExitCode(t, filepath.Join(served, ".update_exit_code"), nil, want)
		})
	}
}
