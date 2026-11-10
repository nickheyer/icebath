package cron

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

type testCronScheduler struct {
	registerErr error
	registered  []string
	changed     int
}

func (t *testCronScheduler) Name() string                                         { return "builtin" }
func (t *testCronScheduler) Start(context.Context, <-chan struct{}, StartOptions) {}
func (t *testCronScheduler) RegisterJob(job *Job) error {
	t.registered = append(t.registered, job.ID)
	return t.registerErr
}
func (t *testCronScheduler) OnJobsChanged() error { t.changed++; return nil }

var testScheduler = &testCronScheduler{}

func init() { schedulerResolver = func(*Store) CronScheduler { return testScheduler } }

// Provider implementing only the two required hooks
type minimalProvider struct{ name string }

func (m *minimalProvider) Name() string                                         { return m.name }
func (m *minimalProvider) Start(context.Context, <-chan struct{}, StartOptions) {}

// Provider whose fire_due predates force, fire_due(job_id, *, adapters=None, loop=None)
type legacyProvider struct{ minimalProvider }

func (l *legacyProvider) FireDue(context.Context, string, FireOptions) (bool, error) {
	return true, nil
}
func (l *legacyProvider) FireDueKeywords() ([]string, bool) {
	return []string{"job_id", "adapters", "loop"}, false
}

// Provider whose fire_due takes force positionally only, fire_due(job_id, force=False, /)
type positionalOnlyProvider struct{ legacyProvider }

func (p *positionalOnlyProvider) FireDueKeywords() ([]string, bool) { return nil, false }

// Provider whose fire_due accepts any keyword, fire_due(job_id, **kwargs)
type sinkProvider struct{ legacyProvider }

func (k *sinkProvider) FireDueKeywords() ([]string, bool) { return []string{"job_id"}, true }

// Legacy provider that also overrides the two phase claim
type splitProvider struct{ legacyProvider }

func (p *splitProvider) ClaimFire(context.Context, string, bool, bool) (*Job, error) { return nil, nil }

type unavailableProvider struct{ minimalProvider }

func (u *unavailableProvider) IsAvailable() bool { return false }

type availableProvider struct{ minimalProvider }

func (a *availableProvider) IsAvailable() bool { return true }

func providerConfig(t *testing.T, s *Store, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Stop handle of one ticker run, created before the ticker so a tick can stop it
type tickerRun struct {
	ch   chan struct{}
	once sync.Once
	done chan struct{}
}

func newTickerRun() *tickerRun { return &tickerRun{ch: make(chan struct{}), done: make(chan struct{})} }

func (r *tickerRun) stop() { r.once.Do(func() { close(r.ch) }) }

// Starts the built in ticker on its own goroutine, stopping it and waiting for its exit at cleanup
func (r *tickerRun) start(t *testing.T, ctx context.Context, o StartOptions) *tickerRun {
	t.Helper()
	go func() {
		defer close(r.done)
		(&InProcessCronScheduler{}).Start(ctx, r.ch, o)
	}()
	t.Cleanup(func() {
		r.stop()
		<-r.done
	})
	return r
}

// Whether the ticker goroutine ended within the join timeout
func (r *tickerRun) joined(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func waitUntil(cond func() bool) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// Records every tick the ticker drives with the home it ran under
type tickRecorder struct {
	mu    sync.Mutex
	opts  []tickOptions
	homes []string
}

func (r *tickRecorder) record(ctx context.Context, s *Store, o tickOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts = append(r.opts, o)
	r.homes = append(r.homes, home.Dir(ctx))
}

func (r *tickRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opts)
}

func (r *tickRecorder) snapshot() ([]tickOptions, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tickOptions(nil), r.opts...), append([]string(nil), r.homes...)
}

func TestBackoffWaitSecondsDoublesAndCaps(t *testing.T) {
	for _, c := range []struct {
		failures int
		want     float64
	}{{0, 60}, {-1, 60}, {1, 60}, {2, 120}, {4, 480}, {6, 900}, {40, 900}} {
		if got := backoffWaitSeconds(60, c.failures); got != c.want {
			t.Fatal(c.failures, got)
		}
	}
}

