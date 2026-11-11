package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const demoCronProviderSrc = `import threading
from cron.scheduler_provider import CronScheduler


class Demo(CronScheduler):
    def __init__(self):
        self.events = []

    @property
    def name(self):
        return "demo"

    def start(self, stop_event, *, adapters=None, loop=None, interval=60):
        self.events.append("start")

        def watch():
            stop_event.wait(30)
            self.events.append("stopped" if stop_event.is_set() else "timeout")

        threading.Thread(target=watch, daemon=True).start()

    def fire_due(self, job_id, *, adapters=None, loop=None, force=False, manual=False):
        self.events.append("fire_due:%s:%s:%s" % (job_id, force, manual))
        return job_id == "go"

    def claim_fire(self, job_id, *, force=False, manual=False):
        self.events.append("claim:%s:%s" % (job_id, force))
        return None

    def register_job(self, job):
        self.events.append("register:" + job["id"])

    def on_jobs_changed(self):
        self.events.append("changed")

    def reconcile(self):
        self.events.append("reconcile")

    def recover_interrupted(self):
        return 3


def register(ctx):
    ctx.register_tool("ignored")
    ctx.register_cron_scheduler(Demo())
`

const flakyCronProviderSrc = `from cron.scheduler_provider import CronScheduler


class Flaky(CronScheduler):
    @property
    def name(self):
        return "flaky"

    def is_available(self):
        raise RuntimeError("probe down")

    def start(self, stop_event, **kwargs):
        pass


class Offline(CronScheduler):
    @property
    def name(self):
        return "offline"

    def is_available(self):
        return False

    def start(self, stop_event, **kwargs):
        pass
`

func writeProviderTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsCronProviderDirReadsTheContractMarkers(t *testing.T) {
	root := t.TempDir()
	writeProviderTree(t, root, map[string]string{
		"capture/__init__.py":  "def register(ctx):\n    ctx.register_cron_scheduler(None)\n",
		"subclass/__init__.py": "\ufefffrom cron.scheduler_provider import CronScheduler\n",
		"other/__init__.py":    "def register(ctx):\n    ctx.register_tool('x')\n",
		"late/__init__.py":     strings.Repeat("#", 8192) + "CronScheduler\n",
		"bare/plugin.yaml":     "name: bare\n",
	})
	for name, want := range map[string]bool{"capture": true, "subclass": true, "other": false, "late": false, "bare": false, "missing": false} {
		if got := isCronProviderDir(filepath.Join(root, name)); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestProviderDirsListBundledFirstAndBundledWinsACollision(t *testing.T) {
	s, buf := newTestStore(t)
	writeProviderTree(t, filepath.Join(s.home, "plugins"), map[string]string{
		"chronos/__init__.py": "class CronScheduler: pass\n",
		"mycron/__init__.py":  "def register(ctx):\n    ctx.register_cron_scheduler(object())\n",
		"notcron/__init__.py": "def register(ctx):\n    pass\n",
	})
	ctx := providerContext(s)
	dirs, err := iterCronProviderDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || dirs[0].name != "chronos" || dirs[0].bundled == nil || dirs[1].name != "mycron" || dirs[1].dir != filepath.Join(s.home, "plugins", "mycron") {
		t.Fatalf("dirs = %+v", dirs)
	}
	if d, ok := findCronProviderDir(ctx, "chronos"); !ok || d.bundled == nil {
		t.Fatalf("chronos resolved to %+v", d)
	}
	if _, ok := findCronProviderDir(ctx, "notcron"); ok {
		t.Fatal("a plugin without the cron contract resolved as a provider")
	}
	p, err := realLoadCronScheduler(s, "chronos")
	if _, isChronos := p.(*ChronosCronScheduler); err != nil || !isChronos {
		t.Fatalf("chronos loaded %T %v", p, err)
	}
	if p, err := realLoadCronScheduler(s, "missing"); p != nil || err != nil {
		t.Fatalf("missing loaded %v %v", p, err)
	}
	if !strings.Contains(buf.String(), "Cron provider 'missing' not found in bundled or user plugins") {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestHostedCronProviderRunsEveryHookInThePluginHost(t *testing.T) {
	s, _ := newTestStore(t)
	pluginLogs := writeBlueprintPlugin(t, s.home, "demo", demoCronProviderSrc)
	writeProviderTree(t, filepath.Join(s.home, "plugins"), map[string]string{"empty/__init__.py": "# CronScheduler lives elsewhere\n"})
	infos, err := DiscoverCronSchedulers(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 || infos[0].Name != "chronos" || infos[0].Description != chronosDescription || infos[0].Available ||
		infos[1].Name != "demo" || !infos[1].Available || infos[2].Name != "empty" || infos[2].Available {
		t.Fatalf("discovered %+v", infos)
	}
	if p, err := realLoadCronScheduler(s, "empty"); p != nil || err != nil {
		t.Fatalf("empty loaded %v %v", p, err)
	}
	if !strings.Contains(pluginLogs.String(), "Cron provider 'empty' loaded but no provider instance found") {
		t.Fatalf("plugin log = %s", pluginLogs.String())
	}
	p, err := realLoadCronScheduler(s, "demo")
	if err != nil || p == nil {
		t.Fatalf("demo loaded %v %v", p, err)
	}
	hosted := p.(*hostedCronScheduler)
	ctx := providerContext(s)
	if p.Name() != "demo" || !schedulerIsAvailable(p) {
		t.Fatalf("name %q available %v", p.Name(), schedulerIsAvailable(p))
	}
	stop := make(chan struct{})
	p.Start(ctx, stop, StartOptions{Adapters: map[string]any{}, Loop: "loop"})
	fired, err := schedulerFireDue(ctx, p, "go", FireOptions{Force: true})
	if err != nil || !fired {
		t.Fatalf("fire_due %v %v", fired, err)
	}
	if !providerSupportsForceFire(p) || providerFireDueAccepts(p, "cancel_event") {
		t.Fatal("fire_due keywords read wrong from the hosted signature")
	}
	if job, err := schedulerClaimFire(ctx, p, "nope", true, false); job != nil || err != nil {
		t.Fatalf("claim %v %v", job, err)
	}
	if err := schedulerRegisterJob(p, &Job{ID: "j1", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := schedulerOnJobsChanged(p); err != nil {
		t.Fatal(err)
	}
	if err := schedulerReconcile(p); err != nil {
		t.Fatal(err)
	}
	if n, err := schedulerRecoverInterrupted(ctx, p); n != 3 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	close(stop)
	want := []any{"start", "fire_due:go:True:False", "claim:nope:True", "register:j1", "changed", "reconcile", "stopped"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, err := hosted.obj.Get(context.Background(), "events")
		list, _ := events.([]any)
		if err == nil && len(list) == len(want) {
			for i := range want {
				if list[i] != want[i] {
					t.Fatalf("events = %v", list)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %v %v", events, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if started, _ := hostedStarts.Load(s.home); started.([2]any)[1] != "loop" {
		t.Fatalf("start transport %v", started)
	}
}

func TestResolveFallsBackWhenAHostedProviderRaisesOrIsUnavailable(t *testing.T) {
	useRealTickSeams(t)
	s, buf := newTestStore(t)
	writeBlueprintPlugin(t, s.home, "flaky", flakyCronProviderSrc)
	writeProviderTree(t, filepath.Join(s.home, "plugins"), map[string]string{"offline/__init__.py": strings.Replace(flakyCronProviderSrc, "class Flaky(", "class _Flaky(", 1)})
	for name, warning := range map[string]string{
		"flaky":   "Failed to load cron.provider 'flaky' (",
		"flaky ":  "probe down",
		"offline": "cron.provider 'offline' not available; using built-in ticker",
	} {
		writeProviderTree(t, s.home, map[string]string{"config.yaml": `{"model": "test-model", "cron": {"provider": "` + name + `"}}`})
		buf.Reset()
		if p := resolveCronScheduler(s); p.Name() != "builtin" {
			t.Fatalf("%s resolved %s", name, p.Name())
		}
		if !strings.Contains(buf.String(), warning) {
			t.Fatalf("%s log = %s", name, buf.String())
		}
	}
}

const splitFireCronProviderSrc = `from cron.scheduler_provider import CronScheduler


class Legacy(CronScheduler):
    @property
    def name(self):
        return "legacy"

    def start(self, stop_event, **kwargs):
        pass

    def fire_due(self, job_id, **kwargs):
        return True


class Split(Legacy):
    @property
    def name(self):
        return "split"

    def fire_claimed(self, job, **kwargs):
        return True


class Plain(CronScheduler):
    @property
    def name(self):
        return "plain"

    def start(self, stop_event, **kwargs):
        pass

`

// A provider that replaces only fire_due keeps its single phase fire, under host isolation the proxy class declares every method
func TestHostedProviderSupportsSplitFireFollowsTheProxyClass(t *testing.T) {
	for _, mode := range []struct {
		isolation string
		legacy    bool
	}{{"in_process", false}, {"host", true}} {
		t.Run(mode.isolation, func(t *testing.T) {
			s, _ := newTestStore(t)
			register := func(class string) string {
				return splitFireCronProviderSrc + "\n\ndef register(ctx):\n    ctx.register_cron_scheduler(" + class + "())\n"
			}
			writeProviderTree(t, filepath.Join(s.home, "plugins"), map[string]string{"legacy/__init__.py": register("Legacy"), "split/__init__.py": register("Split")})
			writeBlueprintPlugin(t, s.home, "plain", register("Plain"))
			cfg := `{"model": "test-model", "plugins": {"enabled": ["plain"], "isolation": "` + mode.isolation + `"}}`
			if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]bool{"legacy": mode.legacy, "split": true, "plain": true} {
				p, err := realLoadCronScheduler(s, name)
				if err != nil || p == nil {
					t.Fatalf("%s loaded %v %v", name, p, err)
				}
				if p.Name() != name {
					t.Fatalf("loaded %q for %s", p.Name(), name)
				}
				if got := providerSupportsSplitFire(p); got != want {
					t.Errorf("%s split fire %v, want %v", name, got, want)
				}
			}
		})
	}
}
