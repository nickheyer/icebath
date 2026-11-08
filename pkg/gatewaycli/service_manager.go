package gatewaycli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	S6DynamicScandir = "/run/service"
	S6ServicePrefix  = "gateway-"
	maxProfileLen    = 251
	s6BinDir         = "/command"
	icebathUID       = 10000
	icebathGID       = 10000
)

var (
	validServiceProfileRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*`)
	s6ProcRoot            = "/proc/1/comm"
	s6BaseDir             = "/run/s6/basedir"
	s6Run                 = runS6
	serviceSleep          = time.Sleep
	writePlannedStop      = gateway.WritePlannedStopMarker
)

// Error unless the name is a filesystem and s6 safe profile name
func ValidateServiceProfileName(name string) error {
	if name == "" {
		return &profiles.ValueError{Msg: "profile name must not be empty"}
	}
	if n := len([]rune(name)); n > maxProfileLen {
		return &profiles.ValueError{Msg: fmt.Sprintf("profile name too long (%d > %d)", n, maxProfileLen)}
	}
	if !validServiceProfileRE.MatchString(name) {
		return &profiles.ValueError{Msg: "profile name must match [a-z0-9][a-z0-9_-]*, got " + pyrepr.StrRepr(name)}
	}
	return nil
}

// Init system specific gateway service operations, runtime registration only on s6
type ServiceManager interface {
	Kind() string
	Start(name string) error
	Stop(name string) error
	Restart(name string) error
	IsRunning(name string) (bool, error)
	SupportsRuntimeRegistration() bool
	RegisterProfileGateway(profile string, startNow bool) error
	RegisterProfileGatewayEnv(profile string, extraEnv map[string]string, startNow bool) error
	UnregisterProfileGateway(profile string) error
	ListProfileGateways() ([]string, error)
}

// Service manager kind of this environment, s6 first then windows, launchd, systemd or none
func DetectServiceManager() (string, error) {
	switch {
	case s6Running():
		return "s6", nil
	case gateway.IsWindows():
		return "windows", nil
	case gateway.IsMacOS():
		return "launchd", nil
	}
	if gateway.SupportsSystemdServices(context.Background()) {
		return "systemd", nil
	}
	return "none", nil
}

// Whether s6-svscan is PID 1 with its base directory present
func s6Running() bool {
	raw, err := os.ReadFile(s6ProcRoot)
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(raw), "�")) == "s6-svscan" && isDirPath(s6BaseDir)
}

// S6Running reports whether s6-svscan is PID 1 with its base directory present
func S6Running() bool { return s6Running() }

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Error of a runtime registration verb no host backend supports
type NotImplementedError struct{ Msg string }

func (e *NotImplementedError) Error() string { return e.Msg }

// Lifecycle shared by the host backends, registration refused on every one
type hostServiceManager struct {
	kind, typeName string
	start, stop    func() error
	restart        func() error
	running        func() (bool, error)
}

func (m *hostServiceManager) Kind() string                           { return m.kind }
func (m *hostServiceManager) Start(string) error                     { return m.start() }
func (m *hostServiceManager) Stop(string) error                      { return m.stop() }
func (m *hostServiceManager) Restart(string) error                   { return m.restart() }
func (m *hostServiceManager) IsRunning(string) (bool, error)         { return m.running() }
func (m *hostServiceManager) SupportsRuntimeRegistration() bool      { return false }
func (m *hostServiceManager) ListProfileGateways() ([]string, error) { return []string{}, nil }

func (m *hostServiceManager) unsupported(verb string) error {
	return &NotImplementedError{Msg: m.typeName + " does not support runtime profile gateway " + verb + " (container-only feature)"}
}

func (m *hostServiceManager) RegisterProfileGateway(string, bool) error {
	return m.unsupported("registration")
}

func (m *hostServiceManager) RegisterProfileGatewayEnv(string, map[string]string, bool) error {
	return m.unsupported("registration")
}

func (m *hostServiceManager) UnregisterProfileGateway(string) error {
	return m.unsupported("unregistration")
}

// Systemd backend over the user scope systemd lifecycle
func NewSystemdServiceManager() ServiceManager {
	return &hostServiceManager{kind: "systemd", typeName: "SystemdServiceManager",
		start:   func() error { return gateway.SystemdStart(context.Background(), false) },
		stop:    func() error { return gateway.SystemdStop(context.Background(), false) },
		restart: func() error { return gateway.SystemdRestart(context.Background(), false) },
		running: func() (bool, error) {
			_, running, err := gateway.ProbeSystemdServiceRunning(context.Background(), false)
			return running, err
		}}
}

// Launchd backend over the launchd lifecycle
func NewLaunchdServiceManager() ServiceManager {
	return &hostServiceManager{kind: "launchd", typeName: "LaunchdServiceManager",
		start:   func() error { return gateway.LaunchdStart(context.Background()) },
		stop:    func() error { return gateway.LaunchdStop(context.Background()) },
		restart: func() error { return gateway.LaunchdRestart(context.Background()) },
		running: func() (bool, error) { return gateway.ProbeLaunchdServiceRunning(context.Background()) }}
}

// Windows backend over the scheduled task or startup folder lifecycle
type WindowsServiceManager struct{ hostServiceManager }

// Installs the Windows gateway task with its platform options
func (m *WindowsServiceManager) Install(o gateway.WindowsInstallOptions) error {
	return gateway.WindowsInstall(context.Background(), o)
}

// Windows backend whose running check needs the installed task and a live gateway pid
func NewWindowsServiceManager() ServiceManager {
	return &WindowsServiceManager{hostServiceManager{kind: "windows", typeName: "WindowsServiceManager",
		start:   func() error { return gateway.WindowsStart(context.Background()) },
		stop:    func() error { return gateway.WindowsStop(context.Background()) },
		restart: func() error { return gateway.WindowsRestart(context.Background()) },
		running: func() (bool, error) {
			installed, err := gateway.WindowsIsInstalled(context.Background())
			if err != nil || !installed {
				return false, err
			}
			pids, err := gateway.FindGatewayPIDsFor(context.Background(), nil, false)
			return len(pids) > 0, err
		}}}
}

// Installs service manager detection and lookup for pkg/profiles/manage
func init() {
	h := &profiles.GatewayServiceHooks
	h.ServiceManagerDetect = DetectServiceManager
	h.ServiceManagerGet = func() (profiles.ProfileServiceManager, error) {
		mgr, err := GetServiceManager()
		if err != nil || mgr == nil {
			return nil, err
		}
		return mgr, nil
	}
}

// Service manager of the current environment, an error when none is supported
func GetServiceManager() (ServiceManager, error) {
	kind, err := DetectServiceManager()
	if err != nil {
		return nil, err
	}
	switch kind {
	case "systemd":
		return NewSystemdServiceManager(), nil
	case "launchd":
		return NewLaunchdServiceManager(), nil
	case "windows":
		return NewWindowsServiceManager(), nil
	case "s6":
		return NewS6ServiceManager(S6DynamicScandir), nil
	}
	return nil, errors.New("no supported service manager detected")
}

// Profile name of an s6 gateway service name
func profileFromService(name string) string { return strings.TrimPrefix(name, S6ServicePrefix) }

// Persistent profile directory of an s6 gateway service under the shared root
func profileDirForGatewayService(name string) (string, error) {
	profile := profileFromService(name)
	if err := ValidateServiceProfileName(profile); err != nil {
		return "", err
	}
	icebathHome, ok := os.LookupEnv("ICEBATH_HOME")
	if !ok {
		icebathHome = "/opt/data"
	}
	icebathHome = filepath.Clean(icebathHome)
	root := icebathHome
	if filepath.Base(filepath.Dir(icebathHome)) == "profiles" {
		root = filepath.Dir(filepath.Dir(icebathHome))
	}
	if profile == "default" {
		return root, nil
	}
	return filepath.Join(root, "profiles", profile), nil
}

// Persists the operator start or stop intent beside the gateway state, silent on filesystem failures
func writeGatewayDesiredState(name, desired string) error {
	profileDir, err := profileDirForGatewayService(name)
	if err != nil {
		return err
	}
	stateFile := filepath.Join(profileDir, "gateway_state.json")
	if _, err := os.Stat(profileDir); err != nil {
		return nil
	}
	data := pyjson.NewObject()
	if raw, err := os.ReadFile(stateFile); err == nil {
		if v, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff")); err == nil {
			if obj, ok := v.(*pyjson.Object); ok {
				data = obj
			}
		}
	}
	data.Set("desired_state", desired)
	data.Set("updated_at", time.Now().Unix())
	text, err := pyjson.Dumps(data, pyjson.Separators(",", ":"))
	if err != nil {
		return nil
	}
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(text+"\n"), 0o666); err != nil {
		return nil
	}
	os.Rename(tmp, stateFile)
	return nil
}

// Registers a down s6 slot for an existing profile directory carrying SOUL.md, false otherwise
func RegisterUnregisteredProfileGateway(mgr ServiceManager, profile string) (bool, error) {
	profileDir, err := profileDirForGatewayService(S6ServicePrefix + profile)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(profileDir, "SOUL.md")); err != nil {
		return false, nil
	}
	return true, mgr.RegisterProfileGateway(profile, false)
}

// Result of one s6 binary run with its decoded output
type s6Result struct {
	ReturnCode     int
	Stdout, Stderr string
}

// Error of an s6 binary that exited non zero under check
type s6CalledProcessError struct{ s6Result }

func (e *s6CalledProcessError) Error() string {
	return fmt.Sprintf("Command returned non-zero exit status %d.", e.ReturnCode)
}

// Runs an s6 binary by absolute path with captured replacement decoded output
func runS6(timeout time.Duration, check bool, cmd string, args ...string) (s6Result, error) {
	c := exec.Command(s6BinDir+"/"+cmd, args...)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Start(); err != nil {
		return s6Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		c.Process.Kill()
		<-done
		return s6Result{}, fmt.Errorf("Command '%s' timed out after %s seconds", s6BinDir+"/"+cmd, pyrepr.Float(timeout.Seconds()))
	}
	res := s6Result{Stdout: strings.ToValidUTF8(out.String(), "�"), Stderr: strings.ToValidUTF8(errOut.String(), "�")}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ReturnCode = exitErr.ExitCode()
	} else if err != nil {
		return res, err
	}
	if check && res.ReturnCode != 0 {
		return res, &s6CalledProcessError{res}
	}
	return res, nil
}

// Hands a path to the icebath service account, ignoring a permission refusal
func chownIcebath(path string) error {
	if err := os.Lchown(path, icebathUID, icebathGID); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return nil
}

// Pre creates account owned supervise and event entries so s6-supervise inherits their ownership
func seedSuperviseSkeleton(svcDir string) error {
	mkdirOwned := func(path string, mode os.FileMode) error {
		if _, err := os.Lstat(path); err == nil {
			return nil
		}
		if err := os.Mkdir(path, 0o777); err != nil {
			return err
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		return chownIcebath(path)
	}
	seed := func(root string) error {
		if err := mkdirOwned(filepath.Join(root, "event"), 0o730|os.ModeSetgid|os.ModeSticky); err != nil {
			return err
		}
		supervise := filepath.Join(root, "supervise")
		if err := mkdirOwned(supervise, 0o755); err != nil {
			return err
		}
		if err := mkdirOwned(filepath.Join(supervise, "event"), 0o730|os.ModeSetgid|os.ModeSticky); err != nil {
			return err
		}
		control := filepath.Join(supervise, "control")
		if _, err := os.Stat(control); err != nil {
			if err := mkfifo(control, 0o660); err != nil {
				return err
			}
			if err := os.Chmod(control, 0o660); err != nil {
				return err
			}
			return chownIcebath(control)
		}
		return nil
	}
	if err := seed(svcDir); err != nil {
		return err
	}
	if logDir := filepath.Join(svcDir, "log"); isDirPath(logDir) {
		return seed(logDir)
	}
	return nil
}

// Base error of the s6 lifecycle carrying the slot name
type S6Error struct {
	Msg     string
	Service string
}

func (e *S6Error) Error() string { return e.Msg }

// Lifecycle call against a missing s6 slot
type GatewayNotRegisteredError struct {
	S6Error
	Profile string
}

func newGatewayNotRegisteredError(profile string) *GatewayNotRegisteredError {
	return &GatewayNotRegisteredError{S6Error{
		Msg: "no such gateway " + pyrepr.StrRepr(profile) + ": register it with `icebath profile create " + profile +
			"` first, or pass an existing profile name via `-p <name>`",
		Service: "gateway-" + profile}, profile}
}

