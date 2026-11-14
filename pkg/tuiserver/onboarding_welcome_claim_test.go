package tuiserver

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/anonportaltest"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch root home behind the fake account service with resume and submit answered by doubles
func welcomeClaimPortal(t *testing.T) (*anonportaltest.Portal, string) {
	t.Helper()
	provider.ResetAnonPortalMemosForTests()
	portal := anonportaltest.Install(t)
	root := filepath.Join(t.TempDir(), "icebath-root")
	mkdirAll(t, root)
	t.Setenv("ICEBATH_HOME", root)
	setLaunchHome(t, root)
	dbMu.Lock()
	savedDB, savedErr := sharedDB, dbError
	sharedDB, dbError = nil, ""
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		opened := sharedDB
		sharedDB, dbError = savedDB, savedErr
		dbMu.Unlock()
		if opened != nil && opened != savedDB {
			state.Release(opened)
		}
	})
	tools.OnboardingDueResetForTests()
	t.Cleanup(tools.ConfineInstallProbeForTests(t.TempDir()))
	prevSkills := tools.AllSkillNames
	tools.AllSkillNames = skills.AllSkillNames
	t.Cleanup(func() { tools.AllSkillNames = prevSkills })
	prevMultiplex := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	t.Cleanup(func() { provider.SetMultiplexActive(prevMultiplex) })
	prevOpen := OpenSessionDBAtPath
	OpenSessionDBAtPath = func(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error) {
		return state.NewSessionDB(ctx, dbPath, readOnly)
	}
	t.Cleanup(func() { OpenSessionDBAtPath = prevOpen })
	t.Cleanup(func() {
		provider.ResetAnonPortalMemosForTests()
		tools.OnboardingDueResetForTests()
	})
	swapMethod(t, "session.resume", func(_ context.Context, rid any, params map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"session_id": "rt-" + relayString(params["session_id"])}}
	})
	swapMethod(t, "prompt.submit", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{"status": "streaming"}}
	})
	return portal, root
}

// Context of an activity that works for the local Desktop app
func welcomeClaimLocal() context.Context {
	return tools.WithServingLocalDesktop(context.Background(), true)
}

// Whole reply of one request through the real request path
func welcomeClaimCall(ctx context.Context, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return pyjson.Plain(handleRequest(ctx, map[string]any{"jsonrpc": "2.0", "id": 9, "method": method, "params": params})).(map[string]any)
}

// Result member of one onboarding.state reply
func welcomeClaimState(t *testing.T, ctx context.Context, params map[string]any) map[string]any {
	t.Helper()
	result, isDict := welcomeClaimCall(ctx, "onboarding.state", params)["result"].(map[string]any)
	if !isDict {
		t.Fatal("onboarding.state answered no result")
	}
	return result
}

// Error code of one reply, zero when it carried a result
func welcomeClaimErrorCode(reply map[string]any) any {
	failure, _ := reply["error"].(map[string]any)
	return failure["code"]
}

