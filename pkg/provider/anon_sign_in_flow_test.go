package provider

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	signInEmail     = "sid@example.test"
	signInFreePick  = "upstage/solar-pro4:free"
	signInRefresh   = "rt-upgraded-1"
	signInInference = "https://inference-api.nousresearch.com/v1"
)

type signInClaim struct {
	authorization string
	body          map[string]any
}

// Fake portal with the device code, token and claim endpoints, its counters guarded by the portal lock
type signInPortal struct {
	*anonFakePortal
	deviceCode   string
	userCode     string
	expiresIn    int
	tokenPending bool
	tokenError   string
	omitScope    bool
	onToken      func()
	onPoll       func()
	tokenGrants  int
	tokenPolls   int
	claims       []signInClaim
	claimReply   map[string]any
}

func installSignInPortal(t *testing.T) (*signInPortal, PoolEnv) {
	t.Helper()
	fake, env := installAnonPortal(t)
	sp := &signInPortal{anonFakePortal: fake, deviceCode: "dev-code-123", userCode: "ABCD-EFGH", expiresIn: 600,
		claimReply: map[string]any{"status": "claimed", "account_email": signInEmail, "cohort_carried": false,
			"connectors": map[string]any{"kind": "moved", "moved_toolkits": []any{"gmail"}, "dropped_toolkits": []any{}}}}
	fake.routes = map[string]func(r *http.Request, req map[string]any) (int, map[string]any){
		"/api/oauth/device/code": func(*http.Request, map[string]any) (int, map[string]any) {
			return 200, map[string]any{"device_code": sp.deviceCode, "user_code": sp.userCode,
				"verification_uri": fake.URL + "/device", "verification_uri_complete": fake.URL + "/device?user_code=" + sp.userCode,
				"expires_in": sp.expiresIn, "interval": 1}
		},
		"/api/oauth/token": sp.token,
		"/api/anonymous/claim": func(r *http.Request, req map[string]any) (int, map[string]any) {
			sp.claims = append(sp.claims, signInClaim{authorization: r.Header.Get("authorization"), body: req})
			return 200, sp.claimReply
		},
	}
	return sp, env
}

// RFC 8628 token endpoint over the fake device code
func (sp *signInPortal) token(r *http.Request, _ map[string]any) (int, map[string]any) {
	if err := r.ParseForm(); err != nil {
		return 400, map[string]any{"error": "invalid_request"}
	}
	if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		return 400, map[string]any{"error": "unsupported_grant_type"}
	}
	if r.PostForm.Get("device_code") != sp.deviceCode {
		return 400, map[string]any{"error": "invalid_grant"}
	}
	sp.tokenPolls++
	if sp.onPoll != nil {
		sp.onPoll()
	}
	if sp.tokenError != "" {
		return 400, map[string]any{"error": sp.tokenError}
	}
	if sp.tokenPending {
		return 400, map[string]any{"error": "authorization_pending"}
	}
	sp.tokenGrants++
	if sp.onToken != nil {
		sp.onToken()
	}
	body := map[string]any{
		"access_token":  anonJWT(map[string]any{"sub": "nas_user:9", "client_id": "hermes-cli", "account_tier": "free", "email": signInEmail}),
		"refresh_token": signInRefresh, "token_type": "Bearer", "expires_in": 900,
		"scope": "inference:invoke", "inference_base_url": signInInference}
	if sp.omitScope {
		delete(body, "scope")
	}
	return 200, body
}

// Reads a portal counter under the portal lock
func (sp *signInPortal) read(fn func()) {
	sp.set(func(*anonFakePortal) { fn() })
}

// The signed in account is a free plan account whose recommended free pick is the only default
func signInFreeAccount(t *testing.T) {
	free, pricing, policy, recommended := checkNousFreeTierFn, nousPricingFn, nousPolicyAllowedIDsFn, fetchNousRecommendedModelsFn
	t.Cleanup(func() {
		checkNousFreeTierFn, nousPricingFn, nousPolicyAllowedIDsFn, fetchNousRecommendedModelsFn = free, pricing, policy, recommended
	})
	checkNousFreeTierFn = func(context.Context, bool, bool) bool { return true }
	nousPricingFn = func(context.Context) PricingTable { return PricingTable{} }
	nousPolicyAllowedIDsFn = func(context.Context, bool) (map[string]bool, bool) { return nil, false }
	fetchNousRecommendedModelsFn = func(context.Context, string, bool) map[string]any {
		return map[string]any{"freeRecommendedModels": []any{map[string]any{"modelName": signInFreePick}}}
	}
}