func TestAbcGrowthStaysAdditive(t *testing.T) {
	it := reflect.TypeOf((*CronScheduler)(nil)).Elem()
	var required []string
	for i := range it.NumMethod() {
		required = append(required, it.Method(i).Name)
	}
	if !reflect.DeepEqual(required, []string{"Name", "Start"}) {
		t.Fatalf("CronScheduler required methods changed to %v; growth must be additive (optional methods with defaults), not new required methods.", required)
	}
	p := &minimalProvider{"current"}
	if schedulerOnJobsChanged(p) != nil || schedulerRegisterJob(p, &Job{}) != nil || schedulerReconcile(p) != nil || !schedulerIsAvailable(p) {
		t.Fatal("optional hooks must default safely")
	}
}

func TestForceFireCapabilityDetectsLegacyOverride(t *testing.T) {
	if !providerSupportsForceFire(&minimalProvider{"current"}) {
		t.Fatal("the base fire_due accepts force")
	}
	if providerSupportsForceFire(&legacyProvider{}) {
		t.Fatal("a legacy fire_due without force")
	}
	if providerSupportsForceFire(&positionalOnlyProvider{}) {
		t.Fatal("a positional only force is not a keyword")
	}
	if !providerSupportsForceFire(&sinkProvider{}) {
		t.Fatal("a keyword sink accepts force")
	}
}

func TestProviderSupportsSplitFireFollowsTheOverrides(t *testing.T) {
	if !providerSupportsSplitFire(&minimalProvider{}) || providerSupportsSplitFire(&legacyProvider{}) || !providerSupportsSplitFire(&splitProvider{}) {
		t.Fatal("split fire detection")
	}
}

func TestInprocessProviderTicksAndStops(t *testing.T) {
	s, _ := newTestStore(t)
	rec := &tickRecorder{}
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		return 0, nil
	})
	prov := &InProcessCronScheduler{}
	if prov.Name() != "builtin" {
		t.Fatal(prov.Name())
	}
	zero := 0.0
	run := newTickerRun().start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero})
	if !waitUntil(func() bool { return rec.count() >= 1 }) {
		t.Fatal("provider never called tick()")
	}
	run.stop()
	if !run.joined(5 * time.Second) {
		t.Fatal("provider did not exit after stop_event was set")
	}
	opts, _ := rec.snapshot()
	if len(opts) < 1 || opts[0].Sync {
		t.Fatalf("ticks %+v", opts)
	}
}

