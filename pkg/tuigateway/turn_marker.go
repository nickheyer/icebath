package tuigateway

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	turnMarkerMaxAgeSecs    = 24 * 3600
	turnMarkerMaxEntries    = 32
	turnMarkerMaxPromptChar = 64_000
)

var turnMarkerLock sync.Mutex

func turnMarkerLogger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.turn_marker") }

// Interrupted turns journal of a profile home
func turnMarkerPath(home string) string {
	return filepath.Join(home, "desktop", "interrupted_turns.json")
}

// Cross process lock beside the journal so sibling backends never resurrect a cleared marker
func turnMarkerCrossProcessLock(path string) (func(), error) {
	return provider.FileLock(path+".lock", 5*time.Second, "Timed out waiting for the turn-marker lock ("+path+")")
}

// Start time of an entry as a float, zero when falsy, the float error when unparseable
func turnMarkerStartedAt(entry *pyjson.Object) (float64, error) {
	v, _ := entry.Get("started_at")
	if !pyrepr.Truthy(v) {
		return 0, nil
	}
	return pyint.Float(v)
}

// Pid and process start time of the writer so a recycled pid never passes as it
func turnMarkerWriterIdentity() (int64, float64, bool) {
	start, ok := activesessions.OwnStartTime()
	return int64(os.Getpid()), start, ok
}

// Alive, dead or unknown for the process that wrote the marker entry
func MarkerWriterState(entry any) string {
	var pid, start any
	switch e := entry.(type) {
	case *pyjson.Object:
		pid, _ = e.Get("writer_pid")
		start, _ = e.Get("writer_start_time")
	case map[string]any:
		pid, start = e["writer_pid"], e["writer_start_time"]
	default:
		return "unknown"
	}
	var n *big.Int
	switch x := pid.(type) {
	case int:
		n = big.NewInt(int64(x))
	case int64:
		n = big.NewInt(x)
	case *big.Int:
		n = x
	default:
		return "unknown"
	}
	if n.Sign() <= 0 {
		return "unknown"
	}
	alive, known := activesessions.PidLiveness(pid, start)
	if !known {
		return "unknown"
	}
	if alive {
		return "alive"
	}
	return "dead"
}

// Journal entries whose values are objects, empty when missing or unreadable
func turnMarkerLoad(path string) *pyjson.Object {
	out := pyjson.NewObject()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			turnMarkerLogger().Debug("unreadable turn-marker file "+path+"; starting fresh", "error", err)
		}
		return out
	}
	data, err := pyjson.Loads(string(raw))
	if err != nil {
		turnMarkerLogger().Debug("unreadable turn-marker file "+path+"; starting fresh", "error", err)
		return out
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return out
	}
	for _, key := range obj.Keys {
		if entry, isObj := obj.Vals[key].(*pyjson.Object); isObj {
			out.Set(key, entry)
		}
	}
	return out
}

// Entries younger than the age cap, the newest ones when more than the count cap remain
func turnMarkerPrune(entries *pyjson.Object, now float64) (*pyjson.Object, error) {
	fresh := pyjson.NewObject()
	started := map[string]float64{}
	for _, key := range entries.Keys {
		entry := entries.Vals[key].(*pyjson.Object)
		at, err := turnMarkerStartedAt(entry)
		if err != nil {
			return nil, err
		}
		if now-at <= turnMarkerMaxAgeSecs {
			fresh.Set(key, entry)
			started[key] = at
		}
	}
	if fresh.Len() <= turnMarkerMaxEntries {
		return fresh, nil
	}
	keys := append([]string{}, fresh.Keys...)
	sort.SliceStable(keys, func(i, j int) bool { return started[keys[i]] > started[keys[j]] })
	out := pyjson.NewObject()
	for _, key := range keys[:turnMarkerMaxEntries] {
		out.Set(key, fresh.Vals[key])
	}
	return out, nil
}

// Writes the journal privately, removing the file when no entry remains
func turnMarkerStore(path string, entries *pyjson.Object) error {
	if entries.Len() == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return config.AtomicJSONWriteMode(context.Background(), path, entries, 0o600)
}

