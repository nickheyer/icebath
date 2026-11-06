package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fake portal on the desktop backend with the install probe seeing only this test's homes and markers
func trialBootMachine(t *testing.T) (*anonFakePortal, PoolEnv) {
	t.Helper()
	portal, env := installAnonPortal(t)
	anonNoAWS(t)
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "os-home"))
	t.Setenv("PATH", filepath.Join(base, "empty-path"))
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "HERMES_HOME",
		"ICEBATH_SHARED_AUTH_DIR", "HERMES_SHARED_AUTH_DIR"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	tools.InstallStateProbeClear()
	resetFreeTierBootstrapForTests()
	t.Cleanup(func() {
		tools.InstallStateProbeClear()
		resetFreeTierBootstrapForTests()
	})
	return portal, anonDesktopEnv(env)
}

// One desktop session with a single user message in the profile's state.db
func trialBootChat(t *testing.T, env PoolEnv, model string) {
	t.Helper()
	db, err := state.Open(filepath.Join(env.Store.Home(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "desktop", Model: model}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(context.Background(), "s1", map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

// The root config's onboarding.run with the managed overlay, nil when unset
func trialBootRootRun(t *testing.T) any {
	t.Helper()
	doc, err := config.LoadUserEffective(home.ConfigPath(home.Root()), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	section, _ := doc["onboarding"].(map[string]any)
	return section["run"]
}

// A relaunch clears the probe and the bootstrap memo then boots again
func trialBootRelaunch(env PoolEnv) *SetupRecord {
	tools.InstallStateProbeClear()
	resetFreeTierBootstrapForTests()
	return RunFreeTierBootstrap(env, false)
}

func TestWelcomeTrialALostTrialIsNeverClaimedAgainAfterARelaunch(t *testing.T) {
	losses := []struct {
		name    string
		lose    func(t *testing.T, env PoolEnv)
		creates int
	}{
		{"reaped", func(t *testing.T, env PoolEnv) {
			if err := clearDeadGuest(env, AnonCredentialDead, ""); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"replaced_by_connector", func(t *testing.T, env PoolEnv) {
			if err := clearDeadGuest(env, AnonCredentialDead, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
				t.Fatal(err)
			}
		}, 2},
	}
	for _, loss := range losses {
		t.Run(loss.name, func(t *testing.T) {
			portal, env := trialBootMachine(t)
			if claimed, err := ClaimDesktopFreeTier(env, false, false); err != nil || claimed == nil {
				blocker, _ := welcomeClaimBlocker(env, false, nil)
				t.Fatalf("%v %v %q", claimed, err, blocker)
			}
			trialBootChat(t, env, GuestModel)
			loss.lose(t, env)
			trialBootRelaunch(env)
			if again, err := ClaimDesktopFreeTier(env, false, false); again != nil || err != nil {
				t.Fatalf("%v %v", again, err)
			}
			if portal.count("/api/anonymous/create") != loss.creates || profileFreeInferenceActive(env) {
				t.Fatal(portal.paths(), profileFreeInferenceActive(env))
			}
			anonResolveRaises(t, env)
			if record := CurrentSetupRecord(); record == nil || record.ProviderConfigured || record.FreeTierAccount {
				t.Fatalf("%+v", record)
			}
		})
	}
}

func TestWelcomeTrialTheTokenSeamRefusesAnInferenceTokenToAGuestWithoutTheTrial(t *testing.T) {
	portal, env := installAnonPortal(t)
	env = anonDesktopEnv(env)
	held, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || held == nil {
		t.Fatalf("%v %v", held, err)
	}
	guest := map[string]any{}
	for k, v := range held {
		guest[k] = v
	}
	_, err = refreshGuestState(env, guest, &http.Client{Timeout: 15 * time.Second}, "inference")
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("%T %v", err, err)
	}
	for _, purpose := range anonPurposes(portal) {
		if purpose != "connectors" {
			t.Fatal(anonPurposes(portal))
		}
	}
}

func TestWelcomeTrialTheFirstDesktopBootRecordsThatOnboardingIsDue(t *testing.T) {
	for _, used := range []bool{false, true} {
		name := map[bool]string{false: "fresh", true: "used"}[used]
		t.Run(name, func(t *testing.T) {
			_, env := trialBootMachine(t)
			if used {
				if err := tools.MarkMachineUsed(); err != nil {
					t.Fatal(err)
				}
			}
			trialBootRelaunch(env)
			if run := trialBootRootRun(t); (used && run != nil) || (!used && run != true) {
				t.Fatalf("%v", run)
			}
			if used {
				return
			}
			if err := tools.SetOnboardingRun(false, false); err != nil {
				t.Fatal(err)
			}
			trialBootRelaunch(env)
			if run := trialBootRootRun(t); run != false {
				t.Fatalf("%v", run)
			}
		})
	}
}

func TestWelcomeTrialAMissedFirstLaunchStaysDueAndKeepsTheTrial(t *testing.T) {
	_, env := trialBootMachine(t)
	trialBootRelaunch(env)
	trialBootChat(t, env, "anthropic/claude")
	tools.InstallStateProbeClear()
	if !tools.InstallStateProbe().Existing() || !tools.OnboardingShouldRun(false) {
		t.Fatal(tools.InstallStateProbe(), tools.OnboardingShouldRun(false))
	}
	if blocker, err := welcomeClaimBlocker(env, false, nil); blocker != "" || err != nil {
		t.Fatalf("%q %v", blocker, err)
	}
	if claimed, err := ClaimDesktopFreeTier(env, false, false); claimed == nil || err != nil {
		t.Fatalf("%v %v", claimed, err)
	}
	if blocker, err := welcomeClaimBlocker(env, false, nil); blocker != "a Nous identity exists" || err != nil {
		t.Fatalf("%q %v", blocker, err)
	}
}
