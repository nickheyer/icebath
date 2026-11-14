package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Provider double recording the options its start received
type recordingScheduler struct {
	name    string
	started bool
	opts    StartOptions
}

func (r *recordingScheduler) Name() string { return r.name }
func (r *recordingScheduler) Start(_ context.Context, _ <-chan struct{}, o StartOptions) {
	r.started, r.opts = true, o
}

func swapDesktop[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	old := *seam
	*seam = value
	t.Cleanup(func() { *seam = old })
}

// Builtin double registered as the in process scheduler class, the hermes _providers fixture
func desktopProviders(t *testing.T) *recordingScheduler {
	t.Helper()
	builtin := &recordingScheduler{name: "builtin"}
	swapDesktop(t, &schedulerResolver, func(*Store) CronScheduler { return builtin })
	swapDesktop(t, &desktopIsBuiltin, func(p CronScheduler) bool { _, ok := p.(*recordingScheduler); return ok })
	return builtin
}

func homesOf(t *testing.T, fn func() ([]ProfileHome, error)) [][2]string {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	out := [][2]string{}
	for _, e := range got {
		out = append(out, [2]string{*e.Name, e.Home})
	}
	return out
}

func TestMultiProfileHomesPassedToBuiltin(t *testing.T) {
	builtin := desktopProviders(t)
	tmp := t.TempDir()
	homes := []profiles.ServedProfile{{Name: "default", Home: filepath.Join(tmp, "root")}, {Name: "coder", Home: filepath.Join(tmp, "profiles", "coder")}}
	swapDesktop(t, &desktopProfilesToServe, func(context.Context) ([]profiles.ServedProfile, error) {
		return append([]profiles.ServedProfile(nil), homes...), nil
	})
	StartDesktopCronTicker(context.Background(), make(chan struct{}), 7)
	if !builtin.started || builtin.opts.Interval == nil || *builtin.opts.Interval != 7 {
		t.Fatalf("%+v", builtin.opts)
	}
	if builtin.opts.ProfileHomesFn == nil {
		t.Fatal("profile homes not callable")
	}
	want := [][2]string{{"default", homes[0].Home}, {"coder", homes[1].Home}}
	if got := homesOf(t, builtin.opts.ProfileHomesFn); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	homes = homes[:1]
	if got := homesOf(t, builtin.opts.ProfileHomesFn); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("re-enumeration %v", got)
	}
}

func TestSingleProfileTicksOnlyWithoutGateway(t *testing.T) {
	for _, gatewayRunning := range []bool{true, false} {
		t.Run(map[bool]string{true: "True", false: "False"}[gatewayRunning], func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "root")
			if err := os.Mkdir(h, 0o755); err != nil {
				t.Fatal(err)
			}
			swapDesktop(t, &desktopProfilesToServe, func(context.Context) ([]profiles.ServedProfile, error) {
				return []profiles.ServedProfile{{Name: "default", Home: h}}, nil
			})
			swapDesktop(t, &desktopCheckGatewayRunning, func(string) bool { return gatewayRunning })
			swapDesktop(t, &schedulerResolver, func(*Store) CronScheduler { return &InProcessCronScheduler{} })
			var ticked []string
			swapDesktop(t, &tickHook, func(ctx context.Context, _ *Store, _ tickOptions) (int, error) {
				ticked = append(ticked, home.Dir(ctx))
				return 0, nil
			})
			stop := make(chan struct{})
			swapDesktop(t, &tickerWait, func(<-chan struct{}, float64) bool { close(stop); return true })
			StartDesktopCronTicker(context.Background(), stop, 0)
			want := []string{h}
			if gatewayRunning {
				want = nil
			}
			if !reflect.DeepEqual(ticked, want) {
				t.Fatalf("ticked %v want %v", ticked, want)
			}
		})
	}
}

func TestEnumerationFailureFailsOpen(t *testing.T) {
	builtin := desktopProviders(t)
	swapDesktop(t, &desktopProfilesToServe, func(context.Context) ([]profiles.ServedProfile, error) {
		return nil, errors.New("profiles dir unreadable")
	})
	StartDesktopCronTicker(context.Background(), make(chan struct{}), 11)
	o := builtin.opts
	if o.Interval == nil || *o.Interval != 11 || o.ProfileHomesFn == nil || o.ProfileGate == nil {
		t.Fatalf("%+v", o)
	}
	if o.Adapters != nil || o.Loop != nil || o.CanDispatch != nil || o.ProfileHomes != nil || o.ProfileAdapters != nil ||
		o.DefaultProfile != nil || o.ProfileBotPlatforms != nil {
		t.Fatalf("extra options %+v", o)
	}
	got := homesOf(t, o.ProfileHomesFn)
	if len(got) != 1 || got[0][1] != home.Dir(context.Background()) {
		t.Fatalf("%v", got)
	}
}

func TestExternalProviderNeverGetsProfileHomes(t *testing.T) {
	external := &recordingScheduler{name: "chronos-test"}
	swapDesktop(t, &schedulerResolver, func(*Store) CronScheduler { return external })
	tmp := t.TempDir()
	swapDesktop(t, &desktopProfilesToServe, func(context.Context) ([]profiles.ServedProfile, error) {
		return []profiles.ServedProfile{{Name: "default", Home: filepath.Join(tmp, "a")}, {Name: "b", Home: filepath.Join(tmp, "b")}}, nil
	})
	StartDesktopCronTicker(context.Background(), make(chan struct{}), 13)
	every := 13.0
	if !external.started || !reflect.DeepEqual(external.opts, StartOptions{Interval: &every}) {
		t.Fatalf("%+v", external.opts)
	}
}

func TestDesktopTickerServesEveryProfileAndYieldsToOwningGateway(t *testing.T) {
	builtin := desktopProviders(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	for _, name := range []string{"worker", "guest", "solo"} {
		dir := filepath.Join(root, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("gateway:\n  multiplex_profiles: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, root)
	home.ResetKeyCache()
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	solo := filepath.Join(root, "profiles", "solo")
	swapDesktop(t, &desktopCheckGatewayRunning, func(h string) bool { return h == solo })
	swapDesktop(t, &desktopServedByRunningMultiplexer, func(name string) bool { return name == "worker" })
	StartDesktopCronTicker(context.Background(), make(chan struct{}), 0)
	if builtin.opts.ProfileHomesFn == nil {
		t.Fatal("profile homes not callable")
	}
	var names []string
	for _, e := range homesOf(t, builtin.opts.ProfileHomesFn) {
		names = append(names, e[0])
	}
	if !reflect.DeepEqual(names, []string{"default", "guest", "solo", "worker"}) {
		t.Fatalf("%v", names)
	}
	gate := builtin.opts.ProfileGate
	name := func(s string) *string { return &s }
	if !gate(name("default"), root) || !gate(name("guest"), filepath.Join(root, "profiles", "guest")) ||
		gate(name("worker"), filepath.Join(root, "profiles", "worker")) || gate(name("solo"), solo) {
		t.Fatal("gate does not yield to the owning gateway")
	}
}
