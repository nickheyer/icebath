package gateway

import (
	"context"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// The desktop managed SSH update observes a Windows remote through the runtime helper, the install having no Python
func init() {
	sshOperations["update-observation"] = sshOperation{2, func(_ context.Context, a []string) (any, error) {
		return sshUpdateObservation(a[0], a[1]), nil
	}}
}

var digitsOnlyRE = regexp.MustCompile(`^[0-9]+$`)

// Python int of a decoded JSON value, false for anything else bool included
func observedInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case *big.Int:
		if n.IsInt64() {
			return n.Int64(), true
		}
		return 0, false
	}
	return 0, false
}

func observedDict(text string) (*pyjson.Object, error) {
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	o, _ := v.(*pyjson.Object)
	return o, nil
}

func observedGet(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Marker, launch intent, terminal code, receipt and coordinator readiness of one managed update correlation
func sshUpdateObservation(rawHome, correlation string) *pyjson.Object {
	home := pypath.Expanduser(rawHome)
	parent := filepath.Base(filepath.Dir(home))
	isProfileHome := strings.EqualFold(parent, "profiles")
	installRoot := home
	if isProfileHome {
		installRoot = filepath.Dir(filepath.Dir(home))
	}
	markerPath := filepath.Join(installRoot, ".icebath-update-in-progress")
	statusPath := filepath.Join(home, ".update_exit_code."+correlation)
	readyPath := filepath.Join(home, ".update_coordinator_ready."+correlation)
	intentPath := filepath.Join(home, ".update_launch_intent."+correlation)

	markerState, markerPID := "absent", any(nil)
	if raw, err := os.ReadFile(markerPath); err == nil {
		verdict, owner, _ := updaterecord.JudgeMarker(raw, nil)
		switch verdict {
		case "malformed":
			markerState = "malformed"
		case "dead":
			markerState = "dead"
		default:
			markerState = "live"
			if owner != nil {
				markerPID = int64(*owner)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		markerState = "unavailable"
	}

	var exitCode any
	if raw, err := os.ReadFile(statusPath); err == nil {
		if !digitsOnlyRE.Match(raw) {
			exitCode = "malformed"
		} else {
			exitCode = pyjson.ParseInt(string(raw))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		exitCode = "unavailable"
	}

	state := pyjson.ObjectOf("marker", markerState, "markerPid", markerPID, "launchIntent", sshLaunchIntent(intentPath, correlation),
		"exitCode", exitCode, "receipt", sshObservedReceipt(installRoot, correlation), "coordinatorReady", sshCoordinatorReady(readyPath, correlation))
	return state
}

// Newest finished receipt of the correlation in the install wide receipt store
func sshObservedReceipt(installRoot, correlation string) any {
	matches, err := filepath.Glob(filepath.Join(installRoot, "logs", "update_receipts", "update_*.json"))
	if err != nil {
		return nil
	}
	type stamped struct {
		path string
		ns   int64
	}
	var paths []stamped
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			return nil
		}
		paths = append(paths, stamped{p, info.ModTime().UnixNano()})
	}
	sort.SliceStable(paths, func(i, j int) bool { return paths[i].ns > paths[j].ns })
	for _, entry := range paths {
		data, err := os.ReadFile(entry.path)
		if err != nil {
			continue
		}
		payload, err := observedDict(string(data))
		if err != nil || payload == nil || !pyjson.Equal(observedGet(payload, "correlation_id"), correlation) {
			continue
		}
		if pyjson.Equal(observedGet(payload, "outcome"), "running") || !pyTruthy(observedGet(payload, "finished_at")) {
			continue
		}
		pre, _ := observedGet(payload, "pre_update").(*pyjson.Object)
		post, _ := observedGet(payload, "post_update").(*pyjson.Object)
		outcome := "unknown"
		if v := observedGet(payload, "outcome"); pyTruthy(v) {
			outcome = pyrepr.Str(v)
		}
		owed := func(f *pyjson.Object) *pyjson.Object {
			reason := ""
			if v := observedGet(f, "reason"); pyTruthy(v) {
				reason = pyrepr.Str(v)
			}
			return pyjson.ObjectOf("step", pyrepr.Str(observedGet(f, "step")), "reason", reason)
		}
		followups := []any{}
		if list, ok := observedGet(payload, "followups").([]any); ok {
			for _, item := range list {
				if f, ok := item.(*pyjson.Object); ok {
					followups = append(followups, owed(f))
				}
			}
		}
		var userAction any
		if action, ok := observedGet(payload, "user_action").(*pyjson.Object); ok {
			userAction = owed(action)
		}
		return pyjson.ObjectOf("correlationId", correlation, "outcome", outcome,
			"startedAt", observedGet(payload, "started_at"), "finishedAt", observedGet(payload, "finished_at"),
			"preSha", observedGet(pre, "sha"), "postSha", observedGet(post, "sha"),
			"preVersion", observedGet(pre, "version"), "postVersion", observedGet(post, "version"),
			"stopReason", observedGet(payload, "stop_reason"), "followups", followups, "userAction", userAction)
	}
	return nil
}

// Correlated readiness proof of the Windows update coordinator, nil when absent or foreign
func sshCoordinatorReady(readyPath, correlation string) any {
	data, err := os.ReadFile(readyPath)
	if err != nil {
		return nil
	}
	payload, err := observedDict(string(data))
	if err != nil || payload == nil || !pyjson.Equal(observedGet(payload, "correlation_id"), correlation) {
		return nil
	}
	pid, ok := observedInt(observedGet(payload, "pid"))
	if !ok || pid <= 0 {
		return nil
	}
	return pyjson.ObjectOf("correlationId", correlation, "pid", pid)
}

// Whether the launch intent names a live launcher of this correlation by pid and creation identity
func sshLaunchIntent(intentPath, correlation string) string {
	data, err := os.ReadFile(intentPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent"
		}
		return "unavailable"
	}
	payload, err := observedDict(string(data))
	if err != nil || payload == nil || !pyjson.Equal(observedGet(payload, "correlation"), correlation) {
		return "malformed"
	}
	pid, ok := observedInt(observedGet(payload, "pid"))
	creation, isString := observedGet(payload, "creation").(string)
	if !ok || pid < 1 || pid > 4294967295 || !isString {
		return "malformed"
	}
	live, known := observedPIDAlive(int(pid))
	if !known {
		return "unavailable"
	}
	if !live {
		return "dead"
	}
	current, known := observedProcessCreation(int(pid))
	if !known {
		return "unavailable"
	}
	if current == creation {
		return "present"
	}
	return "dead"
}
