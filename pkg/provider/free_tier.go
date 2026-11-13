package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// SetupReadyWait is how long the desktop's first setup.status waits for the record before a live probe
	SetupReadyWait  = 8 * time.Second
	SetupReadyEvent = "setup.ready"
)

// SetupRecord is what the startup bootstrap found about who carries inference
type SetupRecord struct {
	ProviderConfigured bool
	InferenceProvider  string
	FreeTierAccount    bool
	HasIdentity        bool
	OtherProviders     bool
	Error              string
	Failure            map[string]any
	FinishedAt         float64
}

// The free tier guest carries inference and the resolver picked nous
func (r *SetupRecord) FreeTierRoute() bool {
	return r.FreeTierAccount && r.InferenceProvider == "nous"
}

// Wire payload with the failure block spread flat and the free tier route flag added
func (r *SetupRecord) AsPayload() map[string]any {
	payload := map[string]any{
		"provider_configured": r.ProviderConfigured,
		"inference_provider":  r.InferenceProvider,
		"free_tier_account":   r.FreeTierAccount,
		"has_identity":        r.HasIdentity,
		"other_providers":     r.OtherProviders,
		"error":               r.Error,
		"finished_at":         r.FinishedAt,
	}
	for k, v := range r.Failure {
		payload[k] = v
	}
	payload["free_tier_route"] = r.FreeTierRoute()
	return payload
}

// Copy of the mint memo verdict the record carries
func (r *SetupRecord) FailureFields() map[string]any {
	out := make(map[string]any, len(r.Failure))
	for k, v := range r.Failure {
		out[k] = v
	}
	return out
}

type setupStampEntry struct {
	version string
	present bool
}

type setupConfigStamp [3]setupStampEntry