func TestCronProviderPackageDoesNotShadowCoreCronPackage(t *testing.T) {
	sdk, err := filepath.Abs(filepath.Join("..", "plugins", "hostchild", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	probe := `import sys
from importlib.machinery import PathFinder
from pathlib import Path
root = Path(sys.argv[1])
sys.path.insert(0, str(root))
sys.path.insert(0, str(root / "plugins"))
cron_spec = PathFinder.find_spec("cron")
assert cron_spec is not None
assert Path(cron_spec.origin).resolve() == root / "cron" / "__init__.py", cron_spec.origin
provider_spec = PathFinder.find_spec("cron.scheduler_provider", [str(root / "cron")])
assert provider_spec is not None
assert Path(provider_spec.origin).resolve() == root / "cron" / "scheduler_provider.py", provider_spec.origin
`
	if out, err := exec.Command(python, "-I", "-c", probe, sdk).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestResolveDefaultsToBuiltin(t *testing.T) {
	s, _ := newTestStore(t)
	providerConfig(t, s, "cron:\n  provider: ''\n")
	if got := resolveCronScheduler(s).Name(); got != "builtin" {
		t.Fatal(got)
	}
}

func TestResolveNoCronSectionFallsBackToBuiltin(t *testing.T) {
	s, _ := newTestStore(t)
	providerConfig(t, s, "{}\n")
	if got := resolveCronScheduler(s).Name(); got != "builtin" {
		t.Fatal(got)
	}
}

func TestResolveUnknownProviderFallsBackToBuiltin(t *testing.T) {
	s, _ := newTestStore(t)
	patchChronos(t, &loadCronScheduler, realLoadCronScheduler)
	providerConfig(t, s, "cron:\n  provider: nope-not-real\n")
	if got := resolveCronScheduler(s).Name(); got != "builtin" {
		t.Fatal(got)
	}
}

func TestResolveUnavailableProviderFallsBack(t *testing.T) {
	s, _ := newTestStore(t)
	patchChronos(t, &loadCronScheduler, func(*Store, string) (CronScheduler, error) {
		return &unavailableProvider{minimalProvider{"unavailable"}}, nil
	})
	providerConfig(t, s, "cron:\n  provider: unavailable\n")
	if got := resolveCronScheduler(s).Name(); got != "builtin" {
		t.Fatal(got)
	}
}

func TestResolveAvailableProviderIsUsed(t *testing.T) {
	s, _ := newTestStore(t)
	patchChronos(t, &loadCronScheduler, func(*Store, string) (CronScheduler, error) {
		return &availableProvider{minimalProvider{"fake"}}, nil
	})
	providerConfig(t, s, "cron:\n  provider: fake\n")
	if got := resolveCronScheduler(s).Name(); got != "fake" {
		t.Fatal(got)
	}
}

func TestResolveFallsBackOnANonStringOrFailingProvider(t *testing.T) {
	s, logs := newTestStore(t)
	providerConfig(t, s, "cron:\n  provider: 7\n")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok {
		t.Fatal("a non string provider must resolve the builtin")
	}
	patchChronos(t, &loadCronScheduler, func(*Store, string) (CronScheduler, error) { return nil, errors.New("boom") })
	providerConfig(t, s, "cron:\n  provider: ' live '\n")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok || !strings.Contains(logs.String(), "Failed to load cron.provider 'live' (boom); using built-in ticker") {
		t.Fatal(logs.String())
	}
	patchChronos(t, &loadCronScheduler, func(*Store, string) (CronScheduler, error) { return nil, nil })
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok || !strings.Contains(logs.String(), "cron.provider 'live' not found; using built-in ticker") {
		t.Fatal(logs.String())
	}
}

func TestExternalProviderFallsBackToBuiltinUnderMultiplex(t *testing.T) {
	s, _ := newTestStore(t)
	external := &minimalProvider{"external"}
	if got := s.schedulerForProfileMode(external, false); got != external {
		t.Fatal(got)
	}
	if _, ok := s.schedulerForProfileMode(external, true).(*InProcessCronScheduler); !ok {
		t.Fatal("multiplex must fall back to the built in ticker")
	}
}

// One recorded store claim of the base claim_fire
type claimCall struct {
	jobID string
	opts  FireClaimOptions
}

func TestFireDueDefaultClaimsThenRuns(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := withCronStore(context.Background(), s)
	var claims []claimCall
	var ran [][2]string
	patchChronos(t, &providerClaimJobForFire, func(_ *Store, _ context.Context, jobID string, o FireClaimOptions) (*Job, bool, error) {
		claims = append(claims, claimCall{jobID, o})
		return &Job{ID: jobID, Name: "t", FireClaim: &Claim{By: "exact-owner"}}, true, nil
	})
	patchChronos(t, &runOneJobSeam, func(_ context.Context, _ *Store, job *Job, _ runOneJobOptions) (bool, error) {
		ran = append(ran, [2]string{job.ID, job.FireClaim.By})
		return true, nil
	})
	ok, err := schedulerFireDue(ctx, &InProcessCronScheduler{}, "j1", FireOptions{})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(claims, []claimCall{{"j1", FireClaimOptions{}}}) {
		t.Fatalf("claims %+v", claims)
	}
	if !reflect.DeepEqual(ran, [][2]string{{"j1", "exact-owner"}}) {
		t.Fatalf("ran %v", ran)
	}
}

func TestClaimFirePersistsAttemptBeforeFireClaimed(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := withCronStore(context.Background(), s)
	var events []string
	patchChronos(t, &providerClaimJobForFire, func(_ *Store, _ context.Context, jobID string, _ FireClaimOptions) (*Job, bool, error) {
		events = append(events, "claim")
		return &Job{ID: jobID, FireClaim: &Claim{By: "owner"}}, true, nil
	})
	create := providerCreateExecution
	patchChronos(t, &providerCreateExecution, func(ctx context.Context, s *Store, jobID, source string) (map[string]any, error) {
		events = append(events, "ledger")
		return create(ctx, s, jobID, source)
	})
	patchChronos(t, &runOneJobSeam, func(_ context.Context, _ *Store, job *Job, _ runOneJobOptions) (bool, error) {
		events = append(events, "run:"+deref(job.ExecutionID))
		return true, nil
	})
	provider := &InProcessCronScheduler{}
	claimed, err := schedulerClaimFire(ctx, provider, "j1", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"ledger", "claim"}) {
		t.Fatalf("events %v", events)
	}
	if claimed == nil || claimed.ExecutionID == nil {
		t.Fatalf("claimed %+v", claimed)
	}
	if row := mustLedger(t)(openExecutionsLedger(s).Get(ctx, *claimed.ExecutionID)); row["status"] != "claimed" {
		t.Fatalf("row %v", row)
	}
	if ok, err := schedulerFireClaimed(ctx, provider, claimed, FireOptions{}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(events, []string{"ledger", "claim", "run:" + *claimed.ExecutionID}) {
		t.Fatalf("events %v", events)
	}
}

func TestFireDueForwardsManualForceToStoreClaim(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := withCronStore(context.Background(), s)
	var claims []claimCall
	patchChronos(t, &providerClaimJobForFire, func(_ *Store, _ context.Context, jobID string, o FireClaimOptions) (*Job, bool, error) {
		claims = append(claims, claimCall{jobID, o})
		return &Job{ID: jobID, Name: "t", FireClaim: &Claim{By: "manual-owner"}}, true, nil
	})
	patchChronos(t, &runOneJobSeam, func(context.Context, *Store, *Job, runOneJobOptions) (bool, error) { return true, nil })
	if ok, err := schedulerFireDue(ctx, &InProcessCronScheduler{}, "j1", FireOptions{Force: true}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(claims, []claimCall{{"j1", FireClaimOptions{Force: true}}}) {
		t.Fatalf("claims %+v", claims)
	}
	if ok, err := schedulerFireDue(ctx, &InProcessCronScheduler{}, "j1", FireOptions{Manual: true}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := claims[len(claims)-1]; got != (claimCall{"j1", FireClaimOptions{Manual: true}}) {
		t.Fatalf("last claim %+v", got)
	}
}

func TestFireDueLostClaimDoesNotRun(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := withCronStore(context.Background(), s)
	var ran []string
	patchChronos(t, &providerClaimJobForFire, func(*Store, context.Context, string, FireClaimOptions) (*Job, bool, error) {
		return nil, false, nil
	})
	patchChronos(t, &runOneJobSeam, func(_ context.Context, _ *Store, job *Job, _ runOneJobOptions) (bool, error) {
		ran = append(ran, job.ID)
		return true, nil
	})
	if ok, err := schedulerFireDue(ctx, &InProcessCronScheduler{}, "j1", FireOptions{}); err != nil || ok {
		t.Fatal(ok, err)
	}
	if len(ran) != 0 {
		t.Fatalf("ran %v", ran)
	}
}

func TestFailingTickRecordsLivenessButNotSuccess(t *testing.T) {
	s, _ := newTestStore(t)
	var mu sync.Mutex
	var beats []bool
	patchChronos(t, &tickHook, func(context.Context, *Store, tickOptions) (int, error) {
		return 0, errors.New("every tick fails")
	})
	patchChronos(t, &recordTickerHeartbeat, func(_ *Store, _ context.Context, success bool) {
		mu.Lock()
		defer mu.Unlock()
		beats = append(beats, success)
	})
	zero := 0.0
	run := newTickerRun().start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero})
	if !waitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return len(beats) >= 2 }) {
		t.Fatal("ticker did not record heartbeats")
	}
	run.stop()
	if !run.joined(5 * time.Second) {
		t.Fatal("ticker did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(beats) < 2 {
		t.Fatalf("beats %v", beats)
	}
	for _, b := range beats {
		if b {
			t.Fatal("a failing tick wrongly bumped the success marker")
		}
	}
}

func TestHeartbeatRoundtripAndAge(t *testing.T) {
	s, _ := newTestStore(t)
	if _, ok := s.TickerHeartbeatAge(); ok {
		t.Fatal("no heartbeat file must read as unknown")
	}
	if _, ok := s.TickerSuccessAge(); ok {
		t.Fatal("no success file must read as unknown")
	}
	s.RecordTickerHeartbeat(context.Background(), false)
	if hb, ok := s.TickerHeartbeatAge(); !ok || hb < 0 || hb >= 5 {
		t.Fatalf("heartbeat age %v %v", hb, ok)
	}
	if _, ok := s.TickerSuccessAge(); ok {
		t.Fatal("a liveness only beat set the success marker")
	}
	s.RecordTickerHeartbeat(context.Background(), true)
	if age, ok := s.TickerSuccessAge(); !ok || age < 0 || age >= 5 {
		t.Fatalf("success age %v %v", age, ok)
	}
}

func TestGuardJobCredentialExfil(t *testing.T) {
	t.Run("test_named_registry_provider_offhost_is_blocked", func(t *testing.T) {
		s, _ := newTestStore(t)
		err := s.guardJobCredentialExfil(&Job{ID: "j1", Provider: strPtr("anthropic"), BaseURL: strPtr("https://evil.example/v1")})
		if err == nil || cronExceptionName(err) != "RuntimeError" || !strings.Contains(err.Error(), "blocked for safety") {
			t.Fatalf("registry off host %v", err)
		}
	})
	t.Run("test_bare_custom_is_allowed", func(t *testing.T) {
		s, _ := newTestStore(t)
		if err := s.guardJobCredentialExfil(&Job{ID: "j4", Provider: strPtr("custom"), BaseURL: strPtr("https://anything.example/v1")}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_no_base_url_is_allowed", func(t *testing.T) {
		s, _ := newTestStore(t)
		if err := s.guardJobCredentialExfil(&Job{ID: "j5", Provider: strPtr("anthropic")}); err != nil {
			t.Fatal(err)
		}
		if err := s.guardJobCredentialExfil(&Job{ID: "j6"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_validator_exception_with_base_url_fails_closed", func(t *testing.T) {
		s, _ := newTestStore(t)
		patchChronos(t, &cronBaseURLValidator, func(*Store, *string, *string) (string, error) {
			return "", errors.New("validator blew up")
		})
		err := s.guardJobCredentialExfil(&Job{ID: "j7", Provider: strPtr("custom:legit"), BaseURL: strPtr("https://evil.example/v1")})
		if err == nil || cronExceptionName(err) != "RuntimeError" || !strings.Contains(err.Error(), "blocked for safety") {
			t.Fatalf("validator failure %v", err)
		}
	})
}

// Profile homes with their cron dirs created, in order
func profileHomes(t *testing.T, root string, names ...string) []ProfileHome {
	t.Helper()
	var out []ProfileHome
	for _, name := range names {
		h := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
			t.Fatal(err)
		}
		out = append(out, ProfileHome{Name: strPtr(name), Home: h})
	}
	return out
}

func TestMultiplexTickerTicksEachProfileOnce(t *testing.T) {
	s, _ := newTestStore(t)
	homes := profileHomes(t, t.TempDir(), "default", "home-ops")
	rec := &tickRecorder{}
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		return 0, nil
	})
	patchChronos(t, &recordTickerHeartbeat, func(*Store, context.Context, bool) {})
	zero := 0.0
	run := newTickerRun().start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero, ProfileHomes: homes})
	waitUntil(func() bool { return rec.count() >= len(homes) })
	for deadline := time.Now().Add(3 * time.Second); rec.count() < len(homes)*2 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	run.stop()
	if !run.joined(5 * time.Second) {
		t.Fatal("ticker did not exit")
	}
	if n := rec.count(); n < len(homes) {
		t.Fatalf("Expected >= %d tick calls, got %d", len(homes), n)
	}
}

