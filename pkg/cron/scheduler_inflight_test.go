package cron

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

func twoProfileStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, h := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", a)
	return openCronStore(a, nil), openCronStore(b, nil)
}

func TestSameJobIDInTwoProfilesDoesNotCollide(t *testing.T) {
	sa, sb := twoProfileStores(t)
	id := "daily-brief"
	t.Cleanup(func() { sa.releaseRunningJob(id, "", nil); sb.releaseRunningJob(id, "", nil) })
	if !sa.tryRegisterRunningJob(id, nil, futurePending) || !sb.tryRegisterRunningJob(id, nil, futurePending) {
		t.Fatal("a second profile's same-named job must not read as a duplicate")
	}
	if sb.tryRegisterRunningJob(id, nil, futurePending) {
		t.Fatal("duplicate registration admitted")
	}
	sa.releaseRunningJob(id, "", nil)
	inflight.Lock()
	held := inflight.running[sb.inflightKey(id, sb.Home())]
	inflight.Unlock()
	if !held || !getRunningJobIDs()[id] {
		t.Fatal("profile A's release must not drop profile B's claim")
	}
}

func TestIsJobRunningAnswersForTheAskingProfileOnly(t *testing.T) {
	sa, sb := twoProfileStores(t)
	id := "daily-brief"
	t.Cleanup(func() { sa.releaseRunningJob(id, "", nil) })
	if !sa.tryRegisterRunningJob(id, nil, futurePending) || !isJobRunning(sa, id) || !sa.jobRunningInThisProcess(id) {
		t.Fatal("registered run not live in its own home")
	}
	if isJobRunning(sb, id) || sb.jobRunningInThisProcess(id) {
		t.Fatal("profile B's idle job must not inherit profile A's run")
	}
	if !getRunningJobIDs()[id] {
		t.Fatal("host wide ids miss the run")
	}
}

func TestReleaseHonoursTheRegistrationOwner(t *testing.T) {
	sa, _ := twoProfileStores(t)
	id := "owned"
	first, second := new(int), new(int)
	t.Cleanup(func() { sa.releaseRunningJob(id, "", nil) })
	if !sa.tryRegisterRunningJob(id, first, futurePending) {
		t.Fatal("register")
	}
	sa.releaseRunningJob(id, "", second)
	if !isJobRunning(sa, id) {
		t.Fatal("a replaced owner released the registration")
	}
	sa.recordExternalCronWorker(id, "4321", true)
	details := getRunningJobDetails()
	if len(details) != 1 || details[0].WorkerPID == nil || *details[0].WorkerPID != 4321 || !details[0].RestartSafe || details[0].ElapsedS == nil {
		t.Fatalf("details %+v", details)
	}
	if c := sa.getRestartWaitCronCounts(); c != (RestartWaitCounts{RestartSafe: 1}) {
		t.Fatalf("counts %+v", c)
	}
	sa.releaseRunningJob(id, "", first)
	if isJobRunning(sa, id) || len(getRunningJobDetails()) != 0 {
		t.Fatal("owner release left the run registered")
	}
}

func TestRetirementRefusesRegistration(t *testing.T) {
	sa, _ := twoProfileStores(t)
	prevFence, prevProof := provider.Retirement, provider.RetirementIdleProof
	t.Cleanup(func() { provider.Retirement, provider.RetirementIdleProof = prevFence, prevProof })
	provider.Retirement = provider.NewRetirementFence()
	provider.RetirementIdleProof = func() (map[string]any, error) { return map[string]any{"idle": true}, nil }
	if permit := provider.Retirement.Prepare(); permit["ok"] != true {
		t.Fatalf("prepare refused %#v", permit)
	}
	if sa.tryRegisterRunningJob("refused", nil, futurePending) {
		t.Fatal("registration admitted during retirement")
	}
}

