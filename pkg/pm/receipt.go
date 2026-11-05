package pm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const receiptKeep = 20

// Per context receipt state, the analogue of the receipt ContextVars
type receiptScope struct {
	mu                sync.Mutex
	current           map[string]any
	completedByUpdate map[string]map[string]any
	workerSet         bool
	workerUpdate      *string
	lastCompleted     map[string]any
}

type receiptScopeKey struct{}

type correlationKey struct{}

// Scope of contexts that carry none, the process main context
var defaultReceiptScope = &receiptScope{}

// Context with a fresh receipt scope whose bindings copy the parent's, so records stay local to it
func WithReceiptScope(ctx context.Context) context.Context {
	parent := scopeOf(ctx)
	parent.mu.Lock()
	child := &receiptScope{current: parent.current, completedByUpdate: parent.completedByUpdate,
		workerSet: parent.workerSet, workerUpdate: parent.workerUpdate, lastCompleted: parent.lastCompleted}
	parent.mu.Unlock()
	return context.WithValue(ctx, receiptScopeKey{}, child)
}

// Context whose ambient update correlation id comes from the open update receipt the provider reads
func WithUpdateCorrelation(ctx context.Context, provider func() string) context.Context {
	return context.WithValue(ctx, correlationKey{}, provider)
}

// Receipt scope of a context, the default scope when it carries none
func scopeOf(ctx context.Context) *receiptScope {
	if ctx != nil {
		if s, ok := ctx.Value(receiptScopeKey{}).(*receiptScope); ok {
			return s
		}
	}
	return defaultReceiptScope
}

// Carries the worker's update correlation into the scope until the returned restore runs
func WorkerContext(ctx context.Context, updateID *string) func() {
	s := scopeOf(ctx)
	s.mu.Lock()
	prevSet, prevUpdate, prevCompleted := s.workerSet, s.workerUpdate, s.lastCompleted
	s.workerSet, s.workerUpdate, s.lastCompleted = true, updateID, nil
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.lastCompleted = prevCompleted
		s.workerSet, s.workerUpdate = prevSet, prevUpdate
		s.mu.Unlock()
	}
}

// Deep copy of the last receipt finalized in this scope
func LastCompleted(ctx context.Context) map[string]any {
	s := scopeOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyReceipt(s.lastCompleted)
}

// Files a worker's finished receipt under its update id after checking the correlation
func AcceptWorkerReceipt(ctx context.Context, data map[string]any, updateID *string) error {
	if data == nil {
		return nil
	}
	if !jsonEquals(data["update_id"], optionalString(updateID)) {
		return errors.New("PM worker receipt correlation mismatch")
	}
	if updateID != nil && *updateID != "" {
		s := scopeOf(ctx)
		s.mu.Lock()
		s.completedByUpdate = withCompleted(s.completedByUpdate, *updateID, data)
		s.mu.Unlock()
	}
	return nil
}

// JSON value of an optional string, nil standing for None
func optionalString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// Copy on write insertion into the completed by update map
func withCompleted(m map[string]map[string]any, id string, data map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[id] = copyReceipt(data)
	return out
}

// Update correlation id in force, the worker's carried id first, else the open update receipt's
func ambientUpdateID(ctx context.Context, s *receiptScope) any {
	if s.workerSet {
		return optionalString(s.workerUpdate)
	}
	if ctx != nil {
		if provider, ok := ctx.Value(correlationKey{}).(func() string); ok && provider != nil {
			if id := provider(); id != "" {
				return id
			}
		}
	}
	return nil
}

// Current UTC time in Python isoformat with an explicit offset
func utcNowISO() string {
	now := time.Now().UTC()
	if now.Nanosecond()/1000 == 0 {
		return now.Format("2006-01-02T15:04:05-07:00")
	}
	return now.Format("2006-01-02T15:04:05.000000-07:00")
}

