package selfupdate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Foreign program whose working directory is inside release/win-unpacked
func drhHolder(t *testing.T) (string, *exec.Cmd) {
	root := t.TempDir()
	release := filepath.Join(root, "apps", "desktop", "release")
	if err := os.MkdirAll(filepath.Join(release, "win-unpacked"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], dblChildEntry, "sleep-holder")
	cmd.Dir = filepath.Join(release, "win-unpacked")
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("holder printed %q", line)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})
	return release, cmd
}

// Consent the desktop's close and retry choice writes for the retry run it starts
func drhConsent(t *testing.T, release string, pid int, runID any, override map[string]any) {
	born, err := proc.Birth(pid)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := proc.Exe(pid)
	seen := map[string]any{"pid": pid, "start_time": born, "exe": exe}
	for k, v := range override {
		seen[k] = v
	}
	raw, _ := json.Marshal(map[string]any{"written_at": float64(time.Now().Unix()), "run_id": runID, "exes": []any{seen["exe"]}, "holders": []any{seen}})
	sbWrite(t, filepath.Join(release, consentFileName), string(raw))
}

// Fails the test when a non interactive run prompts
func drhNeverAsked(t *testing.T) {
	prev := holderTerminalInput
	holderTerminalInput = func(string) (string, error) { t.Fatal("a non-interactive run must never prompt"); return "", nil }
	t.Cleanup(func() { holderTerminalInput = prev })
}

func drhPIDs(holders []releaseHolder) []int {
	out := []int{}
	for _, h := range holders {
		out = append(out, h.PID)
	}
	return out
}

func drhAlive(pid int) bool { return proc.Exists(pid) }

func TestARetryThatFailsBeforeTheSwapLeavesNoConsentForALaterUpdate(t *testing.T) {
	release, cmd := drhHolder(t)
	pid := cmd.Process.Pid
	drhNeverAsked(t)
	sbCapture(t)
	drhConsent(t, release, pid, "desktop-retry-1", nil)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", "desktop-retry-1")
	desktop := filepath.Dir(release)
	staging := filepath.Join(desktop, ".staging-failed")
	_ = os.MkdirAll(staging, 0o755)
	if got := swapStagedDesktopApp(desktop, staging, nil); got != "" {
		t.Fatalf("promoted %q", got)
	}
	if _, err := os.Stat(filepath.Join(release, consentFileName)); err == nil {
		t.Fatal("the swap attempt consumes the consent, failed or not")
	}
	os.Unsetenv("ICEBATH_UPDATE_CORRELATION_ID")
	holders, closed := releaseAfterRefusedRename(release, holderConsent{})
	if !slices.Contains(drhPIDs(holders), pid) || len(closed) != 0 || !drhAlive(pid) {
		t.Fatalf("holders %v closed %v", drhPIDs(holders), closed)
	}
}

func TestAConsentClosesOnlyWhatTheUserSawInTheRunItWasGivenFor(t *testing.T) {
	release, cmd := drhHolder(t)
	pid := cmd.Process.Pid
	drhNeverAsked(t)
	out := sbCapture(t)
	started, _ := proc.Birth(pid)
	for name, row := range map[string]struct {
		consented string
		swap      string
		override  map[string]any
	}{
		"another run's swap":    {"desktop-retry-1", "gateway-update-7", nil},
		"a swap with no run id": {"desktop-retry-1", "", nil},
		"a recycled pid":        {"desktop-retry-1", "desktop-retry-1", map[string]any{"start_time": started - 60}},
		"another program":       {"desktop-retry-1", "desktop-retry-1", map[string]any{"exe": "C:/elsewhere/other.exe"}},
	} {
		drhConsent(t, release, pid, row.consented, row.override)
		consent := takeConsent(release, row.swap)
		if _, err := os.Stat(filepath.Join(release, consentFileName)); err == nil {
			t.Fatalf("%s: the attempt consumes the consent", name)
		}
		holders, closed := releaseAfterRefusedRename(release, consent)
		if !slices.Contains(drhPIDs(holders), pid) || len(closed) != 0 || !drhAlive(pid) {
			t.Fatalf("closed without consent (%s)", name)
		}
	}
	procName, _ := proc.Name(pid)
	if !strings.Contains(out.String(), fmt.Sprintf("%s (pid %d, ", procName, pid)) {
		t.Fatalf("out %s", out.String())
	}
	drhConsent(t, release, pid, "desktop-retry-1", nil)
	_, closed := releaseAfterRefusedRename(release, takeConsent(release, "desktop-retry-1"))
	if !slices.Contains(closed, pid) {
		t.Fatalf("closed %v", closed)
	}
}

func TestAHolderWhoseProgramPathIsUnreadableIsLeftForTheUser(t *testing.T) {
	release, cmd := drhHolder(t)
	pid := cmd.Process.Pid
	drhNeverAsked(t)
	out := sbCapture(t)
	prevExe, prevInteractive := holderExe, holderInteractive
	holderExe = func(int) string { return "" }
	holderInteractive = func() bool { return true }
	t.Cleanup(func() { holderExe, holderInteractive = prevExe, prevInteractive })
	drhConsent(t, release, pid, "desktop-retry-1", map[string]any{"exe": ""})
	holders, closed := releaseAfterRefusedRename(release, takeConsent(release, "desktop-retry-1"))
	if !slices.Contains(drhPIDs(holders), pid) || len(closed) != 0 || !drhAlive(pid) {
		t.Fatalf("holders %v closed %v", drhPIDs(holders), closed)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("(pid %d) yourself", pid)) {
		t.Fatalf("out %s", out.String())
	}
	found := false
	for _, line := range strings.Split(describeBlock(holders, release), "\n") {
		if strings.Contains(line, fmt.Sprintf("(pid %d, started ", pid)) {
			found = strings.HasSuffix(line, "path unreadable)")
		}
	}
	if !found {
		t.Fatal("the holder line does not say the path is unreadable")
	}
}

func TestAConsentFileThatIsNotUtf8IsConsumedWithoutRaising(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	sbWrite(t, filepath.Join(release, consentFileName), "{\"run_id\": \"\xff\xfe\"}")
	if consent := takeConsent(release, "desktop-retry-1"); len(consent.holders) != 0 {
		t.Fatalf("consent %v", consent)
	}
	if _, err := os.Stat(filepath.Join(release, consentFileName)); err == nil {
		t.Fatal("the consent file was left behind")
	}
}