func TestWedgedRunsUseTheIntervalAllowance(t *testing.T) {
	sa, _ := twoProfileStores(t)
	writeJobsFile(t, sa, map[string]any{"jobs": []any{
		map[string]any{"id": "fast", "name": "fast", "prompt": "p", "schedule": map[string]any{"kind": "interval", "minutes": 5}},
		map[string]any{"id": "hourly", "name": "hourly", "prompt": "p", "schedule": map[string]any{"kind": "interval", "minutes": 60}},
	}})
	start := time.Now()
	now := start
	inflightNow = func() time.Time { return now }
	t.Cleanup(func() {
		inflightNow = time.Now
		sa.releaseRunningJob("fast", "", nil)
		sa.releaseRunningJob("hourly", "", nil)
	})
	sa.tryRegisterRunningJob("fast", nil, futurePending)
	sa.tryRegisterRunningJob("hourly", nil, futurePending)
	now = start.Add(31 * time.Minute)
	if w := sa.getWedgedJobIDs(); len(w) != 1 || !w["fast"] {
		t.Fatalf("wedged %v", w)
	}
	if c := sa.getRestartWaitCronCounts(); c != (RestartWaitCounts{Awaitable: 1, Wedged: 1}) {
		t.Fatalf("counts %+v", c)
	}
	now = start.Add(121 * time.Minute)
	if w := sa.getWedgedJobIDs(); len(w) != 2 {
		t.Fatalf("wedged %v", w)
	}
	t.Setenv("ICEBATH_CRON_INFLIGHT_MAX_MINUTES", "500")
	if v := sa.inflightMinAllowanceMinutes(); v != 500 {
		t.Fatalf("env floor %v", v)
	}
}

func TestJobIntervalMinutes(t *testing.T) {
	sa, _ := twoProfileStores(t)
	if v, ok := cronIntervalMinutes("*/15 * * * *"); !ok || v != 15 {
		t.Fatalf("cron interval %v %v", v, ok)
	}
	if _, ok := cronIntervalMinutes("not a cron"); ok {
		t.Fatal("bad expression has an interval")
	}
	if _, ok := sa.jobIntervalMinutes(&Job{Schedule: &Schedule{Kind: "interval", Minutes: 0, HasMinutes: true}}); ok {
		t.Fatal("zero minutes has an interval")
	}
	if v, ok := sa.jobIntervalMinutes(&Job{Schedule: &Schedule{Raw: "every 2h"}}); !ok || v != 120 {
		t.Fatalf("string schedule %v %v", v, ok)
	}
}

func TestPoolIsSizedByTheTickedProfile(t *testing.T) {
	sa, sb := twoProfileStores(t)
	t.Cleanup(shutdownParallelPools)
	one, four := 1, 4
	poolA, poolB := sa.getParallelPool(&one), sb.getParallelPool(&four)
	if poolA == poolB || poolA.max != 1 || poolB.max != 4 {
		t.Fatal("pools not sized per profile")
	}
	again := 1
	if sa.getParallelPool(&again) != poolA {
		t.Fatal("same cap rebuilt the pool")
	}
	if sa.getParallelPool(&four) == poolA {
		t.Fatal("cap change kept the old pool")
	}
	discardParallelPools([]string{home.Key(sb.Home())})
	if poolHomes()[0] != home.Key(sa.Home()) || len(poolHomes()) != 1 {
		t.Fatalf("pools %v", poolHomes())
	}
}

func TestParallelPoolCapsWorkersAndDrainsOnShutdown(t *testing.T) {
	two := 2
	p := newParallelPool(&two)
	var running, peak, done atomic.Int32
	var mu sync.Mutex
	release := make(chan struct{})
	var futures []*poolFuture
	for i := 0; i < 5; i++ {
		f, err := p.Submit(func() {
			n := running.Add(1)
			mu.Lock()
			if n > peak.Load() {
				peak.Store(n)
			}
			mu.Unlock()
			<-release
			running.Add(-1)
			done.Add(1)
		})
		if err != nil {
			t.Fatal(err)
		}
		futures = append(futures, f)
	}
	time.Sleep(50 * time.Millisecond)
	p.Shutdown(false)
	if _, err := p.Submit(func() {}); err != errPoolShutdown {
		t.Fatalf("submit after shutdown %v", err)
	}
	close(release)
	p.Shutdown(true)
	if peak.Load() != 2 || done.Load() != 5 || !futures[4].Done() {
		t.Fatalf("peak %d done %d", peak.Load(), done.Load())
	}
}