func TestMultiplexTickerSkipsDeletedProfileFromStartupSnapshot(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	homes := profileHomes(t, root, "default")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(root, "profiles", "deleted")
	homes = append(homes, ProfileHome{Name: strPtr("deleted"), Home: deleted})
	var mu sync.Mutex
	var ticked []string
	run := newTickerRun()
	patchChronos(t, &tickHook, func(_ context.Context, ps *Store, _ tickOptions) (int, error) {
		mu.Lock()
		ticked = append(ticked, filepath.Dir(ps.cronDir))
		mu.Unlock()
		run.stop()
		return 0, nil
	})
	zero := 0.0
	run.start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero, ProfileHomes: homes})
	if !run.joined(5 * time.Second) {
		t.Fatal("ticker did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(ticked, []string{homes[0].Home}) {
		t.Fatalf("ticked %v", ticked)
	}
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted profile recreated %v", err)
	}
}

func TestExistingProfileHomesFiltersDeleted(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	live, deleted := filepath.Join(root, "live"), filepath.Join(root, "deleted")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range s.existingProfileHomes([]ProfileHome{{Name: strPtr("live"), Home: live}, {Name: strPtr("deleted"), Home: deleted}}, nil) {
		names = append(names, *p.Name)
	}
	if !reflect.DeepEqual(names, []string{"live"}) {
		t.Fatalf("tuples %v", names)
	}
	if got := s.existingProfileHomes([]ProfileHome{{Home: live}, {Home: deleted}}, nil); !reflect.DeepEqual(got, []ProfileHome{{Home: live}}) {
		t.Fatalf("paths %v", got)
	}
}

