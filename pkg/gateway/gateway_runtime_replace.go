package gateway

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Graceful drain before the guarded force kill, and the wait after it
const (
	replaceGracefulStopSeconds = 10.0
	replaceForcedStopSeconds   = 5.0
)

// One line remedy of each refusal to replace an older gateway
var ReplaceRefusalDetail = map[string]string{
	"owner_newer":        "a newer Icebath's gateway owns this profile; update Icebath or run: icebath gateway stop",
	"foreign_install":    "another Icebath install's gateway owns this profile; stop it there: icebath gateway stop",
	"owner_unverified":   "an older Icebath gateway owns this profile and was not verified; run: icebath gateway stop",
	"owner_did_not_exit": "an older Icebath gateway owns this profile and did not stop; run: icebath gateway stop",
	"owner_unreachable":  "an older Icebath gateway owns this profile; run: icebath gateway stop",
}

// Resolved root of this install
func replaceInstallRoot() string {
	root, err := pypath.Resolve(home.Root())
	if err != nil {
		return home.Root()
	}
	return root
}

// Control home and identify payload of the process answering for home, its own socket then the multiplexer's
func replaceIdentify(h string, timeout time.Duration) (string, *pyjson.Object, error) {
	for _, control := range []string{h, MultiplexerRootFor(h)} {
		if control == "" {
			continue
		}
		payload, err := queryIdentifySeam(control, timeout)
		if err == nil {
			return control, payload, nil
		}
		if discoveryErrorKind(err) == "missing" {
			continue
		}
		return "", nil, err
	}
	return "", nil, nil
}

// Whether a reported home resolves to the same case normalised path as b
func replaceSame(a any, b string) bool {
	text, ok := a.(string)
	if !ok {
		return false
	}
	return sameResolvedPath(text, b)
}

// Whether home is this install's root or one of its profiles
func replaceWithinInstall(h, root string) bool {
	resolved, err := pypath.Resolve(h)
	if err != nil {
		return false
	}
	return resolved == root || filepath.Dir(resolved) == filepath.Join(root, "profiles")
}

// Live pid and start time of a verified older owner, or the refusal code naming why it is not one
func replaceVerifiedOwner(control string, payload *pyjson.Object) (int, any, string) {
	protocol, _ := payload.Get("runtime_protocol")
	if n, ok := protocol.(int64); ok && n > 1 {
		return 0, nil, "owner_newer"
	}
	if n, ok := protocol.(*big.Int); ok && n.Cmp(big.NewInt(1)) > 0 {
		return 0, nil, "owner_newer"
	}
	reported, _ := payload.Get("hermes_home")
	if !replaceWithinInstall(control, replaceInstallRootOf()) || !replaceSame(reported, control) {
		return 0, nil, "foreign_install"
	}
	kind, _ := payload.Get("kind")
	rawPid, _ := payload.Get("pid")
	recorded, _ := payload.Get("start_time")
	pid64, isInt := rawPid.(int64)
	if kind != gatewayKind || !isInt || pid64 <= 1 || int(pid64) == os.Getpid() || int(pid64) == os.Getppid() || recorded == nil || !statusPidExists(int(pid64)) {
		return 0, nil, "owner_unverified"
	}
	pid := int(pid64)
	current := GetProcessStartTime(pid)
	if current == nil {
		return 0, nil, "owner_unverified"
	}
	if match, err := StartTimeFingerprintsMatch(recorded, current, StartTimeDriftTolerance); err != nil || !match {
		return 0, nil, "owner_unverified"
	}
	return pid, current, ""
}

// Planned stop marker in the owner's own home so it exits as a planned stop
func replaceWritePlannedStop(control string, pid int, startTime any) {
	record := pyjson.NewObject()
	record.Set("target_pid", int64(pid))
	record.Set("target_start_time", startTime)
	record.Set("stopper_pid", int64(os.Getpid()))
	record.Set("written_at", utcNowISO())
	writeMarker(filepath.Join(control, plannedStopMarkerFilename), record)
}

// Whether pid is gone within seconds, polling every tenth of a second
func replaceWaitGone(pid int, seconds float64) bool {
	end := pythread.Monotonic() + max(seconds, 0)
	for statusPidExists(pid) {
		if pythread.Monotonic() >= end {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// Swappable replacement of an incompatible owner, the seam ensure tests replace
var (
	replaceIncompatibleOwner = ReplaceIncompatibleOwner
	replaceInstallRootOf     = replaceInstallRoot
	replaceIdentifyOf        = replaceIdentify
)

// Stops the older gateway answering for home like gateway stop, empty once gone, else a refusal code
func ReplaceIncompatibleOwner(h string, deadline float64) string {
	budget := deadline - pythread.Monotonic()
	if budget <= 0 {
		return "owner_unreachable"
	}
	control, payload, err := replaceIdentifyOf(h, time.Duration(min(2.0, budget)*float64(time.Second)))
	if err != nil {
		return "owner_unreachable"
	}
	if payload == nil {
		return ""
	}
	pid, startTime, refusal := replaceVerifiedOwner(control, payload)
	if refusal != "" {
		return refusal
	}
	replaceWritePlannedStop(control, pid, startTime)
	graceful := min(replaceGracefulStopSeconds, max(deadline-pythread.Monotonic()-replaceForcedStopSeconds, 1.0))
	err = nil
	if !nativeWindows {
		err = TerminatePid(pid, false, startTime)
	}
	if err == nil {
		if replaceWaitGone(pid, graceful) {
			return ""
		}
		err = TerminatePid(pid, true, startTime)
	}
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return ""
		}
		if !replaceWaitGone(pid, 0) {
			return "owner_did_not_exit"
		}
		return ""
	}
	if replaceWaitGone(pid, replaceForcedStopSeconds) {
		return ""
	}
	return "owner_did_not_exit"
}
