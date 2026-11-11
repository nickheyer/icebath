package selfupdate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	holderLinePrefix      = "Desktop app folder held by: "
	consentFileName       = ".close-holders-consent.json"
	startTimeToleranceS   = 0.5
	holderCloseWaitSecond = 5
)

// Process whose working directory is inside the desktop release folder
type releaseHolder struct {
	PID        int
	Name       string
	Exe        string
	CreateTime float64
}

// Name, pid and program path of a holder for the user
func (h releaseHolder) describe() string {
	exe := h.Exe
	if exe == "" {
		exe = "path unreadable"
	}
	return fmt.Sprintf("%s (pid %d, %s)", h.Name, h.PID, exe)
}

// Machine read line naming a holder with its pid and start time
func (h releaseHolder) holderLine() string {
	exe := h.Exe
	if exe == "" {
		exe = "path unreadable"
	}
	return fmt.Sprintf("%s%s (pid %d, started %.3f, %s)", holderLinePrefix, h.Name, h.PID, h.CreateTime, exe)
}

// One process the user agreed to close
type consentedHolder struct {
	PID   int
	Start float64
	Exe   string
}

// Processes the user agreed to close as pid, start time and normalized exe
type holderConsent struct{ holders []consentedHolder }

// True when the consent names this holder by pid, start time and program path
func (c holderConsent) covers(h releaseHolder) bool {
	if h.Exe == "" {
		return false
	}
	for _, c := range c.holders {
		if c.PID == h.PID && math.Abs(c.Start-h.CreateTime) <= startTimeToleranceS && c.Exe == holderNorm(h.Exe) {
			return true
		}
	}
	return false
}

// Absolute normcased spelling of a path
func holderNorm(path string) string {
	abs, err := pypath.Abspath(path)
	if err != nil {
		abs = path
	}
	return pypath.Normcase(abs)
}

// True when a normalized path is the root or under it
func holderInside(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimRight(root, `\/`)+string(os.PathSeparator))
}

// Normalized resolved release folder, false when it cannot be resolved
func holderReleaseRoot(releaseDir string) (string, bool) {
	resolved, err := pypath.Resolve(releaseDir)
	if err != nil {
		return "", false
	}
	return holderNorm(resolved), true
}

// Program path of a matched holder, empty when the OS refuses it
var holderExe = func(pid int) string {
	exe, err := proc.Exe(pid)
	if err != nil {
		return ""
	}
	return exe
}

// True when both stdin and stdout are terminals a person can answer at
var holderInteractive = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// Every other process whose working directory is the folder or below it
var findReleaseCwdHolders = func(folder string) []releaseHolder {
	root, ok := holderReleaseRoot(folder)
	pids, err := proc.PIDs()
	if err != nil || !ok {
		return nil
	}
	me := os.Getpid()
	var holders []releaseHolder
	for _, pid := range pids {
		if pid == me {
			continue
		}
		cwd, err := proc.Cwd(pid)
		if err != nil || cwd == "" || !holderInside(holderNorm(cwd), root) {
			continue
		}
		born, err := proc.Birth(pid)
		if err != nil {
			continue
		}
		name, err := proc.Name(pid)
		if err != nil || name == "" {
			name = "unknown program"
		}
		exe := holderExe(pid)
		holders = append(holders, releaseHolder{PID: pid, Name: name, Exe: exe, CreateTime: born})
	}
	return holders
}

// Consumes the desktop's close consent of this hand off run, removing the file either way
func takeConsent(releaseDir, runID string) holderConsent {
	path := filepath.Join(releaseDir, consentFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return holderConsent{}
	}
	_ = os.Remove(path)
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return holderConsent{}
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return holderConsent{}
	}
	if runID == "" || data["run_id"] != runID {
		return holderConsent{}
	}
	rows, ok := data["holders"].([]any)
	if !ok {
		return holderConsent{}
	}
	var out []consentedHolder
	for _, row := range rows {
		h, ok := row.(map[string]any)
		if !ok {
			return holderConsent{}
		}
		exe, ok := h["exe"].(string)
		if !ok || exe == "" {
			continue
		}
		pid, okPID := h["pid"].(float64)
		start, okStart := h["start_time"].(float64)
		if !okPID || !okStart || pid != math.Trunc(pid) {
			return holderConsent{}
		}
		out = append(out, consentedHolder{PID: int(pid), Start: start, Exe: holderNorm(exe)})
	}
	return holderConsent{holders: out}
}

