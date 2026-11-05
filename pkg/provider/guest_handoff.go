package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	GuestHandoffIdle     = "idle"
	GuestHandoffPending  = "pending"
	GuestHandoffRetrying = "retrying"
	GuestHandoffClaimed  = "claimed"
	GuestHandoffGone     = "gone"
	GuestHandoffFailed   = "failed"

	GuestHandoffFilename     = "guest_handoff.json"
	guestHandoffClaimTimeout = 10 * time.Second
	guestHandoffLeaseSeconds = 30.0

	handoffOutcomeClaimed = "claimed"
	handoffOutcomeGone    = "gone"
	handoffOutcomeRetry   = "retry"
	handoffOutcomeFail    = "fail"

	guestHandoffWorkerFirstTick = 15 * time.Second
	guestHandoffWorkerInterval  = 60 * time.Second
)

var (
	guestHandoffActive       = map[string]bool{GuestHandoffPending: true, GuestHandoffRetrying: true}
	guestHandoffRetryDelays  = []float64{60, 600, 3600, 6 * 3600}
	guestHandoffForceRefresh = time.Duration(1_000_000_000) * time.Second
	guestHandoffFailureCopy  = map[string]string{
		"client_not_allowed": "This sign-in can't receive them. Sign in again with `icebath auth add nous`.",
		"account_locked":     "This Nous account is locked.",
	}
	guestHandoffRefusals = map[string]string{
		"unknown_token": handoffOutcomeGone, "already_retired": handoffOutcomeGone,
		"client_not_allowed": handoffOutcomeFail, "account_locked": handoffOutcomeFail, "invalid_request": handoffOutcomeFail,
	}
	// Wall clock in epoch seconds, replaced in tests
	guestHandoffNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	// Worker cadence, replaced in tests
	guestHandoffFirstTick = guestHandoffWorkerFirstTick
	guestHandoffInterval  = guestHandoffWorkerInterval
)

func guestHandoffISO(epoch float64) string {
	sec := int64(epoch)
	return nousISO(time.Unix(sec, int64((epoch-float64(sec))*1e9)).UTC())
}

func guestHandoffTokenKey(anonToken string) string {
	sum := sha256.Sum256([]byte(anonToken))
	return hex.EncodeToString(sum[:])[:32]
}

func guestHandoffHomeKey(env PoolEnv) string { return homeKey(envHome(env)) }

// Records file beside the shared Nous store
func guestHandoffRecordsPath(env PoolEnv) string {
	return filepath.Join(filepath.Dir(nousSharedStorePath(env)), GuestHandoffFilename)
}

// Reads the records document, an empty schema one when the file does not exist
func guestHandoffLoad(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"_schema": 1, "records": map[string]any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		return nil, err
	}
	if _, ok := data["records"].(map[string]any); !ok {
		return nil, fmt.Errorf("%s holds no records mapping", filepath.Base(path))
	}
	return data, nil
}

func guestHandoffRecords(env PoolEnv) (map[string]any, error) {
	data, err := guestHandoffLoad(guestHandoffRecordsPath(env))
	if err != nil {
		return nil, err
	}
	return data["records"].(map[string]any), nil
}

