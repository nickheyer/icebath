package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// File name of the emergency stop sentinel under a home
const EstopSentinelName = "ESTOP"

var (
	estopLogMu            sync.Mutex
	estopLoggedComponents = map[string]bool{}
)

// Stat the engaged check runs, a var the fail safe test turns into an error
var estopStat = os.Stat

// Path of the ESTOP sentinel this process writes on icebath pause
func SentinelPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), EstopSentinelName)
}

// Profile home sentinel first, then the fleet root sentinel when that is a different directory
func candidateSentinelPaths(ctx context.Context) []string {
	primary := SentinelPath(ctx)
	root := filepath.Join(home.Root(), EstopSentinelName)
	resolvedRoot, errRoot := pypath.Resolve(root)
	resolvedPrimary, errPrimary := pypath.Resolve(primary)
	distinct := root != primary
	if errRoot == nil && errPrimary == nil {
		distinct = resolvedRoot != resolvedPrimary
	}
	if distinct {
		return []string{primary, root}
	}
	return []string{primary}
}

// True when any candidate sentinel exists, fail safe true on a stat error
func IsEngaged(ctx context.Context) bool {
	sawStatError := false
	for _, path := range candidateSentinelPaths(ctx) {
		if _, err := estopStat(path); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			sawStatError = true
		}
	}
	return sawStatError
}

// Creates the ESTOP sentinel with its reason and time, an empty sentinel still pausing when the body cannot be written
func Engage(ctx context.Context, reason string) string {
	path := SentinelPath(ctx)
	var reasonValue any
	if reason != "" {
		reasonValue = reason
	}
	payload := pyjson.ObjectOf("engaged_at", time.Now().UTC().Format("2006-01-02T15:04:05.999999-07:00"), "reason", reasonValue)
	body, err := pyjson.Dumps(payload, pyjson.Indent(2))
	if err != nil {
		body = "{}"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, []byte(body+"\n"), 0o644)
		if err == nil {
			return path
		}
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	return path
}

// Removes every visible sentinel, true when at least one was removed
func Disengage(ctx context.Context) bool {
	lifted := false
	for _, path := range candidateSentinelPaths(ctx) {
		if err := os.Remove(path); err == nil {
			lifted = true
		}
	}
	return lifted
}

// Reason and engaged time of the active pause, nil when not engaged, both empty for an unreadable body
type EstopState struct {
	Reason, EngagedAt string
}

// State of the pause, nil when not engaged, a corrupt body still reporting engaged with empty fields
func GetState(ctx context.Context) *EstopState {
	if !IsEngaged(ctx) {
		return nil
	}
	state := &EstopState{}
	found := false
	for _, path := range candidateSentinelPaths(ctx) {
		if _, err := estopStat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return state
		}
		found = true
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := estopStripBOM(data)
		if !utf8.Valid(body) {
			continue
		}
		parsed, err := pyjson.Loads(string(body))
		if err != nil {
			continue
		}
		raw, ok := parsed.(*pyjson.Object)
		if !ok {
			continue
		}
		state = &EstopState{Reason: stringField(raw.Vals, "reason"), EngagedAt: stringField(raw.Vals, "engaged_at")}
		break
	}
	if !found {
		return nil
	}
	return state
}

// Bytes without a leading UTF-8 byte order mark
func estopStripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
		return data[3:]
	}
	return data
}

// Python str of a truthy JSON object field, empty for a missing or falsy value
func stringField(raw map[string]any, key string) string {
	if !pyrepr.Truthy(raw[key]) {
		return ""
	}
	return pyrepr.Str(raw[key])
}

// Short notice for new gateway turns while paused, empty when not paused
func PausedReply(ctx context.Context) string {
	state := GetState(ctx)
	if state == nil {
		return ""
	}
	tag := ""
	if state.Reason != "" {
		tag = " (" + state.Reason + ")"
	}
	return "⏸️ Hermes is paused" + tag + ". New work is on hold; run `icebath resume` to pick things back up."
}

// True when engaged, logging once per engagement per component and rearming after a resume
func CheckPaused(ctx context.Context, component string, log *slog.Logger) bool {
	if !IsEngaged(ctx) {
		estopLogMu.Lock()
		delete(estopLoggedComponents, component)
		estopLogMu.Unlock()
		return false
	}
	estopLogMu.Lock()
	first := !estopLoggedComponents[component]
	estopLoggedComponents[component] = true
	estopLogMu.Unlock()
	if first {
		suffix := ""
		if state := GetState(ctx); state != nil && state.Reason != "" {
			suffix = " (reason: " + state.Reason + ")"
		}
		if log == nil {
			log = slog.Default()
		}
		log.Info(fmt.Sprintf("%s dispatch paused by global emergency stop%s — remove with `icebath resume` (%s)",
			component, suffix, SentinelPath(ctx)))
	}
	return true
}
