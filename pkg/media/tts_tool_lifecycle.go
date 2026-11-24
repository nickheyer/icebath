package media

import (
	"strings"

	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Pending keep warm unload, a stopped timer never fires
type keepWarmTimerHandle interface{ Stop() bool }

var (
	ttsLeaseMu         sync.Mutex
	ttsLeases          = map[string]bool{}
	keepWarmTimer      keepWarmTimerHandle
	keepWarmGeneration int
)

// Runs a warm or release hook command, the tts_command_provider module attribute the lifecycle calls through
var lifecycleRunCommandProvider = runCommandProvider

// Starts the keep warm unload after the delay, the threading.Timer seam of the lifecycle
var makeKeepWarmTimer = func(delay time.Duration, fn func()) keepWarmTimerHandle { return time.AfterFunc(delay, fn) }

// Provider name to the loader filling that engine's cache slot under the key synthesis uses
func localTTSWarmers() map[string]func(ctx context.Context, env *tools.Env, req *ttsRequest) error {
	return map[string]func(ctx context.Context, env *tools.Env, req *ttsRequest) error{
		"piper": func(ctx context.Context, env *tools.Env, req *ttsRequest) error {
			_, err := loadPiperVoiceForConfig(ctx, env, req)
			return err
		},
		"kittentts": func(ctx context.Context, _ *tools.Env, req *ttsRequest) error {
			_, err := loadKittenTTSModelForConfig(ctx, req)
			return err
		},
	}
}

// Runs a user provider's warm or release hook, the hook name when signalled, error on failure, empty when none
func signalUserTTSProvider(ctx context.Context, env *tools.Env, name string, req *ttsRequest, hook string) string {
	if name == "" || builtinTTSProviders[name] {
		return ""
	}
	out, err := func() (out string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if p := req.command(name); p != nil {
			template := p.WarmCommand
			if hook == "release" {
				template = p.ReleaseCommand
			}
			if template = strings.TrimSpace(template); template == "" {
				return "", nil
			}
			command := renderCommandTemplate(template, map[string]string{"voice": p.Voice, "model": p.Model, "speed": req.commandSpeed(p)})
			bound := context.WithoutCancel(ctx)
			go func() {
				if _, err := lifecycleRunCommandProvider(bound, env, command, p.Timeout.Seconds(), p.EnvPassthrough); err != nil {
					pkgLog().Debug(fmt.Sprintf("[TTS] %s_command for %s failed: %s", hook, name, pyrepr.ExceptionMessage(err)))
				}
			}()
			return hook, nil
		}
		p, err := lookupPluginTTSProvider(ctx, name, true, false)
		if err != nil {
			return "", err
		}
		if p == nil {
			return "", nil
		}
		if hook == "warm" {
			err = p.Warm(ctx)
		} else {
			err = p.Release(ctx)
		}
		if err != nil {
			return "", err
		}
		return hook, nil
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("[TTS] %s hook for %s failed: %s", hook, name, pyrepr.ExceptionMessage(err)))
		return "error"
	}
	return out
}

// Warms the configured or named TTS provider, loading local engines and signalling user providers
func WarmTTSProvider(ctx context.Context, env *tools.Env, providerName string) map[string]any {
	req := loadTTSRequest(ctx, env)
	name := providerName
	if name == "" {
		name = req.Provider
	}
	name = strings.TrimSpace(strings.ToLower(name))
	result := map[string]any{"provider": name, "warmed": false, "action": "noop"}
	if warmer := localTTSWarmers()[name]; warmer != nil {
		cache := localTTSModelCaches[name]
		before := 0
		if cache != nil {
			before = len(cache.Keys())
		}
		started := time.Now()
		if err := warmer(ctx, env, req); err != nil {
			pkgLog().Warn(fmt.Sprintf("[TTS] warm-up for %s failed: %s", name, pyrepr.ExceptionMessage(err)))
			result["action"], result["error"] = "error", pyrepr.ExceptionMessage(err)
			return result
		}
		after := 0
		if cache != nil {
			after = len(cache.Keys())
		}
		result["warmed"] = true
		result["action"] = "cached"
		if after > before {
			result["action"] = "loaded"
		}
		result["elapsed_ms"] = int(time.Since(started).Milliseconds())
		pkgLog().Info(fmt.Sprintf("[TTS] warm-up %s: %s in %dms", name, result["action"], result["elapsed_ms"]))
		return result
	}
	if signalled := signalUserTTSProvider(ctx, env, name, req, "warm"); signalled != "" {
		ok := signalled != "error"
		result["warmed"] = ok
		result["action"] = "error"
		if ok {
			result["action"] = "warmed"
		}
		return result
	}
	return result
}