// s6 command failure other than a missing slot, with its stderr
type S6CommandError struct {
	S6Error
	Action     string
	ReturnCode int
	Stderr     string
}

func newS6CommandError(service, action string, rc int, stderr string) *S6CommandError {
	msg := fmt.Sprintf("s6-svc %s on %s failed (rc=%d)", action, pyrepr.StrRepr(service), rc)
	if s := strings.TrimSpace(stderr); s != "" {
		msg += ": " + s
	}
	return &S6CommandError{S6Error{msg, service}, action, rc, stderr}
}

// Per profile gateway supervision of runtime registered s6 services
type S6ServiceManager struct{ Scandir string }

func NewS6ServiceManager(scandir string) *S6ServiceManager {
	return &S6ServiceManager{Scandir: scandir}
}

func (m *S6ServiceManager) Kind() string                      { return "s6" }
func (m *S6ServiceManager) SupportsRuntimeRegistration() bool { return true }

func (m *S6ServiceManager) serviceDir(profile string) (string, error) {
	if err := ValidateServiceProfileName(profile); err != nil {
		return "", err
	}
	return filepath.Join(m.Scandir, S6ServicePrefix+profile), nil
}

// Run script of a profile gateway service, dropping to the service account after the env is set
func renderS6RunScript(profile string, extraEnv map[string]string) string {
	lines := []string{"#!/command/with-contenv sh", "# shellcheck shell=sh", "set -e", "export HOME=/opt/data", "cd /opt/data"}
	keys := make([]string, 0, len(extraEnv))
	for k := range extraEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, "export "+k+"="+pyshlex.Quote(extraEnv[k]))
	}
	lines = append(lines, "export ICEBATH_S6_SUPERVISED_CHILD=1", "export ICEBATH_SUPERVISED_CHILD=1")
	cmd := "icebath gateway run --replace"
	if profile != "default" {
		cmd = "icebath -p " + pyshlex.Quote(profile) + " gateway run --replace"
	}
	lines = append(lines, `[ "$(id -u)" = 0 ] || exec `+cmd, "exec s6-setuidgid icebath "+cmd)
	return strings.Join(lines, "\n") + "\n"
}

