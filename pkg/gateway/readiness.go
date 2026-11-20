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
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const readinessDiskDegradedPercent = 90.0

var readinessConnectedStates = map[string]bool{"connected": true, "running": true, "ok": true}

// Readiness check entry with its status, detail when given and extra fields in order
func readinessCheck(status, detail string, extra ...any) *pyjson.Object {
	out := pyjson.ObjectOf("status", status)
	if detail != "" {
		out.Set("detail", detail)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return out
}

// Python type name of an error for a readiness detail
func readinessErrorName(err error) string {
	if typed, ok := err.(interface{ PyTypeName() string }); ok {
		return typed.PyTypeName()
	}
	var pyErr *PyError
	if errors.As(err, &pyErr) {
		return pyErr.Kind
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "OSError"
	}
	return "Error"
}

// State database reachability through a read only schema query, corrupt when the health latch says so
func readinessProbeStateDB(h string) *pyjson.Object {
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
	return readinessCheck("degraded", readinessErrorName(err))
}

// Config document readability, defaults when absent and degraded when unreadable or not a mapping
func readinessProbeConfig(ctx context.Context, h string) *pyjson.Object {
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
		return readinessCheck("degraded", fmt.Sprintf("invalid config (%s)", readinessErrorName(err)))
	}
	if raw == nil {
		return readinessCheck("ok", "")
	}
	switch raw.(type) {
	case map[string]any, config.Tree, *pyjson.Object:
		return readinessCheck("ok", "")
	}
	return readinessCheck("degraded", "top level is not a mapping")
}

// Disk use of the home volume, degraded at the threshold
func readinessProbeDisk(h string) *pyjson.Object {
	total, used, free, err := pyfs.DiskUsage(h)
	if err != nil {
		return readinessCheck("degraded", readinessErrorName(err))
	}
	usedPct := 0.0
	if total != 0 {
		usedPct = pyint.Round(float64(used)/float64(total)*100, 1)
	}
	status := "ok"
	if usedPct >= readinessDiskDegradedPercent {
		status = "degraded"
	}
	return readinessCheck(status, "", "used_percent", usedPct, "free_bytes", int64(free))
}

// Gateway state and connected platform count from the runtime status
func readinessProbeGateway(runtime *pyjson.Object) *pyjson.Object {
	stateText := "unknown"
	if v := runtime.GetOr("gateway_state", nil); pyrepr.Truthy(v) {
		stateText = pyrepr.Str(v)
	}
	platforms, _ := runtime.GetOr("platforms", nil).(*pyjson.Object)
	if platforms == nil {
		platforms = pyjson.NewObject()
	}
	connected := 0
	for _, k := range platforms.Keys {
		v, _ := platforms.Get(k)
		entry, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		label := pyrepr.Or(entry.GetOr("state", nil), pyrepr.Or(entry.GetOr("status", nil), ""))
		if readinessConnectedStates[strings.ToLower(pyrepr.Str(label))] {
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
func readinessProbeSessionStore(runtime, stateDBProbe *pyjson.Object) *pyjson.Object {
	if d, _ := stateDBProbe.Get("detail"); d == "corrupt" {
		return readinessCheck("unavailable", "corrupt")
	}
	stateText := ""
	if store, ok := runtime.GetOr("session_store", nil).(*pyjson.Object); ok {
		stateText = "unknown"
		if v := store.GetOr("status", nil); pyrepr.Truthy(v) {
			stateText = pyrepr.Str(v)
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
func CollectRuntimeReadiness(ctx context.Context, configuredModel string, runtimeStatus *pyjson.Object, activeAPIRuns, processCompletionQueueDepth, activeDelegations int) *pyjson.Object {
	h := home.Dir(ctx)
	runtime := runtimeStatus
	if runtime == nil {
		runtime = pyjson.NewObject()
	}
	stateDBProbe := readinessProbeStateDB(h)
	modelStatus := "degraded"
	if strings.TrimSpace(configuredModel) != "" {
		modelStatus = "ok"
	}
	checks := pyjson.ObjectOf(
		"state_db", stateDBProbe,
		"session_store", readinessProbeSessionStore(runtime, stateDBProbe),
		"config", readinessProbeConfig(ctx, h),
		"model", readinessCheck(modelStatus, ""),
		"disk", readinessProbeDisk(h),
		"gateway", readinessProbeGateway(runtime),
		"background_queues", readinessCheck("ok", "", "active_api_runs", int64(max(0, activeAPIRuns)),
			"process_completions", int64(max(0, processCompletionQueueDepth)), "active_delegations", int64(max(0, activeDelegations))))
	status := "ok"
	for _, k := range checks.Keys {
		v, _ := checks.Get(k)
		if s, _ := v.(*pyjson.Object).Get("status"); s != "ok" {
			status = "degraded"
			break
		}
	}
	return pyjson.ObjectOf("status", status, "checks", checks)
}
