package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	SetupReadyWait  = 8 * time.Second
	SetupReadyEvent = "setup.ready"

	OfferDelay       = 180 * time.Second
	OfferTrial       = "trial"
	OfferConnector   = "connector"
	ConnectedAppFlag = "has_connected_app"
	offerLockTimeout = 5 * time.Second
)

var reofferAfter = []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}

type Install interface {
	ClaimLock(ctx context.Context) (release func(), err error)
	Refresh()
	Existing() bool
	TrialReserved() bool
	MarkMachineUsed() error
	LatchFirstInstall() error
	OnboardingChanged()
}

type SetupRecord struct {
	ProviderConfigured bool
	InferenceProvider  string
	FreeTierAccount    bool
	HasIdentity        bool
	OtherProviders     bool
	Error              string
	Failure            *MintFailureStatus
	FinishedAt         time.Time
}

func (r SetupRecord) FreeTierRoute() bool {
	return r.FreeTierAccount && r.InferenceProvider == Provider
}

func (r SetupRecord) MarshalJSON() ([]byte, error) {
	out := pairs("provider_configured", r.ProviderConfigured, "inference_provider", r.InferenceProvider,
		"free_tier_account", r.FreeTierAccount, "has_identity", r.HasIdentity, "other_providers", r.OtherProviders,
		"error", r.Error, "finished_at", json.Number(floatText(epochOf(r.FinishedAt))))
	if f := r.Failure; f != nil {
		out.set("error_code", f.ErrorCode)
		out.set("error", f.Error)
		out.set("retryable", f.Retryable)
		out.set("retry_after", f.RetryAfter)
	}
	out.set("free_tier_route", r.FreeTierRoute())
	return out.MarshalJSON()
}

type setupState struct {
	record     *SetupRecord
	done       chan struct{}
	doneOnce   sync.Once
	started    bool
	settled    bool
	resettling bool
	stamp      string
	stamped    bool
}

func (a *Account) setupRecord() *SetupRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setup.record
}

func (a *Account) CurrentSetupRecord() (SetupRecord, bool) {
	record := a.setupRecord()
	if record == nil {
		return SetupRecord{}, false
	}
	return *record, true
}

