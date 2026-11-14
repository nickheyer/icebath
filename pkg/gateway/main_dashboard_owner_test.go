package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Installs a holder grammar that reads the subcommand after the program name for the test
func pinHolderGrammar(t *testing.T) {
	t.Helper()
	saved := HolderSubcommand
	t.Cleanup(func() { HolderSubcommand = saved })
	HolderSubcommand = func(cmdline string) (string, bool) {
		fields := strings.Fields(cmdline)
		if len(fields) < 2 || !strings.HasSuffix(fields[0], "icebath") {
			return "", false
		}
		return fields[1], true
	}
}

func TestParseDashboardRuntimeReadsModeHostAndPort(t *testing.T) {
	pinHolderGrammar(t)
	for _, c := range []struct {
		command, mode, host string
		port                int
		ok                  bool
	}{
		{"/usr/bin/icebath dashboard", "dashboard", "127.0.0.1", 9119, true},
		{"icebath serve --port 9200 --host 0.0.0.0", "serve", "0.0.0.0", 9200, true},
		{"icebath serve --port=9300 --host='::1'", "serve", "::1", 9300, true},
		{`icebath dashboard --host "10.0.0.2" --no-open`, "dashboard", "10.0.0.2", 9119, true},
		{"icebath dashboard --host ''", "dashboard", "127.0.0.1", 9119, true},
		{"icebath gateway run --port 1", "", "", 0, false},
		{"python -m something dashboard", "", "", 0, false},
	} {
		mode, host, port, ok := parseDashboardRuntime(c.command)
		if mode != c.mode || host != c.host || port != c.port || ok != c.ok {
			t.Errorf("%q gave %q %q %d %v", c.command, mode, host, port, ok)
		}
	}
}

// Points the proc tree at a temp dir and answers systemctl show with the given main pids per scope
func pinUnitProbes(t *testing.T, mainPIDs map[string]string) *[][]string {
	t.Helper()
	savedRoot, savedProbe := dashboardProcRoot, dashboardRunProbe
	t.Cleanup(func() { dashboardProcRoot, dashboardRunProbe = savedRoot, savedProbe })
	dashboardProcRoot = t.TempDir()
	var calls [][]string
	dashboardRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		calls = append(calls, argv)
		scope := "system"
		if argv[1] == "--user" {
			scope = "user"
		}
		out, known := mainPIDs[scope]
		if !known {
			return nil, errors.New("systemctl not found")
		}
		return &pysubprocess.Completed{Returncode: 0, Stdout: []byte(out + "\n")}, nil
	}
	return &calls
}

