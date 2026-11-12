package tools

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// OnboardingRecordKey is the model config key holding a session's run record
const OnboardingRecordKey = "onboarding_run"

var (
	onbEndedMu        sync.Mutex
	onbEndedListeners []func(map[string]any)
)

// OnOnboardingEnded registers a listener told when a run stops being in progress
func OnOnboardingEnded(listener func(ended map[string]any)) {
	onbEndedMu.Lock()
	onbEndedListeners = append(onbEndedListeners, listener)
	onbEndedMu.Unlock()
}

// Session store the onboarding record reads and writes, the canonical SessionDB or a managed worker's store
type OnboardingStore interface {
	GetModelConfigValue(sessionID, key string) (any, bool, error)
	UpdateConversationModelConfigValue(sessionID, key string, update func(current any) any) (string, error)
	SessionIDsWithModelConfigKey(key string) ([]string, error)
	ResolveResumeSessionID(sessionID string) (string, error)
}

// Whether the store is absent, a typed nil pointer read as None
func onboardingStoreNil(db OnboardingStore) bool {
	if db == nil {
		return true
	}
	v := reflect.ValueOf(db)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// LoadOnboardingRecord reads the session's run record, nil when none
func LoadOnboardingRecord(db OnboardingStore, sessionID string) map[string]any {
	if onboardingStoreNil(db) || sessionID == "" {
		return nil
	}
	value, ok, err := db.GetModelConfigValue(sessionID, OnboardingRecordKey)
	if err != nil || !ok {
		return nil
	}
	record, _ := value.(map[string]any)
	return record
}

// Tells every listener that a run ended
func onbAnnounce(runID, sessionID, outcome string) {
	ended := map[string]any{"run_id": runID, "stored_session_id": sessionID, "outcome": outcome}
	onbEndedMu.Lock()
	listeners := append([]func(map[string]any){}, onbEndedListeners...)
	onbEndedMu.Unlock()
	for _, listener := range listeners {
		listener(ended)
	}
}

// String of a record field, empty when absent
func onbRecordString(record map[string]any, key string) string {
	if v, ok := record[key].(string); ok {
		return v
	}
	return ""
}

// Unix seconds now as a float
func onbNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Applies the event at the compression tip in one transaction, closing the install flag when the run ended
func onbTransition(ctx context.Context, db OnboardingStore, sessionID string, event OnboardingEvent) (*OnboardingStep, error) {
	var step *OnboardingStep
	wasActive := false
	_, err := db.UpdateConversationModelConfigValue(sessionID, OnboardingRecordKey, func(current any) any {
		step = nil
		record, ok := current.(map[string]any)
		if !ok {
			return current
		}
		wasActive = OnboardingActive(record)
		s := OnboardingApply(record, event, OnboardingFactsFromStored(asMapOrEmpty(record["facts"])), onbNow())
		step = &s
		return s.Record
	})
	if err != nil {
		return nil, err
	}
	if step != nil && wasActive && !OnboardingActive(step.Record) {
		onbAnnounce(onbRecordString(step.Record, "run_id"), sessionID, onbRecordString(step.Record, "ended_by"))
	}
	if step != nil && !OnboardingActive(step.Record) && ActiveOnboardingSessionID(db) == "" {
		if err := onbCloseInstallFlag(ctx); err != nil {
			return step, err
		}
	}
	return step, nil
}

// Writes the install level flag closed unless the root config already says so
func onbCloseInstallFlag(ctx context.Context) error {
	doc, err := config.ReadDocument(home.ConfigPath(home.Root()))
	if err != nil {
		doc = map[string]any{}
	}
	section, _ := doc["onboarding"].(map[string]any)
	seen, _ := section["seen"].(map[string]any)
	run, isBool := section["run"].(bool)
	if !isBool || run || !truthy(seen[OnboardingProfileBuildFlag]) {
		return SetOnboardingRun(ctx, false, true)
	}
	return nil
}

// Run record writer of every start, replaced by a case as the hermes cases patch onboarding_record.start
var StartOnboarding = startOnboarding

// Writes a fresh run record and marks the install as mid onboarding
func startOnboarding(ctx context.Context, db *state.DB, sessionID string, facts OnboardingFacts) (map[string]any, error) {
	record := NewOnboardingRecord(facts, onbNow(), state.NewUID())
	if err := db.PatchModelConfig(sessionID, map[string]any{OnboardingRecordKey: record}); err != nil {
		return nil, err
	}
	if err := SetOnboardingRun(ctx, true, false); err != nil {
		return nil, err
	}
	return record, nil
}

// AdvanceOnboarding applies one event to the stored run, an error when the session has no run
func AdvanceOnboarding(ctx context.Context, db OnboardingStore, sessionID string, event OnboardingEvent) (OnboardingStep, error) {
	if onboardingStoreNil(db) || sessionID == "" {
		return OnboardingStep{}, fmt.Errorf("session %s has no onboarding run", sessionID)
	}
	step, err := onbTransition(ctx, db, sessionID, event)
	if err != nil {
		return OnboardingStep{}, err
	}
	if step == nil {
		return OnboardingStep{}, fmt.Errorf("session %s has no onboarding run", sessionID)
	}
	return *step, nil
}

// OpenOnboardingSessionIDs lists the chats whose run is still in progress, newest first, each at its compression tip
func OpenOnboardingSessionIDs(db OnboardingStore) []string {
	ids, err := db.SessionIDsWithModelConfigKey(OnboardingRecordKey)
	if err != nil {
		return nil
	}
	tips := map[string]bool{}
	var open []string
	for _, sid := range ids {
		tip, err := db.ResolveResumeSessionID(sid)
		if err != nil {
			return nil
		}
		if tip == "" {
			tip = sid
		}
		if tips[tip] {
			continue
		}
		tips[tip] = true
		if OnboardingActive(LoadOnboardingRecord(db, tip)) {
			open = append(open, tip)
		}
	}
	return open
}

// ActiveOnboardingSessionID is the newest chat with a run in progress, empty when none
func ActiveOnboardingSessionID(db OnboardingStore) string {
	open := OpenOnboardingSessionIDs(db)
	if len(open) == 0 {
		return ""
	}
	return open[0]
}

// DeleteOnboardingChat runs the delete and, when it removed the chat holding the live run, ends that run on every copy
func DeleteOnboardingChat(ctx context.Context, db *state.DB, sessionID string, deleteChat func() bool) (bool, error) {
	record := LoadOnboardingRecord(db, sessionID)
	tip, err := db.ResolveResumeSessionID(sessionID)
	if err != nil {
		return false, err
	}
	if tip == "" {
		tip = sessionID
	}
	live := OnboardingActive(record) && tip == sessionID
	last := live
	if live {
		for _, sid := range OpenOnboardingSessionIDs(db) {
			if sid != sessionID {
				last = false
			}
		}
	}
	if last {
		if err := onbCloseInstallFlag(ctx); err != nil {
			return false, err
		}
	}
	deleted := false
	func() {
		defer func() {
			if last && !deleted {
				SetOnboardingRun(ctx, true, false)
			}
		}()
		deleted = deleteChat()
	}()
	if !deleted {
		return false, nil
	}
	if live {
		runID := onbRecordString(record, "run_id")
		if runID != "" {
			ids, _ := db.SessionIDsWithModelConfigKey(OnboardingRecordKey)
			for _, sid := range ids {
				older := LoadOnboardingRecord(db, sid)
				if older != nil && onbRecordString(older, "run_id") == runID {
					db.PatchModelConfig(sid, map[string]any{OnboardingRecordKey: onbEndDeleted(older)})
				}
			}
		}
		onbAnnounce(runID, sessionID, "deleted")
	}
	return true, nil
}

// Record ended as deleted, unchanged when the run is not active
func onbEndDeleted(current map[string]any) map[string]any {
	if !OnboardingActive(current) {
		return current
	}
	return OnboardingApply(current, OnbEnd{"deleted"}, OnboardingFactsFromStored(asMapOrEmpty(current["facts"])), onbNow()).Record
}

// ObserveOnboardingToolResult settles a beat from a general tool's result and adds the next step reminder when it did
func ObserveOnboardingToolResult(ctx context.Context, db OnboardingStore, sessionID string, hasSetupBeat bool, name string, args map[string]any, result string) string {
	if !OnboardingAppliesTo(name) || !hasSetupBeat {
		return result
	}
	decoded, err := pyjson.DecodeWTF8([]byte(result))
	parsed, _ := decoded.(map[string]any)
	if err != nil || parsed == nil {
		return result
	}
	if LoadOnboardingRecord(db, sessionID) == nil {
		return result
	}
	if args == nil {
		args = map[string]any{}
	}
	step, err := AdvanceOnboarding(ctx, db, sessionID, OnbApplied{Tool: name, Args: copyMap(args), Result: parsed})
	if err != nil {
		toolsLogger(nil).Warn(fmt.Sprintf("onboarding: %s result did not update the run record", name), "error", err)
		return result
	}
	text := OnboardingReminder(step.Reminder, step.Record, OnboardingFactsFromStored(asMapOrEmpty(step.Record["facts"])), "", nil)
	if text == "" {
		return result
	}
	ordered, err := pyjson.Loads(result)
	out, _ := ordered.(*pyjson.Object)
	if err != nil || out == nil {
		return result
	}
	out.Set("reminder", text)
	dumped, err := pyjson.Dumps(out, pyjson.EnsureASCII(false))
	if err != nil {
		return result
	}
	return dumped
}
