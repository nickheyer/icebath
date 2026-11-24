package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	mathutil "github.com/nickheyer/icebath/pkg/utils/math"
)

const readinessDiskDegradedPercent = 90.0

var readinessConnectedStates = map[string]bool{"connected": true, "running": true, "ok": true}

// Readiness check entry with its status, detail when given and extra fields in order
func readinessCheck(status, detail string, extra ...any) *jsonutil.Object {
	out := jsonutil.ObjectOf("status", status)
	if detail != "" {
		out.Set("detail", detail)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return out
}

// State database reachability through a read only schema query, corrupt when the health latch says so
func readinessProbeStateDB(h string) *jsonutil.Object {
	path := filepath.Join(h, "state.db")
	if _, err := os.Stat(path); err != nil {
		return readinessCheck("ok", "not initialized")
	}
	if state.StorageState(path) == state.StorageCorrupt {
		return readinessCheck("degraded", state.StorageCorrupt)
	}
	err := func() error {
		conn, err := state.ConnectSQLite("file:"+filepath.ToSlash(path)+"?mode=ro", state.SQLiteConfig{Timeout: 1, URI: true})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master LIMIT 1")
		if err != nil {
			return err
		}
		defer rows.Close()
		rows.Next()
		return rows.Err()
	}()
	if err == nil {
		return readinessCheck("ok", "")
	}
	if state.NoteStorageError(path, err) {
		return readinessCheck("degraded", state.StorageCorrupt)
	}
	return readinessCheck("degraded", errorutil.Kind(err))
}

// Config document readability, defaults when absent and degraded when unreadable or not a mapping
func readinessProbeConfig(ctx context.Context, h string) *jsonutil.Object {
	path := filepath.Join(h, "config.yaml")
	exists, err := config.ConfigExists(ctx, path)
	var raw any
	if err == nil {
		if !exists {
			return readinessCheck("ok", "using defaults")
		}
		raw, err = config.ReadConfigDocReadonly(ctx, path)
	}
	if err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			return readinessCheck("degraded", "config backend unavailable")
		}
		return readinessCheck("degraded", fmt.Sprintf("invalid config (%s)", errorutil.Kind(err)))
	}
	if raw == nil {
		return readinessCheck("ok", "")
	}
	switch raw.(type) {
	case map[string]any, config.Tree, *jsonutil.Object:
		return readinessCheck("ok", "")
	}
	return readinessCheck("degraded", "top level is not a mapping")
}

// Disk use of the home volume, degraded at the threshold
func readinessProbeDisk(h string) *jsonutil.Object {
	total, used, free, err := fsutil.DiskUsage(h)
	if err != nil {
		return readinessCheck("degraded", errorutil.Kind(err))
	}
	usedPct := 0.0
	if total != 0 {
		usedPct = mathutil.Round(float64(used)/float64(total)*100, 1)
	}
	status := "ok"
	if usedPct >= readinessDiskDegradedPercent {
		status = "degraded"
	}
	return readinessCheck(status, "", "used_percent", usedPct, "free_bytes", int64(free))
}

// Gateway state and connected platform count from the runtime status
func readinessProbeGateway(runtime *jsonutil.Object) *jsonutil.Object {
	stateText := "unknown"
	if v := runtime.ValueOr("gateway_state", nil); anyutil.IsTrue(v) {
		stateText = anyutil.String(v)
	}
	platforms, _ := runtime.ValueOr("platforms", nil).(*jsonutil.Object)
	if platforms == nil {
		platforms = jsonutil.NewObject()
	}
	connected := 0
	for _, k := range platforms.Keys() {
		v, _ := platforms.Get(k)
		entry, ok := v.(*jsonutil.Object)
		if !ok {
			continue
		}
		label := anyutil.Or(entry.ValueOr("state", nil), anyutil.Or(entry.ValueOr("status", nil), ""))
		if readinessConnectedStates[strings.ToLower(anyutil.String(label))] {
			connected++
		}
	}
	status := "degraded"
	if stateText == "running" || stateText == "draining" {
		status = "ok"
	}
	return readinessCheck(status, "", "state", stateText, "connected_platforms", int64(connected), "platforms", int64(platforms.Len()))
}

// Session store cache state, unavailable whenever the state database reads corrupt
func readinessProbeSessionStore(runtime, stateDBProbe *jsonutil.Object) *jsonutil.Object {
	if d, _ := stateDBProbe.Get("detail"); d == "corrupt" {
		return readinessCheck("unavailable", "corrupt")
	}
	stateText := ""
	if store, ok := runtime.ValueOr("session_store", nil).(*jsonutil.Object); ok {
		stateText = "unknown"
		if v := store.ValueOr("status", nil); anyutil.IsTrue(v) {
			stateText = anyutil.String(v)
		}
	}
	switch stateText {
	case "ok", "unavailable", "retrying":
		return readinessCheck(stateText, "")
	}
	if s, _ := stateDBProbe.Get("status"); s == "ok" {
		return readinessCheck("ok", "")
	}
	return readinessCheck("unavailable", "")
}

// Runtime readiness of this home with status and counts only, never config values or paths
func CollectRuntimeReadiness(ctx context.Context, configuredModel string, runtimeStatus *jsonutil.Object, activeAPIRuns, processCompletionQueueDepth, activeDelegations int) *jsonutil.Object {
	h := home.Dir(ctx)
	runtime := runtimeStatus
	if runtime == nil {
		runtime = jsonutil.NewObject()
	}
	stateDBProbe := readinessProbeStateDB(h)
	modelStatus := "degraded"
	if strings.TrimSpace(configuredModel) != "" {
		modelStatus = "ok"
	}
	checks := jsonutil.ObjectOf(
		"state_db", stateDBProbe,
		"session_store", readinessProbeSessionStore(runtime, stateDBProbe),
		"config", readinessProbeConfig(ctx, h),
		"model", readinessCheck(modelStatus, ""),
		"disk", readinessProbeDisk(h),
		"gateway", readinessProbeGateway(runtime),
		"background_queues", readinessCheck("ok", "", "active_api_runs", int64(max(0, activeAPIRuns)),
			"process_completions", int64(max(0, processCompletionQueueDepth)), "active_delegations", int64(max(0, activeDelegations))))
	status := "ok"
	for _, k := range checks.Keys() {
		v, _ := checks.Get(k)
		if s, _ := v.(*jsonutil.Object).Get("status"); s != "ok" {
			status = "degraded"
			break
		}
	}
	return jsonutil.ObjectOf("status", status, "checks", checks)
}
