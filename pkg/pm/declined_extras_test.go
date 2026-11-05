package pm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type declActor struct{}

// Context naming the goroutine a sync runs on, the counterpart of a Python thread identity
func declAs(name string) context.Context {
	return context.WithValue(context.Background(), declActor{}, name)
}

func declActorOf(ctx context.Context) string {
	name, _ := ctx.Value(declActor{}).(string)
	return name
}

// Checkout holding the upstream pyproject with a client lock and home, the install command steps replaced
func declSynced(t *testing.T) (string, *[][]string) {
	t.Helper()
	tmp := ieClientEnv(t)
	repo := filepath.Join(tmp, "checkout")
	peWrite(t, filepath.Join(repo, "pyproject.toml"), peRead(t, filepath.Join(peReferences, "pyproject.toml")))
	peSwap(t, &repoRoot, func() string { return repo })
	calls := &[][]string{}
	peSwap(t, &cliInstallNames, func(context.Context, []string, *string, bool) (int, error) { return 0, nil })
	peSwap(t, &installActivate, func(context.Context, bool) ([]string, error) { return []string{}, nil })
	peSwap(t, &installSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		*calls = append(*calls, append([]string{}, opts.Extras...))
		return nil
	})
	return repo, calls
}

// Venv double over the checkout whose stamp is fixed, the target selection's package argument
func declSelection(t *testing.T, recorded, requested []string) []string {
	t.Helper()
	v := &pubVenv{Venv: NewVenv(RepoRoot()).(*Venv), stamp: func(context.Context, []string, []MemberDir) (string, error) { return "stamp", nil }}
	v.Venv.BasePackage.bind(v)
	enabled, _, _, err := targetSelection(context.Background(), v, map[string]any{"extras": anySlice(recorded)}, requested, VenvApplyOptions{}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

func anySlice(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Creates the main era venv anchors of matrix and fal under root
func declLegacyVenv(t *testing.T, root string) {
	t.Helper()
	site := filepath.Join(root, "venv", "lib", "python3.11", "site-packages")
	for _, anchor := range append(anchors("matrix"), "fal_client") {
		if err := os.MkdirAll(filepath.Join(site, anchor), 0o777); err != nil {
			t.Fatal(err)
		}
	}
}

func declDeclined(t *testing.T, root string) map[string]bool {
	t.Helper()
	declined, err := declinedExtras(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return declined
}

func TestDeclinedWithoutDropsARecordedExtraFromEveryLaterSelection(t *testing.T) {
	declSynced(t)
	ctx := context.Background()
	if code, err := CmdInstall(ctx, InstallArgs{Without: []string{"matrix"}}); code != 0 || err != nil {
		t.Fatalf("install %d %v", code, err)
	}
	if got := declSelection(t, []string{"all", "fal", "matrix"}, nil); !reflect.DeepEqual(got, []string{"all", "fal"}) {
		t.Fatalf("next selection %v", got)
	}
	tmp := t.TempDir()
	declLegacyVenv(t, tmp)
	if got, err := LegacySelection(ctx, tmp); err != nil || !reflect.DeepEqual(got, []string{"all", "fal", "matrix"}) {
		t.Fatalf("legacy %v %v", got, err)
	}
	if _, err := recordDeclinedExtras(ctx, []string{"matrix"}, nil, nil, tmp); err != nil {
		t.Fatal(err)
	}
	if got, err := LegacySelection(ctx, tmp); err != nil || !reflect.DeepEqual(got, []string{"all", "fal"}) {
		t.Fatalf("legacy after decline %v %v", got, err)
	}
	v := NewVenv(RepoRoot()).(VenvState)
	var ie *InstallError
	if err := refuseDeclined(ctx, v, []string{"matrix"}, false); !errors.As(err, &ie) || !strings.Contains(err.Error(), "pm install --extra matrix") {
		t.Fatalf("lazy refusal %v", err)
	}
}

func TestDeclinedExtraOptsADroppedExtraBackIn(t *testing.T) {
	declSynced(t)
	ctx := context.Background()
	if _, err := recordDeclinedExtras(ctx, []string{"matrix"}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := declSelection(t, []string{"all"}, []string{"matrix"}); !reflect.DeepEqual(got, []string{"all", "matrix"}) {
		t.Fatalf("selection %v", got)
	}
	if err := refuseDeclined(ctx, NewVenv(RepoRoot()).(VenvState), []string{"matrix"}, true); err != nil {
		t.Fatal(err)
	}
}

func TestDeclinedWithoutRefusesAnExtraTheDefaultSyncSelectsAnyway(t *testing.T) {
	_, calls := declSynced(t)
	var out bytes.Buffer
	code, err := CmdInstall(withSyncOutput(context.Background(), &out), InstallArgs{Without: []string{"web"}})
	if code != 1 || err != nil || !strings.Contains(out.String(), "extras outside [all]") || len(*calls) != 0 {
		t.Fatalf("install %d %v %q %v", code, err, out.String(), *calls)
	}
}

// Venv double whose build records its selection and runs a hook once for a held selection
type declBlockingVenv struct {
	*pubVenv
	mu    sync.Mutex
	built [][]string
	hold  map[string]func() error
}

// Registers a blocking venv double over a fresh checkout
func declBlockingSetup(t *testing.T) *declBlockingVenv {
	t.Helper()
	tmp := isolateHome(t)
	repo := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(repo, 0o777); err != nil {
		t.Fatal(err)
	}
	peSwap(t, &repoRoot, func() string { return repo })
	b := &declBlockingVenv{hold: map[string]func() error{}}
	b.pubVenv = pubSwapVenv(t, &pubVenv{
		stamp: func(_ context.Context, extras []string, _ []MemberDir) (string, error) {
			return strings.Join(extras, "+"), nil
		},
		prepare: func(_ context.Context, extras []string, _ VenvApplyOptions) (*VenvGeneration, error) {
			b.mu.Lock()
			b.built = append(b.built, append([]string{}, extras...))
			hook := b.hold[strings.Join(extras, "+")]
			delete(b.hold, strings.Join(extras, "+"))
			b.mu.Unlock()
			if hook != nil {
				if err := hook(); err != nil {
					return nil, err
				}
			}
			return &VenvGeneration{}, nil
		},
	})
	return b
}

func (b *declBlockingVenv) lastBuilt() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.built) == 0 {
		return nil
	}
	return b.built[len(b.built)-1]
}

// Holders of the dependency lock by actor, asked running when an actor other than main requests it
type declHolders struct {
	mu   sync.Mutex
	held map[string]bool
}

func (h *declHolders) has(actor string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[actor]
}

// Wraps the dependency lock to record which actor holds it
func declWatchLock(t *testing.T, asked func()) *declHolders {
	t.Helper()
	h := &declHolders{held: map[string]bool{}}
	real := dependencyLock
	peSwap(t, &dependencyLock, func(ctx context.Context, patient bool) (func(), error) {
		actor := declActorOf(ctx)
		if actor != "main" && asked != nil {
			asked()
		}
		release, err := real(ctx, patient)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.held[actor] = true
		h.mu.Unlock()
		return func() {
			h.mu.Lock()
			delete(h.held, actor)
			h.mu.Unlock()
			release()
		}, nil
	})
	return h
}

func TestDeclinedSyncQueuedBehindAnOptInKeepsTheExtra(t *testing.T) {
	b := declBlockingSetup(t)
	main := declAs("main")
	if err := syncVenv(main, SyncVenvOptions{Extras: []string{"all"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := recordDeclinedExtras(main, []string{"matrix"}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	holders := declWatchLock(t, nil)
	selected := make(chan struct{})
	var once sync.Once
	realTarget, realRecord := selectTarget, recordDeclines
	peSwap(t, &selectTarget, func(ctx context.Context, p VenvState, fact map[string]any, extras []string, inputs VenvApplyOptions, repair bool, shipped, frozen []string) ([]string, string, VenvApplyOptions, error) {
		enabled, stamp, opts, err := realTarget(ctx, p, fact, extras, inputs, repair, shipped, frozen)
		if declActorOf(ctx) != "main" {
			once.Do(func() { close(selected) })
		}
		return enabled, stamp, opts, err
	})
	peSwap(t, &recordDeclines, func(ctx context.Context, add, remove []string, reasons map[string]string, root string) (map[string]bool, error) {
		if len(remove) > 0 && !holders.has(declActorOf(ctx)) {
			select {
			case <-selected:
			case <-time.After(30 * time.Second):
				return nil, errors.New("the queued sync never selected")
			}
		}
		return realRecord(ctx, add, remove, reasons, root)
	})
	queued := make(chan error, 1)
	b.hold["all+matrix"] = func() error {
		go func() { queued <- syncVenv(declAs("queued"), SyncVenvOptions{Explicit: true}) }()
		return nil
	}
	if err := syncVenv(main, SyncVenvOptions{Extras: []string{"matrix"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued sync %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("queued sync still running")
	}
	if got := b.lastBuilt(); !reflect.DeepEqual(got, []string{"all", "matrix"}) {
		t.Fatalf("built %v", got)
	}
	if got := carriedRecorded(t); !reflect.DeepEqual(got, []string{"all", "matrix"}) {
		t.Fatalf("recorded %v", got)
	}
	if got := declDeclined(t, ""); len(got) != 0 {
		t.Fatalf("declined %v", got)
	}
}

func TestDeclinedWithoutDuringAnOptInSyncStaysRecorded(t *testing.T) {
	b := declBlockingSetup(t)
	settled := make(chan struct{})
	var once sync.Once
	settle := func() { once.Do(func() { close(settled) }) }
	declWatchLock(t, settle)
	realRecord := recordDeclines
	peSwap(t, &recordDeclines, func(ctx context.Context, add, remove []string, reasons map[string]string, root string) (map[string]bool, error) {
		result, err := realRecord(ctx, add, remove, reasons, root)
		if len(add) > 0 {
			settle()
		}
		return result, err
	})
	optOut := make(chan error, 1)
	b.hold["all+matrix"] = func() error {
		go func() { optOut <- DeclineExtras(declAs("opt-out"), []string{"matrix"}) }()
		select {
		case <-settled:
			return nil
		case <-time.After(30 * time.Second):
			return errors.New("the opt-out neither queued on the lock nor recorded its decline")
		}
	}
	if err := syncVenv(declAs("main"), SyncVenvOptions{Extras: []string{"all", "matrix"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-optOut:
		if err != nil {
			t.Fatalf("opt-out %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("opt-out still running")
	}
	if got := declDeclined(t, ""); !reflect.DeepEqual(got, map[string]bool{"matrix": true}) {
		t.Fatalf("declined %v", got)
	}
}

func TestDeclinedWithoutAfterAFailedFirstMigrationKeepsTheOtherLegacyExtras(t *testing.T) {
	root, calls := declSynced(t)
	peSwap(t, &cliInstallDefaults, func(context.Context, []string, bool) error { return nil })
	declLegacyVenv(t, root)
	if code, err := CmdInstall(context.Background(), InstallArgs{Without: []string{"matrix"}}); code != 0 || err != nil {
		t.Fatalf("install %d %v", code, err)
	}
	if !reflect.DeepEqual(*calls, [][]string{{"all", "fal"}}) {
		t.Fatalf("syncs %v", *calls)
	}
	if got := declDeclined(t, root); !reflect.DeepEqual(got, map[string]bool{"matrix": true}) {
		t.Fatalf("declined %v", got)
	}
}
