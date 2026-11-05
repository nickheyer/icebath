package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type fakeRM struct {
	startRC uint32
	calls   []any
}

type rmRegisterCall struct {
	count int
	names string
}

func (f *fakeRM) StartSession(session *uint32, _ uint32, _ []uint16) uint32 {
	if f.startRC != 0 {
		return f.startRC
	}
	*session = 7
	return 0
}

func (f *fakeRM) RegisterResources(_ uint32, names []string) uint32 {
	sorted := slices.Sorted(slices.Values(names))
	f.calls = append(f.calls, rmRegisterCall{len(names), strings.Join(sorted, "|")})
	return 0
}

func (f *fakeRM) GetList(_ uint32, needed, count *uint32, apps []rmProcessInfo, _ *uint32) uint32 {
	*needed = 2
	if apps == nil {
		return rmErrorMoreData
	}
	apps[0].Process.PID = uint32(os.Getpid())
	apps[1].Process.PID = 4242
	*count = 2
	return 0
}

func (f *fakeRM) EndSession(uint32) uint32 {
	f.calls = append(f.calls, "end")
	return 0
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRestartManagerScanSizesThenExcludesSelf(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	touch(t, dbPath)
	touch(t, dbPath+"-wal")
	api := &fakeRM{}
	holders, err := windowsRestartManagerHolders(dbPath, api)
	if err != nil {
		t.Fatal(err)
	}
	if got := holderPIDsOf(holders); !slices.Equal(got, []int{4242}) {
		t.Fatalf("pids = %v", got)
	}
	want := rmRegisterCall{2, strings.Join(slices.Sorted(slices.Values([]string{dbPath, dbPath + "-wal"})), "|")}
	if api.calls[0] != want || api.calls[len(api.calls)-1] != "end" {
		t.Fatalf("calls = %v", api.calls)
	}
	api = &fakeRM{startRC: 5}
	_, err = windowsRestartManagerHolders(dbPath, api)
	var rmErr *rmCallError
	if !errors.As(err, &rmErr) || err.Error() != "[Errno 5] RmStartSession failed" {
		t.Fatalf("err = %v", err)
	}
	got := restartManagerScan(dbPath, func() (rmAPI, error) { return api, nil })
	if !slices.Equal(got, []Holder{{-1, "Windows Restart Manager scan failed: [Errno 5] RmStartSession failed"}}) {
		t.Fatalf("scan = %v", got)
	}
	empty := filepath.Join(tmp, "missing", "state.db")
	if holders, err := windowsRestartManagerHolders(empty, &fakeRM{}); err != nil || len(holders) != 0 {
		t.Fatalf("absent family = %v, %v", holders, err)
	}
}

func holderPIDsOf(holders []Holder) []int {
	pids := []int{}
	for _, h := range holders {
		pids = append(pids, h.PID)
	}
	return pids
}

func TestPythonExecutionTargetShapes(t *testing.T) {
	cases := []struct {
		argv        []string
		kind, value string
		ok          bool
	}{
		{[]string{"python3", "-m", "icebath_cli.main"}, "module", "icebath_cli.main", true},
		{[]string{"python3", "-micebath_cli.main"}, "module", "icebath_cli.main", true},
		{[]string{"python3", "-Im", "acp_adapter"}, "module", "acp_adapter", true},
		{[]string{"python3", "-m"}, "", "", false},
		{[]string{"python3", "-c", "print(1)"}, "", "", false},
		{[]string{"python3", "-X", "dev", "run_agent.py"}, "script", "run_agent.py", true},
		{[]string{"python3", "-Xdev", "run_agent.py"}, "script", "run_agent.py", true},
		{[]string{"python3", "-uX", "dev", "run_agent.py"}, "script", "run_agent.py", true},
		{[]string{"python3", "--jit", "on", "a.py"}, "script", "a.py", true},
		{[]string{"python3", "--jit=on", "a.py"}, "script", "a.py", true},
		{[]string{"python3", "--check-hash-based-pycs", "always", "a.py"}, "script", "a.py", true},
		{[]string{"python3", "--version", "a.py"}, "script", "a.py", true},
		{[]string{"python3", "--", "-m"}, "script", "-m", true},
		{[]string{"python3", "--"}, "", "", false},
		{[]string{"python3", "-"}, "script", "-", true},
		{[]string{"python3"}, "", "", false},
	}
	for _, tc := range cases {
		kind, value, ok := pythonExecutionTarget(tc.argv)
		if kind != tc.kind || value != tc.value || ok != tc.ok {
			t.Errorf("%v = %q %q %v", tc.argv, kind, value, ok)
		}
	}
}

func TestLooksLikeIcebathShapes(t *testing.T) {
	cases := map[string]bool{
		"icebath gateway run":                         true,
		"/opt/x/Icebath-Agent.EXE chat":               true,
		"icebath-acp":                                 true,
		"python3 -m icebath_cli.main gateway":         true,
		"python3.12 -m acp_adapter":                   true,
		"pypy3 /srv/run_agent.py":                     true,
		"python /x/ICEBATH_CLI/MAIN.PY":               true,
		"python3 run_agent.py":                        true,
		"python3 not_run_agent.py":                    false,
		"python3 -m icebath_cli":                      false,
		"pythonw -m icebath_cli.main":                 false,
		"python3 -c import icebath_cli.main":          false,
		"journalctl -u icebath":                       false,
		"grep icebath":                                false,
		"/usr/bin/python3.11.4 x/icebath_cli/main.py": true,
	}
	for line, want := range cases {
		if got := looksLikeIcebath(strings.Fields(line)); got != want {
			t.Errorf("%q = %v, want %v", line, got, want)
		}
	}
	if looksLikeIcebath(nil) {
		t.Fatal("empty argv recognised")
	}
}

func TestArgvFlagValueIsTokenExact(t *testing.T) {
	cases := []struct {
		argv  []string
		value string
		ok    bool
	}{
		{[]string{"icebath", "--profile", "timothy"}, "timothy", true},
		{[]string{"icebath", "--profiles", "tim"}, "", false},
		{[]string{"icebath", "--profile=a", "-p", "b"}, "b", true},
		{[]string{"icebath", "-p"}, "", false},
		{[]string{"icebath", "-p=x"}, "x", true},
	}
	for _, tc := range cases {
		if value, ok := argvFlagValue(tc.argv, profileFlags); value != tc.value || ok != tc.ok {
			t.Errorf("%v = %q %v", tc.argv, value, ok)
		}
	}
	if v, ok := argvEnvHome([]string{"ICEBATH_HOME=/a", "env", "ICEBATH_HOME=/b"}); v != "/b" || !ok {
		t.Fatalf("env home = %q %v", v, ok)
	}
}

func TestArgvPathTokensAnchorOptionValues(t *testing.T) {
	got := argvPathTokens([]string{"/bin/x//y/", "--db=/a/./b", "-x=rel", "plain", "--flag"})
	want := []argvPathToken{{0, "/bin/x/y"}, {1, "/a/b"}}
	if !slices.Equal(got, want) {
		t.Fatalf("tokens = %v", got)
	}
}

func TestDescribeHolderPID(t *testing.T) {
	installFakeArgv(t, map[int][]string{
		5: {"/usr/local/bin/icebath", "gateway\trun", "", "x  y"},
		6: {"icebath", strings.Repeat("é", 100)},
	})
	if got := DescribeHolderPID(5); got != "PID 5 (icebath gateway run x y)" {
		t.Fatalf("describe = %q", got)
	}
	if got := DescribeHolderPID(6); got != "PID 6 (icebath "+strings.Repeat("é", 72)+")" {
		t.Fatalf("describe = %q", got)
	}
	if got := DescribeHolderPID(1 << 30); got != "PID 1073741824 (command line unavailable)" {
		t.Fatalf("describe = %q", got)
	}
}

// Serves a fixed argv per pid in place of procfs cmdline reads on every OS
func installFakeArgv(t *testing.T, argvByPID map[int][]string) {
	t.Helper()
	prev := readProcArgv
	t.Cleanup(func() { readProcArgv = prev })
	readProcArgv = func(pid int) ([]string, bool) {
		argv, ok := argvByPID[pid]
		return slices.Clone(argv), ok
	}
}

func TestReadOnlyDBURIEncodesQueryAndFragmentCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a?b#c d%")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", pathAsURI(dbPath)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE marker(x)")
	db.Close()
	if fi, err := os.Stat(dbPath); err != nil || fi.Size() == 0 {
		t.Fatalf("database not written at the literal path: %v", err)
	}
	uri, err := ReadOnlyDBURI(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(uri, "/a%3Fb%23c%20d%25/state.db?mode=ro") || !strings.HasPrefix(uri, "file://") {
		t.Fatalf("uri = %q", uri)
	}
	ro, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'marker'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("read only open saw %d tables, err %v", n, err)
	}
	if _, err := ro.Exec("CREATE TABLE other(x)"); err == nil {
		t.Fatal("read only uri accepted a write")
	}
}