// Applies change to one record under the records lock and returns what was written, nil when nothing was
func guestHandoffUpdate(env PoolEnv, path, key string, change func(record map[string]any) map[string]any) (map[string]any, error) {
	unlock, err := nousSharedLockStore(env).fileLock(path, 15*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := guestHandoffLoad(path)
	if err != nil {
		return nil, err
	}
	records := data["records"].(map[string]any)
	current, _ := records[key].(map[string]any)
	record := change(current)
	if record == nil {
		return nil, nil
	}
	record["updated_at"] = guestHandoffISO(guestHandoffNow())
	records[key] = record
	if err := savePrivateJSON(path, data); err != nil {
		return nil, err
	}
	return record, nil
}

// Store write hook recording a guest the profile's Nous state replaces
func guestHandoffNoteStoreWrite(env PoolEnv, providerID string, previous, next map[string]any) error {
	if providerID != "nous" {
		return nil
	}
	return guestHandoffNoteReplacement(env, previous, next)
}

// Writes a pending handoff record when a non guest Nous state replaces the guest previous
func guestHandoffNoteReplacement(env PoolEnv, previous, next map[string]any) error {
	if !isGuestState(previous) || isGuestState(next) || next == nil {
		return nil
	}
	anonToken, _ := previous["anon_token"].(string)
	if anonToken == "" {
		return nil
	}
	home := guestHandoffHomeKey(env)
	key := guestHandoffTokenKey(anonToken)
	record, err := guestHandoffUpdate(env, guestHandoffRecordsPath(env), key, func(record map[string]any) map[string]any {
		if record == nil {
			record = map[string]any{"state": GuestHandoffPending, "anon_token": anonToken, "attempts": 0,
				"profiles": []any{}, "created_at": guestHandoffISO(guestHandoffNow())}
		}
		profiles, _ := record["profiles"].([]any)
		for _, p := range profiles {
			if p == home {
				return record
			}
		}
		record["profiles"] = append(profiles, home)
		return record
	})
	if err != nil {
		return err
	}
	env.log().Info(fmt.Sprintf("Nous sign-in replaced the free tier; handoff %s (%s)", anyString(record["state"]), key[:8]))
	return nil
}

func guestHandoffDue(record map[string]any, now float64) bool {
	next, _ := anyFloat(record["next_attempt_at"])
	return guestHandoffActive[anyString(record["state"])] && next <= now
}

func guestHandoffHasProfile(record map[string]any, home string) bool {
	profiles, _ := record["profiles"].([]any)
	for _, p := range profiles {
		if p == home {
			return true
		}
	}
	return false
}

// Makes one attempt for each of this profile's due records outside every lock and returns their views
func guestHandoffDrain(env PoolEnv) ([]map[string]any, error) {
	home, now := guestHandoffHomeKey(env), guestHandoffNow()
	records, err := guestHandoffRecords(env)
	if err != nil {
		return nil, err
	}
	var due []string
	for key, raw := range records {
		if record, ok := raw.(map[string]any); ok && guestHandoffHasProfile(record, home) && guestHandoffDue(record, now) {
			due = append(due, key)
		}
	}
	sort.Strings(due)
	views := []map[string]any{}
	for _, key := range due {
		view, err := guestHandoffRunOne(env, guestHandoffRecordsPath(env), key)
		if err != nil {
			return nil, err
		}
		if view != nil {
			views = append(views, view)
		}
	}
	return views, nil
}

// Sends one claim for a leased record and stores its outcome, nil when the record was not taken
func guestHandoffRunOne(env PoolEnv, path, key string) (map[string]any, error) {
	bearer, err := guestHandoffBearer(env, false)
	if err != nil {
		var authErr *AuthError
		var lockErr *LockTimeoutError
		if errors.As(err, &authErr) || errors.As(err, &lockErr) {
			reason := fmt.Sprintf("%T", err)
			if authErr != nil && authErr.Code != "" {
				reason = authErr.Code
			}
			env.log().Info(fmt.Sprintf("Free-tier handoff (%s): account token unavailable (%s)", key[:8], reason))
			return nil, nil
		}
		return nil, err
	}
	record, err := guestHandoffUpdate(env, path, key, guestHandoffReserve)
	if err != nil || record == nil {
		return nil, err
	}
	attempts, _ := anyInt(record["attempts"])
	prior := attempts - 1
	account, err := env.Store.GetProviderAuthState("nous")
	if err != nil {
		return nil, err
	}
	if account == nil {
		account = map[string]any{}
	}
	outcome, payload, retryAfter, hasRetryAfter := guestHandoffPost(env, anyString(record["anon_token"]), bearer, account)
	logged := anyString(payload["error"])
	if !pyTruthy(payload["error"]) {
		logged = outcome
	}
	env.log().Info(fmt.Sprintf("Free-tier handoff attempt %d (%s): %s", prior+1, key[:8], logged))
	record, err = guestHandoffUpdate(env, path, key, func(r map[string]any) map[string]any {
		return guestHandoffApply(r, outcome, payload, retryAfter, hasRetryAfter, prior)
	})
	if err != nil {
		return nil, err
	}
	if outcome == handoffOutcomeClaimed && payload["cohort_carried"] == true {
		if err := guestHandoffRefreshAccountToken(env); err != nil {
			return nil, err
		}
	}
	if record == nil {
		return nil, nil
	}
	return guestHandoffPublicView(record), nil
}

// Counts the attempt and leases the record before the request leaves
func guestHandoffReserve(record map[string]any) map[string]any {
	now := guestHandoffNow()
	if record == nil || !guestHandoffDue(record, now) {
		return nil
	}
	next := cloneMap(record)
	attempts, _ := anyInt(record["attempts"])
	next["attempts"] = attempts + 1
	next["last_attempt_at"] = guestHandoffISO(now)
	next["next_attempt_at"] = now + guestHandoffLeaseSeconds
	return next
}

// The account's access token with the resolver memo dropped, refreshed now when forced
func guestHandoffBearer(env PoolEnv, force bool) (string, error) {
	resolveTokenCacheMu.Lock()
	delete(resolveTokenCache, resolveTokenMemoKey(env))
	resolveTokenCacheMu.Unlock()
	opts := NousAccessTokenOptions{Timeout: guestHandoffClaimTimeout}
	if force {
		opts.RefreshSkew = guestHandoffForceRefresh
	}
	return resolveNousAccessToken(env, opts)
}

// Mints the account token again so it carries the moved cohort, an auth refusal only logged
func guestHandoffRefreshAccountToken(env PoolEnv) error {
	_, err := guestHandoffBearer(env, true)
	var authErr *AuthError
	if err != nil && errors.As(err, &authErr) {
		env.log().Info(fmt.Sprintf("Free-tier handoff: account token refresh after the move failed: %v", err))
		return nil
	}
	return err
}

// Posts the claim and classifies the answer, a transport failure retried as unreachable
func guestHandoffPost(env PoolEnv, anonToken, bearer string, account map[string]any) (string, map[string]any, float64, bool) {
	verify := resolveOAuthVerify(env, nil, "", account)
	client := nousHTTPClientFn(env, guestHandoffClaimTimeout, verify)
	body, _ := json.Marshal(map[string]any{"token": anonToken})
	target := strings.TrimRight(nousPortalBaseURL(account), "/") + "/api/anonymous/claim"
	req, err := http.NewRequestWithContext(env.ctx(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return handoffOutcomeRetry, map[string]any{"error": "unreachable", "detail": fmt.Sprintf("%T", err)}, 0, false
	}
	for k, v := range anonHeaders(env) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		return handoffOutcomeRetry, map[string]any{"error": "unreachable", "detail": fmt.Sprintf("%T", errors.Unwrap(err))}, 0, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return handoffOutcomeRetry, map[string]any{"error": "unreachable", "detail": fmt.Sprintf("%T", err)}, 0, false
	}
	var decoded any
	payload := map[string]any{}
	if json.Unmarshal(raw, &decoded) == nil {
		if m, ok := decoded.(map[string]any); ok {
			payload = m
		}
	}
	retryAfter, has := RetryAfterFromHeaders(resp.Header)
	return guestHandoffClassify(resp.StatusCode, payload), payload, retryAfter.Seconds(), has
}

func guestHandoffClassify(status int, payload map[string]any) string {
	if status == 200 {
		if payload["status"] == "claimed" {
			return handoffOutcomeClaimed
		}
		return handoffOutcomeRetry
	}
	if outcome, ok := guestHandoffRefusals[anyString(payload["error"])]; ok && payload["error"] != nil {
		return outcome
	}
	return handoffOutcomeRetry
}

// Moves an active record to its next state for one claim outcome, a finished record left as it is
func guestHandoffApply(record map[string]any, outcome string, payload map[string]any, retryAfter float64, hasRetryAfter bool, prior int) map[string]any {
	if record == nil {
		return nil
	}
	if !guestHandoffActive[anyString(record["state"])] {
		return record
	}
	next := cloneMap(record)
	next["error"] = payload["error"]
	next["next_attempt_at"] = nil
	switch {
	case outcome == handoffOutcomeClaimed:
		connectors, _ := payload["connectors"].(map[string]any)
		kind := any("unknown")
		if pyTruthy(connectors["kind"]) {
			kind = connectors["kind"]
		}
		next["state"] = GuestHandoffClaimed
		next["error"] = nil
		next["result"] = map[string]any{
			"account_email":      payload["account_email"],
			"destination_org_id": payload["destination_org_id"],
			"connectors_kind":    kind,
			"moved_toolkits":     guestHandoffList(connectors["moved_toolkits"]),
			"dropped_toolkits":   guestHandoffList(connectors["dropped_toolkits"]),
			"cohort_carried":     payload["cohort_carried"] == true,
		}
	case outcome == handoffOutcomeGone:
		next["state"] = GuestHandoffGone
		next["probably_moved"] = payload["error"] == "already_retired" || prior > 0
	case outcome == handoffOutcomeFail || prior >= len(guestHandoffRetryDelays):
		next["state"] = GuestHandoffFailed
	default:
		wait := guestHandoffRetryDelays[prior]
		if hasRetryAfter && retryAfter != 0 {
			wait = retryAfter
		}
		next["state"] = GuestHandoffRetrying
		next["next_attempt_at"] = guestHandoffNow() + wait
	}
	if next["state"] == GuestHandoffClaimed || next["state"] == GuestHandoffGone {
		delete(next, "anon_token")
	}
	return next
}

func guestHandoffList(v any) []any {
	out := []any{}
	if items, ok := v.([]any); ok {
		out = append(out, items...)
	}
	return out
}

var (
	guestHandoffWorkerMu   sync.Mutex
	guestHandoffWorkerStop chan struct{}
	guestHandoffWorkerDone chan struct{}
)

// Starts the process wide retry worker once, a running worker left as it is
func guestHandoffStartWorker(env PoolEnv) {
	guestHandoffWorkerMu.Lock()
	defer guestHandoffWorkerMu.Unlock()
	if guestHandoffWorkerDone != nil {
		select {
		case <-guestHandoffWorkerDone:
		default:
			return
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	guestHandoffWorkerStop, guestHandoffWorkerDone = stop, done
	spawnNamedFn("nous-guest-handoff", func() { guestHandoffWorkerLoop(env, stop, done) })
}

// Stops the retry worker and waits up to timeout for it to exit
func guestHandoffStopWorker(timeout time.Duration) {
	guestHandoffWorkerMu.Lock()
	stop, done := guestHandoffWorkerStop, guestHandoffWorkerDone
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	guestHandoffWorkerMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(timeout):
		}
	}
	guestHandoffWorkerMu.Lock()
	if guestHandoffWorkerDone == done {
		guestHandoffWorkerStop, guestHandoffWorkerDone = nil, nil
	}
	guestHandoffWorkerMu.Unlock()
}

// Drains due records on every tick inside the launch profile scope until stopped
func guestHandoffWorkerLoop(env PoolEnv, stop, done chan struct{}) {
	defer close(done)
	wait := guestHandoffFirstTick
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		scoped := env
		scoped.Ctx = withLaunchProfileScopeIfMultiplexed(env.ctx())
		if _, err := guestHandoffDrain(scoped); err != nil {
			env.log().Warn(fmt.Sprintf("Free-tier handoff retry tick failed; the records stay for the next one: %v", err))
		}
		wait = guestHandoffInterval
	}
}

