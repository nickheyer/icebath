package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

var (
	transientRunningStates = map[string]bool{"draining": true, "degraded": true}
	staleRuntimeFiles      = []string{"gateway.pid", "processes.json"}
	containerBootLogRotate = int64(256 * 1024)
	procRoot               = "/proc"
)

func containerBootLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.container_boot")
}

// What the boot reconcile did with one profile slot
type ReconcileAction struct {
	Profile        string
	PriorState     *string
	Action         string
	PriorExit      string
	FoldedIntoRoot bool
}

func slotAction(profile, profileDir string, prior *string, start, folded bool) ReconcileAction {
	action := "registered"
	if start {
		action = "started"
	}
	return ReconcileAction{Profile: profile, PriorState: prior, Action: action, PriorExit: gateway.ReadPriorExitLabel(profileDir),
		FoldedIntoRoot: folded}
}

// Named profile directories carrying SOUL.md in name order, a profiles/default directory skipped with a warning
func namedProfileDirs(home string) [][2]string {
	root := filepath.Join(home, "profiles")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var found [][2]string
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !isDirPath(dir) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "SOUL.md")); err != nil {
			continue
		}
		if e.Name() == "default" {
			containerBootLog().Warn("profiles/default/ exists — skipping to avoid colliding with the reserved root-profile s6 slot")
			continue
		}
		found = append(found, [2]string{e.Name(), dir})
	}
	return found
}

// Recreates the s6 slot of the root profile and of every persistent named profile after a container restart
func ReconcileProfileGateways(ctx context.Context, home, scandir string, dryRun bool, containerArgv []string) ([]ReconcileAction, error) {
	var actions []ReconcileAction
	type named struct {
		name, dir string
		prior     *string
	}
	var all []named
	for _, pair := range namedProfileDirs(home) {
		all = append(all, named{pair[0], pair[1], readDesiredState(pair[1])})
	}
	standalone := map[string]bool{}
	for _, n := range all {
		ok, err := profiles.ProfileIsStandalone(ctx, n.dir)
		if err != nil {
			return nil, err
		}
		if ok {
			standalone[n.name] = true
		}
	}
	legacy, err := maybeMigrateLegacyGatewayRunState(home, containerArgv, dryRun)
	if err != nil {
		return nil, err
	}
	defaultPrior := legacy
	if defaultPrior == nil {
		defaultPrior = readDesiredState(home)
	}
	var states []NamedSlotState
	for _, n := range all {
		if !standalone[n.name] {
			states = append(states, NamedSlotState{Profile: n.name, Prior: n.prior})
		}
	}
	fold := FoldNamedSlotIntent(defaultPrior, states)
	if len(fold.Folded) > 0 && (defaultPrior == nil || !AutostartStates[*defaultPrior]) {
		containerBootLog().Warn(BootNotice(fold.Folded))
	}
	if !dryRun {
		cleanupStaleRuntimeFiles(home)
		if err := registerBootService(scandir, "default", fold.RootShouldStart); err != nil {
			return nil, err
		}
	}
	actions = append(actions, slotAction("default", home, defaultPrior, fold.RootShouldStart, len(fold.Folded) > 0))
	folded := map[string]bool{}
	for _, f := range fold.Folded {
		folded[f] = true
	}
	for _, n := range all {
		start := standalone[n.name] && n.prior != nil && AutostartStates[*n.prior]
		if !dryRun {
			cleanupStaleRuntimeFiles(n.dir)
			if err := registerBootService(scandir, n.name, start); err != nil {
				return nil, err
			}
		}
		actions = append(actions, slotAction(n.name, n.dir, n.prior, start, folded[n.name]))
	}
	if !dryRun {
		if err := writeReconcileLog(home, actions); err != nil {
			return nil, err
		}
	}
	return actions, nil
}

// Notice naming the named slots whose autostart the root slot took over
func BootNotice(folded []string) string {
	sorted := append([]string(nil), folded...)
	sort.Strings(sorted)
	return "reconcile: profile gateway(s) " + strings.Join(sorted, ", ") +
		" asked to autostart; one gateway per container serves every non-standalone profile, so the ROOT " +
		"slot (gateway-default) was started instead and multiplexes them."
}

