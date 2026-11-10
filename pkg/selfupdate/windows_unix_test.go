//go:build !windows

package selfupdate

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

type wpChild struct {
	cmd *exec.Cmd
	out *bufio.Reader
}

func wpStart(t *testing.T, mode string, args ...string) *wpChild {
	t.Helper()
	argv := cuChildArgv(mode, args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &wpChild{cmd: cmd, out: bufio.NewReader(stdout)}
}

func (c *wpChild) line() string {
	line, _ := c.out.ReadString('\n')
	return strings.TrimSpace(line)
}

// Runs n launches' pause recovery in turn and returns the resume lines each printed
func wpLaunches(t *testing.T, n int) [][]string {
	t.Helper()
	seen := [][]string{}
	for range n {
		argv := cuChildArgv("pause-recover")
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			t.Fatalf("launch failed %v %q", err, out)
		}
		resumed := []string{}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "resume") {
				resumed = append(resumed, line)
			}
		}
		seen = append(seen, resumed)
	}
	return seen
}

func wpRecordFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if strings.HasPrefix(e.Name(), updaterecord.PauseRecordStem) && (ext == ".json" || ext == ".claim") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func TestPauseARequestOnDiskIsOwedEvenBeforeItIsRecordedSent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	marker := filepath.Join(tmp, "profile", ".gateway-planned-stop.json")
	if err := os.Mkdir(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	gw := wpStart(t, "pause-draining", filepath.Join(tmp, "unused"))
	if line := gw.line(); line != "up" {
		t.Fatal(line)
	}
	updater := wpStart(t, "planned-stop-updater", strconv.Itoa(gw.cmd.Process.Pid), marker)
	if line := updater.line(); line != "requested" {
		t.Fatal(line)
	}
	_ = updater.cmd.Process.Kill()
	_ = updater.cmd.Wait()
	nothing := [][]string{{}}
	if got := wpLaunches(t, 1); !reflect.DeepEqual(got, nothing) || len(wpRecordFiles(t, tmp)) != 1 {
		t.Fatalf("an issued request lost its debt %v %v", got, wpRecordFiles(t, tmp))
	}
	user := pyjson.NewObject()
	user.Set("target_pid", gw.cmd.Process.Pid)
	user.Set("stopper_pid", 1)
	text, _ := pyjson.Dumps(user)
	if err := os.WriteFile(marker, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := wpLaunches(t, 1); !reflect.DeepEqual(got, nothing) || len(wpRecordFiles(t, tmp)) != 1 {
		t.Fatalf("resolved evidence was re-judged %v", got)
	}
	_ = gw.cmd.Process.Kill()
	_ = gw.cmd.Wait()
	if got := wpLaunches(t, 2); !reflect.DeepEqual(got, [][]string{{"resume ['default']"}, {}}) {
		t.Fatal(got)
	}
}

func TestPausePlannedStopMarkerRecordsTheRequestAndRefusesAnUnwritableHome(t *testing.T) {
	home := t.TempDir()
	ok, err := writeUpdatePlannedStopMarker(context.Background(), home, os.Getpid())
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	raw, _ := os.ReadFile(plannedStopMarkerPath(home))
	loaded, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	body := loaded.(*pyjson.Object)
	if !reflect.DeepEqual(body.Keys, []string{"target_pid", "target_start_time", "stopper_pid", "written_at"}) ||
		strings.Contains(string(raw), " ") || !strings.HasSuffix(body.Vals["written_at"].(string), "+00:00") {
		t.Fatalf("%s", raw)
	}
	if os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if ok, err := writeUpdatePlannedStopMarker(context.Background(), locked, os.Getpid()); ok || err != nil {
		t.Fatal(ok, err)
	}
}