// Terminal prompt of a person at a tty agreeing to close the holders
var holderTerminalInput = func(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// True only when a person at a terminal agreed to close the holders
func askAtTerminal(holders []releaseHolder) bool {
	if len(holders) == 0 || !holderInteractive() {
		return false
	}
	names := make([]string, len(holders))
	for i, h := range holders {
		names[i] = h.describe()
	}
	answer, err := holderTerminalInput(fmt.Sprintf("  Close %s and retry? [y/N] ", strings.Join(names, ", ")))
	if err != nil {
		return false
	}
	answer = strings.ToLower(pystr.Strip(answer))
	return answer == "y" || answer == "yes"
}

// Terminates exactly these holders after rechecking each, the pids that are gone afterwards
func closeHolders(holders []releaseHolder, folder string) []int {
	root, rootOK := holderReleaseRoot(folder)
	ancestorList, err := proc.Parents(os.Getpid())
	if err != nil {
		return nil
	}
	ancestors := map[int]bool{}
	for _, pid := range ancestorList {
		ancestors[pid] = true
	}
	var targets []int
	for _, h := range holders {
		born, err := proc.Birth(h.PID)
		if err != nil || ancestors[h.PID] || math.Abs(born-h.CreateTime) > startTimeToleranceS {
			continue
		}
		cwd, err := proc.Cwd(h.PID)
		if err != nil || !rootOK || !holderInside(holderNorm(cwd), root) {
			continue
		}
		if proc.Terminate(h.PID) != nil {
			continue
		}
		targets = append(targets, h.PID)
	}
	if len(targets) > 0 {
		alive := waitPIDsGone(targets, holderCloseWaitSecond*time.Second)
		for _, pid := range alive {
			_ = proc.KillPID(pid)
		}
		if len(alive) > 0 {
			waitPIDsGone(alive, holderCloseWaitSecond*time.Second)
		}
	}
	var closed []int
	for _, pid := range targets {
		if !proc.Exists(pid) {
			closed = append(closed, pid)
		}
	}
	return closed
}

// Polls until the pids exit or the timeout passes, the ones still alive
func waitPIDsGone(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			if proc.Exists(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive
		}
		pids = alive
		time.Sleep(100 * time.Millisecond)
	}
}

// Names every cwd holder of a refused rename and closes the consented ones, the holders and closed pids
func releaseAfterRefusedRename(folder string, consent holderConsent) ([]releaseHolder, []int) {
	holders := findReleaseCwdHolders(folder)
	if len(holders) == 0 {
		return nil, nil
	}
	for _, h := range holders {
		fmt.Fprintf(desktopOut, "  %s\n", h.describe())
	}
	var chosen, closable []releaseHolder
	for _, h := range holders {
		if consent.covers(h) {
			chosen = append(chosen, h)
		}
		if h.Exe != "" {
			closable = append(closable, h)
		}
	}
	for _, h := range holders {
		if h.Exe == "" {
			fmt.Fprintf(desktopOut, "  Close %s (pid %d) yourself: Icebath cannot close it.\n", h.Name, h.PID)
		}
	}
	if len(chosen) == 0 && askAtTerminal(closable) {
		chosen = closable
	}
	if len(chosen) == 0 {
		return holders, nil
	}
	closed := closeHolders(chosen, folder)
	if len(closed) > 0 {
		isClosed := map[int]bool{}
		for _, pid := range closed {
			isClosed[pid] = true
		}
		var names []string
		for _, h := range chosen {
			if isClosed[h.PID] {
				names = append(names, h.describe())
			}
		}
		fmt.Fprintf(desktopOut, "  ✓ Closed with your consent: %s\n", strings.Join(names, ", "))
	}
	return holders, closed
}

// Final report of a refused swap naming the holders still in the folder
func describeBlock(holders []releaseHolder, releaseDir string) string {
	var lines, names, pids []string
	for _, h := range holders {
		lines = append(lines, "  "+h.holderLine()+"\n")
		names = append(names, h.describe())
		pids = append(pids, fmt.Sprintf("/PID %d", h.PID))
	}
	return strings.Join(lines, "") +
		fmt.Sprintf("✗ Windows would not replace the desktop app: %s is using a folder inside %s.\n", strings.Join(names, ", "), releaseDir) +
		fmt.Sprintf("    It is not part of Icebath, so it was left running. Close it (Task Manager, or `taskkill %s`)\n", strings.Join(pids, " ")) +
		"    and run `icebath desktop --force-build`, or run Update now in Icebath Desktop and choose to close it."
}

// Names and pids of the holders
func holderNames(holders []releaseHolder) string {
	names := make([]string, len(holders))
	for i, h := range holders {
		names[i] = fmt.Sprintf("%s (pid %d)", h.Name, h.PID)
	}
	return strings.Join(names, ", ")
}
