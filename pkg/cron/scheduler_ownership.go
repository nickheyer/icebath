package cron

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Home key to home path and to resolved cron store for every profile this process ticks
var ticked = struct {
	sync.Mutex
	homes  map[string]string
	stores map[string]string
}{homes: map[string]string{}, stores: map[string]string{}}

// Gateway status probes the ownership predicates read, swapped by tests
var gatewayProbes = struct {
	ownsRuntimeLock      func() bool
	runtimeLockActive    func(lockPath string) (bool, error)
	runningPid           func(pidPath string, cleanupStale bool) (int, bool, error)
	readRuntimeStatus    func(path string) *pyjson.Object
	runtimeStatusIsStale func(record *pyjson.Object, ttl int) bool
	sameHermesHome       func(left, right string) (bool, error)
	profileLabelForHome  func(h string) string
	pidExists            func(pid int) bool
	hostname             func() (string, error)
}{
	ownsRuntimeLock:      gateway.OwnsGatewayRuntimeLock,
	runtimeLockActive:    gateway.IsGatewayRuntimeLockActive,
	runningPid:           gateway.GetRunningPid,
	readRuntimeStatus:    gateway.ReadRuntimeStatus,
	runtimeStatusIsStale: gateway.RuntimeStatusIsStale,
	sameHermesHome:       gateway.SameHermesHome,
	profileLabelForHome:  gateway.ProfileLabelForHome,
	pidExists:            proc.Exists,
	hostname:             os.Hostname,
}

// Runs a probe and reports a panic as a failed probe
func probeSafely(fn func() bool) bool {
	ok, err := probeResult(func() (bool, error) { return fn(), nil })
	return ok && err == nil
}

// Runs a probe and turns a panic into its error
func probeResult(fn func() (bool, error)) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			ok, err = false, fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

// Active cron scope home when none is given
func ownershipHome(h string) string {
	if h == "" {
		return home.Dir(context.Background())
	}
	return h
}

// Publishes the homes this ticker owns, reaping the worker pools and store state of departed homes
func registerTickedHomes(homes []string) {
	resolved, stores := map[string]string{}, map[string]string{}
	for _, h := range homes {
		key := home.Key(h)
		resolved[key] = h
		stores[key] = StoreKey(h)
	}
	kept := map[string]bool{}
	for _, store := range stores {
		kept[store] = true
	}
	ticked.Lock()
	var departed []string
	departedStores := map[string]bool{}
	for key := range ticked.homes {
		if _, still := resolved[key]; still {
			continue
		}
		departed = append(departed, key)
		if store, ok := ticked.stores[key]; ok && !kept[store] {
			departedStores[store] = true
		}
	}
	ticked.homes, ticked.stores = resolved, stores
	ticked.Unlock()
	if len(departed) == 0 {
		return
	}
	sort.Strings(departed)
	discardParallelPools(departed)
	forget := make([]string, 0, len(departedStores))
	for store := range departedStores {
		forget = append(forget, store)
	}
	sort.Strings(forget)
	ForgetStores(forget)
}

// Snapshot of home key to home for the profiles this process ticks
func tickedHomes() map[string]string {
	ticked.Lock()
	defer ticked.Unlock()
	out := make(map[string]string, len(ticked.homes))
	for k, v := range ticked.homes {
		out[k] = v
	}
	return out
}

// True when this process's cron ticker owns the home, the active scope when empty
func servesProfile(h string) bool {
	key := home.Key(ownershipHome(h))
	ticked.Lock()
	defer ticked.Unlock()
	_, ok := ticked.homes[key]
	return ok
}

// True when this process is the host gateway and its ticker visits the home
func ownsCronTickFor(h string) bool {
	if !probeSafely(gatewayProbes.ownsRuntimeLock) {
		return false
	}
	return servesProfile(h)
}

// True when a gateway runtime status record proves the home, its own or one in its served set
func recordServesProfile(record *pyjson.Object, h string) bool {
	if record == nil {
		return false
	}
	target := ownershipHome(h)
	same, err := probeResult(func() (bool, error) {
		recordHome, _ := record.Get("hermes_home")
		text, ok := recordHome.(string)
		if !ok || pystr.Strip(text) == "" {
			text = home.Process()
		}
		return gatewayProbes.sameHermesHome(text, target)
	})
	if err != nil {
		return false
	}
	if same {
		return true
	}
	raw, _ := record.Get("served_profiles")
	served, ok := raw.([]any)
	if !ok {
		return false
	}
	label := gatewayProbes.profileLabelForHome(target)
	if label == "" {
		return false
	}
	for _, entry := range served {
		if pyrepr.Equal(entry, label) {
			return true
		}
	}
	return false
}

// Runtime status record of another live host gateway that ticks the home, nil when that cannot be proven
func liveGatewayTicking(h string) *pyjson.Object {
	var record *pyjson.Object
	proven := probeSafely(func() bool {
		if gatewayProbes.ownsRuntimeLock() {
			return false
		}
		active, err := gatewayProbes.runtimeLockActive("")
		if err != nil || !active {
			return false
		}
		holderPid, found, err := gatewayProbes.runningPid("", false)
		if err != nil {
			return false
		}
		record = gatewayProbes.readRuntimeStatus("")
		if !found || record == nil {
			return false
		}
		pid, _ := record.Get("pid")
		if !pyrepr.Equal(pid, holderPid) {
			return false
		}
		return !gatewayProbes.runtimeStatusIsStale(record, gateway.RuntimeStatusStaleTTL)
	})
	if !proven || !recordServesProfile(record, h) {
		return nil
	}
	return record
}

// True when the claim owner names a process on this host that provably no longer exists
func claimOwnerIsDead(claim *Claim) bool {
	if claim == nil {
		return false
	}
	parts := strings.Split(claim.By, ":")
	if len(parts) < 2 || !isDigitText(parts[1]) {
		return false
	}
	return probeSafely(func() bool {
		host, err := gatewayProbes.hostname()
		if err != nil || parts[0] != host {
			return false
		}
		pid, ok := pyint.Parse(parts[1])
		if !ok || !pid.IsInt64() || int64(int(pid.Int64())) != pid.Int64() {
			return false
		}
		return !gatewayProbes.pidExists(int(pid.Int64()))
	})
}

// True for a nonempty string of Unicode digits, the str.isdigit test
func isDigitText(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}
