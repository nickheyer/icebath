package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Readiness budgets, the overlapping poll join grace and the retryable in progress code
const (
	readinessShareWaitSeconds       = 4.0
	readinessJoinGraceSeconds       = 0.5
	readinessStatusShareWaitSeconds = 12.0
	readinessInProgressErr          = 5097
)

// One single flighted readiness probe, its answer or failure set once it settles
type readinessFuture struct {
	done      chan struct{}
	result    map[string]any
	failure   any
	cancelled bool
}

// Dedicated readiness executor and the probes in flight by kind and profile
var (
	readinessPool     = newRPCWorkerPool(max(2, min(4, envInt("ICEBATH_TUI_RPC_POOL_WORKERS", 8))))
	readinessMu       sync.Mutex
	readinessInflight = map[[2]string]*readinessFuture{}
)

var configSetupRegistry = &handlerRegistry{}

func init() {
	registerAtExit(func() { readinessPool.Shutdown(true) })
	configSetupRegistry.method("setup.status", setupStatus)
	configSetupRegistry.install()
}

// Forgets a settled probe so the next poll re-probes, logging a failure nobody waited for
func readinessCleared(key [2]string, future *readinessFuture) {
	readinessMu.Lock()
	if readinessInflight[key] == future {
		delete(readinessInflight, key)
	}
	readinessMu.Unlock()
	if !future.cancelled && future.failure != nil {
		pkgLog().Debug(fmt.Sprintf("readiness probe %s failed after its callers returned: %s", pyrepr.Repr(key), failureText(future.failure)))
	}
}

// Single flights a probe under key on the readiness pool, answering the retryable error when it outlives the wait
func readinessShare(rid any, key [2]string, runProbe func() map[string]any, waitSeconds float64) map[string]any {
	readinessMu.Lock()
	future := readinessInflight[key]
	owner := future == nil
	if owner {
		future = &readinessFuture{done: make(chan struct{})}
		readinessInflight[key] = future
		started := false
		err := readinessPool.Submit(func() {
			started = true
			defer func() {
				if failure := recover(); failure != nil {
					future.failure = failure
				}
			}()
			future.result = runProbe()
		}, func() {
			future.cancelled = !started
			close(future.done)
			readinessCleared(key, future)
		})
		if err != nil {
			delete(readinessInflight, key)
			readinessMu.Unlock()
			panic(err)
		}
	}
	readinessMu.Unlock()
	wait := waitSeconds
	if !owner {
		wait = min(waitSeconds, readinessJoinGraceSeconds)
	}
	select {
	case <-future.done:
	case <-time.After(time.Duration(wait * float64(time.Second))):
		pkgLog().Warn(fmt.Sprintf("readiness probe %s exceeded its budget (%.1fs); it continues in the background", pyrepr.Repr(key), waitSeconds))
		return rpcErr(rid, readinessInProgressErr, "readiness check still in progress; retrying next tick", nil)
	}
	if future.cancelled {
		panic(fmt.Errorf("CancelledError"))
	}
	if future.failure != nil {
		panic(future.failure)
	}
	return ok(rid, future.result)
}

// Probe of setup.status or setup.runtime_check under the optional profile's runtime scope, an unknown profile answering ok false
func readinessCheck(ctx context.Context, rid any, params map[string]any, probe func(ctx context.Context, profile string, scoped map[string]any) map[string]any, probeKey string, waitSeconds float64) map[string]any {
	profile := ""
	if params != nil {
		profile = strings.TrimSpace(relayString(params["profile"]))
	}
	scopeHome := ""
	if profile != "" {
		if !profiles.ProfileExists(profile) {
			return ok(rid, map[string]any{"ok": false, "profile": params["profile"],
				"error": fmt.Sprintf("Profile '%s' does not exist on this backend.", profile)})
		}
		resolved, err := profileHome(profile)
		if err != nil {
			panic(err)
		}
		scopeHome = resolved
	}
	worker := context.WithoutCancel(ctx)
	scoped := map[string]any{}
	if profile != "" {
		scoped["profile"] = profile
	}
	return readinessShare(rid, [2]string{probeKey, profile}, func() map[string]any {
		return probe(profileRuntimeScope(worker, scopeHome, true), profile, scoped)
	}, waitSeconds)
}

// Merges scoped into out
func withScoped(out, scoped map[string]any) map[string]any {
	for key, value := range scoped {
		out[key] = value
	}
	return out
}

// Loose provider check answered from a live probe of the profile
func setupStatus(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5016, failureText(failure), nil)
		}
	}()
	probe := func(scoped context.Context, profile string, stamp map[string]any) map[string]any {
		return withScoped(map[string]any{"provider_configured": gateway.HasAnyProviderConfigured(scoped, profile != "")}, stamp)
	}
	return readinessCheck(ctx, rid, params, probe, "status", readinessStatusShareWaitSeconds)
}

// First truthy value, else the last one as Python or does
func firstTruthy(values ...any) any {
	for _, value := range values {
		if truthy(value) {
			return value
		}
	}
	return values[len(values)-1]
}
