package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// RuntimeStartError is a start the service manager refused or failed, with the verdict state it reports
type RuntimeStartError struct {
	Reason string
	State  string
}

func (e *RuntimeStartError) Error() string { return e.Reason }

func runtimeStartError(reason string) *RuntimeStartError {
	return &RuntimeStartError{Reason: reason, State: "inaccessible"}
}

// ErrRuntimeDeadline is the ensure deadline passing, an OS level timeout like Python's TimeoutError
var ErrRuntimeDeadline = errors.New("ensure deadline passed")

// Seconds left before a monotonic deadline, the deadline error once none remain
func runtimeRemaining(deadline float64) (float64, error) {
	budget := deadline - pythread.Monotonic()
	if budget <= 0 {
		return 0, ErrRuntimeDeadline
	}
	return budget, nil
}

// Whether err is an OS level failure, the ensure deadline included
func runtimeOSError(err error) bool {
	return errors.Is(err, ErrRuntimeDeadline) || pypath.IsOSError(err)
}

// Whether err is a subprocess timeout
func subprocessTimedOut(err error) bool {
	var expired *pysubprocess.TimeoutExpired
	return errors.As(err, &expired)
}

// Host service suffix of the requested home, empty for the native default and a path hash for a foreign root
func ServiceSuffix(ctx context.Context, h string) (string, error) {
	def, err := pypath.Resolve(home.AccountDefault(ctx))
	if err != nil {
		return "", err
	}
	resolved, err := pypath.Resolve(h)
	if err != nil {
		return "", err
	}
	if resolved == def {
		return "", nil
	}
	root := def
	if !pathIsRelativeTo(resolved, def) {
		root = resolved
		if filepath.Base(filepath.Dir(resolved)) == "profiles" {
			root = filepath.Dir(filepath.Dir(resolved))
		}
	}
	if resolved != root {
		if name := profileNameFromHome(resolved, root); name != "" {
			return name, nil
		}
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:8], nil
}

// Whether path equals base or lies under it, by whole path components
func pathIsRelativeTo(path, base string) bool {
	if path == base {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(base, string(filepath.Separator))+string(filepath.Separator))
}

// ExistingService is an installed supervisor definition and the argv that asks it to start
type ExistingService struct {
	Backend   string
	StartArgv []string
	Running   bool
}

// Runs argv with no stdin under the remaining budget, stdout decoded as strict UTF-8 when text is set
func runServiceProbe(ctx context.Context, argv []string, deadline float64, text bool) (*pysubprocess.Completed, string, error) {
	budget, err := runtimeRemaining(deadline)
	if err != nil {
		return nil, "", err
	}
	res, err := serviceProbeRun(ctx, argv, pysubprocess.Options{Timeout: time.Duration(budget * float64(time.Second)), Attr: hiddenServiceProbeAttr()})
	if err != nil {
		return nil, "", err
	}
	if !text {
		return res, "", nil
	}
	if !utf8.Valid(res.Stdout) || !utf8.Valid(res.Stderr) {
		return nil, "", serviceValueError{"utf-8 codec can't decode service manager output"}
	}
	return res, strings.ReplaceAll(strings.ReplaceAll(string(res.Stdout), "\r\n", "\n"), "\r", "\n"), nil
}

type serviceValueError struct{ msg string }

func (e serviceValueError) Error() string { return e.msg }

// Maps a binding verifier failure to its refusal, unknown failures to an unverified identity
func verifyServiceBinding(err error, reasons ...string) error {
	if err == nil {
		return nil
	}
	reason := err.Error()
	for _, allowed := range reasons {
		if reason == allowed {
			return runtimeStartError(reason)
		}
	}
	var identity *ServiceIdentityError
	var plistErr *pyplist.Error
	var value serviceValueError
	if errors.As(err, &identity) || errors.As(err, &plistErr) || errors.As(err, &value) {
		return runtimeStartError("service_identity_unverified")
	}
	return err
}

// Whether a path exists without following a final symlink
func serviceLexists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Swappable host facts of the service probes, the runner, systemctl argv and the account home
var (
	serviceProbeRun     = pysubprocess.RunWith
	runtimeSystemctlCmd = SystemctlCmd
	runtimeAccountHome  = os.UserHomeDir
	runtimeUID          = os.Getuid
)

// S6 supervision of the container, wired by cmd/icebath from gatewaycli
var (
	S6Running   func() bool
	S6Scandir   func() string
	S6IsRunning func(name string) (bool, error)
	S6Start     func(name string) error
)