// Finish script stopping restarts after a fatal config exit or a clean stop
func renderS6FinishScript() string {
	code := strconv.Itoa(gateway.GatewayFatalConfigExitCode)
	return "#!/command/with-contenv sh\n" +
		"# shellcheck shell=sh\n" +
		"# $1 = exit code from the run script.\n" +
		"# Exit " + code + " (EX_CONFIG) = fatal config error — don't restart.\n" +
		"# Exit 0 (clean stop) = intentional stop — don't restart.\n" +
		`if [ "$1" = "` + code + `" ]; then` + "\n" +
		"  exit 125\n" +
		"fi\n" +
		`if [ "$1" = "0" ]; then` + "\n" +
		"  exit 125\n" +
		"fi\n" +
		"exit 0\n"
}

// Log run script writing timestamped rotated logs under the profile gateway log directory
func renderS6LogRun(profile string) string {
	prof := pyshlex.Quote(profile)
	return "#!/command/with-contenv sh\n" +
		"# shellcheck shell=sh\n" +
		`: "${ICEBATH_HOME:=/opt/data}"` + "\n" +
		`log_dir="$ICEBATH_HOME/logs/gateways/` + prof + `"` + "\n" +
		`if [ "$(id -u)" = 0 ]; then` + "\n" +
		`  s6-setuidgid icebath mkdir -p "$log_dir"` + "\n" +
		`  s6-setuidgid icebath rm -f "$log_dir/lock"` + "\n" +
		"else\n" +
		`  mkdir -p "$log_dir"` + "\n" +
		`  rm -f "$log_dir/lock"` + "\n" +
		"fi\n" +
		`[ "$(id -u)" = 0 ] || exec s6-log 1 n10 s1000000 T "$log_dir"` + "\n" +
		`exec s6-setuidgid icebath s6-log 1 n10 s1000000 T "$log_dir"` + "\n"
}