// Saves a provider key in the root profile env file the way a later key save does
func welcomeClaimSaveKey(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENROUTER_API_KEY=sk-or-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestANewRunOnAFirstInstallClaimsTheTrialOnce(t *testing.T) {
	portal, _ := welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	if got := welcomeClaimState(t, ctx, nil)["run"]; got != true {
		t.Fatalf("run %v", got)
	}
	if got := welcomeClaimState(t, ctx, nil)["eligible"]; got != true {
		t.Fatalf("eligible %v", got)
	}
	first := welcomeClaimCall(ctx, "onboarding.start", nil)
	welcomeClaimCall(ctx, "onboarding.start", nil)
	welcomeClaimCall(ctx, "onboarding.start", map[string]any{"rerun": true})
	if portal.Creates() != 1 || !provider.FreeInferenceActive(scopedPoolEnv(ctx)) {
		t.Fatalf("creates %d calls %v after a first start answered %v", portal.Creates(), portal.Calls(), first)
	}
}

func TestARerunOnAFirstInstallNeverClaims(t *testing.T) {
	portal, _ := welcomeClaimPortal(t)
	welcomeClaimCall(welcomeClaimLocal(), "onboarding.start", map[string]any{"rerun": true})
	if portal.Creates() != 0 {
		t.Fatalf("creates %d", portal.Creates())
	}
}

func TestAFailedClaimOpensNothingAndTheNextLaunchTriesAgain(t *testing.T) {
	portal, _ := welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	provider.RunFreeTierBootstrap(scopedPoolEnv(ctx), false)
	portal.SetCreateResponse(503, map[string]any{"error": "unavailable"})
	if code := welcomeClaimErrorCode(welcomeClaimCall(ctx, "onboarding.start", map[string]any{"fresh_launch": true})); !pyjson.Equal(code, 5079) {
		t.Fatalf("failed claim code %v", code)
	}
	if got := welcomeClaimState(t, ctx, nil)["session_id"]; got != nil {
		t.Fatalf("session_id %v", got)
	}
	portal.SetCreateResponse(0, nil)
	provider.ResetMintMemoForTests()
	tools.InstallStateProbeClear()
	state := welcomeClaimState(t, ctx, nil)
	if state["run"] != true || state["eligible"] != true {
		t.Fatalf("next launch state %v", state)
	}
	if _, has := welcomeClaimCall(ctx, "onboarding.start", map[string]any{"fresh_launch": true})["result"]; !has {
		t.Fatal("the next launch did not start")
	}
	if !provider.FreeInferenceActive(scopedPoolEnv(ctx)) {
		t.Fatal("free inference is not active")
	}
}

func TestATrialAnotherInstallTookFirstOpensNoChatItCannotRun(t *testing.T) {
	portal, root := welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	provider.RunFreeTierBootstrap(scopedPoolEnv(ctx), false)
	if got := welcomeClaimState(t, ctx, nil)["eligible"]; got != true {
		t.Fatalf("eligible %v", got)
	}
	if err := tools.MarkMachineUsed(ctx); err != nil {
		t.Fatal(err)
	}
	if code := welcomeClaimErrorCode(welcomeClaimCall(ctx, "onboarding.start", map[string]any{"fresh_launch": true})); !pyjson.Equal(code, 5080) {
		t.Fatalf("refused code %v", code)
	}
	state := welcomeClaimState(t, ctx, nil)
	if state["run"] != true || state["eligible"] != false || state["session_id"] != nil {
		t.Fatalf("state %v", state)
	}
	if portal.Creates() != 0 {
		t.Fatalf("creates %d", portal.Creates())
	}
	welcomeClaimSaveKey(t, root)
	if _, has := welcomeClaimCall(ctx, "onboarding.start", map[string]any{"fresh_launch": true})["result"]; !has {
		t.Fatal("a start with a provider set did not run")
	}
}

func TestACrashRightAfterTheClaimKeepsOnboardingDue(t *testing.T) {
	portal, _ := welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	restore := provider.SwapWelcomeMarkUsedForTests(func(context.Context) error { return errors.New("the process died here") })
	if code := welcomeClaimErrorCode(welcomeClaimCall(ctx, "onboarding.start", nil)); !pyjson.Equal(code, 5078) {
		restore()
		t.Fatalf("crash code %v", code)
	}
	restore()
	tools.InstallStateProbeClear()
	state := welcomeClaimState(t, ctx, nil)
	if state["run"] != true || state["eligible"] != true {
		t.Fatalf("state %v", state)
	}
	if portal.Creates() != 1 {
		t.Fatalf("creates %d", portal.Creates())
	}
}

func TestOnlyTheLocalDesktopBackendOpensOnboardingByItself(t *testing.T) {
	_, root := welcomeClaimPortal(t)
	if got := welcomeClaimState(t, context.Background(), nil)["eligible"]; got != false {
		t.Fatalf("remote backend eligible %v", got)
	}
	ctx := welcomeClaimLocal()
	if err := tools.MarkMachineUsed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := welcomeClaimState(t, ctx, nil); !pyjson.Equal(got, map[string]any{"run": false, "eligible": false, "session_id": nil, "run_id": nil}) {
		t.Fatalf("used machine state %v", got)
	}
	welcomeClaimSaveKey(t, root)
	if got := welcomeClaimState(t, ctx, nil)["eligible"]; got != true {
		t.Fatalf("eligible with a key %v", got)
	}
}

func TestEligibleFollowsTheDefaultProfileNotTheLaunchProfile(t *testing.T) {
	_, root := welcomeClaimPortal(t)
	t.Setenv("HOME", filepath.Dir(root))
	work, err := manage.CreateProfile(context.Background(), "work", manage.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", work)
	t.Setenv("OPENROUTER_API_KEY", "")
	os.Unsetenv("OPENROUTER_API_KEY")
	welcomeClaimSaveKey(t, root)
	ctx := welcomeClaimLocal()
	if err := tools.MarkMachineUsed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := welcomeClaimState(t, ctx, nil)["eligible"]; got != true {
		t.Fatalf("eligible %v", got)
	}
}

func TestAClaimRefreshesTheSetupRecord(t *testing.T) {
	welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	provider.RunFreeTierBootstrap(scopedPoolEnv(ctx), false)
	if _, has := welcomeClaimCall(ctx, "onboarding.start", nil)["result"]; !has {
		t.Fatal("onboarding.start answered no result")
	}
	if record := provider.CurrentSetupRecord(); record == nil || !record.FreeTierAccount {
		t.Fatalf("setup record %+v", record)
	}
}

func TestTheDevFreshInstallSwitchTreatsAUsedMachineAsNew(t *testing.T) {
	portal, _ := welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	if err := tools.MarkMachineUsed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := welcomeClaimState(t, ctx, map[string]any{"assume_fresh": true})["run"]; got != true {
		t.Fatalf("run %v", got)
	}
	if _, has := welcomeClaimCall(ctx, "onboarding.start", map[string]any{"assume_fresh": true})["result"]; !has {
		t.Fatal("onboarding.start answered no result")
	}
	if portal.Creates() != 1 {
		t.Fatalf("creates %d", portal.Creates())
	}
}

func TestTheDueAnswerIsWorkedOutAtBootAndServedFromThere(t *testing.T) {
	welcomeClaimPortal(t)
	ctx := welcomeClaimLocal()
	provider.RunFreeTierBootstrap(scopedPoolEnv(ctx), false)
	var mu sync.Mutex
	onBoot := []bool{}
	t.Cleanup(provider.WrapWelcomeResolveForTests(func(next func() (string, error)) (string, error) {
		stack := make([]byte, 64<<10)
		stack = stack[:runtime.Stack(stack, false)]
		mu.Lock()
		onBoot = append(onBoot, bytes.Contains(stack, []byte("tuiserver.startDueBoot")))
		mu.Unlock()
		return next()
	}))
	startDueBoot(ctx)
	answers := []map[string]any{}
	for range 3 {
		answers = append(answers, welcomeClaimState(t, ctx, nil))
	}
	if answers[0]["run"] != true || answers[0]["eligible"] != true {
		t.Fatalf("first answer %v", answers[0])
	}
	for _, answer := range answers[1:] {
		if !reflect.DeepEqual(answer, answers[0]) {
			t.Fatalf("answers %v", answers)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(onBoot) == 0 || slices.Contains(onBoot, false) {
		t.Fatalf("ladder walks on the boot thread %v", onBoot)
	}
}

func TestTheCachedDueAnswerFollowsSharedAuthAndTheManagedLayer(t *testing.T) {
	for _, c := range []struct {
		change, moved string
		write         func(t *testing.T, ctx context.Context, portal *anonportaltest.Portal, managed string)
	}{
		{"shared_connector_guest", "eligible", func(t *testing.T, ctx context.Context, portal *anonportaltest.Portal, managed string) {
			provider.WriteSharedNousState(scopedPoolEnv(ctx), map[string]any{"auth_method": provider.AnonAuthMethod,
				"account_tier": provider.AnonAccountTier, "anon_token": "anon_other", "client_id": provider.AnonClientID,
				"portal_base_url": portal.URL, provider.WelcomeClaimFlag: false}, false)
		}},
		{"managed_onboarding_off", "run", func(t *testing.T, ctx context.Context, portal *anonportaltest.Portal, managed string) {
			if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("onboarding:\n  run: false\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.change, func(t *testing.T) {
			portal, _ := welcomeClaimPortal(t)
			managed := filepath.Join(t.TempDir(), "managed")
			mkdirAll(t, managed)
			t.Setenv("ICEBATH_MANAGED_DIR", managed)
			ctx := welcomeClaimLocal()
			provider.RunFreeTierBootstrap(scopedPoolEnv(ctx), false)
			cached := welcomeClaimState(t, ctx, nil)
			if cached["run"] != true || cached["eligible"] != true {
				t.Fatalf("cached answer %v", cached)
			}
			c.write(t, ctx, portal, managed)
			if got := welcomeClaimState(t, ctx, nil)[c.moved]; got != false {
				t.Fatalf("%s after the change = %v", c.moved, got)
			}
		})
	}
}
