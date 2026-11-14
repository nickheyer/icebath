package gateway

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestReadProcessCmdlineSources(t *testing.T) {
	patch(t, &statusProcRead, func(string) ([]byte, error) { return nil, fs.ErrNotExist })
	patch(t, &statusPsutilCmdline, func(int) ([]string, error) { return nil, errors.New("no such process") })
	patch(t, &statusPsCommand, func(int) (int, string, error) { return 0, "/usr/libexec/bluetoothuserd\n", nil })
	if got, ok := readProcessCmdlineLive(873); !ok || got != "/usr/libexec/bluetoothuserd" {
		t.Fatalf("%q %v", got, ok)
	}
	var calls []string
	patch(t, &statusProcRead, func(string) ([]byte, error) {
		calls = append(calls, "proc")
		return []byte("python\x00icebath_cli/main.py\x00gateway\x00"), nil
	})
	patch(t, &statusPsCommand, func(int) (int, string, error) { calls = append(calls, "ps"); return 0, "", nil })
	if got, _ := readProcessCmdlineLive(12345); !strings.Contains(got, "icebath_cli/main.py") || strings.Join(calls, ",") != "proc" {
		t.Fatalf("%q %v", got, calls)
	}
}

func TestReadJSONFileReturnsNilOnBinaryGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runtime.json")
	os.WriteFile(p, []byte("\xff\xfe\x00\x80not utf-8\x81"), 0o644)
	if readJSONFile(p, false) != nil {
		t.Fatal("garbage parsed")
	}
	os.WriteFile(p, []byte(" 4242 \n"), 0o644)
	if r := readJSONFile(p, true); !pyEq(recGet(r, "pid"), 4242) {
		t.Fatal("bare pid")
	}
}

func TestActiveAgentsAndBusyDerivation(t *testing.T) {
	for in, want := range map[any]int64{3: 3, 0: 0, -4: 0, "x": 0, nil: 0, "7": 7} {
		if got, err := ParseActiveAgents(in); err != nil || !pyEq(got, want) {
			t.Errorf("%v = %v %v", in, got, err)
		}
	}
	busy := func(running bool, state, agents any) bool {
		b, _ := DeriveGatewayBusy(running, state, agents)
		return b
	}
	if !busy(true, "running", 1) || busy(true, "running", 0) || !DeriveGatewayDrainable(true, "running") ||
		!busy(true, "degraded", 2) || DeriveGatewayDrainable(false, "degraded") {
		t.Fatal("derivation")
	}
}

func TestRespawnStormBreaker(t *testing.T) {
	dir := t.TempDir()
	for range 5 {
		if info := RecordStartAndCheckStorm(context.Background(), dir, 5, 120, 300); info != nil {
			t.Fatal("storm under threshold")
		}
	}
	info := RecordStartAndCheckStorm(context.Background(), dir, 5, 120, 300)
	if info == nil || info.Count != 6 || info.BackoffS != 10 {
		t.Fatalf("storm %+v", info)
	}
}

func TestInaccessibleLockIsNotDeletedOrTreatedAsAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission semantics need an unprivileged user")
	}
	dir := statusHome(t)
	lock := filepath.Join(dir, "gateway.lock")
	os.WriteFile(lock, []byte("existing owner"), 0o644)
	writeRecord(t, filepath.Join(dir, "gateway.pid"), rec("pid", int64(os.Getpid())))
	st, _ := os.Stat(lock)
	os.Chmod(lock, 0)
	if active, _ := IsGatewayRuntimeLockActive(lock); !active {
		t.Fatal("inaccessible lock read as absent")
	}
	if AcquireGatewayRuntimeLock(context.Background()) {
		t.Fatal("acquired an inaccessible lock")
	}
	os.Chmod(lock, 0o600)
	if after, _ := os.Stat(lock); !os.SameFile(st, after) {
		t.Fatal("inode replaced")
	}
	if data, _ := os.ReadFile(lock); string(data) != "existing owner" {
		t.Fatal("content changed")
	}
	if !AcquireGatewayRuntimeLock(context.Background()) {
		t.Fatal("acquire after repair")
	}
	ReleaseGatewayRuntimeLock()
	if after, _ := os.Stat(lock); !os.SameFile(st, after) {
		t.Fatal("inode replaced after release")
	}
}