// Seeds the root gateway state for a container whose command is gateway run, nil when nothing was seeded
func maybeMigrateLegacyGatewayRunState(home string, containerArgv []string, dryRun bool) (*string, error) {
	stateFile := filepath.Join(home, "gateway_state.json")
	if _, err := os.Stat(stateFile); err == nil {
		return nil, nil
	}
	switch strings.ToLower(os.Getenv("ICEBATH_GATEWAY_NO_SUPERVISE")) {
	case "1", "true", "yes":
		return nil, nil
	}
	argv := containerArgv
	if argv == nil {
		argv = readContainerArgv()
	}
	if !isLegacyGatewayRunRequest(argv) {
		return nil, nil
	}
	if !dryRun {
		text, err := pyjson.Dumps(pyjson.ObjectOf("gateway_state", "running", "desired_state", "running",
			"timestamp", time.Now().Unix(), "migrated_from", "legacy-container-cmd"))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(stateFile, []byte(text+"\n"), 0o666); err != nil {
			return nil, err
		}
	}
	running := "running"
	return &running, nil
}

// NUL separated argv of a proc cmdline file
func cmdlineArgv(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, part := range bytes.Split(raw, []byte{0}) {
		if len(part) > 0 {
			out = append(out, strings.ToValidUTF8(string(part), "\uFFFD"))
		}
	}
	return out, nil
}

// Argv of the container supervisor, PID 1 first and then every process, empty when none carries the init token
func readContainerArgv() []string {
	paths := []string{filepath.Join(procRoot, "1", "cmdline")}
	if entries, err := os.ReadDir(procRoot); err == nil {
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				paths = append(paths, filepath.Join(procRoot, e.Name(), "cmdline"))
			}
		}
	}
	for _, p := range paths {
		argv, err := cmdlineArgv(p)
		if err != nil {
			continue
		}
		if len(argv) > 0 && filepath.Base(argv[0]) == "s6-svscan" {
			return argv
		}
		for _, part := range argv {
			if strings.Contains(part, "main-wrapper.sh") {
				return argv
			}
		}
	}
	return []string{}
}

// Container command with everything through the supervisor or wrapper token and a leading icebath dropped
func StripContainerArgvPrefix(argv []string) []string {
	args := append([]string(nil), argv...)
	wrapper := -1
	for i, a := range args {
		if strings.HasSuffix(a, "main-wrapper.sh") {
			wrapper = i
			break
		}
	}
	switch {
	case wrapper >= 0:
		args = args[wrapper+1:]
	case len(args) > 0 && (filepath.Base(args[0]) == "init" || filepath.Base(args[0]) == "s6-svscan"):
		args = args[1:]
	}
	if len(args) > 0 && strings.HasSuffix(args[0], "entrypoint-dispatch.sh") {
		args = args[1:]
	}
	if len(args) > 0 && filepath.Base(args[0]) == "icebath" {
		args = args[1:]
	}
	return args
}

func isLegacyGatewayRunRequest(argv []string) bool {
	args := StripContainerArgvPrefix(argv)
	for _, a := range args {
		if a == "--no-supervise" {
			return false
		}
	}
	return len(args) >= 2 && args[0] == "gateway" && args[1] == "run"
}

// True when the container command is the dashboard
func IsDashboardContainer(argv []string) bool {
	args := StripContainerArgvPrefix(argv)
	return len(args) > 0 && args[0] == "dashboard"
}

// Desired state a slot recorded, transient running states counting as running, nil for none or an unreadable file
func readDesiredState(profileDir string) *string {
	stateFile := filepath.Join(profileDir, "gateway_state.json")
	raw, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil
	}
	var data map[string]any
	if err == nil {
		err = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data)
	}
	if err != nil {
		containerBootLog().Warn(fmt.Sprintf("could not read %s; treating as no prior state", stateFile))
		return nil
	}
	if desired, ok := data["desired_state"]; ok && desired != nil {
		s := fmt.Sprint(desired)
		return &s
	}
	g, ok := data["gateway_state"]
	if !ok || g == nil {
		return nil
	}
	s := fmt.Sprint(g)
	if transientRunningStates[s] {
		s = "running"
	}
	return &s
}

