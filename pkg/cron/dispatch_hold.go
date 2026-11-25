package cron

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
		since = at
		if held := readDispatchHeld(s.cronDir); held != nil && at-held.Last <= 2*DispatchHoldTickSlackSeconds {
			since = held.Since
		}
		holdSince[s.cronDir] = since
	}
	holdSinceMu.Unlock()
	record := jsonutil.NewObject()
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

// Hold stamp of a cron dir, nil when missing, unreadable or malformed, a number that is not finite counting as malformed
func readDispatchHeld(cronDir string) *dispatchHeld {
	data, err := os.ReadFile(filepath.Join(cronDir, dispatchHeldFile))
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		return nil
	}
	obj, ok := parsed.(*jsonutil.Object)
	if !ok {
		return nil
	}
	sinceRaw, hasSince := obj.Get("since")
	lastRaw, hasLast := obj.Get("last")
	if !hasSince || !hasLast {
		return nil
	}
	since, last := heldFloat(sinceRaw), heldFloat(lastRaw)
	if since == nil || last == nil {
		return nil
	}
	return &dispatchHeld{Since: *since, Last: *last}
}

// Epoch seconds of a stamp value given as a number or numeric text, nil when it is no number or not finite
func heldFloat(v any) *float64 {
	if _, isBool := v.(bool); isBool {
		return nil
	}
	f, err := anyutil.ToFloat(v)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	return &f
}

// Instant a one shot due at dueAt starts its grace, the end of a hold it came due in, else its due time
func dispatchHoldGraceStartsAt(cronDir string, dueAt, grace float64) float64 {
	held := readDispatchHeld(cronDir)
	if held != nil && held.Since-grace <= dueAt && dueAt <= held.Last+DispatchHoldTickSlackSeconds &&
		dueAt >= held.Last-DispatchHoldMaxExtensionSeconds {
		return max(dueAt, held.Last+DispatchHoldTickSlackSeconds)
	}
	return dueAt
}