var (
	setupMu             sync.Mutex
	setupRecord         *SetupRecord
	setupDone           = make(chan struct{})
	setupDoneSet        bool
	setupStarted        bool
	setupSettled        bool
	setupResettle       sync.Mutex
	setupInventory      *setupConfigStamp
	setupInventoryFiles = [3]string{"config.yaml", ".env", "auth.json"}
	setupNow            = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

// The record, nil until the first bootstrap finishes
func CurrentSetupRecord() *SetupRecord {
	setupMu.Lock()
	defer setupMu.Unlock()
	return setupRecord
}

// Waits for an in flight bootstrap then returns its record reconciled, nil when none ever started
func WaitForSetupRecord(env PoolEnv, timeout time.Duration) *SetupRecord {
	setupMu.Lock()
	started, done := setupStarted, setupDone
	setupMu.Unlock()
	if !started {
		return nil
	}
	timer := time.NewTimer(timeout)
	select {
	case <-done:
	case <-timer.C:
	}
	timer.Stop()
	return ReconcileSetupRecord(env)
}

// Replaces a record that found no provider once the launch profile's config files moved and one now carries inference
func ReconcileSetupRecord(env PoolEnv) *SetupRecord {
	setupMu.Lock()
	record, inventory := setupRecord, setupInventory
	setupMu.Unlock()
	if record == nil || record.ProviderConfigured {
		return record
	}
	if home.Key(envHome(env)) != home.Key(home.Process()) {
		return record
	}
	if stamp := setupStampConfig(env); inventory != nil && *inventory == stamp {
		return record
	}
	if !setupInventoryFn(env) {
		return CurrentSetupRecord()
	}
	refreshed := *record
	refreshed.ProviderConfigured, refreshed.OtherProviders = true, true
	refreshed.InferenceProvider = setupResolveInference(env)
	refreshed.FinishedAt = setupNow()
	setupMu.Lock()
	if setupRecord != record {
		current := setupRecord
		setupMu.Unlock()
		return current
	}
	setupRecord = &refreshed
	setupMu.Unlock()
	setupDueChanged()
	setupBroadcast(env, &refreshed)
	return &refreshed
}

// Clears the process record, the started and settled flags and the inventory stamp
func resetFreeTierBootstrapForTests() {
	setupMu.Lock()
	defer setupMu.Unlock()
	setupRecord, setupStarted, setupSettled, setupInventory = nil, false, false, nil
	setupDone, setupDoneSet = make(chan struct{}), false
}

// Version stamps of the inventory files in the current home, absent for an unreadable file
func setupStampConfig(env PoolEnv) setupConfigStamp {
	var stamp setupConfigStamp
	h := envHome(env)
	for i, name := range setupInventoryFiles {
		if v, err := fileCacheVersion(filepath.Join(h, name)); err == nil {
			stamp[i] = setupStampEntry{version: v, present: true}
		}
	}
	return stamp
}

// Inventory step the bootstrap, refresh and reconcile paths run, replaced by tests
var setupInventoryFn = setupInventoryOtherProviders

// Asks the resolver with the free tier rung hidden whether anything besides the free tier carries inference
func setupInventoryOtherProviders(env PoolEnv) bool {
	stamp := setupStampConfig(env)
	setupMu.Lock()
	setupInventory = &stamp
	setupMu.Unlock()
	provider, err := resolveProviderSkippingFreeTier(env, "auto")
	if err != nil {
		env.log().Debug(fmt.Sprintf("free tier bootstrap: nothing else carries inference (%v)", err))
		return false
	}
	return provider != "nous"
}

// The auto resolver's provider, empty when nothing resolves
func setupResolveInference(env PoolEnv) string {
	provider, err := resolveProvider(env, "auto")
	if err != nil {
		return ""
	}
	return provider
}

// One read only inventory pass into a record, never creating an identity
func setupBuildRecord(env PoolEnv, other bool) *SetupRecord {
	state := currentNousState(env)
	failure := map[string]any{}
	if len(state) == 0 {
		if last := lastMintFailure(env); len(last) > 0 {
			failure = last
		}
	}
	freeTier := len(state) > 0 && freeInferenceActive(env, state)
	errText := ""
	if v := failure["error"]; config.PyTruthy(v) {
		errText = config.PyStr(v)
	}
	return &SetupRecord{
		ProviderConfigured: other || freeTier || (len(state) > 0 && !isGuestState(state)),
		InferenceProvider:  setupResolveInference(env),
		FreeTierAccount:    freeTier,
		HasIdentity:        len(state) > 0,
		OtherProviders:     other,
		Error:              errText,
		Failure:            failure,
		FinishedAt:         setupNow(),
	}
}

// Settles an interrupted claim, inventories, records and broadcasts once per process, a second call returns the record
func RunFreeTierBootstrap(env PoolEnv, announce bool) *SetupRecord {
	return runFreeTierBootstrap(env, announce, func() {})
}

// The bootstrap run, began called once the run holds or waits on the started mark
func runFreeTierBootstrap(env PoolEnv, announce bool, began func()) *SetupRecord {
	setupMu.Lock()
	if setupRecord != nil {
		record := setupRecord
		setupMu.Unlock()
		began()
		return record
	}
	if setupStarted {
		done := setupDone
		setupMu.Unlock()
		began()
		timer := time.NewTimer(SetupReadyWait)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
		setupMu.Lock()
		if setupRecord != nil {
			record := setupRecord
			setupMu.Unlock()
			return record
		}
	}
	setupStarted = true
	setupMu.Unlock()
	began()

	settled := setupSettleInstall(env)
	record := setupBuildRecord(env, setupInventoryFn(env))
	setupMu.Lock()
	setupRecord, setupSettled = record, settled
	if !setupDoneSet {
		setupDoneSet = true
		close(setupDone)
	}
	setupMu.Unlock()
	if announce {
		setupBroadcast(env, record)
	}
	return record
}

// Latches a fresh desktop install and settles an interrupted claim, true when neither step failed
func setupSettleInstall(env PoolEnv) bool {
	settled := true
	if onLocalDesktopBackend(env) {
		if _, err := tools.OnboardingLatchFirstInstall(env.ctx()); err != nil {
			settled = false
			env.log().Warn("onboarding: could not record the first-install decision", "error", err)
		}
	}
	if _, err := SettleInterruptedClaim(env); err != nil {
		settled = false
		env.log().Warn("free tier: could not settle an interrupted claim", "error", err)
	}
	return settled
}

// Retries the install settle steps of a boot that could not settle, rebuilding the record on success
func ResettleAfterFailedBoot(env PoolEnv) {
	setupMu.Lock()
	unsettled := setupRecord != nil && !setupSettled
	setupMu.Unlock()
	if !unsettled || !setupResettle.TryLock() {
		return
	}
	defer setupResettle.Unlock()
	if setupSettleInstall(env) {
		setupMu.Lock()
		setupSettled = true
		setupMu.Unlock()
		RefreshSetupRecord(env, true)
	}
}

// Rebuilds the record after the identity changed, nil before the first bootstrap
func RefreshSetupRecord(env PoolEnv, announce bool) *SetupRecord {
	if CurrentSetupRecord() == nil {
		return nil
	}
	record := setupBuildRecord(env, setupInventoryFn(env))
	setupMu.Lock()
	setupRecord = record
	setupMu.Unlock()
	setupDueChanged()
	if announce {
		setupBroadcast(env, record)
	}
	return record
}

func setupDueChanged() { tools.OnboardingDueInvalidate() }

// Sends setup.ready to every connected client when a gateway server is loaded
func setupBroadcast(env PoolEnv, record *SetupRecord) {
	hooks := freeTierGatewayHooks()
	if hooks.BroadcastGlobal == nil {
		return
	}
	if err := hooks.BroadcastGlobal(SetupReadyEvent, record.AsPayload()); err != nil {
		env.log().Debug(fmt.Sprintf("setup.ready not broadcast: %v", err))
	}
}

// Runs the bootstrap on its own goroutine once it is marked started, a raised failure logged as a thread exception
func StartBackgroundBootstrap(env PoolEnv) <-chan struct{} {
	finished, started := make(chan struct{}), make(chan struct{})
	var once sync.Once
	began := func() { once.Do(func() { close(started) }) }
	go func() {
		defer close(finished)
		defer began()
		defer func() {
			if failure := recover(); failure != nil {
				env.log().Error("Exception in thread free-tier-bootstrap", "error", failure)
			}
		}()
		runFreeTierBootstrap(env, true, began)
	}()
	<-started
	return finished
}

const (
	OfferDelaySeconds      = 180
	FreeTierOfferTrial     = "trial"
	FreeTierOfferConnector = "connector"
	ConnectedAppFlag       = "has_connected_app"
)

// ReofferAfterSeconds is the back off after the first, the second, then every later offer
var ReofferAfterSeconds = [3]float64{60 * 60, 4 * 60 * 60, 24 * 60 * 60}

var (
	freeTierOfferRecords = map[string]string{FreeTierOfferTrial: "sign_in_offer.json", FreeTierOfferConnector: "connector_offer.json"}
	freeTierOfferClock   = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

// Install wide record path of an offer kind under the default root
func freeTierOfferPath(kind string) (string, error) {
	name, ok := freeTierOfferRecords[kind]
	if !ok {
		return "", errors.New(config.PyRepr(kind))
	}
	return filepath.Join(home.Root(), "free_tier", name), nil
}

// The offer record, empty when missing, unreadable, malformed or without a numeric first task stamp
func freeTierOfferRead(kind string) (map[string]any, error) {
	path, err := freeTierOfferPath(kind)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}, nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var data any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &data) != nil {
		return map[string]any{}, nil
	}
	m, ok := data.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	switch m["first_task_at"].(type) {
	case float64, bool:
		return m, nil
	}
	return map[string]any{}, nil
}