func (a *Account) WaitForSetupRecord(ctx context.Context, timeout time.Duration) (SetupRecord, bool) {
	a.mu.Lock()
	started := a.setup.started
	a.mu.Unlock()
	if !started {
		return SetupRecord{}, false
	}
	if timeout <= 0 {
		timeout = SetupReadyWait
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-a.setup.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return a.ReconcileSetupRecord(ctx)
}

func (a *Account) configStamp() string {
	sum := sha256.New()
	for _, path := range []string{home.Config.In(a.home), home.EnvFile.In(a.home), a.store.Path()} {
		fi, err := os.Stat(path)
		data, rerr := os.ReadFile(path)
		if err != nil || rerr != nil {
			sum.Write([]byte("|absent"))
			continue
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(sum, "|%d|%d|%s", fi.ModTime().UnixNano(), fi.Size(), hex.EncodeToString(digest[:]))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (a *Account) inventoryOtherProviders(ctx context.Context) bool {
	stamp := a.configStamp()
	a.mu.Lock()
	a.setup.stamp, a.setup.stamped = stamp, true
	a.mu.Unlock()
	provider, err := a.resolveProvider(ctx, true)
	if err != nil {
		slog.Debug("free tier bootstrap: nothing else carries inference", "error", err)
		return false
	}
	return provider != Provider
}

func (a *Account) resolveInference(ctx context.Context) string {
	provider, err := a.resolveProvider(ctx, false)
	if err != nil {
		return ""
	}
	return provider
}

func (a *Account) ReconcileSetupRecord(ctx context.Context) (SetupRecord, bool) {
	record := a.setupRecord()
	if record == nil {
		return SetupRecord{}, false
	}
	if record.ProviderConfigured {
		return *record, true
	}
	if scoped := home.Override(ctx); scoped != "" {
		if key, err := a.homes.Key(scoped); err == nil && key != a.homeKey() {
			return *record, true
		}
	}
	ctx = a.bind(ctx)
	current := a.configStamp()
	a.mu.Lock()
	unchanged := a.setup.stamped && a.setup.stamp == current
	a.mu.Unlock()
	if unchanged || !a.inventoryOtherProviders(ctx) {
		return a.CurrentSetupRecord()
	}
	refreshed := *record
	refreshed.ProviderConfigured, refreshed.OtherProviders = true, true
	refreshed.InferenceProvider, refreshed.FinishedAt = a.resolveInference(ctx), time.Now()
	a.mu.Lock()
	if a.setup.record != record {
		newer := *a.setup.record
		a.mu.Unlock()
		return newer, true
	}
	a.setup.record = &refreshed
	a.mu.Unlock()
	a.install.OnboardingChanged()
	a.broadcastSetup(refreshed)
	return refreshed, true
}

func (a *Account) buildSetupRecord(ctx context.Context, other bool) *SetupRecord {
	state := a.quietState(ctx)
	hasIdentity := state.size() > 0
	record := &SetupRecord{OtherProviders: other, HasIdentity: hasIdentity, FinishedAt: time.Now()}
	if hasIdentity {
		record.FreeTierAccount = a.freeInferenceActive(ctx, state)
	} else if failure, ok := a.LastMintFailure(); ok {
		record.Failure, record.Error = &failure, failure.Error
	}
	record.ProviderConfigured = other || record.FreeTierAccount || (hasIdentity && !isGuestState(state))
	record.InferenceProvider = a.resolveInference(ctx)
	return record
}

func (a *Account) RunBootstrap(ctx context.Context, announce bool) SetupRecord {
	ctx = a.bind(ctx)
	a.mu.Lock()
	if a.setup.record != nil {
		record := *a.setup.record
		a.mu.Unlock()
		return record
	}
	if a.setup.started {
		a.mu.Unlock()
		timer := time.NewTimer(SetupReadyWait)
		select {
		case <-a.setup.done:
		case <-timer.C:
		}
		timer.Stop()
		if record := a.setupRecord(); record != nil {
			return *record
		}
		a.mu.Lock()
	}
	a.setup.started = true
	a.mu.Unlock()
	settled := a.settleInstall(ctx)
	record := a.buildSetupRecord(ctx, a.inventoryOtherProviders(ctx))
	a.mu.Lock()
	a.setup.record, a.setup.settled = record, settled
	a.mu.Unlock()
	a.setup.doneOnce.Do(func() { close(a.setup.done) })
	if announce {
		a.broadcastSetup(*record)
	}
	return *record
}

func (a *Account) settleInstall(ctx context.Context) bool {
	settled := true
	if a.localDesktop(ctx) {
		if err := a.install.LatchFirstInstall(); err != nil {
			settled = false
			slog.Warn("onboarding: could not record the first-install decision", "error", err)
		}
	}
	if _, err := a.SettleInterruptedClaim(ctx); err != nil {
		settled = false
		slog.Warn("free tier: could not settle an interrupted claim", "error", err)
	}
	return settled
}

func (a *Account) ResettleAfterFailedBoot(ctx context.Context) {
	ctx = a.bind(ctx)
	a.mu.Lock()
	unsettled := a.setup.record != nil && !a.setup.settled
	if !unsettled || a.setup.resettling {
		a.mu.Unlock()
		return
	}
	a.setup.resettling = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.setup.resettling = false
		a.mu.Unlock()
	}()
	if a.settleInstall(ctx) {
		a.mu.Lock()
		a.setup.settled = true
		a.mu.Unlock()
		a.RefreshSetupRecord(ctx, true)
	}
}

func (a *Account) RefreshSetupRecord(ctx context.Context, announce bool) (SetupRecord, bool) {
	ctx = a.bind(ctx)
	if a.setupRecord() == nil {
		return SetupRecord{}, false
	}
	record := a.buildSetupRecord(ctx, a.inventoryOtherProviders(ctx))
	a.mu.Lock()
	a.setup.record = record
	a.mu.Unlock()
	a.install.OnboardingChanged()
	if announce {
		a.broadcastSetup(*record)
	}
	return *record, true
}

func (a *Account) broadcastSetup(record SetupRecord) {
	if a.events == nil {
		slog.Debug(SetupReadyEvent + " not broadcast: no client channel")
		return
	}
	payload, err := record.MarshalJSON()
	if err == nil {
		err = a.events.Broadcast(SetupReadyEvent, payload)
	}
	if err != nil {
		slog.Debug(SetupReadyEvent+" not broadcast", "error", err)
	}
}

func (a *Account) StartBackgroundBootstrap(ctx context.Context) {
	detached := context.WithoutCancel(ctx)
	go a.RunBootstrap(detached, true)
}

func (a *Account) offerPath(kind string) string {
	if kind == OfferConnector {
		return home.FreeTierConnectorOffer.In(a.root)
	}
	return home.FreeTierSignInOffer.In(a.root)
}

func (a *Account) readOffer(kind string) *object {
	data, err := os.ReadFile(a.offerPath(kind))
	if err != nil {
		return newObject()
	}
	state, ok := parseObject(data)
	if !ok {
		return newObject()
	}
	if _, numeric := claimNumber(state.get("first_task_at")); !numeric {
		return newObject()
	}
	return state
}

func (a *Account) offerTransaction(ctx context.Context, kind string, fn func(state *object, write func(*object) error) error) error {
	path := a.offerPath(kind)
	lctx, unlock, err := openFileLock(ctx, lockPathFor(path), offerLockTimeout, fmt.Sprintf("Timed out waiting for the sign-in offer record (%s)", path))
	if err != nil {
		return err
	}
	defer unlock()
	return fn(a.readOffer(kind), func(data *object) error {
		if err := a.homes.Mkdir(lctx, filepath.Dir(path)); err != nil {
			return err
		}
		return fileutil.WriteText(lctx, path, spacedJSON(data))
	})
}

func (a *Account) offerEligible(ctx context.Context, kind string) bool {
	state := a.quietState(ctx)
	if kind == OfferTrial {
		return a.freeInferenceActive(ctx, state)
	}
	return isGuestState(state) && state.get(ConnectedAppFlag) == true && a.guestAllowed(ctx) && !a.freeInferenceActive(ctx, state)
}

func offerNumber(state *object, key string) float64 {
	f, _ := floatOf(state.get(key))
	return f
}

func offerDueAt(state *object) (float64, bool) {
	if state.size() == 0 {
		return 0, false
	}
	offers := int(offerNumber(state, "offers"))
	first := offerNumber(state, "first_task_at")
	lastTask := offerNumber(state, "last_task_at")
	if lastTask == 0 {
		lastTask = first
	}
	if offers == 0 {
		return first + OfferDelay.Seconds(), true
	}
	lastOffer := offerNumber(state, "last_offered_at")
	if lastTask <= lastOffer {
		return 0, false
	}
	return math.Max(lastTask+OfferDelay.Seconds(), lastOffer+reofferAfter[min(offers, len(reofferAfter))-1].Seconds()), true
}

func (a *Account) offerDueIn(ctx context.Context, kind string, state *object) (int, bool) {
	dueAt, ok := offerDueAt(state)
	if !ok || !a.offerEligible(ctx, kind) {
		return 0, false
	}
	return max(0, int(math.Ceil(dueAt-epochOf(time.Now())))), true
}

func stampNow() json.Number {
	return json.Number(floatText(epochOf(time.Now())))
}

func (a *Account) RecordTaskDone(ctx context.Context, kind string) error {
	return a.offerTransaction(a.bind(ctx), kind, func(state *object, write func(*object) error) error {
		now := stampNow()
		if !state.has("first_task_at") {
			state.set("first_task_at", now)
		}
		state.set("last_task_at", now)
		return write(state)
	})
}

func (a *Account) NoteAppConnected(ctx context.Context) {
	ctx = a.bind(ctx)
	if !isGuestState(a.quietState(ctx)) {
		return
	}
	err := a.RecordTaskDone(ctx, OfferConnector)
	if err == nil {
		_, err = a.SetGuestFlag(ctx, ConnectedAppFlag)
	}
	if err != nil {
		slog.Debug("connector sign-in offer record failed", "error", err)
	}
}

func (a *Account) NoteConnectedAppsSeen(ctx context.Context, anyConnected bool) {
	if !anyConnected {
		return
	}
	state := a.quietState(ctx)
	if isGuestState(state) && !truthy(state.get(ConnectedAppFlag)) {
		a.NoteAppConnected(ctx)
	}
}

func (a *Account) OfferDueIn(ctx context.Context, kind string) (int, bool) {
	ctx = a.bind(ctx)
	return a.offerDueIn(ctx, kind, a.readOffer(kind))
}

func (a *Account) ClaimOffer(ctx context.Context, kind string) (bool, error) {
	ctx = a.bind(ctx)
	claimed := false
	err := a.offerTransaction(ctx, kind, func(state *object, write func(*object) error) error {
		if due, ok := a.offerDueIn(ctx, kind, state); !ok || due != 0 {
			return nil
		}
		state.set("offers", int(offerNumber(state, "offers"))+1)
		state.set("last_offered_at", stampNow())
		claimed = true
		return write(state)
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func (a *Account) hasPortalIdentity(ctx context.Context) bool {
	return a.quietState(ctx) != nil || a.readShared() != nil
}

func (a *Account) ProviderConfigured(ctx context.Context) (bool, error) {
	ctx = a.bind(ctx)
	provider, err := a.resolveProvider(ctx, true)
	if err != nil {
		if _, ok := authErrorOf(err); ok {
			return false, nil
		}
		return false, err
	}
	return provider != Provider || !a.HasGuest(ctx), nil
}

func (a *Account) InferenceAvailable(ctx context.Context, assumeFresh bool) (bool, error) {
	ctx = a.bind(ctx)
	if a.FreeInferenceActive(ctx) {
		return true, nil
	}
	configured, err := a.ProviderConfigured(ctx)
	if err != nil || configured {
		return configured, err
	}
	reason, err := a.claimBlocker(ctx, assumeFresh, &configured)
	return reason == "" && err == nil, err
}

func (a *Account) ClaimBlocker(ctx context.Context, assumeFresh bool) (string, error) {
	return a.claimBlocker(a.bind(ctx), assumeFresh, nil)
}

func (a *Account) claimBlocker(ctx context.Context, assumeFresh bool, configured *bool) (string, error) {
	switch {
	case !a.localDesktop(ctx):
		return "not the desktop's local backend", nil
	case !a.guestAllowed(ctx):
		return "guests are switched off", nil
	case a.hasPortalIdentity(ctx):
		return "an Icebath Portal identity exists", nil
	}
	isConfigured := false
	if configured != nil {
		isConfigured = *configured
	} else {
		resolved, err := a.ProviderConfigured(ctx)
		if err != nil {
			return "", err
		}
		isConfigured = resolved
	}
	if isConfigured {
		return "a provider is configured", nil
	}
	if !assumeFresh && a.install.Existing() && !a.install.TrialReserved() {
		return "icebath was used on this machine", nil
	}
	return "", nil
}

func (a *Account) ClaimDesktopFreeTier(ctx context.Context, assumeFresh, force bool) (json.RawMessage, error) {
	ctx = a.bind(ctx)
	release, err := a.install.ClaimLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	a.install.Refresh()
	if _, err := a.SettleInterruptedClaim(ctx); err != nil {
		return nil, err
	}
	reason, err := a.claimBlocker(ctx, assumeFresh, nil)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		slog.Debug("welcome trial not claimed: " + reason)
		return nil, nil
	}
	state, err := a.EnsureIdentity(ctx, EnsureOptions{Force: force, WelcomeClaim: true})
	if err != nil {
		return nil, err
	}
	if state == nil {
		message, code, retryAfter := AnonFailureCopy(AnonServerError, 0), AnonServerError, time.Duration(0)
		if failure, ok := a.LastMintFailure(); ok {
			if failure.Error != "" {
				message = failure.Error
			}
			if failure.ErrorCode != "" {
				code = failure.ErrorCode
			}
			retryAfter = time.Duration(failure.RetryAfter) * time.Second
		}
		return nil, anonErr(message, code, retryAfter)
	}
	if err := a.install.MarkMachineUsed(); err != nil {
		return nil, err
	}
	slog.Info("Icebath Portal free tier: welcome trial claimed on this machine")
	return state, nil
}

func (a *Account) SettleInterruptedClaim(ctx context.Context) (bool, error) {
	ctx = a.bind(ctx)
	if !a.install.TrialReserved() {
		return false, nil
	}
	claimed := false
	for _, store := range []*object{a.quietState(ctx), a.readShared()} {
		claimed = claimed || (isGuestState(store) && store.get(auth.WelcomeClaimFlag) == true)
	}
	if !claimed {
		return false, nil
	}
	if err := a.install.MarkMachineUsed(); err != nil {
		return false, err
	}
	slog.Info("Icebath Portal free tier: an interrupted welcome-trial claim was settled")
	return true, nil
}