func TestExistingProfileHomesSkipsAFailingEnumeratorCycle(t *testing.T) {
	s, logs := newTestStore(t)
	if got := s.existingProfileHomes(nil, func() ([]ProfileHome, error) { return nil, errors.New("enum") }); len(got) != 0 || !strings.Contains(logs.String(), "cron profile enumeration failed; skipping this cycle") {
		t.Fatal(got, logs.String())
	}
}

// Adapters of the first default and secondary tick of one multiplex cycle
func runMultiplexCapture(t *testing.T, profileAdapters map[string]any, shared any) (any, any) {
	t.Helper()
	s, _ := newTestStore(t)
	homes := profileHomes(t, t.TempDir(), "default", "home-ops")
	var mu sync.Mutex
	var captured []any
	patchChronos(t, &tickHook, func(_ context.Context, _ *Store, o tickOptions) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, o.Adapters)
		return 0, nil
	})
	patchChronos(t, &recordTickerHeartbeat, func(*Store, context.Context, bool) {})
	zero := 0.0
	run := newTickerRun().start(t, withCronStore(context.Background(), s), StartOptions{
		Interval: &zero, ProfileHomes: homes, Adapters: shared, ProfileAdapters: profileAdapters, DefaultProfile: strPtr("default"),
	})
	waitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return len(captured) >= 2 })
	run.stop()
	if !run.joined(5 * time.Second) {
		t.Fatal("ticker did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) < 2 {
		t.Fatalf("expected >= 2 tick calls, got %d", len(captured))
	}
	return captured[0], captured[1]
}

