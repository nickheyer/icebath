package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// Profile env under an install root with a fake offer clock starting at one million seconds
func offerTestEnv(t *testing.T, profile string) (PoolEnv, *float64) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	return offerProfileEnv(t, root, profile), offerFakeClock(t)
}

// Env of a named profile under root, ICEBATH_HOME pointing at it
func offerProfileEnv(t *testing.T, root, profile string) PoolEnv {
	t.Helper()
	h := filepath.Join(root, "profiles", profile)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return modelsPoolEnv(context.Background())
}

func offerFakeClock(t *testing.T) *float64 {
	now := 1_000_000.0
	prev := freeTierOfferClock
	freeTierOfferClock = func() float64 { return now }
	t.Cleanup(func() { freeTierOfferClock = prev })
	return &now
}

// Writes the profile's providers.nous state, nil clearing it
func offerSetNous(t *testing.T, env PoolEnv, state map[string]any) {
	t.Helper()
	err := env.Store.Update(func(data AuthData) (bool, error) {
		providers := section(data, "providers")
		if state == nil {
			delete(providers, "nous")
		} else {
			providers["nous"] = state
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A guest carrying the welcome trial, the state free inference reads as active
func offerOnTrial(t *testing.T, env PoolEnv) {
	offerSetNous(t, env, map[string]any{"auth_method": AnonAuthMethod, WelcomeClaimFlag: true})
}

func offerDue(t *testing.T, env PoolEnv, kind string) (int, bool) {
	t.Helper()
	due, ok, err := OfferDueIn(env, kind)
	if err != nil {
		t.Fatal(err)
	}
	return due, ok
}

func offerClaim(t *testing.T, env PoolEnv, kind string) bool {
	t.Helper()
	claimed, err := ClaimOffer(env, kind)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func offerTask(t *testing.T, env PoolEnv, kind string) {
	t.Helper()
	if err := RecordTaskDone(env, kind); err != nil {
		t.Fatal(err)
	}
}

func offerWantDue(t *testing.T, env PoolEnv, kind string, want int) {
	t.Helper()
	if due, ok := offerDue(t, env, kind); !ok || due != want {
		t.Fatalf("offer due in = %d %v, want %d", due, ok, want)
	}
}

func offerWantNone(t *testing.T, env PoolEnv, kind string) {
	t.Helper()
	if due, ok := offerDue(t, env, kind); ok {
		t.Fatalf("offer due in = %d, want none", due)
	}
}

// Advances the clock to the due offer and claims it
func offerTakeDue(t *testing.T, env PoolEnv, clock *float64) {
	t.Helper()
	due, ok := offerDue(t, env, FreeTierOfferTrial)
	if !ok {
		t.Fatal("no offer pending")
	}
	*clock += float64(due)
	if !offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("due offer not granted")
	}
}

func TestFreeTierOfferFirstIsDueAfterTheDelayFromTheFirstTaskAndLaterTasksDoNotMoveIt(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerWantNone(t, env, FreeTierOfferTrial)
	offerTask(t, env, FreeTierOfferTrial)
	offerWantDue(t, env, FreeTierOfferTrial, OfferDelaySeconds)
	*clock += 60
	offerTask(t, env, FreeTierOfferTrial)
	offerWantDue(t, env, FreeTierOfferTrial, OfferDelaySeconds-60)
	*clock += OfferDelaySeconds
	offerWantDue(t, env, FreeTierOfferTrial, 0)
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(envHome(env))), "free_tier", "sign_in_offer.json")); err != nil {
		t.Fatalf("record not under the default root: %v", err)
	}
}

func TestFreeTierOfferClaimIsRefusedBeforeDueThenGrantedOnce(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerTask(t, env, FreeTierOfferTrial)
	if offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("claimed before due")
	}
	*clock += OfferDelaySeconds
	if !offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("due offer refused")
	}
	if offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("offer granted twice")
	}
}

func TestFreeTierOfferNoReofferWithoutATaskFinishedAfterTheOffer(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerTask(t, env, FreeTierOfferTrial)
	offerTakeDue(t, env, clock)
	*clock += 10 * ReofferAfterSeconds[2]
	offerWantNone(t, env, FreeTierOfferTrial)
	if offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("time alone brought the offer back")
	}
}