// Writes the cgroup file of a pid under the pinned proc tree
func writeCgroup(t *testing.T, pid, text string) {
	t.Helper()
	dir := filepath.Join(dashboardProcRoot, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdOwnerForPidRequiresTheUnitMainPid(t *testing.T) {
	ctx := context.Background()
	calls := pinUnitProbes(t, map[string]string{"user": "4242", "system": "1"})
	writeCgroup(t, "4242", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/icebath-dashboard.service\n")
	unit, scope, ok := SystemdOwnerForPid(ctx, 4242)
	if !ok || unit != "icebath-dashboard.service" || scope != "user" {
		t.Fatalf("owner %q %q %v", unit, scope, ok)
	}
	if want := []string{"systemctl", "--user", "show", "icebath-dashboard.service", "--property=MainPID", "--value"}; len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("probes %v", *calls)
	}
	writeCgroup(t, "77", "0::/system.slice/cron.service\n")
	if _, _, ok := SystemdOwnerForPid(ctx, 77); ok {
		t.Fatal("a process inside another unit's cgroup read as owned by it")
	}
	writeCgroup(t, "88", "12:pids:/legacy\n0::/custom/odd.service\n")
	*calls = nil
	pinned := map[string]string{"user": "88"}
	dashboardRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		*calls = append(*calls, argv)
		if argv[1] == "--user" {
			return &pysubprocess.Completed{Returncode: 0, Stdout: []byte(pinned["user"])}, nil
		}
		return &pysubprocess.Completed{Returncode: 1}, nil
	}
	unit, scope, ok = SystemdOwnerForPid(ctx, 88)
	if !ok || unit != "odd.service" || scope != "" || len(*calls) != 2 || (*calls)[0][1] != "show" {
		t.Fatalf("unknown scope owner %q %q %v after %v", unit, scope, ok, *calls)
	}
	if _, _, ok := SystemdOwnerForPid(ctx, 999); ok {
		t.Fatal("a process with no cgroup file read as owned")
	}
	writeCgroup(t, "55", "0::/user.slice/session-3.scope\n")
	if _, _, ok := SystemdOwnerForPid(ctx, 55); ok {
		t.Fatal("a scope cgroup read as a service")
	}
	if got, ok := getPidCgroupPath(55); !ok || got != "/user.slice/session-3.scope" || extractScopeFromCgroup("/elsewhere") != "" {
		t.Fatalf("cgroup path %q %v", got, ok)
	}
}

const backendPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>LABEL</string><key>ProgramArguments</key><array>ARGS</array></dict></plist>`

func plistWith(label string, args ...string) []byte {
	items := ""
	for _, a := range args {
		items += "<string>" + a + "</string>"
	}
	return []byte(strings.Replace(strings.Replace(backendPlist, "LABEL", label, 1), "ARGS", items, 1))
}

func TestLoadedLaunchdBackendJobsKeepsOnlyLoadedBackendPlists(t *testing.T) {
	pinHolderGrammar(t)
	savedOS, savedUID, savedPrint := dashboardGOOS, dashboardUID, dashboardLaunchdPrintPID
	t.Cleanup(func() { dashboardGOOS, dashboardUID, dashboardLaunchdPrintPID = savedOS, savedUID, savedPrint })
	dashboardUID = func() int { return 501 }
	agents, daemons := t.TempDir(), t.TempDir()
	write := func(dir, name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(agents, "b-serve.plist", plistWith("ai.icebath.serve", "/usr/local/bin/icebath", "serve", "--port", "9200"))
	write(agents, "a-dash.plist", plistWith("ai.icebath.dashboard", "/usr/local/bin/icebath", "dashboard"))
	write(agents, "gateway.plist", plistWith("ai.icebath.gateway", "/usr/local/bin/icebath", "gateway", "run"))
	write(agents, "unloaded.plist", plistWith("ai.icebath.unloaded", "/usr/local/bin/icebath", "serve"))
	write(agents, "broken.plist", []byte("<plist><dict><key>Label</key><string>a & b</string>"))
	write(agents, "nolabel.plist", plistWith("", "/usr/local/bin/icebath", "serve"))
	write(daemons, "system.plist", plistWith("ai.icebath.system", "/usr/local/bin/icebath", "dashboard"))
	var asked []string
	dashboardLaunchdPrintPID = func(ctx context.Context, domain, label string) (bool, int, bool, error) {
		asked = append(asked, domain+"/"+label)
		switch domain + "/" + label {
		case "gui/501/ai.icebath.dashboard":
			return true, 900, true, nil
		case "gui/501/ai.icebath.serve":
			return false, 0, false, errors.New("launchctl timed out")
		case "user/501/ai.icebath.serve":
			return true, 0, false, nil
		case "system/ai.icebath.system":
			return true, 12, true, nil
		}
		return false, 0, false, nil
	}
	dirs := []launchdPlistDir{{"agent", agents}, {"agent", filepath.Join(agents, "missing")}, {"daemon", daemons}}
	dashboardGOOS = "linux"
	if jobs := loadedLaunchdBackendJobs(context.Background(), dirs); len(jobs) != 0 || len(asked) != 0 {
		t.Fatalf("jobs off macOS %v after asking %v", jobs, asked)
	}
	dashboardGOOS = "darwin"
	jobs := loadedLaunchdBackendJobs(context.Background(), dirs)
	if len(jobs) != 3 {
		t.Fatalf("jobs %+v", jobs)
	}
	if jobs[0].Label != "ai.icebath.dashboard" || jobs[0].Domain != "gui/501" || jobs[0].LivePID == nil || *jobs[0].LivePID != 900 {
		t.Fatalf("first job %+v", jobs[0])
	}
	if jobs[1].Label != "ai.icebath.serve" || jobs[1].Domain != "user/501" || jobs[1].LivePID != nil ||
		!reflect.DeepEqual(jobs[1].Argv, []string{"/usr/local/bin/icebath", "serve", "--port", "9200"}) {
		t.Fatalf("second job %+v", jobs[1])
	}
	if jobs[2].Domain != "system" || *jobs[2].LivePID != 12 {
		t.Fatalf("daemon job %+v", jobs[2])
	}
	want := []string{"gui/501/ai.icebath.dashboard", "gui/501/ai.icebath.serve", "user/501/ai.icebath.serve", "gui/501/ai.icebath.unloaded",
		"user/501/ai.icebath.unloaded", "system/ai.icebath.system"}
	if !reflect.DeepEqual(asked, want) {
		t.Fatalf("asked %v", asked)
	}
	pid := func(n int) *int { return &n }
	owned := []LaunchdBackendJob{{Domain: "gui/501", Label: "one", Argv: []string{"icebath", "serve"}, LivePID: pid(40)},
		{Domain: "gui/501", Label: "two", Argv: []string{"icebath", "dashboard", "--port", "9"}}}
	if job := LaunchdJobOwningBackend(40, nil, owned, nil); job == nil || job.Label != "one" {
		t.Fatalf("live pid owner %+v", job)
	}
	if job := LaunchdJobOwningBackend(41, nil, owned, []int{7, 40}); job == nil || job.Label != "one" {
		t.Fatalf("ancestor owner %+v", job)
	}
	if job := LaunchdJobOwningBackend(41, []string{"icebath", "dashboard", "--no-open", "--port", "9"}, owned, nil); job == nil || job.Label != "two" {
		t.Fatalf("argv owner %+v", job)
	}
	if job := LaunchdJobOwningBackend(41, []string{"icebath", "dashboard"}, owned, nil); job != nil {
		t.Fatalf("unowned process claimed by %+v", job)
	}
	if got := launchdPlistDirs(); len(got) != 3 || got[1] != (launchdPlistDir{"agent", "/Library/LaunchAgents"}) || got[2].kind != "daemon" || !strings.HasSuffix(got[0].dir, filepath.Join("Library", "LaunchAgents")) {
		t.Fatalf("plist dirs %v", got)
	}
}
