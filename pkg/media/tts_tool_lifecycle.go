package media

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const defaultKeepWarmSeconds = 60.0

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

// Cloud SDK features installed lazily per provider
var lazySDKFeatures = map[string]string{"edge": "edge-tts", "elevenlabs": "tts-premium", "mistral": "mistral"}

// Provider name to the loader filling that engine's cache slot under the key synthesis uses
func localTTSWarmers() map[string]func(ctx context.Context, env *tools.Env, cfg map[string]any) error {
	return map[string]func(ctx context.Context, env *tools.Env, cfg map[string]any) error{
		"piper": func(ctx context.Context, env *tools.Env, cfg map[string]any) error {
			_, _, err := loadPiperVoiceForConfig(ctx, env, cfg)
			return err
		},
		"kittentts": func(ctx context.Context, _ *tools.Env, cfg map[string]any) error {
			_, _, err := loadKittenTTSModelForConfig(ctx, cfg)
			return err
		},
	}
}

// Runs a user provider's warm or release hook, the hook name when signalled, error on failure, empty when none
func signalUserTTSProvider(ctx context.Context, env *tools.Env, name string, ttsConfig map[string]any, hook string) string {
	if name == "" || builtinTTSProviders[name] {
		return ""
	}
	cfg := namedProviderConfig(ttsConfig, name, builtinTTSProviders)
	out, err := func() (out string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if isCommandProviderConfig(cfg) {
			template := pystr.Strip(pyrepr.Str(pyrepr.Or(cfg[hook+"_command"], "")))
			if template == "" {
				return "", nil
			}
			speed, ok := cfg["speed"]
			if !ok {
				speed = ttsConfig["speed"]
				if _, has := ttsConfig["speed"]; !has {
					speed = ""
				}
			}
			command := renderCommandTemplate(template, map[string]string{
				"voice": pyrepr.Str(mapGetOr(cfg, "voice", "")), "model": pyrepr.Str(mapGetOr(cfg, "model", "")), "speed": pyrepr.Str(speed)})
			bound := context.WithoutCancel(ctx)
			go func() {
				if _, err := lifecycleRunCommandProvider(bound, env, command, getCommandTTSTimeout(cfg), commandEnvPassthrough(cfg)); err != nil {
					ttsLog().Debug(fmt.Sprintf("[TTS] %s_command for %s failed: %s", hook, name, pyrepr.ExceptionMessage(err)))
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
		ttsLog().Debug(fmt.Sprintf("[TTS] %s hook for %s failed: %s", hook, name, pyrepr.ExceptionMessage(err)))
		return "error"
	}
	return out
}

// Value of key in a map, def when absent
func mapGetOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Warms the configured or named TTS provider, loading local engines and installing lazy SDKs
func WarmTTSProvider(ctx context.Context, env *tools.Env, ttsConfig map[string]any, providerName string) map[string]any {
	if ttsConfig == nil {
		ttsConfig = loadTTSConfig(ctx, env)
	}
	name := providerName
	if name == "" {
		name = getTTSProvider(ttsConfig)
	}
	name = pystr.Strip(pystr.Lower(name))
	result := map[string]any{"provider": name, "warmed": false, "action": "noop"}
	if warmer := localTTSWarmers()[name]; warmer != nil {
		cache := localTTSModelCaches[name]
		before := 0
		if cache != nil {
			before = len(cache.Keys())
		}
		started := time.Now()
		if err := warmer(ctx, env, ttsConfig); err != nil {
			ttsLog().Warn(fmt.Sprintf("[TTS] warm-up for %s failed: %s", name, pyrepr.ExceptionMessage(err)))
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
		ttsLog().Info(fmt.Sprintf("[TTS] warm-up %s: %s in %dms", name, result["action"], result["elapsed_ms"]))
		return result
	}
	if signalled := signalUserTTSProvider(ctx, env, name, ttsConfig, "warm"); signalled != "" {
		ok := signalled != "error"
		result["warmed"] = ok
		result["action"] = "error"
		if ok {
			result["action"] = "warmed"
		}
		return result
	}
	if feature, ok := lazySDKFeatures[name]; ok {
		available, err := pm.Available(ctx, feature)
		if err == nil {
			if available {
				result["warmed"], result["action"] = true, "cached"
			} else if err = pm.EnsureImport(ctx, feature); err == nil {
				result["warmed"], result["action"] = true, "installed"
			}
		}
		if err != nil {
			ttsLog().Debug(fmt.Sprintf("[TTS] SDK warm-up for %s skipped: %s", name, pyrepr.ExceptionMessage(err)))
			result["action"], result["error"] = "error", pyrepr.ExceptionMessage(err)
		}
	}
	return result
}

// Clears the resident local model caches, one named provider or all and the configured user provider signalled
func ReleaseTTSProvider(ctx context.Context, env *tools.Env, providerName string) map[string]any {
	name := pystr.Strip(pystr.Lower(providerName))
	if name == "" {
		cfg := loadTTSConfig(ctx, env)
		signalUserTTSProvider(ctx, env, getTTSProvider(cfg), cfg, "release")
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
		ttsLog().Info(fmt.Sprintf("[TTS] released %d resident local model(s)", released))
	}
	return map[string]any{"released": released}
}

// Keep warm window from tts.keep_warm_seconds, zero meaning unload at once
func keepWarmSeconds(ctx context.Context, env *tools.Env) float64 {
	raw := mapGetOr(loadTTSConfig(ctx, env), "keep_warm_seconds", defaultKeepWarmSeconds)
	f, err := pyint.Float(raw)
	if err != nil {
		return defaultKeepWarmSeconds
	}
	return max(0.0, f)
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
func AcquireTTSLease(ctx context.Context, env *tools.Env, lease string, ttsConfig map[string]any) map[string]any {
	ttsLeaseMu.Lock()
	cancelKeepWarmLocked()
	ttsLeases[lease] = true
	holders := len(ttsLeases)
	ttsLeaseMu.Unlock()
	result := WarmTTSProvider(ctx, env, ttsConfig, "")
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

// Sorted names of the live leases
func TTSLeaseHolders() []string {
	ttsLeaseMu.Lock()
	defer ttsLeaseMu.Unlock()
	out := make([]string, 0, len(ttsLeases))
	for k := range ttsLeases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Cancels the pending unload and drops every lease
func ResetTTSLeasesForTests() {
	ttsLeaseMu.Lock()
	defer ttsLeaseMu.Unlock()
	cancelKeepWarmLocked()
	clear(ttsLeases)
}