// Clears the resident local model caches, one named provider or all and the configured user provider signalled
func ReleaseTTSProvider(ctx context.Context, env *tools.Env, providerName string) map[string]any {
	name := strings.TrimSpace(strings.ToLower(providerName))
	if name == "" {
		req := loadTTSRequest(ctx, env)
		signalUserTTSProvider(ctx, env, req.Provider, req, "release")
	}
	released := 0
	names := make([]string, 0, len(localTTSModelCaches))
	for n := range localTTSModelCaches {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, cacheName := range names {
		if name == "" || cacheName == name {
			cache := localTTSModelCaches[cacheName]
			released += len(cache.Keys())
			cache.Clear()
		}
	}
	if released > 0 {
		pkgLog().Info(fmt.Sprintf("[TTS] released %d resident local model(s)", released))
	}
	return map[string]any{"released": released}
}

// Keep warm window from tts.keep_warm_seconds, zero meaning unload at once
func keepWarmSeconds(ctx context.Context, env *tools.Env) float64 {
	return loadTTSRequest(ctx, env).KeepWarm.Seconds()
}

// Cancels a pending keep warm unload, lock held
func cancelKeepWarmLocked() {
	keepWarmGeneration++
	if keepWarmTimer != nil {
		keepWarmTimer.Stop()
		keepWarmTimer = nil
	}
}

// Starts the keep warm window with no holders left, or unloads inline when it is zero, lock held
func scheduleReleaseLocked(ctx context.Context, env *tools.Env) int {
	cancelKeepWarmLocked()
	delay := keepWarmSeconds(ctx, env)
	if delay <= 0 {
		return ReleaseTTSProvider(ctx, env, "")["released"].(int)
	}
	generation := keepWarmGeneration
	bound := context.WithoutCancel(ctx)
	keepWarmTimer = makeKeepWarmTimer(time.Duration(delay*float64(time.Second)), func() { releaseAfterKeepWarm(bound, env, generation) })
	return 0
}

// Unloads resident models once the window passed unless a newer window or a holder superseded it
func releaseAfterKeepWarm(ctx context.Context, env *tools.Env, generation int) {
	ttsLeaseMu.Lock()
	defer ttsLeaseMu.Unlock()
	if generation != keepWarmGeneration || len(ttsLeases) > 0 {
		return
	}
	keepWarmTimer = nil
	ReleaseTTSProvider(ctx, env, "")
}

// Registers a lease and warms the provider, cancelling any pending unload
func AcquireTTSLease(ctx context.Context, env *tools.Env, lease string) map[string]any {
	ttsLeaseMu.Lock()
	cancelKeepWarmLocked()
	ttsLeases[lease] = true
	holders := len(ttsLeases)
	ttsLeaseMu.Unlock()
	result := WarmTTSProvider(ctx, env, "")
	ttsLeaseMu.Lock()
	if len(ttsLeases) == 0 {
		scheduleReleaseLocked(ctx, env)
	}
	ttsLeaseMu.Unlock()
	out := make(map[string]any, len(result)+1)
	for k, v := range result {
		out[k] = v
	}
	out["leases"] = holders
	return out
}

// Drops a lease, the last one out starting the keep warm window before resident models unload
func ReleaseTTSLease(ctx context.Context, env *tools.Env, lease string) map[string]any {
	ttsLeaseMu.Lock()
	defer ttsLeaseMu.Unlock()
	held := ttsLeases[lease]
	delete(ttsLeases, lease)
	holders := len(ttsLeases)
	released := 0
	if held && holders == 0 {
		released = scheduleReleaseLocked(ctx, env)
	}
	return map[string]any{"leases": holders, "released": released}
}