func TestRuntimeLockDoesNotFollowSymlink(t *testing.T) {
	dir := statusHome(t)
	target := filepath.Join(dir, "unrelated")
	os.WriteFile(target, []byte("unchanged"), 0o644)
	os.Symlink(target, filepath.Join(dir, "gateway.lock"))
	if AcquireGatewayRuntimeLock(context.Background()) {
		ReleaseGatewayRuntimeLock()
		t.Fatal("symlink followed")
	}
	if data, _ := os.ReadFile(target); string(data) != "unchanged" {
		t.Fatal("target written")
	}
}

func TestNormalizeUpdatedAt(t *testing.T) {
	check := func(in any, want string) {
		t.Helper()
		got, ok, err := NormalizeUpdatedAt(in)
		if err != nil || (want == "") == ok || got != want {
			t.Errorf("%v = %q %v %v", in, got, ok, err)
		}
	}
	check(1750000000, pydatetime.ISOFormat(time.Unix(1750000000, 0).UTC()))
	check("2026-07-21T12:00:00Z", "2026-07-21T12:00:00+00:00")
	check("2026-07-21T12:00:00", "2026-07-21T12:00:00+00:00")
	check("2026-07-21T12:00:00+00:00", "2026-07-21T12:00:00+00:00")
	check(0, "")
	check(946684799, "")
	check(-1750000000, "")
	check(true, "")
	check("garbage", "")
}

func TestResolveGatewayLiveness(t *testing.T) {
	healthCalls, runtimeCalls := 0, 0
	r := ResolveGatewayLiveness(LivenessOptions{RuntimeGiven: true,
		HealthProbe:     func() (bool, *pyjson.Object, error) { healthCalls++; return true, rec("pid", int64(999)), nil },
		PidProbe:        func(string) (int, bool, error) { return 4242, true, nil },
		RuntimePidProbe: func(*pyjson.Object, string) (int, bool, error) { runtimeCalls++; return 888, true, nil }})
	if !r.Running || r.PID != 4242 || r.Source != "pid" || healthCalls+runtimeCalls != 0 {
		t.Fatalf("%+v", r)
	}
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
	r = ResolveGatewayLiveness(LivenessOptions{RuntimeGiven: true,
		PidProbe:        func(string) (int, bool, error) { return 0, false, errors.New("probe exploded") },
		RuntimePidProbe: func(*pyjson.Object, string) (int, bool, error) { return 0, false, nil }})
	if r.Running || !r.ProbeError {
		t.Fatalf("%+v", r)
	}
	profileDir := filepath.Join(t.TempDir(), "profiles", "worker")
	os.MkdirAll(profileDir, 0o755)
	seen := map[string]string{}
	ResolveGatewayLiveness(LivenessOptions{ProfileDir: profileDir,
		PidProbe:        func(p string) (int, bool, error) { seen["pid"] = p; return 0, false, nil },
		RuntimeReader:   func(p string) (*pyjson.Object, error) { seen["status"] = p; return nil, nil },
		RuntimePidProbe: func(_ *pyjson.Object, h string) (int, bool, error) { seen["home"] = h; return 0, false, nil }})
	if seen["pid"] != filepath.Join(profileDir, "gateway.pid") || seen["status"] != filepath.Join(profileDir, "gateway_state.json") || seen["home"] != profileDir {
		t.Fatalf("%v", seen)
	}
}

