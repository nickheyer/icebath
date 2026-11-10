package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	dispatchHeldFile = "dispatch_held.json"
	// DispatchHoldTickSlackSeconds is the tick spacing a held one shot's grace restarts after
	DispatchHoldTickSlackSeconds = 60.0
	// DispatchHoldMaxExtensionSeconds bounds how long before the hold end a due one shot is extended
	DispatchHoldMaxExtensionSeconds = 1800.0
)

var (
	holdSinceMu sync.Mutex
	holdSince   = map[string]float64{}
)

// Hold window a drain stamped into a cron store
type dispatchHeld struct {
	Since float64
	Last  float64
}

// Wall clock seconds the hold stamps use
func dispatchHoldNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Stamps that this tick skipped dispatch for the store, keeping the start of a hold that crossed a restart
func (s *Store) noteHeld(ctx context.Context, now *float64) error {
	at := dispatchHoldNow()
	if now != nil {
		at = *now
	}
	holdSinceMu.Lock()
	since, ok := holdSince[s.cronDir]
	if !ok {
		held, err := readDispatchHeld(s.cronDir)
		if err != nil {
			holdSinceMu.Unlock()
			return err
		}
		since = at
		if held != nil && at-held.Last <= 2*DispatchHoldTickSlackSeconds {
			since = held.Since
		}
		holdSince[s.cronDir] = since
	}
	holdSinceMu.Unlock()
	record := pyjson.NewObject()
	record.Set("since", since)
	record.Set("last", at)
	if err := config.AtomicJSONWrite(home.WithOverride(ctx, s.home), filepath.Join(s.cronDir, dispatchHeldFile), record); err != nil && !isOSError(err) {
		return err
	}
	return nil
}

// Ends this process's hold on the store once the ticker dispatches it again
func (s *Store) noteDispatching() {
	holdSinceMu.Lock()
	delete(holdSince, s.cronDir)
	holdSinceMu.Unlock()
}

// Stamps the hold for every profile home a multiplex ticker serves, each in its own cron store
func noteHeldProfiles(ctx context.Context, homes []string) error {
	for _, h := range homes {
		if err := openCronStore(h, nil).noteHeld(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

// Hold stamp of a cron dir, nil when missing, unreadable or malformed, an error only for a number float overflows
func readDispatchHeld(cronDir string) (*dispatchHeld, error) {
	data, err := os.ReadFile(filepath.Join(cronDir, dispatchHeldFile))
	if err != nil || !utf8.Valid(data) {
		return nil, nil
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	sinceRaw, hasSince := obj.Get("since")
	if !hasSince {
		return nil, nil
	}
	since, err := heldFloat(sinceRaw)
	if err != nil || since == nil {
		return nil, err
	}
	lastRaw, hasLast := obj.Get("last")
	if !hasLast {
		return nil, nil
	}
	last, err := heldFloat(lastRaw)
	if err != nil || last == nil {
		return nil, err
	}
	return &dispatchHeld{Since: *since, Last: *last}, nil
}

// Python float of a stamp value, nil when float rejects it and an error when it overflows
func heldFloat(v any) (*float64, error) {
	f, err := pyint.Float(v)
	if err != nil {
		var pe *pyint.Error
		if errors.As(err, &pe) && pe.Kind == "OverflowError" {
			return nil, err
		}
		return nil, nil
	}
	return &f, nil
}

// Instant a one shot due at dueAt starts its grace, the end of a hold it came due in, else its due time
func dispatchHoldGraceStartsAt(cronDir string, dueAt, grace float64) (float64, error) {
	held, err := readDispatchHeld(cronDir)
	if err != nil {
		return 0, err
	}
	if held != nil && held.Since-grace <= dueAt && dueAt <= held.Last+DispatchHoldTickSlackSeconds &&
		dueAt >= held.Last-DispatchHoldMaxExtensionSeconds {
		return max(dueAt, held.Last+DispatchHoldTickSlackSeconds), nil
	}
	return dueAt, nil
}