func TestCanonicalSQLitePathStripsDeletedSuffix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("procfs descriptor targets exist only on posix hosts")
	}
	got, err := CanonicalSQLitePath("/a/b/../state.db-wal (deleted)")
	if err != nil || got != "/a/state.db-wal" {
		t.Fatalf("canonical = %q, %v", got, err)
	}
	cwd, _ := os.Getwd()
	if got, _ := CanonicalSQLitePath("pipe:[12]"); got != filepath.Join(cwd, "pipe:[12]") {
		t.Fatalf("relative target = %q", got)
	}
}

func TestPythonPathHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posixpath semantics")
	}
	for in, want := range map[string]string{"": ".", "//a//b/": "//a/b", "///a/../b": "/b", "../x/..": "..", "a/./b/": "a/b", "/..": "/"} {
		if got := pyNormpath(in); got != want {
			t.Errorf("normpath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]string{"/a/b/": {"", "/a/b"}, "/a": {"a", "/"}, "a": {"a", ""}, "//x": {"x", "//"}} {
		if pyBasename(in) != want[0] || pyDirname(in) != want[1] {
			t.Errorf("%q = %q %q", in, pyBasename(in), pyDirname(in))
		}
	}
	if pathlibName("/a/b/") != "b" || pathlibName("/") != "" || pathlibStr("a//./b/") != "a/b" {
		t.Fatal("pathlib name or str")
	}
	err := &os.PathError{Op: "stat", Path: "/x'y", Err: syscall.ENOENT}
	if got := pyOSErrorStr(err); got != `[Errno 2] No such file or directory: "/x'y"` {
		t.Fatalf("oserror = %q", got)
	}
	if got := pyOSErrorStr(os.NewSyscallError("kinfo_getfile()", syscall.EINVAL)); got != "[Errno 22] (originated from kinfo_getfile())" {
		t.Fatalf("syscall error = %q", got)
	}
}

func TestInProcessStateDBHoldersListsOtherGenerations(t *testing.T) {
	withRegistryOpen(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	d := mustAcquire(t, path)
	want := []Holder{{os.Getpid(), "in-process live SessionDB generation (refcount 1)"}}
	if got := InProcessStateDBHolders(path, nil); !slices.Equal(got, want) {
		t.Fatalf("holders = %v", got)
	}
	if got := InProcessStateDBHolders(path, d); len(got) != 0 {
		t.Fatalf("excluded holders = %v", got)
	}
}

type testProbeConn struct {
	*sql.Conn
	db *sql.DB
}

func (p testProbeConn) Close() error {
	p.Conn.Close()
	return p.db.Close()
}

func testRepairConnect(dbPath string, _ float64) (RepairConn, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, err
	}
	return testProbeConn{conn, db}, nil
}

func TestLiveWriterHoldsDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE t(x)")
	db.Close()
	if held, err := LiveWriterHoldsDB(dbPath, testRepairConnect); err != nil || held {
		t.Fatalf("quiet store held = %v err = %v", held, err)
	}
	locker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if held, err := LiveWriterHoldsDB(dbPath, testRepairConnect); err != nil || !held {
		t.Fatalf("locked store held = %v err = %v", held, err)
	}
	conn.ExecContext(context.Background(), "ROLLBACK")
	conn.Close()
	locker.Close()

	garbage := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if held, err := LiveWriterHoldsDB(garbage, testRepairConnect); err != nil || held {
		t.Fatalf("malformed store held = %v err = %v", held, err)
	}
	locked := func(string, float64) (RepairConn, error) {
		return nil, &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked"}
	}
	if held, err := LiveWriterHoldsDB(dbPath, locked); err != nil || !held {
		t.Fatalf("locked connect held = %v err = %v", held, err)
	}
	boom := errors.New("boom")
	if _, err := LiveWriterHoldsDB(dbPath, func(string, float64) (RepairConn, error) { return nil, boom }); err != boom {
		t.Fatalf("foreign error = %v", err)
	}
}