func TestStrictGatewayIdentity(t *testing.T) {
	tmp := t.TempDir()
	pidFile, lock := filepath.Join(tmp, "gateway.pid"), filepath.Join(tmp, "gateway.lock")
	if _, _, ok, err := GetRunningPidIdentityStrict(pidFile); ok || err != nil {
		t.Fatal("absence")
	}
	os.WriteFile(pidFile, []byte("bad"), 0o644)
	os.WriteFile(lock, []byte("bad"), 0o644)
	patch(t, &statusIsGatewayRuntimeLockActiveStrict, func(string) (bool, error) { return true, nil })
	if _, _, _, err := GetRunningPidIdentityStrict(pidFile); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed %v", err)
	}
	r := rec("pid", int64(123), "start_time", 10.0, "kind", "icebath-gateway")
	writeRecord(t, pidFile, r)
	writeRecord(t, lock, r)
	patch(t, &statusPidExists, func(int) bool { return true })
	patch(t, &statusProcessStartTime, fixedInt(20.0))
	if _, _, _, err := GetRunningPidIdentityStrict(pidFile); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("reused %v", err)
	}
	os.Remove(pidFile)
	writeRecord(t, lock, rec("pid", int64(123), "start_time", 10.0, "kind", "icebath-gateway",
		"argv", strs("python", "-m", "icebath_cli.main", "gateway", "run", "--replace")))
	patch(t, &statusProcessStartTime, fixedInt(10.0))
	patch(t, &statusReadProcessCmdline, cmdline("python -m icebath_cli.main gateway run --replace"))
	patch(t, &statusIsWindows, false)
	if pid, start, ok, err := GetRunningPidIdentityStrict(pidFile); err != nil || !ok || pid != 123 || start != 10.0 {
		t.Fatalf("adopt %d %v %v %v", pid, start, ok, err)
	}
	patch(t, &statusProcessStartTime, fixedInt(20.0))
	if _, _, _, err := GetRunningPidIdentityStrict(pidFile); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed %v", err)
	}
}

func TestStrictGatewayIdentityRaisesWhenMetadataStatIsDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission semantics need an unprivileged user")
	}
	dir := filepath.Join(t.TempDir(), "sealed")
	os.MkdirAll(dir, 0o755)
	os.Chmod(dir, 0)
	defer os.Chmod(dir, 0o755)
	if _, _, _, err := GetRunningPidIdentityStrict(filepath.Join(dir, "gateway.pid")); err == nil || !strings.Contains(err.Error(), "not inspectable") {
		t.Fatalf("%v", err)
	}
}

func TestPidRecordNamesTheEntryPointUnderAnInlineLauncher(t *testing.T) {
	statusHome(t)
	exe, _ := os.Executable()
	old := os.Args
	os.Args = []string{"-c", "gateway", "run"}
	defer func() { os.Args = old }()
	record := buildPidRecordLive()
	argv := recGet(record, "argv").([]any)
	if argv[0] != exe || argv[1] != "gateway" {
		t.Fatalf("%v", argv)
	}
}

func TestRetainedGatewayState(t *testing.T) {
	watchdog := func(extra ...any) *pyjson.Object {
		return rec(append([]any{"gateway_state", "degraded", "exit_reason", "loop_liveness_watchdog"}, extra...)...)
	}
	cases := []struct {
		r    any
		want string
	}{
		{watchdog(), "degraded"},
		{rec("gateway_state", "degraded", "exit_reason", "shutdown_watchdog"), "degraded"},
		{watchdog("desired_state", "stopped"), "stopped"},
		{rec("gateway_state", "degraded", "exit_reason", nil), "stopped"},
		{rec("gateway_state", "startup_failed", "exit_reason", "x"), "startup_failed"},
		{nil, "stopped"},
	}
	for _, c := range cases {
		if got := RetainedGatewayState(c.r); got != c.want {
			t.Errorf("%v = %s", c.r, got)
		}
	}
}