// Runs fn on the record under its file lock, fn writing through the returned writer
func freeTierOfferTransaction(kind string, fn func(state map[string]any, write func(map[string]any) error) error) error {
	path, err := freeTierOfferPath(kind)
	if err != nil {
		return err
	}
	unlock, err := new(Store).fileLock(path, 5*time.Second)
	if err != nil {
		var timeout *LockTimeoutError
		if errors.As(err, &timeout) {
			msg := fmt.Sprintf("Timed out waiting for the sign-in offer record (%s)", path)
			if timeout.Hint != "" {
				msg += "; " + timeout.Hint
			}
			return errors.New(msg)
		}
		return err
	}
	defer unlock()
	state, err := freeTierOfferRead(kind)
	if err != nil {
		return err
	}
	return fn(state, func(data map[string]any) error { return freeTierOfferWrite(path, data) })
}

// Atomic JSON write keeping mkstemp bits on an existing file and the umask mode on a new one
func freeTierOfferWrite(path string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if _, err := os.Stat(path); err != nil {
		if mode, err = umaskFileMode(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return config.AtomicWrite(path, raw, mode)
}

// Mode a plain create gives a new file in dir under the process umask
func umaskFileMode(dir string) (os.FileMode, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	probe := filepath.Join(dir, fmt.Sprintf(".umask-%d-%d", os.Getpid(), time.Now().UnixNano()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	st, err := f.Stat()
	f.Close()
	os.Remove(probe)
	if err != nil {
		return 0, err
	}
	return st.Mode().Perm(), nil
}

// Whether the user may see the offer kind now
func freeTierOfferEligible(env PoolEnv, kind string) bool {
	if kind == FreeTierOfferTrial {
		return profileFreeInferenceActive(env)
	}
	state := currentNousState(env)
	return isGuestState(state) && state[ConnectedAppFlag] == true && guestAllowed(env) && !freeInferenceActive(env, state)
}

// Python float of a JSON record value
func offerFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, fmt.Errorf("could not convert string to float: %s", config.PyRepr(n))
		}
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", config.PyTypeName(v))
}

// Python int of a JSON record value, truncating floats
func offerInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("cannot convert float %s to integer", config.PyRepr(n))
		}
		return int(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		i, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(n), "_", ""))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", config.PyRepr(n))
		}
		return i, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", config.PyTypeName(v))
}

