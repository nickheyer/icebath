//go:build !windows

package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Exec chain length past which a launch counts as looping
const vsGenerationCap = 8

// Runs one launch as a separate process and counts the generations it exec'd
func vsLaunch(t *testing.T, tmp, root, syncMode string, launch int, argv []string) (int, string, string, int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(tmp, "state")); err != nil {
		vsWrite(t, filepath.Join(tmp, "state"), "stale")
	}
	encoded, _ := json.Marshal(argv)
	if argv == nil {
		encoded = []byte("[]")
	}
	cmd := exec.Command(os.Args[0], vsChildArgv("launch", root, tmp, syncMode, strconv.Itoa(launch))[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + tmp, "ICEBATH_HOME=" + filepath.Join(tmp, "home"),
		"LAUNCH_COMMAND=" + string(encoded), "GENERATION_CAP=" + strconv.Itoa(vsGenerationCap), "GORACE=" + os.Getenv("GORACE")}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	data, _ := os.ReadFile(filepath.Join(tmp, "generations-"+strconv.Itoa(launch)))
	return code, stdout.String(), stderr.String(), len(strings.Fields(string(data)))
}

func vsSyncs(tmp string) int {
	data, err := os.ReadFile(filepath.Join(tmp, "syncs"))
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(data)))
}

func TestBackedOffFailedSyncBootsOnThePreviousDependencies(t *testing.T) {
	for _, tc := range []struct {
		id   string
		argv []string
	}{{"launch", nil}, {"update", []string{"update"}}, {"update-check", []string{"update", "--check"}}} {
		t.Run(tc.id, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "checkout")
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
				t.Fatal(err)
			}
			vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
			vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
			for launch := 1; launch < 5; launch++ {
				code, stdout, stderr, generations := vsLaunch(t, tmp, root, "fail", launch, tc.argv)
				if code != 0 {
					t.Fatalf("launch %d looped: %d generations\n%s", launch, generations, stderr)
				}
				if strings.TrimSpace(stdout) != "BOOTED" || generations != 1 {
					t.Fatalf("launch %d stdout %q generations %d\n%s", launch, stdout, generations, stderr)
				}
			}
			expected := CompletionRetryBackoffAttempts
			if len(tc.argv) == 1 && tc.argv[0] == "update" {
				expected = 4
			}
			if syncs := vsSyncs(tmp); syncs != expected {
				t.Fatalf("a backed-off launch re-ran the failing repair: %d syncs", syncs)
			}
			if tc.argv != nil {
				return
			}
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
			record := completionAttemptsPath(context.Background(), root)
			if st, err := os.Stat(record); err != nil || !st.Mode().IsRegular() {
				t.Fatal("the failed syncs were not recorded where completion_retry_state reads")
			}
			vsWrite(t, record, strconv.Itoa(CompletionRetryMaxAttempts)+"\n")
			code, _, stderr, generations := vsLaunch(t, tmp, root, "fail", 5, tc.argv)
			if code != 0 || generations != 1 {
				t.Fatalf("capped launch %d %d\n%s", code, generations, stderr)
			}
			if n := strings.Count(stderr, "could not be finished automatically"); n != 1 {
				t.Fatalf("cap pointer printed %d times\n%s", n, stderr)
			}
		})
	}
}

// A committed checkout whose install stamp names its clean HEAD
func vsCommittedCheckout(t *testing.T, tmp string) string {
	t.Helper()
	root := filepath.Join(tmp, "checkout")
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, ".gitignore"), "install-stamp.json\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + tmp}
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"},
		{"add", "-A"}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	stamp, err := WriteSourceStamp(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if dirty, _ := stamp.Get("dirty"); dirty != false {
		t.Fatalf("source stamp dirty %v", dirty)
	}
	return root
}

func TestRelaunchedChildNeverRelaunchesIntoItself(t *testing.T) {
	tmp := t.TempDir()
	root := vsCommittedCheckout(t, tmp)
	code, stdout, stderr, generations := vsLaunch(t, tmp, root, "ok", 1, nil)
	if code != 0 {
		t.Fatalf("looped: %d generations\n%s", generations, stderr)
	}
	if strings.TrimSpace(stdout) != "BOOTED" || generations != 2 {
		t.Fatalf("stdout %q generations %d, the first relaunch into the store binary is still taken\n%s", stdout, generations, stderr)
	}
	if !strings.Contains(stderr, "relaunching did not leave this install current") {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestMarkerKeptByARestoreDoesNotRefuseARelaunchThatWorked(t *testing.T) {
	tmp := t.TempDir()
	root := vsCommittedCheckout(t, tmp)
	code, stdout, stderr, generations := vsLaunch(t, tmp, root, "restore", 1, nil)
	if code != 0 {
		t.Fatalf("looped: %d generations\n%s", generations, stderr)
	}
	if strings.TrimSpace(stdout) != "BOOTED" || strings.Contains(stderr, "relaunching did not leave this install current") {
		t.Fatalf("stdout %q\n%s", stdout, stderr)
	}
	if generations != 4 {
		t.Fatalf("generations %d\n%s", generations, stderr)
	}
}