// Installed systemd unit for home in exactly one scope, refusing masked, failed or unverified units
func systemdExistingService(ctx context.Context, h string, deadline float64) (*ExistingService, error) {
	if S6Running() {
		return nil, runtimeStartError("external_supervisor")
	}
	suffix, err := ServiceSuffix(ctx, h)
	if err != nil {
		return nil, err
	}
	unit := serviceBase
	if suffix != "" {
		unit += "-" + suffix
	}
	unit += ".service"
	accountHome, err := runtimeAccountHome()
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(accountHome, ".config/systemd/user", unit), filepath.Join("/etc/systemd/system", unit)}
	installed := false
	for _, p := range append(append([]string{}, paths...), filepath.Join("/usr/lib/systemd/system", unit), filepath.Join("/lib/systemd/system", unit)) {
		ok, err := serviceLexists(p)
		if err != nil {
			return nil, err
		}
		installed = installed || ok
	}
	if !installed {
		return nil, nil
	}
	var found []ExistingService
	for _, system := range []bool{false, true} {
		command := runtimeSystemctlCmd(system)
		res, stdout, err := runServiceProbe(ctx, append(append([]string{}, command...), "show", unit, "--no-pager", "--all",
			"--property=LoadState,ActiveState,SubState,UnitFileState,"+strings.Join(SystemdIdentityProperties, ",")), deadline, true)
		if err != nil {
			return nil, err
		}
		props := map[string]string{}
		for _, line := range pystr.Splitlines(stdout, false) {
			if key, value, ok := strings.Cut(line, "="); ok {
				props[key] = value
			}
		}
		scoped, err := serviceLexists(paths[map[bool]int{false: 0, true: 1}[system]])
		if err != nil {
			return nil, err
		}
		if !scoped && (res.Returncode != 0 || props["LoadState"] == "not-found") {
			continue
		}
		if res.Returncode != 0 || props["LoadState"] != "loaded" {
			return nil, runtimeStartError("service_manager_unavailable")
		}
		if state := props["UnitFileState"]; state == "masked" || state == "masked-runtime" {
			return nil, runtimeStartError("service_masked")
		}
		state := props["ActiveState"]
		switch state {
		case "deactivating":
			return nil, &RuntimeStartError{Reason: "service_draining", State: "draining"}
		case "inactive", "active", "activating", "reloading":
		case "failed":
			return nil, runtimeStartError("service_failed")
		default:
			return nil, runtimeStartError("service_state_unknown")
		}
		envRes, envOut, err := runServiceProbe(ctx, append(append([]string{}, command...), "show-environment"), deadline, true)
		if err != nil {
			return nil, err
		}
		if envRes.Returncode != 0 {
			return nil, runtimeStartError("service_identity_unverified")
		}
		if err := VerifySystemd(props, envOut, h, system); err != nil {
			var identity *ServiceIdentityError
			if errors.As(err, &identity) && identity.Foreign {
				continue
			}
			if errors.As(err, &identity) {
				if identity.Reason == "profile_mismatch" || identity.Reason == "service_account_mismatch" {
					return nil, runtimeStartError(identity.Reason)
				}
				return nil, runtimeStartError("service_identity_unverified")
			}
			return nil, err
		}
		found = append(found, ExistingService{Backend: "systemd", StartArgv: append(append([]string{}, command...), "--no-ask-password", "start", unit), Running: state != "inactive"})
	}
	if len(found) > 1 {
		return nil, &RuntimeStartError{Reason: "service_scope_conflict", State: "conflict"}
	}
	if len(found) == 1 {
		return &found[0], nil
	}
	return nil, nil
}

// Path of the s6 container environment's home, read by the slot run script
var s6ContainerHomePath = "/run/s6/container_environment/ICEBATH_HOME"

// Registered s6 slot owning home, none for a named profile the root multiplexer serves
func s6ExistingService(ctx context.Context, h string, deadline float64) (*ExistingService, error) {
	raw, err := os.ReadFile(s6ContainerHomePath)
	if err != nil {
		return nil, runtimeStartError("service_identity_unverified")
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, runtimeStartError("service_identity_unverified")
	}
	containerHome := strings.TrimSpace(text)
	if containerHome == "" {
		return nil, runtimeStartError("service_identity_unverified")
	}
	root, err := pypath.Resolve(containerHome)
	if err != nil {
		return nil, err
	}
	resolved, err := pypath.Resolve(h)
	if err != nil {
		return nil, err
	}
	if resolved != root && filepath.Dir(resolved) != filepath.Join(root, "profiles") {
		return nil, runtimeStartError("profile_mismatch")
	}
	if resolved != root {
		host, err := ImpliedHostRoot(ctx, resolved)
		if err != nil {
			return nil, err
		}
		if host == root {
			return nil, nil
		}
	}
	profile := "default"
	if resolved != root {
		profile = filepath.Base(resolved)
	}
	name := "gateway-" + profile
	if info, err := os.Stat(filepath.Join(S6Scandir(), name)); err != nil || !info.IsDir() {
		return nil, runtimeStartError("service_manager_unavailable")
	}
	running, err := S6IsRunning(name)
	if err != nil {
		return nil, err
	}
	return &ExistingService{Backend: "s6", StartArgv: []string{name}, Running: running}, nil
}

