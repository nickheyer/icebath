//go:build windows

package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// SCM service that starts the one child its child.txt names, like a service wrapper
type debtServiceWrapper struct{}

// Starts the configured child and keeps reporting running until the SCM stops the service
func (debtServiceWrapper) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	exe, err := os.Executable()
	if err != nil {
		return true, 1
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "child.txt"))
	if err != nil {
		return true, 1
	}
	spec := strings.Split(strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), "\n")
	child := exec.Command(spec[0])
	child.SysProcAttr = &syscall.SysProcAttr{CmdLine: syscall.EscapeArg(spec[0]) + " " + spec[1],
		HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	child.Dir = spec[2]
	child.Env = os.Environ()
	for _, line := range spec[3:] {
		if eq := strings.Index(line, "="); eq > 0 {
			child.Env = append(child.Env, line)
		}
	}
	if err := child.Start(); err != nil {
		return true, 1
	}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for request := range requests {
		switch request.Cmd {
		case svc.Interrogate:
			status <- request.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// Windows stand-ins of the live restart debt case, the service wrapper, its idle child and a recovering launch
func debtLiveChild(mode string, args []string) (int, bool) {
	switch mode {
	case "debt_service_wrapper":
		if err := svc.Run(args[0], debtServiceWrapper{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case "debt_sleep":
		time.Sleep(900 * time.Second)
		return 0, true
	case "debt_recovering_launch":
		ctx := context.Background()
		started := time.Now()
		if err := updaterecord.RecoverPauses(ctx, []string{"status"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		found, err := updaterecord.PauseOrphans(ctx, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		owed := []any{}
		for _, f := range found {
			token, _ := f.Body["token"].(map[string]any)
			owed = append(owed, token["services"])
		}
		text, err := json.Marshal(map[string]any{"seconds": time.Since(started).Seconds(), "owed": owed})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		fmt.Println(string(text))
		return 0, true
	}
	return 0, false
}

func debtSC(args ...string) (string, int) {
	out, err := exec.Command("sc.exe", args...).CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		return string(out) + err.Error(), -1
	}
	return string(out), 0
}

// Kills the service's process tree and deletes the service
func debtDropService(name string) {
	pid := 0
	if service, err := gatewaycli.WinServiceGet(name); err == nil {
		pid, _ = service.PID()
	}
	if pid != 0 {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
	debtSC("delete", name)
}

func TestALaunchRecoveringAnUnreadyServiceKeepsTheDebtWithoutStalling(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	binDir := filepath.Join(tmp, "svc")
	for _, dir := range []string{home, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", home)
	image, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "wrapper.exe")
	if err := os.WriteFile(exe, image, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	spec := strings.Join([]string{os.Args[0], cuChildEntry + " debt_sleep", cwd}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "child.txt"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("icebath-r9-recover-%d", os.Getpid())
	binPath := fmt.Sprintf("%s %s debt_service_wrapper %s", syscall.EscapeArg(exe), cuChildEntry, name)
	if out, code := debtSC("create", name, "binPath=", binPath, "start=", "demand"); code != 0 {
		t.Fatal(out)
	}
	token, err := updaterecord.StampTree(ctx, map[string]any{
		"resume_needed": true, "services": []any{name}, "expected_services": []any{name}, "restarted_services": []any{},
		"service_profiles": map[string]any{name: "default"}}, "")
	if err == nil {
		err = updaterecord.WritePause(ctx, token, updaterecord.PauseUnowned(), "")
	}
	if err != nil {
		debtDropService(name)
		t.Fatal(err)
	}
	type launch struct {
		Seconds float64 `json:"seconds"`
		Owed    []any   `json:"owed"`
	}
	var launches []launch
	failure := ""
	for range 2 {
		runCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
		cmd := exec.CommandContext(runCtx, os.Args[0], cuChildEntry, "debt_recovering_launch")
		cmd.Dir = cwd
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		var out []byte
		if err == nil {
			out, _ = io.ReadAll(stdout)
			err = cmd.Wait()
		}
		cancel()
		if err != nil {
			failure = string(out) + stderr.String() + err.Error()
			break
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		var got launch
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
			failure = string(out) + stderr.String() + err.Error()
			break
		}
		launches = append(launches, got)
	}
	debtDropService(name)
	if failure != "" {
		t.Fatal(failure)
	}
	owed := []any{}
	for _, l := range launches {
		owed = append(owed, l.Owed)
	}
	if want := []any{[]any{[]any{name}}, []any{[]any{name}}}; !pyrepr.Equal(owed, want) {
		t.Fatalf("the unready service's debt was lost: %v", launches)
	}
	if first := launches[0].Seconds; first >= 40 {
		t.Fatalf("the first launch stalled %.0fs on the service's readiness", first)
	}
	if second := launches[1].Seconds; second >= 20 {
		t.Fatalf("the next launch stalled %.0fs on a service it had just found unready", second)
	}
}