func TestFreeTierOfferReofferIsTheLaterOfTaskPlusDelayAndOfferPlusBackOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		taskAfter float64
		wantDueIn int
	}{
		{"back-off-bound", 60, int(ReofferAfterSeconds[0]) - 60},
		{"task-bound", ReofferAfterSeconds[0], OfferDelaySeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, clock := offerTestEnv(t, "setup")
			offerOnTrial(t, env)
			offerTask(t, env, FreeTierOfferTrial)
			offerTakeDue(t, env, clock)
			*clock += tc.taskAfter
			offerTask(t, env, FreeTierOfferTrial)
			offerWantDue(t, env, FreeTierOfferTrial, tc.wantDueIn)
		})
	}
}

func TestFreeTierOfferBackOffClimbsTheLadderThenHoldsAtItsLastStep(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerTask(t, env, FreeTierOfferTrial)
	offerTakeDue(t, env, clock)
	for _, backoff := range append(ReofferAfterSeconds[:], ReofferAfterSeconds[2]) {
		*clock += 1
		offerTask(t, env, FreeTierOfferTrial)
		offerWantDue(t, env, FreeTierOfferTrial, int(backoff)-1)
		offerTakeDue(t, env, clock)
	}
}

func TestFreeTierOfferConcurrentClaimsGrantExactlyOne(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerTask(t, env, FreeTierOfferTrial)
	*clock += OfferDelaySeconds
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := []bool{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := ClaimOffer(env, FreeTierOfferTrial)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			results = append(results, claimed)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(results, func(a, b bool) int {
		if a == b {
			return 0
		}
		if !a {
			return -1
		}
		return 1
	})
	want := []bool{false, false, false, false, false, false, false, true}
	if !slices.Equal(results, want) {
		t.Fatalf("claims = %v", results)
	}
}

func TestFreeTierOfferRecordIsInstallWideAcrossProfiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	setup := offerProfileEnv(t, root, "setup")
	offerFakeClock(t)
	offerOnTrial(t, setup)
	offerTask(t, setup, FreeTierOfferTrial)
	work := offerProfileEnv(t, root, "work")
	offerOnTrial(t, work)
	offerWantDue(t, work, FreeTierOfferTrial, OfferDelaySeconds)
}

func TestFreeTierOfferNeverOfferedOffTheFreeTier(t *testing.T) {
	env, clock := offerTestEnv(t, "setup")
	offerOnTrial(t, env)
	offerTask(t, env, FreeTierOfferTrial)
	*clock += OfferDelaySeconds
	offerSetNous(t, env, map[string]any{"auth_method": AnonAuthMethod, WelcomeClaimFlag: false})
	offerWantNone(t, env, FreeTierOfferTrial)
	if offerClaim(t, env, FreeTierOfferTrial) {
		t.Fatal("offered off the free tier")
	}
}

func TestFreeTierOfferConnectorIsOnlyForAGuestThatConnectedAnAppWithoutTheTrial(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		guest, connected, trial, allowed, offered bool
	}{
		{"connector-guest", true, true, false, true, true},
		{"nothing-connected", true, false, false, true, false},
		{"on-trial", true, true, true, true, false},
		{"signed-in", false, true, false, true, false},
		{"guests-off", true, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := offerTestEnv(t, "setup")
			state := map[string]any{"auth_method": "device_code"}
			if tc.guest {
				state["auth_method"] = AnonAuthMethod
			}
			if tc.connected {
				state[ConnectedAppFlag] = true
			}
			if tc.trial {
				state[WelcomeClaimFlag] = true
			}
			offerSetNous(t, env, state)
			if !tc.allowed {
				anonWriteNousConfig(t, env, "  guest: false\n")
			}
			offerTask(t, env, FreeTierOfferConnector)
			if tc.offered {
				offerWantDue(t, env, FreeTierOfferConnector, OfferDelaySeconds)
			} else {
				offerWantNone(t, env, FreeTierOfferConnector)
			}
			offerWantNone(t, env, FreeTierOfferTrial)
		})
	}
}
