package media

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	sttLeaseMu sync.Mutex
	sttLeases  = map[string]bool{}
)

func sttLeaseLog() *slog.Logger { return slog.Default().With("logger", "tools.stt_lease") }

// Preloads the configured local STT model so the next transcription starts hot, the result naming what happened
func WarmSTTProvider(ctx context.Context, sttConfig map[string]any, provider string) (*pyjson.Object, error) {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	name := provider
	if name == "" {
		resolved, err := getProvider(ctx, sttConfig)
		if err != nil {
			return nil, err
		}
		name = resolved
	}
	name = pystr.Strip(pystr.Lower(name))
	result := pyjson.ObjectOf("provider", name, "warmed", false, "action", "noop")
	if name != "local" && name != "local_command" {
		return result, nil
	}
	if name == "local_command" {
		result.Set("warmed", true)
		return result, nil
	}
	if !hasFasterWhisper() && !tryLazyInstallSTT(ctx) {
		result.Set("action", "error")
		result.Set("error", "faster-whisper not installed")
		return result, nil
	}
	localCfg, _ := sttConfig["local"].(map[string]any)
	if localCfg == nil {
		localCfg = map[string]any{}
	}
	modelName := normalizeLocalModel(sttStrOrEmpty(localCfg["model"]))
	localModelMu.Lock()
	hotBefore := localModel != nil && localModelName == modelName
	localModelMu.Unlock()
	started := time.Now()
	model, err := getOrLoadLocalModel(ctx, modelName, localCfg)
	if err != nil {
		sttLeaseLog().Warn(fmt.Sprintf("[STT] warm-up for local model '%s' failed: %v", modelName, err))
		result.Set("action", "error")
		result.Set("error", err.Error())
		return result, nil
	}
	if model == nil {
		result.Set("action", "error")
		result.Set("error", "Local whisper model failed to load")
		return result, nil
	}
	model.release()
	touchTranscriptionTime()
	if idle := getIdleUnloadSeconds(localCfg); idle > 0 {
		startIdleUnloadWatcher(ctx, idle)
	}
	elapsedMS := time.Since(started).Milliseconds()
	action := "loaded"
	if hotBefore {
		action = "cached"
	}
	result.Set("warmed", true)
	result.Set("action", action)
	result.Set("elapsed_ms", elapsedMS)
	sttLeaseLog().Info(fmt.Sprintf("[STT] warm-up local model '%s': %s in %dms", modelName, action, elapsedMS))
	return result, nil
}

// Registers a lease and warms the engine, a repeat acquire still rewarming
func AcquireSTTLease(ctx context.Context, lease string, sttConfig map[string]any) (*pyjson.Object, error) {
	sttLeaseMu.Lock()
	sttLeases[lease] = true
	holders := len(sttLeases)
	sttLeaseMu.Unlock()
	result, err := WarmSTTProvider(ctx, sttConfig, "")
	if err != nil {
		return nil, err
	}
	result.Set("leases", int64(holders))
	return result, nil
}

// Drops a lease and reports the holder count, never unloading the shared model
func ReleaseSTTLease(lease string) *pyjson.Object {
	sttLeaseMu.Lock()
	defer sttLeaseMu.Unlock()
	delete(sttLeases, lease)
	return pyjson.ObjectOf("leases", int64(len(sttLeases)))
}

// Sorted names of the live STT leases
func STTLeaseHolders() []string {
	sttLeaseMu.Lock()
	defer sttLeaseMu.Unlock()
	out := make([]string, 0, len(sttLeases))
	for k := range sttLeases {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Drops every STT lease, the reset the lease cases run between them
func ResetSTTLeases() {
	sttLeaseMu.Lock()
	clear(sttLeases)
	sttLeaseMu.Unlock()
}
