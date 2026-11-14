package gateway

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Namespace of a dashboard launch with the argparse defaults, overridden by the given pairs
func dashNS(pairs ...any) map[string]any {
	ns := map[string]any{"port": 9119, "host": "127.0.0.1", "no_open": false, "insecure": false, "stop": false, "status": false}
	for i := 0; i+1 < len(pairs); i += 2 {
		ns[pairs[i].(string)] = pairs[i+1]
	}
	return ns
}

// One dashboard launch whose runtime preparation and server start are recorded instead of run
type dashRun struct {
	out, errOut bytes.Buffer
	prepared    int
	started     []DashboardServerOptions
	resolveDist bool
}

func (r *dashRun) run(t *testing.T, ns map[string]any, headless bool) int {
	t.Helper()
	swapSeam(t, &dashboardStartServer, func(_ context.Context, o DashboardServerOptions) error {
		r.started = append(r.started, o)
		return nil
	})
	code, err := CmdDashboard(context.Background(), DashboardLaunch{Namespace: ns, HeadlessBackend: headless, Stdout: &r.out, Stderr: &r.errOut,
		PrepareRuntime: func(_ context.Context, _ bool, resolveWebDist func() error) (bool, error) {
			r.prepared++
			if r.resolveDist {
				return false, resolveWebDist()
			}
			return false, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestDashboardStatusStatusNoProcesses(t *testing.T) {
	swapSeam(t, &dashboardScan, func(context.Context, map[int]bool) []pidCommand { return nil })
	r := &dashRun{}
	if code := r.run(t, dashNS("status", true), false); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(r.out.String(), "No icebath dashboard or serve processes running") {
		t.Fatalf("out = %q", r.out.String())
	}
}

func TestDashboardStatusStatusWithProcesses(t *testing.T) {
	pinServeGrammar(t)
	swapSeam(t, &dashboardLedgerEntries, func() []map[string]any { return nil })
	swapSeam(t, &dashboardScan, func(context.Context, map[int]bool) []pidCommand {
		return []pidCommand{{12345, "icebath dashboard --port 9119"},
			{12346, "python -m icebath_cli.main dashboard --host 0.0.0.0 --port 9120"},
			{12347, "icebath serve --host 100.94.65.93 --port 9119"}}
	})
	swapSeam(t, &dashboardPidExists, func(int) bool { return true })
	swapSeam(t, &dashboardIsListening, func(string, int) bool { return true })
	r := &dashRun{}
	if code := r.run(t, dashNS("status", true), false); code != 0 {
		t.Fatalf("code = %d", code)
	}
	out := r.out.String()
	for _, want := range []string{"PID 12345", "PID 12346", "PID 12347", "[serve]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("out = %q lacks %q", out, want)
		}
	}
}

func TestDashboardStatusStatusListsOsAssignedPortServeThatStopTargets(t *testing.T) {
	pinServeGrammar(t)
	const pid = 424242
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	realPort := listener.Addr().(*net.TCPAddr).Port
	swapSeam(t, &dashboardGOOS, "linux")
	swapSeam(t, &dashboardRunProbe, func(context.Context, []string, time.Duration) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{Stdout: []byte(strconv.Itoa(pid) + " /usr/local/bin/icebath serve --host 127.0.0.1 --port 0 --ssh-isolated\n")}, nil
	})
	swapSeam(t, &dashboardLedgerEntries, func() []map[string]any {
		return []map[string]any{{"pid": pid, "purpose": "serve", "host": "127.0.0.1", "port": realPort}}
	})
	swapSeam(t, &dashboardPidExists, func(p int) bool { return p == pid })
	swapSeam(t, &dashboardProcPPID, func(int) (int, bool) { return 0, false })
	if got := findStaleDashboardPIDs(context.Background(), nil, ""); !slices.Equal(got, []int{pid}) {
		t.Fatalf("pids = %v", got)
	}
	r := &dashRun{}
	if code := r.run(t, dashNS("status", true), false); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(r.out.String(), "PID "+strconv.Itoa(pid)+" [serve]") {
		t.Fatalf("out = %q", r.out.String())
	}
}

func TestDashboardStatusStatusDoesNotTryToImportFastapi(t *testing.T) {
	swapSeam(t, &dashboardScan, func(context.Context, map[int]bool) []pidCommand { return nil })
	r := &dashRun{}
	if code := r.run(t, dashNS("status", true), false); code != 0 || r.prepared != 0 || len(r.started) != 0 {
		t.Fatalf("code = %d prepared = %d started = %d", code, r.prepared, len(r.started))
	}
}

// Pins the stop scan and the kill and records the scope home and reason each was called with
type stopSeams struct {
	scanHomes, killHomes, killReasons []string
}

func pinStop(t *testing.T, scans [][]int, result DashboardKillResult) *stopSeams {
	t.Helper()
	s := &stopSeams{}
	swapSeam(t, &dashboardFindStale, func(_ context.Context, _ map[int]bool, scopeHome string) []int {
		s.scanHomes = append(s.scanHomes, scopeHome)
		next := scans[0]
		if len(scans) > 1 {
			scans = scans[1:]
		}
		return next
	})
	swapSeam(t, &dashboardKillStale, func(_ context.Context, _ io.Writer, reason string, _ bool, _ map[string]bool, scopeHome string) DashboardKillResult {
		s.killHomes, s.killReasons = append(s.killHomes, scopeHome), append(s.killReasons, reason)
		return result
	})
	return s
}

func TestDashboardStopStopKillsAndExitsZeroWhenAllKilled(t *testing.T) {
	s := pinStop(t, [][]int{{12345, 12346}, {12347}}, DashboardKillResult{Matched: []int{12345, 12346}, Killed: []int{12345, 12346}, Unrecovered: []int{12345, 12346}})
	r := &dashRun{}
	code := r.run(t, dashNS("stop", true), false)
	if len(s.killReasons) != 1 || !strings.Contains(strings.ToLower(s.killReasons[0]), "stop") {
		t.Fatalf("kill reasons = %v", s.killReasons)
	}
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
}

func TestDashboardStopStopScopesScanAndKillToTheInvokingHermesHome(t *testing.T) {
	ownHome := t.TempDir() + "/profiles/work"
	t.Setenv("ICEBATH_HOME", ownHome)
	s := pinStop(t, [][]int{{12345}}, DashboardKillResult{Matched: []int{12345}, Killed: []int{12345}})
	(&dashRun{}).run(t, dashNS("stop", true), false)
	if len(s.scanHomes) == 0 || s.scanHomes[len(s.scanHomes)-1] != ownHome || len(s.killHomes) != 1 || s.killHomes[0] != ownHome {
		t.Fatalf("scan homes = %v kill homes = %v", s.scanHomes, s.killHomes)
	}
	s = pinStop(t, [][]int{{}}, DashboardKillResult{})
	r := &dashRun{}
	if code := r.run(t, dashNS("stop", true), false); code != 0 || len(s.killHomes) != 0 {
		t.Fatalf("code = %d kills = %v", code, s.killHomes)
	}
	if !strings.Contains(r.out.String(), "for this profile") {
		t.Fatalf("out = %q", r.out.String())
	}
}

func TestDashboardStopStopExitsNonzeroIfKillLeavesSurvivors(t *testing.T) {
	pinStop(t, [][]int{{12345}}, DashboardKillResult{Matched: []int{12345}, Failed: []DashboardKillFailure{{12345, "Operation not permitted"}}})
	if code := (&dashRun{}).run(t, dashNS("stop", true), false); code != 1 {
		t.Fatalf("code = %d", code)
	}
}

func TestDashboardStopStopDoesNotTryToImportFastapi(t *testing.T) {
	pinStop(t, [][]int{{}}, DashboardKillResult{})
	r := &dashRun{}
	if code := r.run(t, dashNS("stop", true), false); code != 0 || r.prepared != 0 {
		t.Fatalf("code = %d prepared = %d", code, r.prepared)
	}
}

func TestLifecycleFlagsTakePrecedenceStopDoesNotFallThroughToServerStart(t *testing.T) {
	pinStop(t, [][]int{{}}, DashboardKillResult{})
	r := &dashRun{}
	r.run(t, dashNS("stop", true), false)
	if len(r.started) != 0 {
		t.Fatal("the server was started after a stop")
	}
}

// Namespace of a full dashboard launch with the argparse defaults
func launchNS(pairs ...any) map[string]any {
	ns := dashNS("no_open", true, "skip_build", false, "isolated", false, "open_profile", "")
	for i := 0; i+1 < len(pairs); i += 2 {
		ns[pairs[i].(string)] = pairs[i+1]
	}
	return ns
}

func TestUnifiedDashboardRoutingProfileLaunchReexecsMachineDashboard(t *testing.T) {
	hostDir(t)
	t.Setenv("ICEBATH_HOME", "")
	t.Cleanup(home.PatchUserHome(t.TempDir()))
	t.Setenv("ICEBATH_DESKTOP", "")
	swapSeam(t, &dashboardActiveProfileName, func(context.Context) string { return "worker_x" })
	swapSeam(t, &dashboardIsListening, func(string, int) bool { return false })
	swapSeam(t, &dashboardSelfExecutable, func() (string, error) { return "/opt/icebath/bin/icebath", nil })
	type reexec struct {
		argv []string
		env  map[string]string
	}
	var execs []reexec
	swapSeam(t, &dashboardReexec, func(argv []string, env map[string]string) error {
		execs = append(execs, reexec{argv, env})
		return &DashboardExit{Code: 0}
	})
	r := &dashRun{}
	r.run(t, launchNS(), false)
	if len(execs) != 1 || len(r.started) != 0 {
		t.Fatalf("execs = %d started = %d", len(execs), len(r.started))
	}
	argv, env := execs[0].argv, execs[0].env
	if argv[0] != "/opt/icebath/bin/icebath" {
		t.Fatalf("argv = %v", argv)
	}
	if i := slices.Index(argv, "-p"); i < 0 || argv[i+1] != "default" {
		t.Fatalf("argv = %v", argv)
	}
	if i := slices.Index(argv, "--open-profile"); i < 0 || argv[i+1] != "worker_x" {
		t.Fatalf("argv = %v", argv)
	}
	if env["ICEBATH_HOME"] != home.Root() {
		t.Fatalf("ICEBATH_HOME = %q want %q", env["ICEBATH_HOME"], home.Root())
	}
}

func TestUnifiedDashboardRoutingDesktopProfileBackendSkipsMachineDashboardReroute(t *testing.T) {
	hostDir(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "desktop-spawn-token")
	swapSeam(t, &dashboardActiveProfileName, func(context.Context) string { return "worker_x" })
	listeningCalls, execs := 0, 0
	swapSeam(t, &dashboardIsListening, func(string, int) bool { listeningCalls++; return false })
	swapSeam(t, &dashboardReexec, func([]string, map[string]string) error { execs++; return nil })
	swapSeam(t, &dashboardRequiresAuth, func(context.Context, string) (bool, error) { return false, nil })
	(&dashRun{}).run(t, launchNS(), false)
	if listeningCalls != 0 || execs != 0 {
		t.Fatalf("listening calls = %d execs = %d", listeningCalls, execs)
	}
}

// Pins a default profile launch with no host record, no auth gate and every web UI build recorded
func pinWebDistLaunch(t *testing.T, projectRoot string, build func(webDir string, fatal bool) bool) *[][2]any {
	t.Helper()
	hostDir(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DESKTOP", "")
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	swapSeam(t, &dashboardActiveProfileName, func(context.Context) string { return "default" })
	swapSeam(t, &dashboardRequiresAuth, func(context.Context, string) (bool, error) { return false, nil })
	swapSeam(t, &dashboardProjectRoot, func() string { return projectRoot })
	builds := &[][2]any{}
	swapSeam(t, &dashboardBuildWebUI, func(_ context.Context, _ io.Writer, webDir string, fatal bool) (bool, error) {
		*builds = append(*builds, [2]any{webDir, fatal})
		return build == nil || build(webDir, fatal), nil
	})
	return builds
}

func webDistNS(pairs ...any) map[string]any {
	ns := map[string]any{"host": "127.0.0.1", "port": 0, "no_open": true, "skip_build": false, "tui": false}
	for i := 0; i+1 < len(pairs); i += 2 {
		ns[pairs[i].(string)] = pairs[i+1]
	}
	return ns
}

func TestEnvDistWithoutIndexExits(t *testing.T) {
	emptyDist := filepath.Join(t.TempDir(), "empty_dist")
	if err := os.Mkdir(emptyDist, 0o755); err != nil {
		t.Fatal(err)
	}
	builds := pinWebDistLaunch(t, t.TempDir(), nil)
	t.Setenv("ICEBATH_WEB_DIST", emptyDist)
	r := &dashRun{resolveDist: true}
	if code := r.run(t, webDistNS(), false); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if len(r.started) != 0 || len(*builds) != 0 {
		t.Fatalf("started = %d builds = %v", len(r.started), *builds)
	}
	if out := r.out.String(); !strings.Contains(out, "ICEBATH_WEB_DIST") || !strings.Contains(out, emptyDist) {
		t.Fatalf("out = %q", out)
	}
}

func TestSkipBuildMissingDistAttemptsOneRecoveryBuild(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "proj")
	dist := filepath.Join(projectRoot, "cmd", "icebath", "web_dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	builds := pinWebDistLaunch(t, projectRoot, func(string, bool) bool {
		return os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html></html>"), 0o644) == nil
	})
	os.Unsetenv("ICEBATH_WEB_DIST")
	r := &dashRun{resolveDist: true}
	r.run(t, webDistNS("skip_build", true), false)
	if len(*builds) != 1 || (*builds)[0][0] != filepath.Join(projectRoot, "web") {
		t.Fatalf("builds = %v", *builds)
	}
	if len(r.started) != 1 || !strings.Contains(strings.ToLower(r.out.String()), "recovery build") {
		t.Fatalf("started = %d out = %q", len(r.started), r.out.String())
	}
}

const packagedRendererDist = "/Applications/Icebath.app/Contents/Resources/app.asar.unpacked/dist"

func TestDesktopChildDashboardDropsPackagedRenderer(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	t.Setenv("ICEBATH_WEB_DIST", packagedRendererDist)
	t.Setenv("ICEBATH_SERVE_HEADLESS", "1")
	dashboardSanitizeDesktopEnv(false)
	if _, set := os.LookupEnv("ICEBATH_WEB_DIST"); set {
		t.Fatal("the packaged renderer dist was kept")
	}
	if _, set := os.LookupEnv("ICEBATH_SERVE_HEADLESS"); set {
		t.Fatal("the headless marker was kept")
	}
}

func TestDesktopHeadlessServeKeepsPackagedRenderer(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_WEB_DIST", packagedRendererDist)
	dashboardSanitizeDesktopEnv(true)
	if os.Getenv("ICEBATH_WEB_DIST") != packagedRendererDist {
		t.Fatalf("ICEBATH_WEB_DIST = %q", os.Getenv("ICEBATH_WEB_DIST"))
	}
}

func TestDesktopOwnedFallbackDashboardKeepsPackagedRenderer(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "desktop-spawn-token")
	t.Setenv("ICEBATH_WEB_DIST", packagedRendererDist)
	dashboardSanitizeDesktopEnv(false)
	if os.Getenv("ICEBATH_WEB_DIST") != packagedRendererDist {
		t.Fatalf("ICEBATH_WEB_DIST = %q", os.Getenv("ICEBATH_WEB_DIST"))
	}
}