// s6-svc action on a slot, a missing slot and any other failure as their typed errors
func (m *S6ServiceManager) runSvc(flag, label, name string) error {
	dir := filepath.Join(m.Scandir, name)
	if !isDirPath(dir) {
		return newGatewayNotRegisteredError(profileFromService(name))
	}
	if _, err := s6Run(5*time.Second, true, "s6-svc", flag, dir); err != nil {
		var cpe *s6CalledProcessError
		if errors.As(err, &cpe) {
			return newS6CommandError(name, label, cpe.ReturnCode, cpe.Stderr)
		}
		return err
	}
	return nil
}

func (m *S6ServiceManager) Start(name string) error {
	if err := m.runSvc("-u", "start", name); err != nil {
		return err
	}
	return writeGatewayDesiredState(name, "running")
}

// s6-svstat field values of the slot, nil on a non zero exit or a count mismatch
func (m *S6ServiceManager) svstatFields(name string, fields ...string) ([]string, error) {
	res, err := s6Run(5*time.Second, false, "s6-svstat", "-o", strings.Join(fields, ","), filepath.Join(m.Scandir, name))
	if err != nil {
		return nil, err
	}
	if res.ReturnCode != 0 {
		return nil, nil
	}
	values := strings.Fields(res.Stdout)
	if len(values) != len(fields) {
		return nil, nil
	}
	return values, nil
}

