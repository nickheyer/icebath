package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