// Receipts directory of the active home, a pure path with no mkdir
func receiptDir(ctx context.Context) string {
	return filepath.Join(home.LogsDir(home.Dir(ctx)), "update_receipts")
}

// ReceiptToken restores the receipt that was current before a nested begin
type ReceiptToken struct {
	scope    *receiptScope
	previous map[string]any
	used     bool
}

// Starts recording a sync of kind stamped with the ambient update id, returning the restoring token
func BeginReceipt(ctx context.Context, kind string) *ReceiptToken {
	s := scopeOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	token := &ReceiptToken{scope: s, previous: s.current}
	s.current = map[string]any{
		"schema":       jsonInt(1),
		"kind":         kind,
		"update_id":    ambientUpdateID(ctx, s),
		"started_at":   utcNowISO(),
		"steps":        []any{},
		"venv_rebuild": nil,
		"feature_list": nil,
		"platform":     nil,
		"outcome":      nil,
		"warnings":     []any{},
		"refusal":      nil,
	}
	return token
}

// Copy on write mutation of the in flight receipt, a no op when nothing was begun
func recordReceipt(ctx context.Context, mutate func(map[string]any)) {
	s := scopeOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return
	}
	updated := make(map[string]any, len(s.current))
	for k, v := range s.current {
		updated[k] = v
	}
	mutate(updated)
	s.current = copyReceipt(updated)
}

// Deep copy of a receipt document
func copyReceipt(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return deepCopyJSON(m).(map[string]any)
}

// List value of a receipt field
func receiptList(v any) []any {
	list, _ := v.([]any)
	return list
}

// Records one named step and its result
func RecordStep(ctx context.Context, name string, ok bool, detail string) {
	recordReceipt(ctx, func(r map[string]any) {
		r["steps"] = append(append([]any{}, receiptList(r["steps"])...),
			map[string]any{"name": name, "ok": ok, "detail": detail, "at": utcNowISO()})
	})
}

// Records whether the dependency environment rebuild succeeded and why
func RecordVenvRebuild(ctx context.Context, ok bool, reason string) {
	recordReceipt(ctx, func(r map[string]any) { r["venv_rebuild"] = map[string]any{"ok": ok, "reason": reason} })
}

// Records the selected extras, nil standing for none recorded
func RecordFeatureList(ctx context.Context, extras []string) {
	recordReceipt(ctx, func(r map[string]any) {
		if extras == nil {
			r["feature_list"] = nil
			return
		}
		list := make([]any, len(extras))
		for i, e := range extras {
			list[i] = e
		}
		r["feature_list"] = list
	})
}

// Records the platform id the sync ran for
func RecordPlatform(ctx context.Context, platformID string) {
	recordReceipt(ctx, func(r map[string]any) { r["platform"] = platformID })
}

// JSONRecord is a check result that serializes itself into the receipt
type JSONRecord interface {
	ToJSON() map[string]any
}

// Records plugin update check results, each serialized when it knows how
func RecordPluginChecks(ctx context.Context, results []any) {
	recordReceipt(ctx, func(r map[string]any) {
		list := make([]any, len(results))
		for i, item := range results {
			if rec, ok := item.(JSONRecord); ok {
				list[i] = rec.ToJSON()
			} else {
				list[i] = item
			}
		}
		r["plugin_checks"] = list
	})
}

// Records a warning that reached the user during this sync
func RecordWarning(ctx context.Context, message string) {
	recordReceipt(ctx, func(r map[string]any) {
		r["warnings"] = append(append([]any{}, receiptList(r["warnings"])...),
			map[string]any{"message": message, "at": utcNowISO()})
	})
}

// Records why this sync refused to act
func RecordRefusal(ctx context.Context, code, detail string) {
	recordReceipt(ctx, func(r map[string]any) {
		r["refusal"] = map[string]any{"code": code, "detail": detail, "at": utcNowISO()}
	})
}