// Counts settle calls, running the real settle unless fail names a panic
func signInCountSettles(t *testing.T, fail string) *int {
	prev := signInSettleFn
	t.Cleanup(func() { signInSettleFn = prev })
	calls := 0
	signInSettleFn = func(env PoolEnv, state map[string]any) (string, bool) {
		calls++
		if fail != "" {
			panic(errors.New(fail))
		}
		return prev(env, state)
	}
	return &calls
}

func signInSeedFreeTier(t *testing.T, env PoolEnv) map[string]any {
	t.Helper()
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func signInWriteModelConfig(t *testing.T, env PoolEnv) {
	t.Helper()
	body := "model:\n  provider: nous\n  default: " + GuestModel + "\n  base_url: " + anonWelcome + "\n"
	if err := os.WriteFile(home.ConfigPath(env.Store.Home()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func signInAuthBytes(env PoolEnv) string {
	raw, _ := os.ReadFile(home.AuthPath(env.Store.Home()))
	return string(raw)
}

func signInDrain(env PoolEnv, opts SignInOptions) []SignInState {
	var states []SignInState
	for state := range RunSignIn(env, opts) {
		states = append(states, state)
	}
	return states
}

func signInKinds(states []SignInState) []string {
	kinds := make([]string, len(states))
	for i, s := range states {
		kinds[i] = s.Kind()
	}
	return kinds
}

func signInNous(t *testing.T, env PoolEnv) map[string]any {
	nous, _ := anonProviders(t, env)["nous"].(map[string]any)
	return nous
}

func TestSignInFlowCompletedYieldsCodeWaitingThenCompleted(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	signInWriteModelConfig(t, env)
	states := signInDrain(env, SignInOptions{})
	if !reflect.DeepEqual(signInKinds(states), []string{"code", "waiting", "completed"}) {
		t.Fatalf("kinds %v", signInKinds(states))
	}
	code, completed := states[0].(SignInCode), states[2].(SignInCompleted)
	if code.Link != sp.URL+"/device?user_code="+sp.userCode || code.Code != sp.userCode {
		t.Fatalf("code %+v", code)
	}
	if completed.Email != signInEmail || completed.Model != signInFreePick || !completed.ModelChanged {
		t.Fatalf("completed %+v", completed)
	}
}

func TestSignInFlowNoSurfaceCallsThePromotionEndpoints(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	signInDrain(env, SignInOptions{})
	for _, path := range sp.paths() {
		if strings.Contains(path, "promotion") {
			t.Fatalf("promotion endpoint called %v", sp.paths())
		}
	}
}

func TestSignInFlowEmailComesFromTheAccessTokenNotTheClaimAnswer(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	sp.read(func() { sp.claimReply["account_email"] = "someone-else@example.test" })
	states := signInDrain(env, SignInOptions{})
	if done, ok := states[len(states)-1].(SignInCompleted); !ok || done.Email != signInEmail {
		t.Fatalf("final %#v", states[len(states)-1])
	}
}

func TestSignInFlowHandoffResultRidesTheCompletedState(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	sp.read(func() {
		sp.claimReply["connectors"] = map[string]any{"kind": "moved", "moved_toolkits": []any{"gmail"}, "dropped_toolkits": []any{"slack"}}
	})
	states := signInDrain(env, SignInOptions{})
	done, ok := states[len(states)-1].(SignInCompleted)
	want := []string{"Your apps moved to " + signInEmail + ": Gmail.", "Reconnect in " + signInEmail + ": Slack."}
	if !ok || !reflect.DeepEqual(done.Handoff, want) {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	if lines := strings.Split(done.Copy(), "\n"); !reflect.DeepEqual(lines[:3], append([]string{"Signed in as " + signInEmail + "."}, want...)) {
		t.Fatalf("copy %q", done.Copy())
	}
}

func TestSignInFlowNoIdentityOnDiskSignsInWithoutMintingOrClaiming(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	states := signInDrain(env, SignInOptions{})
	done, ok := states[len(states)-1].(SignInCompleted)
	if !ok || len(done.Handoff) != 0 {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	sp.read(func() {
		if sp.minted != 0 || len(sp.claims) != 0 {
			t.Fatalf("minted %d claims %v", sp.minted, sp.claims)
		}
	})
	if isGuestState(signInNous(t, env)) {
		t.Fatal("the stored identity is a guest")
	}
}

func TestSignInFlowExpiredCodeYieldsTimedOutAndKeepsTheEnrichedDetail(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	sp.read(func() { sp.tokenPending, sp.expiresIn = true, 1 })
	states := signInDrain(env, SignInOptions{})
	timedOut, ok := states[len(states)-1].(SignInTimedOut)
	if !ok || timedOut.Copy() != UpgradeTimedOut || timedOut.Detail != nousDeviceAuthTimeoutMessage(sp.URL) {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	sp.read(func() {
		if sp.tokenGrants != 0 {
			t.Fatal("token granted")
		}
	})
	if signInAuthBytes(env) != before {
		t.Fatal("auth store changed")
	}
}

func TestSignInFlowExpiredTokenAnswerYieldsTimedOut(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	sp.read(func() { sp.tokenError = "expired_token" })
	states := signInDrain(env, SignInOptions{})
	if s, ok := states[len(states)-1].(SignInTimedOut); !ok || s.Copy() != UpgradeTimedOut {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	sp.read(func() {
		if sp.tokenGrants != 0 {
			t.Fatal("token granted")
		}
	})
}

func TestSignInFlowPortalOverrideIsReadInsideTheProfileScope(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	os.Unsetenv("ICEBATH_PORTAL_BASE_URL")
	entered := 0
	scope := func(body func() error) error {
		entered++
		os.Setenv("ICEBATH_PORTAL_BASE_URL", sp.URL)
		defer os.Unsetenv("ICEBATH_PORTAL_BASE_URL")
		return body()
	}
	states := signInDrain(env, SignInOptions{Scope: scope})
	if states[len(states)-1].Kind() != "completed" || entered == 0 || sp.count("/api/oauth/device/code") != 1 {
		t.Fatalf("final %#v entered %d calls %v", states[len(states)-1], entered, sp.paths())
	}
}

func TestSignInFlowPortalOverrideWinsOverASavedPortal(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	anonPatchNous(t, env, map[string]any{"portal_base_url": "https://portal.nousresearch.com"})
	signInDrain(env, SignInOptions{})
	if sp.count("/api/oauth/device/code") != 1 {
		t.Fatalf("the device code request missed the override %v", sp.paths())
	}
}

type signInBrokenBody struct{ err error }

func (b signInBrokenBody) Read([]byte) (int, error) { return 0, b.err }
func (b signInBrokenBody) Close() error             { return nil }

type signInBrokenTransport struct{ err error }

func (tr signInBrokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: signInBrokenBody(tr), Request: r}, nil
}

func TestSignInFlowTransportErrorYieldsFailedWithoutLeakingTheDetailIntoChatCopy(t *testing.T) {
	_, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	detail := "boom at https://portal.example.test/api/oauth/device/code"
	factory := func(time.Duration, oauthVerify) *http.Client {
		return &http.Client{Transport: signInBrokenTransport{err: errors.New(detail)}}
	}
	states := signInDrain(env, SignInOptions{ClientFactory: factory})
	failed, ok := states[len(states)-1].(SignInFailed)
	if !ok || failed.Copy() != UpgradeNotCompleted || strings.Contains(strings.ToLower(failed.Copy()), "https://") ||
		!strings.Contains(failed.CopyTerminal(), detail) {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	if signInAuthBytes(env) != before {
		t.Fatal("auth store changed")
	}
}

func TestSignInFlowPersistFailureYieldsFailedRatherThanRaising(t *testing.T) {
	_, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	settles := signInCountSettles(t, "")
	lockPath := strings.TrimSuffix(env.Store.Path(), ".json") + ".lock"
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	states := signInDrain(env, SignInOptions{})
	last := states[len(states)-1]
	if last.Kind() != "failed" || !last.Terminal() || !strings.Contains(last.CopyTerminal(), "is a directory") {
		t.Fatalf("final %#v", last)
	}
	if *settles != 0 {
		t.Fatal("settled after a failed persist")
	}
}

func TestSignInFlowSettleFailureYieldsFailedRatherThanRaising(t *testing.T) {
	_, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	settles := signInCountSettles(t, "config is read-only")
	states := signInDrain(env, SignInOptions{})
	last := states[len(states)-1]
	if last.Kind() != "failed" || !strings.Contains(last.CopyTerminal(), "config is read-only") || *settles != 1 {
		t.Fatalf("final %#v settles %d", last, *settles)
	}
	if nous := signInNous(t, env); nous["refresh_token"] != signInRefresh {
		t.Fatalf("the account was not persisted once %v", nous)
	}
}

func TestSignInFlowAlreadySignedInShortCircuitsBeforeAnyNetworkUnlessReplacing(t *testing.T) {
	sp, env := installSignInPortal(t)
	if _, err := env.Store.SaveActiveProviderState("nous", map[string]any{"auth_method": "oauth_device_code", "access_token": "x"}); err != nil {
		t.Fatal(err)
	}
	sp.clearCalls()
	states := signInDrain(env, SignInOptions{})
	if !reflect.DeepEqual(signInKinds(states), []string{"already_signed_in"}) || !states[0].OK() || !states[0].Precondition() {
		t.Fatalf("states %v", signInKinds(states))
	}
	if len(sp.paths()) != 0 {
		t.Fatalf("network %v", sp.paths())
	}
	next, stop := iter.Pull(RunSignIn(env, SignInOptions{ReplaceAccount: true}))
	defer stop()
	if first, ok := next(); !ok || first.Kind() != "code" {
		t.Fatalf("replacing first state %#v", first)
	}
}

func TestSignInFlowCancellingBeforeTheWaitPersistsNothing(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	var cancel atomic.Bool
	next, stop := iter.Pull(RunSignIn(env, SignInOptions{Cancelled: cancel.Load}))
	defer stop()
	if first, _ := next(); first.Kind() != "code" {
		t.Fatalf("first %#v", first)
	}
	cancel.Store(true)
	var rest []string
	for s, ok := next(); ok; s, ok = next() {
		rest = append(rest, s.Kind())
	}
	if !reflect.DeepEqual(rest, []string{"cancelled"}) {
		t.Fatalf("rest %v", rest)
	}
	sp.read(func() {
		if sp.tokenPolls != 0 {
			t.Fatal("token polled")
		}
	})
	if signInAuthBytes(env) != before {
		t.Fatal("auth store changed")
	}
}

func TestSignInFlowCancellingDuringTheTokenPollStopsItWithinASecond(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	sp.read(func() { sp.tokenPending = true })
	var cancel atomic.Bool
	next, stop := iter.Pull(RunSignIn(env, SignInOptions{Cancelled: cancel.Load}))
	defer stop()
	if first, _ := next(); first.Kind() != "code" {
		t.Fatalf("first %#v", first)
	}
	timer := time.AfterFunc(200*time.Millisecond, func() { cancel.Store(true) })
	defer timer.Stop()
	started := time.Now()
	var rest []string
	for s, ok := next(); ok; s, ok = next() {
		rest = append(rest, s.Kind())
	}
	elapsed := time.Since(started)
	var polls int
	sp.read(func() { polls = sp.tokenPolls })
	if !reflect.DeepEqual(rest, []string{"waiting", "cancelled"}) || elapsed >= 3*time.Second || polls < 1 {
		t.Fatalf("rest %v elapsed %v polls %d", rest, elapsed, polls)
	}
	time.Sleep(1200 * time.Millisecond)
	sp.read(func() {
		if sp.tokenPolls != polls {
			t.Fatal("polling went on after the sign in ended")
		}
	})
	if signInAuthBytes(env) != before {
		t.Fatal("auth store changed")
	}
}

func TestSignInFlowCancelThatLandsWithTheTokenGrantStillSavesNothing(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	var cancel atomic.Bool
	sp.read(func() { sp.onToken = func() { cancel.Store(true) } })
	states := signInDrain(env, SignInOptions{Cancelled: cancel.Load})
	if states[len(states)-1].Kind() != "cancelled" {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	sp.read(func() {
		if sp.tokenGrants != 1 || len(sp.claims) != 0 {
			t.Fatalf("grants %d claims %v", sp.tokenGrants, sp.claims)
		}
	})
	if signInAuthBytes(env) != before {
		t.Fatal("auth store changed")
	}
}

func TestSignInFlowPersistGuardThatRefusesPersistsNothingAndNeverSettles(t *testing.T) {
	_, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	settles := signInCountSettles(t, "")
	refuse := func(body func(bool) error) error { return body(false) }
	states := signInDrain(env, SignInOptions{PersistGuard: refuse})
	if states[len(states)-1].Kind() != "cancelled" || *settles != 0 || signInAuthBytes(env) != before {
		t.Fatalf("final %#v settles %d", states[len(states)-1], *settles)
	}
}

func TestSignInFlowPersistenceHappensOnlyAfterTheTokenGrant(t *testing.T) {
	_, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	before := signInAuthBytes(env)
	seen := map[string]string{}
	for state := range RunSignIn(env, SignInOptions{}) {
		seen[state.Kind()] = signInAuthBytes(env)
	}
	if seen["code"] != before || seen["waiting"] != before || seen["completed"] == before {
		t.Fatalf("persistence order %v", seen)
	}
}

func TestSignInFlowTokenWithoutAScopeKeepsTheRequestedScope(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	sp.read(func() { sp.omitScope = true })
	states := signInDrain(env, SignInOptions{})
	if states[len(states)-1].Kind() != "completed" {
		t.Fatalf("final %#v", states[len(states)-1])
	}
	if nous := signInNous(t, env); nous["scope"] != DefaultNousScope {
		t.Fatalf("scope %v", nous["scope"])
	}
}

func TestSignInFlowCABundleIsHonouredForTheWholeSignIn(t *testing.T) {
	_, env := installSignInPortal(t)
	signInFreeAccount(t)
	bundle := os.Getenv("ICEBATH_CA_BUNDLE")
	var seen []oauthVerify
	factory := func(timeout time.Duration, verify oauthVerify) *http.Client {
		seen = append(seen, verify)
		return nousHTTPClientFn(env, timeout, verify)
	}
	states := signInDrain(env, SignInOptions{ClientFactory: factory})
	if states[len(states)-1].Kind() != "completed" || len(seen) == 0 || seen[0].CABundle != bundle || seen[0].Insecure {
		t.Fatalf("final %#v verify %+v", states[len(states)-1], seen)
	}
}

func TestSignInFlowScopeIsEnteredForThePreconditionAndThePersistButNeverAroundAWait(t *testing.T) {
	sp, env := installSignInPortal(t)
	signInFreeAccount(t)
	signInSeedFreeTier(t, env)
	var mu sync.Mutex
	var events []string
	record := func(kind string) {
		mu.Lock()
		events = append(events, kind)
		mu.Unlock()
	}
	sp.read(func() { sp.onPoll = func() { record("poll") } })
	scope := func(body func() error) error {
		record("enter")
		defer record("exit")
		return body()
	}
	var states []SignInState
	for state := range RunSignIn(env, SignInOptions{Scope: scope}) {
		record("yield")
		states = append(states, state)
	}
	want := []string{"enter", "exit", "yield", "yield", "poll", "enter", "exit", "yield"}
	if states[len(states)-1].Kind() != "completed" || !reflect.DeepEqual(events, want) {
		t.Fatalf("final %#v events %v", states[len(states)-1], events)
	}
}
