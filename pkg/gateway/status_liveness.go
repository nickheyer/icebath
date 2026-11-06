package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Respawn storm check result, start count in the window and the backoff to sleep
type StormInfo struct {
	Count    int
	WindowS  float64
	BackoffS float64
}

// Records this start in gateway-starts.log, a storm reported past maxStarts in the window, bookkeeping failures swallowed
func RecordStartAndCheckStorm(h string, maxStarts int, windowS, backoffCapS float64) *StormInfo {
	info, err := recordStartAndCheckStorm(h, maxStarts, windowS, backoffCapS)
	if err != nil {
		slog.Debug(fmt.Sprintf("respawn-storm breaker bookkeeping failed (non-fatal): %v", err))
		return nil
	}
	return info
}

func recordStartAndCheckStorm(h string, maxStarts int, windowS, backoffCapS float64) (*StormInfo, error) {
	path := filepath.Join(h, "gateway-starts.log")
	if _, err := home.MakeUserDir(filepath.Dir(path), 0o777, true, true); err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixMicro()) / 1e6
	var existing []float64
	if data, err := os.ReadFile(path); err == nil {
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(data) {
			return nil, errors.New("UnicodeDecodeError: 'utf-8' codec can't decode gateway-starts.log")
		}
		for _, line := range pystr.Splitlines(string(data), false) {
			line = pystr.Strip(line)
			if line == "" {
				continue
			}
			if f, err := pyint.ParseFloat(line); err == nil {
				existing = append(existing, f)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	existing = append(existing, now)
	recent := 0
	for _, ts := range existing {
		if now-ts <= windowS {
			recent++
		}
	}
	keep := max(maxStarts*4, 40)
	toWrite := existing
	if len(toWrite) > keep {
		toWrite = toWrite[len(toWrite)-keep:]
	}
	lines := make([]string, len(toWrite))
	for i, ts := range toWrite {
		lines[i] = pyrepr.Repr(ts)
	}
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o666); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	if recent <= maxStarts {
		return nil, nil
	}
	backoff := min(backoffCapS, 5.0*float64(int(1)<<min(recent-maxStarts, 6)))
	return &StormInfo{Count: recent, WindowS: windowS, BackoffS: backoff}, nil
}

// Resolved gateway liveness for one surface, Source naming the ladder rung that answered
type GatewayLiveness struct {
	Running    bool
	PID        any
	Source     string
	HealthBody *pyjson.Object
	ProbeError bool
	Runtime    *pyjson.Object
}

// Profile id of any home, the profiles child name, default for the default root, false otherwise
func ProfileNameForHome(profileHome string) (string, bool) {
	if named, ok := profileNameForHomeDir(profileHome); ok && named != "" {
		return named, true
	}
	resolved, err := pypath.Realpath(profileHome, pypath.Lax)
	if err != nil {
		return "", false
	}
	root, err := pypath.Realpath(home.Root(), pypath.Lax)
	if err != nil {
		return "", false
	}
	if resolved == root {
		return "default", true
	}
	return "", false
}

// Multiplexer rung of the liveness ladder, swapped by tests that pin the roster answer
var statusMultiplexerLivenessForProfile = func(profileDir string) (int, *pyjson.Object, bool, error) {
	return MultiplexerLivenessForProfile(profileDir)
}

// Pid and host gateway_state.json when the live host multiplexer serves the profile at profileDir
func MultiplexerLivenessForProfile(profileDir string) (int, *pyjson.Object, bool, error) {
	name, ok := ProfileNameForHome(profileDir)
	if !ok || name == "" {
		return 0, nil, false, nil
	}
	defaultRoot := home.Root()
	if name != "default" {
		same, err := sameHermesHome(profileDir, pypath.PathJoin(defaultRoot, "profiles", name))
		if err != nil {
			return 0, nil, false, err
		}
		if !same {
			return 0, nil, false, nil
		}
	}
	topology, err := hostGatewayTopology()
	if err != nil {
		return 0, nil, false, err
	}
	launchHome := defaultRoot
	var pid int
	var havePid bool
	switch {
	case topology != nil && topology.Serves(name):
		if topology.PID != nil {
			pid, havePid = *topology.PID, true
		}
		if topology.Home != "" {
			launchHome = topology.Home
		}
	case name != "default":
		served, err := namedProfileServed(name)
		if err != nil || !served {
			return 0, nil, false, err
		}
		if pid, havePid, err = LiveDefaultGatewayPid(); err != nil {
			return 0, nil, false, err
		}
	default:
		return 0, nil, false, nil
	}
	if !havePid {
		return 0, nil, false, nil
	}
	runtime := ReadRuntimeStatus(pypath.PathJoin(launchHome, runtimeStatusFile))
	if runtime == nil || runtime.Len() == 0 {
		runtime = pyjson.NewObject()
	}
	return pid, runtime, true, nil
}

// Answers whether a live default multiplexer serves a named profile, installed by pkg/gatewaycli
var NamedProfileServedByRunningMultiplexer func(profileName string) bool

// Hook result, an error while pkg/gatewaycli has not installed the hook
func namedProfileServed(name string) (bool, error) {
	if NamedProfileServedByRunningMultiplexer == nil {
		return false, errors.New("gateway: pkg/gatewaycli must install NamedProfileServedByRunningMultiplexer (hermes_cli/gateway.py named_profile_served_by_running_multiplexer)")
	}
	return NamedProfileServedByRunningMultiplexer(name), nil
}

// The api_server and webhook mirrors a served profile gets from the default listener
func SharedListenerMirrorPlatformsFor(runtime *pyjson.Object, profile string) *pyjson.Object {
	out := pyjson.NewObject()
	plats, ok := recGet(runtime, "platforms").(*pyjson.Object)
	if profile == "" || profile == "default" || !ok {
		return out
	}
	names := make([]string, 0, len(SharedListenerMirrorPlatforms))
	for name := range SharedListenerMirrorPlatforms {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, ok := recGet(plats, name).(*pyjson.Object)
		state := recGet(entry, "state")
		if !ok || !(pyEq(state, "connected") || pyEq(state, "connecting") || pyEq(state, "retrying")) {
			continue
		}
		var url any
		if base, ok := recGet(entry, "listener_base").(string); ok && base != "" {
			url = base + "/p/" + profile + SharedListenerMirrorPaths[name]
		}
		mirrored := pyjson.NewObject()
		for _, k := range entry.Keys {
			if k != "listener_base" {
				mirrored.Set(k, recGet(entry, k))
			}
		}
		mirrored.Set("ingress_url", url)
		mirrored.Set("mirrored_from", "default")
		out.Set(name, mirrored)
	}
	return out
}

// A multiplexer record's entries for profile re-keyed to bare platform names plus its listener mirrors
func ProfilePlatformsFromMultiplexer(runtime *pyjson.Object, profile string) *pyjson.Object {
	plats, ok := recGet(runtime, "platforms").(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	prefix := profile + ":"
	own := pyjson.NewObject()
	for _, k := range plats.Keys {
		if v, isDict := recGet(plats, k).(*pyjson.Object); isDict && strings.HasPrefix(k, prefix) {
			own.Set(k[len(prefix):], v)
		}
	}
	if profile == "default" {
		for _, k := range plats.Keys {
			if v, isDict := recGet(plats, k).(*pyjson.Object); isDict && !strings.Contains(k, ":") {
				own.Set(k, v)
			}
		}
	}
	out := SharedListenerMirrorPlatformsFor(runtime, profile)
	for _, k := range own.Keys {
		out.Set(k, recGet(own, k))
	}
	return out
}

// Ladder options, ProfileDir scoping every read and the probes replacing the default rungs
type LivenessOptions struct {
	ProfileDir      string
	Runtime         *pyjson.Object
	RuntimeGiven    bool
	HealthProbe     func() (bool, *pyjson.Object, error)
	NoCache         bool
	PidProbe        func(pidPath string) (int, bool, error)
	RuntimeReader   func(path string) (*pyjson.Object, error)
	RuntimePidProbe func(runtime *pyjson.Object, expectedHome string) (int, bool, error)
}

// One answer to whether the gateway is up, a failing rung degrading to the next with ProbeError set
func ResolveGatewayLiveness(o LivenessOptions) GatewayLiveness {
	pidProbe := o.PidProbe
	if pidProbe == nil {
		pidProbe = func(p string) (int, bool, error) {
			if o.NoCache {
				return GetRunningPid(p, true)
			}
			return GetRunningPidCached(p, true, gatewayRunningPidCacheTTL)
		}
	}
	reader := o.RuntimeReader
	if reader == nil {
		reader = func(p string) (*pyjson.Object, error) { return ReadRuntimeStatus(p), nil }
	}
	runtimePidProbe := o.RuntimePidProbe
	if runtimePidProbe == nil {
		runtimePidProbe = GetRuntimeStatusRunningPid
	}
	probeError := false
	scoped := o.ProfileDir != ""
	pidArg := ""
	if scoped {
		pidArg = pypath.PathJoin(o.ProfileDir, "gateway.pid")
	}
	if pid, ok, err := pidProbe(pidArg); err != nil {
		probeError = true
	} else if ok {
		return GatewayLiveness{Running: true, PID: pid, Source: "pid"}
	}
	var healthBody *pyjson.Object
	if o.HealthProbe != nil {
		alive, body, err := o.HealthProbe()
		if err != nil {
			probeError = true
			alive, body = false, nil
		}
		healthBody = body
		if alive {
			return GatewayLiveness{Running: true, PID: recGet(healthBody, "pid"), Source: "health", HealthBody: healthBody}
		}
	}
	runtime := o.Runtime
	if !o.RuntimeGiven {
		statusArg := ""
		if scoped {
			statusArg = pypath.PathJoin(o.ProfileDir, runtimeStatusFile)
		}
		r, err := reader(statusArg)
		if err != nil {
			probeError = true
		}
		runtime = r
	}
	if scoped && runtime == nil {
		runtime = pyjson.NewObject()
	}
	if pid, ok, err := runtimePidProbe(runtime, o.ProfileDir); err != nil {
		probeError = true
	} else if ok {
		return GatewayLiveness{Running: true, PID: pid, Source: "runtime_status", HealthBody: healthBody}
	}
	ownHome := o.ProfileDir
	if !scoped {
		ownHome = processHermesHome()
	}
	if pid, muxRuntime, ok, err := statusMultiplexerLivenessForProfile(ownHome); err != nil {
		probeError = true
	} else if ok {
		return GatewayLiveness{Running: true, PID: pid, Source: "multiplexer", HealthBody: healthBody, Runtime: muxRuntime}
	}
	return GatewayLiveness{Source: "none", HealthBody: healthBody, ProbeError: probeError}
}