// The wire shape the surfaces read, never the credential
func guestHandoffPublicView(record map[string]any) map[string]any {
	result, _ := record["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	state := record["state"]
	if _, ok := record["state"]; !ok {
		state = GuestHandoffIdle
	}
	moved, ok := result["moved_toolkits"]
	if !ok {
		moved = []any{}
	}
	dropped, ok := result["dropped_toolkits"]
	if !ok {
		dropped = []any{}
	}
	retryAfter := 0
	if pyTruthy(record["next_attempt_at"]) {
		next, _ := anyFloat(record["next_attempt_at"])
		retryAfter = max(0, int(next-guestHandoffNow()))
	}
	return map[string]any{
		"state": state, "account_email": result["account_email"], "connectors_kind": result["connectors_kind"],
		"moved_toolkits": moved, "dropped_toolkits": dropped, "probably_moved": pyTruthy(record["probably_moved"]),
		"error": record["error"], "retry_after": retryAfter, "updated_at": record["updated_at"],
	}
}

// This profile's latest handoff, or the idle state, read without a lock or network
func guestHandoffStatus(env PoolEnv) (map[string]any, error) {
	home := guestHandoffHomeKey(env)
	records, err := guestHandoffRecords(env)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var latest map[string]any
	for _, key := range keys {
		record, ok := records[key].(map[string]any)
		if !ok || !guestHandoffHasProfile(record, home) {
			continue
		}
		if latest == nil || anyString(record["updated_at"]) > anyString(latest["updated_at"]) {
			latest = record
		}
	}
	if latest == nil {
		return map[string]any{"state": GuestHandoffIdle}, nil
	}
	return guestHandoffPublicView(latest), nil
}

// The handoff of one free tier credential, nil when no record exists
func guestHandoffRecordStatus(env PoolEnv, anonToken string) (map[string]any, error) {
	records, err := guestHandoffRecords(env)
	if err != nil {
		return nil, err
	}
	record, ok := records[guestHandoffTokenKey(anonToken)].(map[string]any)
	if !ok || len(record) == 0 {
		return nil, nil
	}
	return guestHandoffPublicView(record), nil
}

// A status with its user copy lines added, as the desktop reads it
func guestHandoffWireStatus(status map[string]any) map[string]any {
	out := cloneMap(status)
	out["lines"] = guestHandoffResultLines(status)
	return out
}

// User copy for a non idle handoff, naming the account when nothing moved
func guestHandoffStatusLines(status map[string]any) []string {
	lines := guestHandoffResultLines(status)
	kind := anyString(status["connectors_kind"])
	movedSome := (kind == "moved" || kind == "swapped") && len(guestHandoffList(status["moved_toolkits"])) > 0
	if status["state"] == GuestHandoffClaimed && !movedSome && kind != "conflict" {
		where := "your account"
		if pyTruthy(status["account_email"]) {
			where = anyString(status["account_email"])
		}
		lines[0] = fmt.Sprintf("Signed in as %s. %s", where, lines[0])
	}
	return lines
}

// What a handoff did, for the line after the signed in line on every sign in surface
func guestHandoffResultLines(status map[string]any) []string {
	state := anyString(status["state"])
	where := "your account"
	if pyTruthy(status["account_email"]) {
		where = anyString(status["account_email"])
	}
	titles := func(v any) []string {
		var out []string
		for _, slug := range guestHandoffList(v) {
			out = append(out, tools.AppTitle(anyString(slug)))
		}
		return out
	}
	moved, dropped := titles(status["moved_toolkits"]), titles(status["dropped_toolkits"])
	if guestHandoffActive[state] {
		return []string{"Moving your free-tier apps to this account; Icebath keeps trying in the background."}
	}
	if state == GuestHandoffFailed {
		line := "Your free-tier apps could not be moved."
		if reason := guestHandoffFailureCopy[anyString(status["error"])]; reason != "" && status["error"] != nil {
			line += " " + reason
		}
		return []string{line}
	}
	if state == GuestHandoffGone {
		if pyTruthy(status["probably_moved"]) {
			return []string{"Your free-tier apps were probably moved already."}
		}
		return []string{"Nothing left to move from the free tier."}
	}
	if state != GuestHandoffClaimed {
		return []string{}
	}
	kind := anyString(status["connectors_kind"])
	var lines []string
	switch {
	case (kind == "moved" || kind == "swapped") && len(moved) > 0:
		lines = []string{fmt.Sprintf("Your apps moved to %s: %s.", where, strings.Join(moved, ", "))}
	case kind == "conflict":
		lines = []string{fmt.Sprintf("%s already had its own apps, so the free-tier apps stayed behind.", where)}
	case kind == "none" || kind == "moved" || kind == "swapped":
		lines = []string{"There were no free-tier apps to move."}
	default:
		lines = []string{"Any free-tier apps are in this account now."}
	}
	if len(dropped) > 0 {
		lines = append(lines, fmt.Sprintf("Reconnect in %s: %s.", where, strings.Join(dropped, ", ")))
	}
	return lines
}