// Python identity of two adapter maps
func sameAdapters(a, b any) bool {
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	return okA && okB && reflect.ValueOf(ma).UnsafePointer() == reflect.ValueOf(mb).UnsafePointer()
}

func TestMultiplexDefaultProfileUsesSharedAdapters(t *testing.T) {
	shared := map[string]any{"kind": "shared"}
	defaultAd, _ := runMultiplexCapture(t, map[string]any{"home-ops": map[string]any{"kind": "secondary"}}, shared)
	if !sameAdapters(defaultAd, shared) {
		t.Fatalf("default %v", defaultAd)
	}
}

func TestMultiplexConnectedSecondaryUsesItsOwnAdapters(t *testing.T) {
	shared, sec := map[string]any{"kind": "shared"}, map[string]any{"kind": "secondary"}
	defaultAd, secAd := runMultiplexCapture(t, map[string]any{"home-ops": sec}, shared)
	if !sameAdapters(defaultAd, shared) || !sameAdapters(secAd, sec) {
		t.Fatalf("default %v secondary %v", defaultAd, secAd)
	}
}

func TestMultiplexEmptySecondaryDoesNotFallBackToShared(t *testing.T) {
	shared := map[string]any{"kind": "shared"}
	defaultAd, secAd := runMultiplexCapture(t, map[string]any{"home-ops": map[string]any{}}, shared)
	if !sameAdapters(defaultAd, shared) || sameAdapters(secAd, shared) || pyTruthy(secAd) {
		t.Fatalf("default %v secondary %v", defaultAd, secAd)
	}
}

func TestMultiplexMissingSecondaryDoesNotFallBackToShared(t *testing.T) {
	shared := map[string]any{"kind": "shared"}
	defaultAd, secAd := runMultiplexCapture(t, map[string]any{}, shared)
	if !sameAdapters(defaultAd, shared) || sameAdapters(secAd, shared) || pyTruthy(secAd) {
		t.Fatalf("default %v secondary %v", defaultAd, secAd)
	}
}

