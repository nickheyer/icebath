package tuihost

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	turnMarkerMaxAgeSecs    = 24 * 3600
	turnMarkerMaxEntries    = 32
	turnMarkerMaxPromptChar = 64_000
)

var turnMarkerLock sync.Mutex

func turnMarkerLogger() *slog.Logger { return slog.Default().With("logger", "tuigateway") }

// Interrupted turns journal of a profile home
func turnMarkerPath(home string) string {
	return filepath.Join(home, "interrupted_turns.json")
}

// Cross process lock beside the journal so sibling backends never resurrect a cleared marker
func turnMarkerCrossProcessLock(path string) (func(), error) {
	return provider.FileLock(path+".lock", 5*time.Second, "Timed out waiting for the turn-marker lock ("+path+")")
}

// Start time of an entry as a float, zero when falsy, the float error when unparseable
func turnMarkerStartedAt(entry *jsonutil.Object) (float64, error) {
	v, _ := entry.Get("started_at")
	if !anyutil.IsTrue(v) {
		return 0, nil
	}
	return anyutil.ToFloat(v)
}

// Pid and process start time of the writer so a recycled pid never passes as it
func turnMarkerWriterIdentity() (int64, float64, bool) {
	start, ok := activesessions.OwnStartTime()
	return int64(os.Getpid()), start, ok
}

// Journal entries whose values are objects, empty when missing or unreadable
func turnMarkerLoad(path string) *jsonutil.Object {
	out := jsonutil.NewObject()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			turnMarkerLogger().Debug("unreadable turn-marker file "+path+"; starting fresh", "error", err)
		}
		return out
	}
	data, err := jsonutil.Decode(string(raw))
	if err != nil {
		turnMarkerLogger().Debug("unreadable turn-marker file "+path+"; starting fresh", "error", err)
		return out
	}
	obj, ok := data.(*jsonutil.Object)
	if !ok {
		return out
	}
	for _, key := range obj.Keys() {
		if entry, isObj := obj.Value(key).(*jsonutil.Object); isObj {
			out.Set(key, entry)
		}
	}
	return out
}

// Entries younger than the age cap, the newest ones when more than the count cap remain
func turnMarkerPrune(entries *jsonutil.Object, now float64) (*jsonutil.Object, error) {
	fresh := jsonutil.NewObject()
	started := map[string]float64{}
	for _, key := range entries.Keys() {
		entry := entries.Value(key).(*jsonutil.Object)
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
	keys := append([]string{}, fresh.Keys()...)
	sort.SliceStable(keys, func(i, j int) bool { return started[keys[i]] > started[keys[j]] })
	out := jsonutil.NewObject()
	for _, key := range keys[:turnMarkerMaxEntries] {
		out.Set(key, fresh.Value(key))
	}
	return out, nil
}

// Writes the journal privately, removing the file when no entry remains
func turnMarkerStore(path string, entries *jsonutil.Object) error {
	if entries.Len() == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return config.AtomicJSONWriteMode(context.Background(), path, entries, 0o600, jsonutil.Spaced())
}

// Loads, mutates and stores the journal under both locks, a nil mutation result skipping the write
func turnMarkerUpdate(home, sessionKey string, mutate func(*jsonutil.Object) (*jsonutil.Object, error), what string) {
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
	entry := jsonutil.NewObject()
	entry.Set("attempts", int64(max(0, attempts)))
	entry.Set("prompt", textutil.First(prompt, turnMarkerMaxPromptChar))
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
	turnMarkerUpdate(home, sessionKey, func(entries *jsonutil.Object) (*jsonutil.Object, error) {
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
	turnMarkerUpdate(home, sessionKey, func(entries *jsonutil.Object) (*jsonutil.Object, error) {
		if _, ok := entries.Get(sessionKey); !ok {
			return nil, nil
		}
		out := jsonutil.NewObject()
		for _, key := range entries.Keys() {
			if key != sessionKey {
				out.Set(key, entries.Value(key))
			}
		}
		return out, nil
	}, "clear")
}
