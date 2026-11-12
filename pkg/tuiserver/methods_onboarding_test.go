package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Root profile home with a work profile the backend runs under, its multiplex and served state restored after
func launchedUnderWork(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	mkdirAll(t, root)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	work, err := manage.CreateProfile(context.Background(), "work", manage.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", work)
	prevMultiplex := provider.IsMultiplexActive(context.Background())
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	tools.OnboardingDueResetForTests()
	t.Cleanup(func() {
		provider.SetMultiplexActive(prevMultiplex)
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
		tools.OnboardingDueResetForTests()
	})
	return root, work
}

// One request through the real request path on ctx
func onboardingCall(ctx context.Context, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return handleRequest(ctx, map[string]any{"jsonrpc": "2.0", "id": 5, "method": method, "params": params})
}

// Onboarding section of a home's raw config
func onboardingSection(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	section, _ := raw["onboarding"].(map[string]any)
	if section == nil {
		return map[string]any{}
	}
	return section
}

func onboardingSeen(section map[string]any) map[string]any {
	seen, _ := section["seen"].(map[string]any)
	if seen == nil {
		return map[string]any{}
	}
	return seen
}

// test_onboarding_run_rpc.py test_state_answers_run_and_eligible
func TestStateAnswersRunAndEligible(t *testing.T) {
	root, _ := launchedUnderWork(t)
	local := tools.WithServingLocalDesktop(context.Background(), true)
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	want := map[string]any{"run": false, "eligible": false, "session_id": nil, "run_id": nil}
	if got := onboardingCall(local, "onboarding.state", nil)["result"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("state %v", got)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENROUTER_API_KEY=sk-or-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := onboardingCall(local, "onboarding.state", nil)["result"].(map[string]any); got["eligible"] != true {
		t.Fatalf("state with a default profile key %v", got)
	}
}

// test_onboarding_run_rpc.py test_set_run_writes_the_root_config
func TestSetRunWritesTheRootConfig(t *testing.T) {
	root, work := launchedUnderWork(t)
	ctx := context.Background()
	want := map[string]any{"run": true, "eligible": false, "session_id": nil, "run_id": nil}
	if got := onboardingCall(ctx, "onboarding.set_run", map[string]any{"run": true})["result"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("set_run %v", got)
	}
	if onboardingSection(t, root)["run"] != true {
		t.Fatal("root config run not written")
	}
	if _, present := onboardingSection(t, work)["run"]; present {
		t.Fatal("work config got the run flag")
	}
	if got := onboardingCall(ctx, "onboarding.state", nil)["result"].(map[string]any); got["run"] != true {
		t.Fatalf("state %v", got)
	}
}

// test_onboarding_run_rpc.py test_start_marks_the_first_chat_offer_in_the_root_config
func TestStartMarksTheFirstChatOfferInTheRootConfig(t *testing.T) {
	root, work := launchedUnderWork(t)
	onboardingCall(context.Background(), "onboarding.set_run", map[string]any{"run": false, "mark_profile_offered": true})
	section := onboardingSection(t, root)
	if section["run"] != false || onboardingSeen(section)[tools.OnboardingProfileBuildFlag] != true {
		t.Fatalf("root onboarding %v", section)
	}
	if _, present := onboardingSeen(onboardingSection(t, work))[tools.OnboardingProfileBuildFlag]; present {
		t.Fatal("work config got the offer flag")
	}
}

// test_onboarding_run_rpc.py test_skip_leaves_the_first_chat_offer_alone
func TestSkipLeavesTheFirstChatOfferAlone(t *testing.T) {
	root, _ := launchedUnderWork(t)
	onboardingCall(context.Background(), "onboarding.set_run", map[string]any{"run": false})
	section := onboardingSection(t, root)
	if section["run"] != false {
		t.Fatalf("root onboarding %v", section)
	}
	if _, present := onboardingSeen(section)[tools.OnboardingProfileBuildFlag]; present {
		t.Fatal("skip marked the offer")
	}
}

// test_onboarding_run_rpc.py test_a_failed_write_fails_the_call_and_writes_nothing
func TestAFailedWriteFailsTheCallAndWritesNothing(t *testing.T) {
	root, _ := launchedUnderWork(t)
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o700) })
	if _, failed := onboardingCall(context.Background(), "onboarding.set_run", map[string]any{"run": false, "mark_profile_offered": true})["error"]; !failed {
		t.Fatal("a failed write answered a result")
	}
	section := onboardingSection(t, root)
	if _, present := section["run"]; present {
		t.Fatalf("run written %v", section)
	}
	if _, present := onboardingSeen(section)[tools.OnboardingProfileBuildFlag]; present {
		t.Fatal("offer flag written")
	}
}

// test_onboarding_run_rpc.py test_set_run_requires_a_bool
func TestSetRunRequiresABool(t *testing.T) {
	launchedUnderWork(t)
	ctx := context.Background()
	if _, failed := onboardingCall(ctx, "onboarding.set_run", nil)["error"]; !failed {
		t.Fatal("missing run accepted")
	}
	if _, failed := onboardingCall(ctx, "onboarding.set_run", map[string]any{"run": "yes"})["error"]; !failed {
		t.Fatal("string run accepted")
	}
}