// Supervised gateway PID, zero when down or on any failure
func (m *S6ServiceManager) supervisedPID(name string) int {
	values, err := m.svstatFields(name, "pid")
	if err != nil || len(values) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (m *S6ServiceManager) Stop(name string) error {
	if pid := m.supervisedPID(name); pid != 0 {
		writePlannedStop(pid)
	}
	if err := m.runSvc("-d", "stop", name); err != nil {
		return err
	}
	return writeGatewayDesiredState(name, "stopped")
}

func (m *S6ServiceManager) Restart(name string) error {
	if err := m.runSvc("-t", "restart", name); err != nil {
		return err
	}
	return writeGatewayDesiredState(name, "running")
}

func (m *S6ServiceManager) IsRunning(name string) (bool, error) {
	values, err := m.svstatFields(name, "up")
	return len(values) == 1 && values[0] == "true", err
}

func (m *S6ServiceManager) RegisterProfileGateway(profile string, startNow bool) error {
	return m.RegisterProfileGatewayEnv(profile, nil, startNow)
}

// Creates the s6 service directory atomically and rescans so the slot is supervised at once
func (m *S6ServiceManager) RegisterProfileGatewayEnv(profile string, extraEnv map[string]string, startNow bool) error {
	svcDir, err := m.serviceDir(profile)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(svcDir); err == nil {
		return &profiles.ValueError{Msg: "profile gateway " + pyrepr.StrRepr(profile) + " already registered at " + svcDir}
	}
	tmpDir := filepath.Join(filepath.Dir(svcDir), "."+filepath.Base(svcDir)+".tmp")
	if _, err := os.Lstat(tmpDir); err == nil {
		os.RemoveAll(tmpDir)
	}
	if err := os.MkdirAll(tmpDir, 0o777); err != nil {
		return err
	}
	writeScript := func(path, text string) error {
		if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
			return err
		}
		return os.Chmod(path, 0o755)
	}
	build := func() error {
		if err := os.WriteFile(filepath.Join(tmpDir, "type"), []byte("longrun\n"), 0o666); err != nil {
			return err
		}
		if err := writeScript(filepath.Join(tmpDir, "run"), renderS6RunScript(profile, extraEnv)); err != nil {
			return err
		}
		if err := writeScript(filepath.Join(tmpDir, "finish"), renderS6FinishScript()); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(tmpDir, "log"), 0o777); err != nil {
			return err
		}
		if err := writeScript(filepath.Join(tmpDir, "log", "run"), renderS6LogRun(profile)); err != nil {
			return err
		}
		if err := seedSuperviseSkeleton(tmpDir); err != nil {
			return err
		}
		if !startNow {
			f, err := os.OpenFile(filepath.Join(tmpDir, "down"), os.O_CREATE|os.O_WRONLY, 0o666)
			if err != nil {
				return err
			}
			f.Close()
		}
		return os.Rename(tmpDir, svcDir)
	}
	if err := build(); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	res, err := s6Run(5*time.Second, false, "s6-svscanctl", "-a", m.Scandir)
	if err != nil {
		return err
	}
	if res.ReturnCode != 0 {
		os.RemoveAll(svcDir)
		detail := res.Stderr
		if detail == "" {
			detail = res.Stdout
		}
		return errors.New("s6-svscanctl failed: " + detail)
	}
	return nil
}

// Stops the slot, waits for it to go down, rescans and removes its directory, absent slots a no op
func (m *S6ServiceManager) UnregisterProfileGateway(profile string) error {
	svcDir, err := m.serviceDir(profile)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(svcDir); err != nil {
		return nil
	}
	if _, err := s6Run(5*time.Second, false, "s6-svc", "-d", svcDir); err != nil {
		return err
	}
	if _, err := s6Run(15*time.Second, false, "s6-svwait", "-D", "-t", "10000", svcDir); err != nil {
		return err
	}
	if _, err := s6Run(5*time.Second, false, "s6-svscanctl", "-an", m.Scandir); err != nil {
		return err
	}
	serviceSleep(200 * time.Millisecond)
	os.RemoveAll(svcDir)
	return nil
}

// Profile names of every registered gateway service
func (m *S6ServiceManager) ListProfileGateways() ([]string, error) {
	if _, err := os.Stat(m.Scandir); err != nil {
		return []string{}, nil
	}
	entries, err := os.ReadDir(m.Scandir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") && isDirPath(filepath.Join(m.Scandir, name)) && strings.HasPrefix(name, S6ServicePrefix) {
			out = append(out, name[len(S6ServicePrefix):])
		}
	}
	return out, nil
}