func TestServeGuardServeHeadlessRefusesCorruptConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "")
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte("model: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &dashRun{}
	code := r.run(t, map[string]any{"ignore_user_config": false, "ssh_session_token_file": nil, "ssh_owner_nonce": nil, "status": false, "stop": false}, true)
	if code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(r.errOut.String(), "Icebath stopped because your settings file") {
		t.Fatalf("stderr = %q", r.errOut.String())
	}
}

func TestServeStartupAppliesLimitBeforeWebServer(t *testing.T) {
	hostDir(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "0")
	calls := []string{}
	swapSeam(t, &dashboardApplyNofileLimit, func(map[string]any) bool {
		calls = append(calls, "limit")
		return false
	})
	swapSeam(t, &dashboardRequiresAuth, func(context.Context, string) (bool, error) { return false, nil })
	swapSeam(t, &dashboardReadSSHToken, func(string) (string, error) { return strings.Repeat("s", 64), nil })
	tokenDir := filepath.Join("/home/u/.icebath/desktop-ssh", strings.Repeat("f", 32))
	ns := map[string]any{"status": false, "stop": false, "ssh_owner_nonce": "0123456789abcdef",
		"ssh_session_token_file": filepath.Join(tokenDir, "0123456789abcdef.token"), "host": "127.0.0.1", "port": 0,
		"no_open": true, "insecure": false, "open_profile": "", "isolated": true, "skip_build": false}
	var served []DashboardServerOptions
	swapSeam(t, &dashboardStartServer, func(_ context.Context, o DashboardServerOptions) error {
		calls = append(calls, "server")
		served = append(served, o)
		return nil
	})
	var out bytes.Buffer
	code, err := CmdDashboard(context.Background(), DashboardLaunch{Namespace: ns, HeadlessBackend: true, Stdout: &out, Stderr: &out,
		PrepareRuntime: func(_ context.Context, _ bool, resolveWebDist func() error) (bool, error) {
			return false, resolveWebDist()
		}})
	if err != nil || code != 0 {
		t.Fatalf("code = %d err = %v out = %q", code, err, out.String())
	}
	if !slices.Equal(calls, []string{"limit", "server"}) {
		t.Fatalf("calls = %v", calls)
	}
	if len(served) != 1 || served[0].SSHLockPath != filepath.Join(tokenDir, "backend.lock.json") {
		t.Fatalf("served = %+v", served)
	}
}