// Loads, mutates and stores the journal under both locks, a nil mutation result skipping the write
func turnMarkerUpdate(home, sessionKey string, mutate func(*pyjson.Object) (*pyjson.Object, error), what string) {
	err := func() error {
		path := turnMarkerPath(home)
		turnMarkerLock.Lock()
		defer turnMarkerLock.Unlock()
		release, err := turnMarkerCrossProcessLock(path)
		if err != nil {
			return err
		}
		defer release()
		entries, err := mutate(turnMarkerLoad(path))
		if err != nil || entries == nil {
			return err
		}
		return turnMarkerStore(path, entries)
	}()
	if err != nil {
		turnMarkerLogger().Debug("failed to "+what+" turn marker for "+sessionKey, "error", err)
	}
}

// Persists the marker of a turn about to run with how many auto continues led to it
func RecordTurnStart(home, sessionKey, prompt string, attempts int, autoContinue bool, notificationCategory string) {
	if sessionKey == "" || prompt == "" {
		return
	}
	now := float64(time.Now().UnixNano()) / 1e9
	entry := pyjson.NewObject()
	entry.Set("attempts", int64(max(0, attempts)))
	entry.Set("prompt", pystr.Slice(prompt, 0, turnMarkerMaxPromptChar))
	entry.Set("started_at", now)
	entry.Set("auto_continue", autoContinue)
	pid, start, hasStart := turnMarkerWriterIdentity()
	entry.Set("writer_pid", pid)
	if hasStart {
		entry.Set("writer_start_time", start)
	}
	if notificationCategory == "diagnostic" {
		entry.Set("notification_category", notificationCategory)
	}
	turnMarkerLogger().Debug("turn marker recorded for session " + sessionKey + " by writer pid " + strconv.FormatInt(pid, 10))
	turnMarkerUpdate(home, sessionKey, func(entries *pyjson.Object) (*pyjson.Object, error) {
		pruned, err := turnMarkerPrune(entries, now)
		if err != nil {
			return nil, err
		}
		pruned.Set(sessionKey, entry)
		return pruned, nil
	}, "record")
}

// Removes the marker once its turn concluded
func ClearTurnMarker(home, sessionKey string) {
	if sessionKey == "" {
		return
	}
	turnMarkerUpdate(home, sessionKey, func(entries *pyjson.Object) (*pyjson.Object, error) {
		if _, ok := entries.Get(sessionKey); !ok {
			return nil, nil
		}
		out := pyjson.NewObject()
		for _, key := range entries.Keys {
			if key != sessionKey {
				out.Set(key, entries.Vals[key])
			}
		}
		return out, nil
	}, "clear")
}

// Marker left by a turn that never concluded, nil when none or unreadable
func ReadTurnMarker(home, sessionKey string) map[string]any {
	if sessionKey == "" {
		return nil
	}
	turnMarkerLock.Lock()
	raw, _ := turnMarkerLoad(turnMarkerPath(home)).Get(sessionKey)
	turnMarkerLock.Unlock()
	entry, isObj := raw.(*pyjson.Object)
	prompt := ""
	if isObj {
		if p, _ := entry.Get("prompt"); pyrepr.Truthy(p) {
			prompt = pyrepr.Str(p)
		}
	}
	if pystr.Strip(prompt) == "" {
		return nil
	}
	attempts := int64(0)
	if v, _ := entry.Get("attempts"); pyrepr.Truthy(v) {
		n, err := pyint.Int64(v)
		if err != nil {
			return nil
		}
		attempts = max(0, n)
	}
	startedAt, err := turnMarkerStartedAt(entry)
	if err != nil {
		return nil
	}
	autoContinue, has := entry.Get("auto_continue")
	if !has {
		autoContinue = true
	}
	marker := map[string]any{"attempts": attempts, "prompt": prompt, "started_at": startedAt, "auto_continue": pyrepr.Truthy(autoContinue)}
	for _, key := range []string{"writer_pid", "writer_start_time"} {
		if v, present := entry.Get(key); present && v != nil {
			marker[key] = v
		}
	}
	if category, _ := entry.Get("notification_category"); category == "diagnostic" {
		marker["notification_category"] = "diagnostic"
	}
	return marker
}