// Loaded or installed launchd job for home in one domain, bootstrapping only an existing plist
func launchdExistingService(ctx context.Context, h string, deadline float64) (*ExistingService, error) {
	suffix, err := ServiceSuffix(ctx, h)
	if err != nil {
		return nil, err
	}
	label := "ai.icebath.gateway"
	if suffix != "" {
		label += "-" + suffix
	}
	uid := runtimeUID()
	account, err := pypwd.Getpwuid(uid)
	if err != nil {
		return nil, err
	}
	plist := filepath.Join(account.Dir, "Library/LaunchAgents", label+".plist")
	installed, err := serviceLexists(plist)
	if err != nil {
		return nil, err
	}
	domains := []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)}
	var found []ExistingService
	var loaded []string
	for _, domain := range domains {
		res, stdout, err := runServiceProbe(ctx, []string{"launchctl", "print", domain + "/" + label}, deadline, true)
		if err != nil {
			return nil, err
		}
		if res.Returncode == 0 {
			loaded = append(loaded, stdout)
			found = append(found, ExistingService{Backend: "launchd", StartArgv: []string{"launchctl", "kickstart", domain + "/" + label}, Running: strings.Contains(stdout, "state = running")})
			continue
		}
		if res.Returncode != 113 {
			return nil, runtimeStartError("service_manager_unavailable")
		}
	}
	if len(found) > 1 {
		return nil, &RuntimeStartError{Reason: "service_scope_conflict", State: "conflict"}
	}
	if len(found) == 1 {
		if err := verifyServiceBinding(VerifyLaunchdLoaded(loaded[0], h, uid, account.Name), "profile_mismatch", "service_account_mismatch", "service_disabled"); err != nil {
			return nil, err
		}
		return &found[0], nil
	}
	if installed {
		raw, err := ReadDefinition(plist)
		if err != nil {
			return nil, err
		}
		decoded, err := pyplist.Load(raw)
		if err != nil {
			return nil, verifyServiceBinding(err)
		}
		definition, ok := decoded.(map[string]any)
		if !ok {
			return nil, runtimeStartError("service_identity_unverified")
		}
		if err := verifyServiceBinding(VerifyLaunchdPlist(definition, label, h), "profile_mismatch", "service_account_mismatch", "service_disabled"); err != nil {
			return nil, err
		}
		return &ExistingService{Backend: "launchd", StartArgv: []string{"launchctl", "bootstrap", domains[0], plist}}, nil
	}
	return nil, nil
}

// Android has no service manager, an unmanaged detached start is its only owner
func noSupervisorService(context.Context, string, float64) (*ExistingService, error) { return nil, nil }

type serviceBackend func(ctx context.Context, h string, deadline float64) (*ExistingService, error)

// Service probe of each supported host platform
var serviceBackends = map[string]serviceBackend{
	"linux":   systemdExistingService,
	"darwin":  launchdExistingService,
	"android": noSupervisorService,
}

// Swappable discovery of an installed service, the seam ensure tests replace
var discoverExistingGatewayService = DiscoverExistingGatewayService

// Installed service owning home on this host, a start error for an unsupported or failing manager
func DiscoverExistingGatewayService(ctx context.Context, h string, deadline float64) (*ExistingService, error) {
	backend := serviceBackends[runtime.GOOS]
	if S6Running() {
		backend = s6ExistingService
	}
	if backend == nil {
		return nil, runtimeStartError("unsupported_supervisor_platform")
	}
	service, err := backend(ctx, h, deadline)
	if err == nil {
		return service, nil
	}
	var start *RuntimeStartError
	var value serviceValueError
	var identity *ServiceIdentityError
	switch {
	case errors.As(err, &start):
		return nil, err
	case subprocessTimedOut(err):
		return nil, ErrRuntimeDeadline
	case runtimeOSError(err) || errors.As(err, &value) || errors.As(err, &identity):
		return nil, runtimeStartError("service_manager_unavailable")
	}
	return nil, err
}

// Swappable start request of an installed service, the seam ensure tests replace
var startExistingGatewayService = StartExistingGatewayService

// Requests one start of a stopped service, never treating the request as readiness
func StartExistingGatewayService(ctx context.Context, service *ExistingService, deadline float64) error {
	if service.Running {
		return nil
	}
	if service.Backend == "s6" {
		if err := S6Start(service.StartArgv[0]); err != nil {
			if subprocessTimedOut(err) {
				return ErrRuntimeDeadline
			}
			return runtimeStartError("service_start_failed")
		}
		return nil
	}
	res, _, err := runServiceProbe(ctx, service.StartArgv, deadline, false)
	if err != nil {
		if subprocessTimedOut(err) {
			return ErrRuntimeDeadline
		}
		if runtimeOSError(err) {
			return runtimeStartError("service_start_failed")
		}
		return err
	}
	if res.Returncode != 0 {
		return runtimeStartError("service_start_failed")
	}
	return nil
}