func TestGatewayCommandLineMatching(t *testing.T) {
	cases := map[string]string{
		"icebath gateway run":                                    "run",
		"icebath gateway":                                        "run",
		"icebath -p gateway gateway restart":                     "restart",
		"icebath --profile=ops gateway status":                   "status",
		"python -m icebath_cli.main gateway run":                 "run",
		"python -m gateway.run":                                  "run",
		"/opt/x/gateway/run.py":                                  "run",
		"/opt/desktop-gateway.py":                                "run",
		`"C:\Program Files\icebath-gateway.exe"`:                 "run",
		"python -m tui_gateway":                                  "",
		"icebath chat":                                           "",
		"osascript -e icebath gateway run":                       "",
		"python -c src 1 python -m icebath_cli.main gateway run": "",
	}
	for cmd, want := range cases {
		if got, _ := gatewayCommandSubcommand(cmd); got != want {
			t.Errorf("%q = %q want %q", cmd, got, want)
		}
	}
	if got, ok := GatewaySpawnIntentSubcommand("python -c src 1 python -m icebath_cli.main gateway run"); !ok || got != "run" {
		t.Fatalf("spawn intent %q", got)
	}
	if got, ok := ProfileFlagValue("icebath -p OPS gateway run"); !ok || got != "ops" {
		t.Fatal(got)
	}
	bootstrap := "python -P -c import sys, runpy; sys.path.insert(0, '/opt/app'); runpy.run_module('icebath_cli.main', run_name='__main__', alter_sys=True) gateway run"
	if got, ok := ProfileFlagValue(bootstrap); ok {
		t.Fatal(got)
	}
	if got, ok := ProfileFlagValue("python -P -c x"); !ok || got != "-c" {
		t.Fatal(got)
	}
}

func TestInlineBootstrapArgv(t *testing.T) {
	src := "import sys, runpy; sys.path.insert(0, '/opt/app'); runpy.run_module('icebath_cli.main', run_name='__main__', alter_sys=True)"
	tokens := append([]string{"python", "-X", "utf8", "-c"}, append(strings.Fields(src), "gateway", "run")...)
	argv, ok := InlineBootstrapArgv(tokens)
	if !ok || strings.Join(argv, " ") != "python -m icebath_cli.main gateway run" {
		t.Fatalf("%v %v", argv, ok)
	}
	if idx, ok := InlineSourceFlagIndex([]string{"python", "-X", "utf8", "-c", "x"}); !ok || idx != 3 {
		t.Fatal(idx)
	}
	if _, ok := InlineSourceFlagIndex([]string{"python", "-m", "icebath_cli.main"}); ok {
		t.Fatal("module run read as inline source")
	}
	if idx, ok := InlineSourceFlagIndex([]string{"python", "-qc", "x"}); !ok || idx != 1 {
		t.Fatal("cluster")
	}
}

func TestSharedListenerMirrorsAndMultiplexerProjection(t *testing.T) {
	runtime := rec("platforms", rec(
		"api_server", rec("state", "connected", "listener_base", "http://127.0.0.1:8642"),
		"webhook", rec("state", "fatal"),
		"telegram", rec("state", "connected"),
		"coder:slack", rec("state", "connected"),
	))
	mirrors := SharedListenerMirrorPlatformsFor(runtime, "coder")
	api := recGet(mirrors, "api_server").(*pyjson.Object)
	if mirrors.Len() != 1 || recGet(api, "ingress_url") != "http://127.0.0.1:8642/p/coder/v1" || recGet(api, "mirrored_from") != "default" {
		t.Fatal("mirror")
	}
	if _, has := api.Get("listener_base"); has {
		t.Fatal("listener_base leaked")
	}
	coder := ProfilePlatformsFromMultiplexer(runtime, "coder")
	if strings.Join(coder.Keys, ",") != "api_server,slack" {
		t.Fatal(coder.Keys)
	}
	def := ProfilePlatformsFromMultiplexer(runtime, "default")
	if strings.Join(def.Keys, ",") != "api_server,webhook,telegram" {
		t.Fatal(def.Keys)
	}
}

func TestSameHermesHomeAndProfileLabelForHomeExports(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	if same, err := SameHermesHome(root, root+string(os.PathSeparator)); err != nil || !same {
		t.Fatalf("same %v %v", same, err)
	}
	if same, err := SameHermesHome(root, filepath.Join(root, "other")); err != nil || same {
		t.Fatalf("distinct %v %v", same, err)
	}
	if got := ProfileLabelForHome(filepath.Join(root, "profiles", "coder")); got != "coder" {
		t.Fatalf("profile label %q", got)
	}
	if got := ProfileLabelForHome(filepath.Join(root, "elsewhere", "Bad Name")); got != "" {
		t.Fatalf("unknown label %q", got)
	}
}