func cleanupStaleRuntimeFiles(profileDir string) {
	for _, name := range staleRuntimeFiles {
		os.Remove(filepath.Join(profileDir, name))
	}
}

// Builds a profile slot in a sibling temp dir, the down marker deciding its start state, then swaps it in
func registerBootService(scandir, profile string, start bool) error {
	if err := ValidateServiceProfileName(profile); err != nil {
		return err
	}
	serviceDir := filepath.Join(scandir, S6ServicePrefix+profile)
	tmpDir := filepath.Join(scandir, "."+S6ServicePrefix+profile+".tmp")
	if _, err := os.Lstat(tmpDir); err == nil {
		os.RemoveAll(tmpDir)
	}
	if err := os.MkdirAll(tmpDir, 0o777); err != nil {
		return err
	}
	writeExec := func(path, content string) error {
		if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
			return err
		}
		return os.Chmod(path, 0o755)
	}
	build := func() error {
		if err := os.WriteFile(filepath.Join(tmpDir, "type"), []byte("longrun\n"), 0o666); err != nil {
			return err
		}
		if err := writeExec(filepath.Join(tmpDir, "run"), renderS6RunScript(profile, map[string]string{})); err != nil {
			return err
		}
		if err := writeExec(filepath.Join(tmpDir, "finish"), renderS6FinishScript()); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(tmpDir, "log"), 0o777); err != nil {
			return err
		}
		if err := writeExec(filepath.Join(tmpDir, "log", "run"), renderS6LogRun(profile)); err != nil {
			return err
		}
		if !start {
			if err := os.WriteFile(filepath.Join(tmpDir, "down"), nil, 0o666); err != nil {
				return err
			}
		}
		if err := seedSuperviseSkeleton(tmpDir); err != nil {
			return err
		}
		if _, err := os.Lstat(serviceDir); err == nil {
			if err := os.RemoveAll(serviceDir); err != nil {
				return err
			}
		}
		return os.Rename(tmpDir, serviceDir)
	}
	if err := build(); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	return nil
}

// Python spelling of an optional state and a bool in the reconcile lines
func pyOptional(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Appends one line per action to logs/container-boot.log, rotating it to .1 past the size cap
func writeReconcileLog(home string, actions []ReconcileAction) error {
	logDir := filepath.Join(home, "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return err
	}
	logPath := filepath.Join(logDir, "container-boot.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() >= containerBootLogRotate {
		if err := os.Rename(logPath, filepath.Join(logDir, "container-boot.log.1")); err != nil {
			containerBootLog().Warn(fmt.Sprintf("could not rotate %s: %s", logPath, err))
		}
	}
	ts := time.Now().Format("2006-01-02T15:04:05-0700")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	for _, a := range actions {
		fmt.Fprintf(f, "%s profile=%s prior_state=%s action=%s prior_exit=%s folded_into_root=%s\n",
			ts, a.Profile, pyOptional(a.PriorState), a.Action, a.PriorExit, pyBool(a.FoldedIntoRoot))
	}
	return f.Close()
}

// Boot reconcile entry, skipped in a dashboard container, printing the notice and one line per slot
func ContainerBootMain(ctx context.Context, out io.Writer) int {
	if IsDashboardContainer(readContainerArgv()) {
		fmt.Fprintln(out, "reconcile: skipping (dashboard container — does not need per-profile gateways)")
		return 0
	}
	home := os.Getenv("ICEBATH_HOME")
	if home == "" {
		home = "/opt/data"
	}
	scandir := os.Getenv("S6_PROFILE_GATEWAY_SCANDIR")
	if scandir == "" {
		scandir = S6DynamicScandir
	}
	actions, err := ReconcileProfileGateways(ctx, home, scandir, false, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		return 1
	}
	var folded []string
	for _, a := range actions {
		if a.Profile != "default" && a.FoldedIntoRoot {
			folded = append(folded, a.Profile)
		}
	}
	if len(folded) > 0 {
		fmt.Fprintln(out, BootNotice(folded))
	}
	for _, a := range actions {
		fmt.Fprintf(out, "reconcile: profile=%s prior_state=%s action=%s\n", a.Profile, pyOptional(a.PriorState), a.Action)
	}
	return 0
}