func TestMultiplexTickerIsolatesProfileFailures(t *testing.T) {
	s, _ := newTestStore(t)
	homes := profileHomes(t, t.TempDir(), "failing", "healthy")
	failing, healthy := homes[0].Home, homes[1].Home
	for _, h := range []string{failing, healthy} {
		openCronStore(h, nil).RecordTickerError(context.Background(), "RuntimeError: stale failure")
	}
	rec := &tickRecorder{}
	run := newTickerRun()
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		if home.Dir(ctx) == failing {
			return 0, errors.New("profile-local failure")
		}
		run.stop()
		return 0, nil
	})
	zero := 0.0
	run.start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero, ProfileHomes: homes})
	if !run.joined(5 * time.Second) {
		run.stop()
		if !run.joined(5 * time.Second) {
			t.Fatal("ticker did not exit")
		}
	}
	_, ticked := rec.snapshot()
	found := false
	for _, h := range ticked {
		found = found || h == healthy
	}
	if !found {
		t.Fatal("healthy sibling was skipped")
	}
	if _, err := os.Stat(filepath.Join(failing, "cron", "ticker_last_success")); err == nil {
		t.Fatal("failing profile recorded success")
	}
	if _, err := os.Stat(filepath.Join(healthy, "cron", "ticker_last_success")); err != nil {
		t.Fatal("healthy profile missing its success marker")
	}
	if msg, ok := openCronStore(failing, nil).TickerLastError(); !ok || msg != "RuntimeError: profile-local failure" {
		t.Fatalf("failing error %q %v", msg, ok)
	}
	if msg, ok := openCronStore(healthy, nil).TickerLastError(); ok {
		t.Fatalf("healthy error %q", msg)
	}
}

func TestMultiplexRecoveryIsolatesProfileFailures(t *testing.T) {
	s, _ := newTestStore(t)
	homes := profileHomes(t, t.TempDir(), "failing", "healthy")
	failing, healthy := homes[0].Home, homes[1].Home
	var mu sync.Mutex
	var recovered []string
	patchChronos(t, &recoverInterruptedExecutions, func(ctx context.Context, _ *Store) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		recovered = append(recovered, home.Dir(ctx))
		if home.Dir(ctx) == failing {
			return 0, &state.SQLiteError{Class: state.SQLiteOperationalError, Msg: "unable to open database file"}
		}
		return 0, nil
	})
	rec := &tickRecorder{}
	run := newTickerRun()
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		if rec.count() >= 2 {
			run.stop()
		}
		return 0, nil
	})
	zero := 0.0
	run.start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero, ProfileHomes: homes})
	if !run.joined(5 * time.Second) {
		run.stop()
		if !run.joined(5 * time.Second) {
			t.Fatal("ticker did not exit")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(recovered, []string{failing, healthy}) {
		t.Fatalf("recovery homes %v", recovered)
	}
	_, ticked := rec.snapshot()
	set := map[string]bool{}
	for _, h := range ticked {
		set[h] = true
	}
	if !reflect.DeepEqual(set, map[string]bool{failing: true, healthy: true}) {
		t.Fatalf("tick homes %v", ticked)
	}
}

func TestMultiplexTickerReenumeratesProfilesEachCycle(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	homes := profileHomes(t, root, "alpha")
	gamma := filepath.Join(root, "gamma")
	rec := &tickRecorder{}
	run := newTickerRun()
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		if rec.count() == 1 {
			if err := os.MkdirAll(filepath.Join(gamma, "cron"), 0o700); err != nil {
				t.Error(err)
			}
			homes = append(homes, ProfileHome{Name: strPtr("gamma"), Home: gamma})
		}
		if rec.count() >= 4 {
			run.stop()
		}
		return 0, nil
	})
	zero := 0.0
	run.start(t, withCronStore(context.Background(), s), StartOptions{Interval: &zero, ProfileHomesFn: func() ([]ProfileHome, error) {
		return append([]ProfileHome(nil), homes...), nil
	}})
	if !run.joined(5 * time.Second) {
		run.stop()
		if !run.joined(5 * time.Second) {
			t.Fatal("ticker did not exit")
		}
	}
	_, ticked := rec.snapshot()
	found := false
	for _, h := range ticked {
		found = found || h == gamma
	}
	if !found {
		t.Fatalf("gamma never ticked %v", ticked)
	}
}

func TestRoutedProfileFireComparesTheLaunchHome(t *testing.T) {
	launch := home.RoutingProcess()
	if routedProfileFire(t.Context(), &launch) {
		t.Fatal("the launch home is not a routed fire")
	}
	other := t.TempDir()
	if !routedProfileFire(t.Context(), &other) {
		t.Fatal("a sibling home is a routed fire")
	}
}