// Deep copy of the in flight receipt, nil when nothing was begun
func ReceiptSnapshot(ctx context.Context) map[string]any {
	s := scopeOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyReceipt(s.current)
}

// Writes and rotates the receipt, a token restoring the outer receipt and panicking on reuse or another scope
func FinalizeReceipt(ctx context.Context, outcome string, exitCode int, token *ReceiptToken) string {
	s := scopeOf(ctx)
	s.mu.Lock()
	current := copyReceipt(s.current)
	if current == nil {
		s.mu.Unlock()
		return ""
	}
	if token != nil {
		if token.used {
			s.mu.Unlock()
			panic("receipt token has already been used once")
		}
		if token.scope != s {
			s.mu.Unlock()
			panic("receipt token was created in a different receipt scope")
		}
		token.used = true
		s.current = token.previous
	} else {
		s.current = nil
	}
	current["outcome"] = outcome
	current["exit_code"] = jsonInt(exitCode)
	current["finished_at"] = utcNowISO()
	s.lastCompleted = copyReceipt(current)
	if id, ok := current["update_id"].(string); ok && id != "" {
		s.completedByUpdate = withCompleted(s.completedByUpdate, id, current)
	}
	s.mu.Unlock()
	path, err := writeRotatedReceipt(ctx, current)
	if err != nil {
		return ""
	}
	return path
}

// Last receipt completed in this scope under an update id, optionally consuming it
func LastForUpdate(ctx context.Context, updateID string, consume bool) map[string]any {
	if updateID == "" {
		return nil
	}
	s := scopeOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	completed := s.completedByUpdate[updateID]
	if consume {
		entries := make(map[string]map[string]any, len(s.completedByUpdate))
		for k, v := range s.completedByUpdate {
			if k != updateID {
				entries[k] = v
			}
		}
		s.completedByUpdate = entries
	}
	return copyReceipt(completed)
}

// Newest receipt of any kind, a pure read that never creates the receipts dir
func LatestReceipt(ctx context.Context) map[string]any {
	point := filepath.Join(receiptDir(ctx), "latest.json")
	st, err := os.Stat(point)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(point)
	if err != nil {
		return nil
	}
	v, err := loadJSON(data)
	if err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// Unique receipt file name from a UTC stamp, the kind, the pid and a random uuid
func receiptName(data map[string]any) string {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	kind, _ := data["kind"].(string)
	if kind == "" {
		kind = "sync"
	}
	return fmt.Sprintf("pm_%s-%s-%d-%s.json", stamp, kind, os.Getpid(), randomHex32())
}

// Thirty two hex digits of randomness, the shape of a uuid4 hex
func randomHex32() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Writes the receipt and latest.json under the shared write lock and rotates old receipts
func writeRotatedReceipt(ctx context.Context, data map[string]any) (string, error) {
	d := receiptDir(ctx)
	if err := os.MkdirAll(d, 0o777); err != nil {
		return "", err
	}
	path := filepath.Join(d, receiptName(data))
	lock, err := os.OpenFile(filepath.Join(d, ".pm-write.lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if _, err := LockFD(lock, true, WaitForever); err != nil {
		return "", err
	}
	if err := writeLockDocument(path, data); err != nil {
		return "", err
	}
	if err := writeLockDocument(filepath.Join(d, "latest.json"), data); err != nil {
		return "", err
	}
	rotateReceipts(d)
	return path, nil
}

// Keeps the newest PM receipts, leaving the updater's own receipts alone
func rotateReceipts(d string) {
	entries, err := os.ReadDir(d)
	if err != nil {
		return
	}
	var receipts []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "pm_") && strings.HasSuffix(name, ".json") && isRegularFile(filepath.Join(d, name)) {
			receipts = append(receipts, name)
		}
	}
	sort.Strings(receipts)
	if len(receipts) <= receiptKeep {
		return
	}
	for _, stale := range receipts[:len(receipts)-receiptKeep] {
		_ = os.Remove(filepath.Join(d, stale))
	}
}