// Value of key, or the fallback when the value is falsy
func offerOr(state map[string]any, key string, fallback any) any {
	if v := state[key]; config.PyTruthy(v) {
		return v
	}
	return fallback
}

// When the next offer comes due, false when none is pending
func freeTierOfferDueAt(state map[string]any) (float64, bool, error) {
	if len(state) == 0 {
		return 0, false, nil
	}
	offers, err := offerInt(offerOr(state, "offers", 0.0))
	if err != nil {
		return 0, false, err
	}
	lastTask, err := offerFloat(offerOr(state, "last_task_at", state["first_task_at"]))
	if err != nil {
		return 0, false, err
	}
	if offers == 0 {
		first, err := offerFloat(state["first_task_at"])
		if err != nil {
			return 0, false, err
		}
		return first + OfferDelaySeconds, true, nil
	}
	lastOffer, err := offerFloat(offerOr(state, "last_offered_at", 0.0))
	if err != nil {
		return 0, false, err
	}
	if lastTask <= lastOffer {
		return 0, false, nil
	}
	idx := min(offers, len(ReofferAfterSeconds)) - 1
	if idx < 0 {
		idx += len(ReofferAfterSeconds)
	}
	if idx < 0 {
		return 0, false, errors.New("tuple index out of range")
	}
	return max(lastTask+OfferDelaySeconds, lastOffer+ReofferAfterSeconds[idx]), true, nil
}

// Whole seconds until the offer is due for an eligible user, false when none is pending
func freeTierOfferDueIn(env PoolEnv, kind string, state map[string]any) (int, bool, error) {
	dueAt, pending, err := freeTierOfferDueAt(state)
	if err != nil || !pending || !freeTierOfferEligible(env, kind) {
		return 0, false, err
	}
	return max(0, int(math.Ceil(dueAt-freeTierOfferClock()))), true, nil
}

// Starts the offer clock on the first finished task and re arms it on later ones
func RecordTaskDone(env PoolEnv, kind string) error {
	return freeTierOfferTransaction(kind, func(state map[string]any, write func(map[string]any) error) error {
		now := freeTierOfferClock()
		next := make(map[string]any, len(state)+2)
		for k, v := range state {
			next[k] = v
		}
		if _, ok := state["first_task_at"]; !ok {
			next["first_task_at"] = now
		}
		next["last_task_at"] = now
		return write(next)
	})
}

// Counts a connected app as a finished connector task for a guest then flags the guest, never failing the connection
func NoteAppConnected(env PoolEnv) {
	if !isGuestState(currentNousState(env)) {
		return
	}
	if err := RecordTaskDone(env, FreeTierOfferConnector); err != nil {
		env.log().Debug("connector sign-in offer record failed", "error", err)
		return
	}
	if _, err := setGuestFlag(env, ConnectedAppFlag); err != nil {
		env.log().Debug("connector sign-in offer record failed", "error", err)
	}
}

// Treats the first connector list showing a guest a connected app as the connect
func NoteConnectedAppsSeen(env PoolEnv, rows []any) {
	seen := false
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && config.PyTruthy(m["connected"]) {
			seen = true
			break
		}
	}
	if !seen {
		return
	}
	state := currentNousState(env)
	if isGuestState(state) && !config.PyTruthy(state[ConnectedAppFlag]) {
		NoteAppConnected(env)
	}
}

// Seconds until the offer is due, zero meaning now, false when none is pending or the user is ineligible
func OfferDueIn(env PoolEnv, kind string) (int, bool, error) {
	state, err := freeTierOfferRead(kind)
	if err != nil {
		return 0, false, err
	}
	return freeTierOfferDueIn(env, kind, state)
}

// Grants a due offer to exactly one caller under the record lock
func ClaimOffer(env PoolEnv, kind string) (bool, error) {
	claimed := false
	err := freeTierOfferTransaction(kind, func(state map[string]any, write func(map[string]any) error) error {
		due, pending, err := freeTierOfferDueIn(env, kind, state)
		if err != nil || !pending || due != 0 {
			return err
		}
		offers, err := offerInt(offerOr(state, "offers", 0.0))
		if err != nil {
			return err
		}
		next := make(map[string]any, len(state)+2)
		for k, v := range state {
			next[k] = v
		}
		next["offers"] = float64(offers + 1)
		next["last_offered_at"] = freeTierOfferClock()
		if err := write(next); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}
