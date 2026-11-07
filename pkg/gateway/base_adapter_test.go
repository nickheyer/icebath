package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

type baseFakeAdapter struct {
	*BasePlatformAdapter
}

func (a *baseFakeAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *baseFakeAdapter) Disconnect(context.Context) error            { return nil }
func (a *baseFakeAdapter) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: true}, nil
}
func (a *baseFakeAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

func newBaseFake(platform Platform) *baseFakeAdapter {
	a := &baseFakeAdapter{}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, platform, a)
	return a
}

// Replaces the runtime status publisher for one test
func quietRuntimeStatus(t *testing.T) {
	prev := publishRuntimeStatusFn
	publishRuntimeStatusFn = func(RuntimeStatusUpdate) (int, error) { return 0, nil }
	t.Cleanup(func() { publishRuntimeStatusFn = prev })
}

// test_notify_fatal_error_shield.py
func TestNotifyFatalErrorHandlerSurvivesCarrierCancellation(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	var completed atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	a.SetFatalErrorHandler(func(context.Context, PlatformAdapter) error {
		cancel()
		time.Sleep(50 * time.Millisecond)
		completed.Store(true)
		return nil
	})
	if err := a.NotifyFatalError(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("carrier error %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if !completed.Load() {
		t.Fatal("fatal-error handler was killed by carrier cancellation")
	}
	if a.DetachedFatalTaskCount() != 0 {
		t.Fatal("finished handler still tracked")
	}
}

func TestNotifyFatalErrorCarrierCancellationStillPropagates(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	ctx, cancel := context.WithCancel(context.Background())
	a.SetFatalErrorHandler(func(context.Context, PlatformAdapter) error {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err := a.NotifyFatalError(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("carrier error %v", err)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestNotifyFatalErrorUncancelledPathUnchanged(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	var completed atomic.Bool
	var seen PlatformAdapter
	a.SetFatalErrorHandler(func(_ context.Context, adapter PlatformAdapter) error {
		seen = adapter
		completed.Store(true)
		return nil
	})
	if err := a.NotifyFatalError(context.Background()); err != nil || !completed.Load() || seen != PlatformAdapter(a) {
		t.Fatalf("err %v completed %v seen %v", err, completed.Load(), seen)
	}
}

func TestNotifyFatalErrorSyncHandlerStillSupported(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	completed := false
	a.SetFatalErrorHandler(func(context.Context, PlatformAdapter) error { completed = true; return nil })
	if err := a.NotifyFatalError(context.Background()); err != nil || !completed {
		t.Fatalf("err %v completed %v", err, completed)
	}
	boom := errors.New("boom")
	a.SetFatalErrorHandler(func(context.Context, PlatformAdapter) error { return boom })
	if err := a.NotifyFatalError(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("handler error not raised: %v", err)
	}
}

func TestNotifyFatalErrorNoHandlerIsNoop(t *testing.T) {
	if err := newBaseFake(PlatformTelegram).NotifyFatalError(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// test_stale_platform_lock_retryable.py
func lockRecord(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func patchAcquire(t *testing.T, results ...[2]any) *int {
	calls := 0
	prev := acquireScopedLockFn
	acquireScopedLockFn = func(scope, identity string, meta *pyjson.Object) (bool, *pyjson.Object, error) {
		r := results[calls]
		calls++
		existing, _ := r[1].(*pyjson.Object)
		return r[0].(bool), existing, nil
	}
	t.Cleanup(func() { acquireScopedLockFn = prev })
	return &calls
}

func TestStaleLockFailureIsRetryable(t *testing.T) {
	quietRuntimeStatus(t)
	a := newBaseFake(PlatformTelegram)
	patchAcquire(t, [2]any{false, lockRecord("pid", 99999, "start_time", "2026-01-01T00:00:00Z")})
	ok, err := a.AcquirePlatformLock("telegram-bot-token", "test-token", "Telegram bot token")
	if err != nil || ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if !a.FatalErrorRetryable() || *a.FatalErrorCode() != "telegram-bot-token_lock" {
		t.Fatalf("retryable %v code %v", a.FatalErrorRetryable(), *a.FatalErrorCode())
	}
}

func TestExplicitReplaceTakeoverReacquiresLockOnce(t *testing.T) {
	quietRuntimeStatus(t)
	a := newBaseFake(PlatformTelegram)
	existing := lockRecord("pid", 4242, "kind", "icebath-gateway", "argv", []any{"icebath", "gateway", "run"}, "start_time", 123)
	calls := patchAcquire(t, [2]any{false, existing}, [2]any{true, nil})
	var taken []*pyjson.Object
	prev := takeOverScopedLockHolderFn
	takeOverScopedLockHolderFn = func(record *pyjson.Object) (int, bool) { taken = append(taken, record); return 4242, true }
	t.Cleanup(func() { takeOverScopedLockHolderFn = prev })
	a.SetPlatformLockTakeoverAllowed(true)
	ok, err := a.AcquirePlatformLock("telegram-bot-token", "test-token", "Telegram bot token")
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if a.platformLockTakeoverAllowed || !a.platformLockTakeoverAttempted {
		t.Fatal("takeover flags not consumed")
	}
	if len(taken) != 1 || taken[0] != existing || *calls != 2 {
		t.Fatalf("takeover calls %d acquire calls %d", len(taken), *calls)
	}
}

func TestLockConflictNamesOwningProfile(t *testing.T) {
	quietRuntimeStatus(t)
	a := newBaseFake(PlatformTelegram)
	patchAcquire(t, [2]any{false, lockRecord("pid", 559, "start_time", 123, "profile", "lead-gen-outreach",
		"hermes_home", "/opt/data/profiles/lead-gen-outreach")})
	ok, _ := a.AcquirePlatformLock("telegram-bot-token", "test-token", "Telegram bot token")
	msg := *a.FatalErrorMessage()
	if ok || !strings.Contains(msg, "lead-gen-outreach") || !strings.Contains(msg, "559") || !a.FatalErrorRetryable() ||
		*a.FatalErrorCode() != "telegram-bot-token_lock" {
		t.Fatalf("ok %v message %q", ok, msg)
	}
}

func TestLockConflictInfersProfileFromLegacyHermesHome(t *testing.T) {
	quietRuntimeStatus(t)
	a := newBaseFake(PlatformTelegram)
	patchAcquire(t, [2]any{false, lockRecord("pid", 559, "start_time", 123, "hermes_home", "/opt/data/profiles/lead-gen-outreach")})
	ok, _ := a.AcquirePlatformLock("telegram-bot-token", "test-token", "Telegram bot token")
	if msg := *a.FatalErrorMessage(); ok || !strings.Contains(msg, "lead-gen-outreach") {
		t.Fatalf("ok %v message %q", ok, msg)
	}
}

// test_multiplex_adapter_session_key_namespace.py
const nsUID = "8693894969"

type nsStore struct {
	active    string
	multiplex bool
}

func (s *nsStore) ResolveProfileForKey(source *SessionSource) (any, error) {
	if !s.multiplex {
		return nil, nil
	}
	if source != nil && source.Profile != nil && *source.Profile != "" {
		return *source.Profile, nil
	}
	return s.active, nil
}

type boomStore struct{}

func (boomStore) ResolveProfileForKey(*SessionSource) (any, error) {
	return nil, errors.New("store unavailable")
}

type mockStore struct{}

func (mockStore) ResolveProfileForKey(*SessionSource) (any, error) { return struct{ mock int }{1}, nil }

func nsSource(profile *string) *SessionSource {
	return &SessionSource{Platform: PlatformTelegram, ChatID: nsUID, ChatType: "dm", UserID: strPtr(nsUID),
		UserName: strPtr("Lucas"), Profile: profile}
}

func nsKey(t *testing.T, source *SessionSource, profile string) string {
	key, err := BuildSessionKey(context.Background(), source, true, false, profile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSecondaryAdapterKeysIntoOwnNamespace(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(&nsStore{active: "default", multiplex: true})
	a.SetOwnerProfile("medicina")
	if key := nsKey(t, nsSource(nil), a.sessionKeyProfile(nsSource(nil))); !strings.HasPrefix(key, "agent:medicina:") {
		t.Fatal(key)
	}
}

func TestTwoBotsSameChatDoNotCollide(t *testing.T) {
	def, sec := newBaseFake(PlatformTelegram), newBaseFake(PlatformTelegram)
	def.SetSessionStore(&nsStore{active: "default", multiplex: true})
	sec.SetSessionStore(&nsStore{active: "default", multiplex: true})
	sec.SetOwnerProfile("medicina")
	src := nsSource(nil)
	kd, ks := nsKey(t, src, def.sessionKeyProfile(src)), nsKey(t, src, sec.sessionKeyProfile(src))
	if kd == ks || !strings.HasPrefix(kd, "agent:main:") || !strings.HasPrefix(ks, "agent:medicina:") {
		t.Fatalf("%s %s", kd, ks)
	}
}

func TestStampedSourceWinsOverOwner(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(&nsStore{active: "default", multiplex: true})
	a.SetOwnerProfile("medicina")
	if got := a.sessionKeyProfile(nsSource(strPtr("finances"))); got != "finances" {
		t.Fatal(got)
	}
}

func TestPrimaryAdapterUnchanged(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(&nsStore{active: "default", multiplex: true})
	key := nsKey(t, nsSource(nil), a.sessionKeyProfile(nsSource(nil)))
	if key != nsKey(t, nsSource(nil), "") || key != "agent:main:telegram:dm:"+nsUID {
		t.Fatal(key)
	}
}

func TestSingleProfileGatewayUnchanged(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(&nsStore{multiplex: false})
	if got := a.sessionKeyProfile(nsSource(nil)); got != "" {
		t.Fatal(got)
	}
}

func TestOwnerDefaultIsNormalizedToNone(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetOwnerProfile("default")
	if a.OwnerProfile() != "" || nsKey(t, nsSource(nil), a.sessionKeyProfile(nsSource(nil))) != nsKey(t, nsSource(nil), "") {
		t.Fatal(a.OwnerProfile())
	}
}

func TestBlankOwnerIsNone(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		a := newBaseFake(PlatformTelegram)
		a.SetOwnerProfile(blank)
		if a.OwnerProfile() != "" {
			t.Fatalf("%q kept", blank)
		}
	}
}

func TestOwnerUsedWhenStoreAbsent(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetOwnerProfile("medicina")
	if got := a.sessionKeyProfile(nsSource(nil)); got != "medicina" {
		t.Fatal(got)
	}
}

func TestAdapterWithoutBaseInitDoesNotRaise(t *testing.T) {
	bare := &BasePlatformAdapter{}
	if got := bare.sessionKeyProfile(nsSource(nil)); got != "" {
		t.Fatal(got)
	}
	bare.SetStatusText("1", "thinking")
	if text, _ := bare.StatusText("1"); text != "thinking" {
		t.Fatal(text)
	}
}

func TestResolverExceptionFallsBackToNone(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(boomStore{})
	if got := a.sessionKeyProfile(nsSource(nil)); got != "" {
		t.Fatal(got)
	}
}

func TestNonStringResolverResultIsRejected(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(mockStore{})
	if got := a.sessionKeyProfile(nsSource(nil)); got != "" {
		t.Fatal(got)
	}
}

func TestNonStringOrBlankOwnerIsIgnored(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.ownerProfile = "   "
	if got := a.sessionKeyProfile(nsSource(nil)); got != "" {
		t.Fatal(got)
	}
}

func TestNoSourceStillResolvesOwner(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetSessionStore(&nsStore{active: "default", multiplex: true})
	a.SetOwnerProfile("medicina")
	if got := a.sessionKeyProfile(nil); got != "medicina" {
		t.Fatal(got)
	}
}

func TestPinnedIdentityWinsForSessionKeyProfile(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetOwnerProfile("medicina")
	src := nsSource(strPtr("finances"))
	src.identity = &RoutingIdentity{TransportProfile: "medicina", RuntimeProfile: "coder", Multiplexed: true}
	if got := a.sessionKeyProfile(src); got != "coder" {
		t.Fatal(got)
	}
	src.identity.Multiplexed = false
	if got := a.sessionKeyProfile(src); got != "" {
		t.Fatal(got)
	}
}

// test_platform_base.py TestIsSenderAuthorized
func TestIsSenderAuthorizedNoCheckRegisteredIsUnknown(t *testing.T) {
	if newBaseFake(PlatformTelegram).IsSenderAuthorized("user", nil, nil, AuthorizationOpts{}) != nil {
		t.Fatal("not unknown")
	}
}

func TestIsSenderAuthorizedEmptyUserIDIsUnknown(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetAuthorizationCheck(func(string, *string, *string, AuthorizationOpts) (any, error) { return true, nil })
	if a.IsSenderAuthorized("", nil, nil, AuthorizationOpts{}) != nil {
		t.Fatal("not unknown")
	}
}

func TestIsSenderAuthorizedTrueAndFalsePropagate(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	for _, want := range []bool{true, false} {
		a.SetAuthorizationCheck(func(string, *string, *string, AuthorizationOpts) (any, error) { return want, nil })
		if got := a.IsSenderAuthorized("user", nil, nil, AuthorizationOpts{}); got == nil || *got != want {
			t.Fatalf("want %v got %v", want, got)
		}
	}
}

func TestIsSenderAuthorizedTruthyNonBooleanIsUnknown(t *testing.T) {
	for _, result := range []any{"allowed", 1, struct{}{}, []any{1}} {
		a := newBaseFake(PlatformTelegram)
		a.SetAuthorizationCheck(func(string, *string, *string, AuthorizationOpts) (any, error) { return result, nil })
		if a.IsSenderAuthorized("user", nil, nil, AuthorizationOpts{}) != nil {
			t.Fatalf("%v treated as authorization", result)
		}
	}
}

func TestIsSenderAuthorizedRaisingCheckIsUnknown(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	a.SetAuthorizationCheck(func(string, *string, *string, AuthorizationOpts) (any, error) {
		return nil, errors.New("auth backend down")
	})
	if a.IsSenderAuthorized("user", nil, nil, AuthorizationOpts{}) != nil {
		t.Fatal("not unknown")
	}
}

func TestIsSenderAuthorizedCheckReceivesChatContext(t *testing.T) {
	var seen [][3]string
	a := newBaseFake(PlatformTelegram)
	a.SetAuthorizationCheck(func(userID string, chatType, chatID *string, opts AuthorizationOpts) (any, error) {
		seen = append(seen, [3]string{userID, *chatType, *chatID})
		if opts.IsBot || opts.ThreadID != nil {
			t.Fatal("unset keywords forwarded")
		}
		return true, nil
	})
	a.IsSenderAuthorized("user", strPtr("group"), strPtr("chan"), AuthorizationOpts{})
	if len(seen) != 1 || seen[0] != [3]string{"user", "group", "chan"} {
		t.Fatal(seen)
	}
}

// test_platform_plugin_handlers.py TestAdapterPluginWiring and test_late_plugin_rewire.py idempotence
func patchFactories(t *testing.T, fn func(ctx context.Context, platform string) ([]*plugins.PlatformHandlerFactory, error)) {
	prev := platformHandlerFactoriesFn
	platformHandlerFactoriesFn = fn
	t.Cleanup(func() { platformHandlerFactoriesFn = prev })
}

func TestFactoryInvokedWithNativeAndAdapter(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	native := &struct{ handlers int }{}
	var calls [][2]any
	var asked []string
	patchFactories(t, func(_ context.Context, platform string) ([]*plugins.PlatformHandlerFactory, error) {
		asked = append(asked, platform)
		return []*plugins.PlatformHandlerFactory{{Plugin: "biz_plugin", Factory: func(n, adp any) {
			calls = append(calls, [2]any{n, adp})
			n.(*struct{ handlers int }).handlers++
		}}}, nil
	})
	a.WirePluginHandlers(context.Background(), native)
	if len(calls) != 1 || calls[0][0] != any(native) || calls[0][1] != any(a) || native.handlers != 1 {
		t.Fatalf("calls %v handlers %d", calls, native.handlers)
	}
	if len(asked) != 1 || asked[0] != "telegram" {
		t.Fatal(asked)
	}
}

func TestRaisingFactoryDoesNotBlockOthers(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	var wired []string
	patchFactories(t, func(context.Context, string) ([]*plugins.PlatformHandlerFactory, error) {
		return []*plugins.PlatformHandlerFactory{
			{Plugin: "bad_plugin", Factory: func(any, any) error { return errors.New("boom") }},
			{Plugin: "panic_plugin", Factory: func(any, any) { panic("boom") }},
			{Plugin: "good_plugin", Factory: func(any, any) { wired = append(wired, "good") }},
		}, nil
	})
	a.WirePluginHandlers(context.Background(), "app")
	if len(wired) != 1 {
		t.Fatal(wired)
	}
}

func TestManagerLoadFailureDoesNotRaise(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	patchFactories(t, func(context.Context, string) ([]*plugins.PlatformHandlerFactory, error) {
		return nil, errors.New("plugin system down")
	})
	a.WirePluginHandlers(context.Background(), "app")
	a.RewirePluginHandlers(context.Background())
}

func TestNativeNoneSupported(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	var seen []any
	patchFactories(t, func(context.Context, string) ([]*plugins.PlatformHandlerFactory, error) {
		return []*plugins.PlatformHandlerFactory{{Plugin: "p", Factory: func(n, _ any) { seen = append(seen, n) }}}, nil
	})
	a.WirePluginHandlers(context.Background(), nil)
	if len(seen) != 1 || seen[0] != nil {
		t.Fatal(seen)
	}
}

func lateFactories(calls *[]any) func(context.Context, string) ([]*plugins.PlatformHandlerFactory, error) {
	return func(context.Context, string) ([]*plugins.PlatformHandlerFactory, error) {
		return []*plugins.PlatformHandlerFactory{{Plugin: "p", Factory: func(n, _ any) { *calls = append(*calls, n) }}}, nil
	}
}

func TestRewireIsIdempotentPerNativeClient(t *testing.T) {
	var calls []any
	a := newBaseFake(PlatformTelegram)
	patchFactories(t, lateFactories(&calls))
	a.RewirePluginHandlers(context.Background())
	if len(calls) != 0 {
		t.Fatal(calls)
	}
	a.WirePluginHandlers(context.Background(), "app-1")
	a.RewirePluginHandlers(context.Background())
	a.RewirePluginHandlers(context.Background())
	if len(calls) != 1 || calls[0] != "app-1" {
		t.Fatal(calls)
	}
	platformHandlerFactoriesFn = lateFactories(&calls)
	a.RewirePluginHandlers(context.Background())
	if len(calls) != 1 {
		t.Fatal(calls)
	}
	a.WirePluginHandlers(context.Background(), "app-2")
	if len(calls) != 2 || calls[1] != "app-2" {
		t.Fatal(calls)
	}
}
