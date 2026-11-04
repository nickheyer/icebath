package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func setScanBudget(t *testing.T, bytesMax, lines, lineBytes, paths, remote int) {
	t.Helper()
	pb, pl, plb, pp, pr := maxLifecycleScanBytes, maxLifecycleScanLines, maxLifecycleScanLineBytes, maxLifecycleScanPaths, maxLifecycleScanRemoteReads
	if bytesMax > 0 {
		maxLifecycleScanBytes = bytesMax
	}
	if lines > 0 {
		maxLifecycleScanLines = lines
	}
	if lineBytes > 0 {
		maxLifecycleScanLineBytes = lineBytes
	}
	if paths > 0 {
		maxLifecycleScanPaths = paths
	}
	if remote > 0 {
		maxLifecycleScanRemoteReads = remote
	}
	t.Cleanup(func() {
		maxLifecycleScanBytes, maxLifecycleScanLines, maxLifecycleScanLineBytes, maxLifecycleScanPaths, maxLifecycleScanRemoteReads = pb, pl, plb, pp, pr
	})
}

func guardScan(command, cwd string, remote ReadRemoteScriptFunc) bool {
	return ContainsGatewayLifecycleCommandOrReferencedScript(command, cwd, remote)
}

func writeScript(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// test_lifecycle_guard_budget.py
func TestLifecycleRootByteLimit(t *testing.T) {
	setScanBudget(t, 8, 0, 8, 0, 0)
	if guardScan(strings.Repeat("x", 8), "", nil) {
		t.Fatal("exact budget rejected")
	}
	if !guardScan(strings.Repeat("x", 9), "", nil) {
		t.Fatal("over budget allowed")
	}
}

func TestLifecycleRootLineLimit(t *testing.T) {
	setScanBudget(t, 0, 2, 0, 0, 0)
	if guardScan("one\ntwo", "", nil) || !guardScan("one\ntwo\nthree", "", nil) {
		t.Fatal("line limit")
	}
}

func TestLifecycleSingleGiantLineRejected(t *testing.T) {
	setScanBudget(t, 0, 0, 8, 0, 0)
	if !guardScan("xxxxxxxxx\necho ok", "", nil) {
		t.Fatal("giant line allowed")
	}
}

func TestLifecycleRootBudgetCountsUTF8Bytes(t *testing.T) {
	setScanBudget(t, 4, 0, 4, 0, 0)
	if guardScan("éé", "", nil) || !guardScan("ééé", "", nil) {
		t.Fatal("utf8 byte accounting")
	}
}

func TestLifecycleScanRootWithinBudgetIsNotAVerdict(t *testing.T) {
	setScanBudget(t, 8, 0, 8, 0, 0)
	if !LifecycleScanRootWithinBudget(strings.Repeat("x", 8)) || LifecycleScanRootWithinBudget(strings.Repeat("x", 9)) {
		t.Fatal("root budget")
	}
}

func TestLifecycleUniquePathBudgetFailsClosed(t *testing.T) {
	setScanBudget(t, 0, 0, 0, 2, 0)
	dir := t.TempDir()
	var parts []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, "s"+string(rune('0'+i))+".sh")
		writeScript(t, p, "echo ok\n")
		parts = append(parts, "bash "+shQuote(p))
	}
	if guardScan(strings.Join(parts[:2], " && "), "", nil) {
		t.Fatal("two paths rejected")
	}
	if !guardScan(strings.Join(parts, " && "), "", nil) {
		t.Fatal("three paths allowed")
	}
}

func TestLifecycleRepeatedPathDoesNotSpendBudget(t *testing.T) {
	setScanBudget(t, 0, 0, 0, 1, 0)
	p := filepath.Join(t.TempDir(), "s.sh")
	writeScript(t, p, "echo ok\n")
	q := shQuote(p)
	if guardScan("bash "+q+" && bash "+q+" && sh "+q, "", nil) {
		t.Fatal("repeated path charged")
	}
}

func TestLifecycleRemoteReadBudgetChargedBeforeRead(t *testing.T) {
	setScanBudget(t, 0, 0, 0, 0, 1)
	var reads []string
	remote := func(p string) (string, bool) {
		reads = append(reads, p)
		return "echo ok\n", true
	}
	if !guardScan("bash /remote/a.sh && bash /remote/b.sh", "", remote) {
		t.Fatal("second remote read must fail closed")
	}
	if len(reads) != 1 || reads[0] != "/remote/a.sh" {
		t.Fatalf("reads %v", reads)
	}
}

func TestLifecycleCumulativeTextBudgetBoundsRecursiveScan(t *testing.T) {
	setScanBudget(t, 48, 0, 0, 0, 0)
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "a.sh"), "echo "+strings.Repeat("a", 10)+"\n")
	writeScript(t, filepath.Join(dir, "b.sh"), "echo "+strings.Repeat("b", 10)+"\n")
	if guardScan("bash a.sh", dir, nil) {
		t.Fatal("single script rejected")
	}
	if !guardScan("bash a.sh;bash b.sh", dir, nil) {
		t.Fatal("cumulative budget not enforced")
	}
}

func TestLifecycleRemoteSanitizerHonoursBudget(t *testing.T) {
	if text, ok, unsafe := sanitizeRemoteScriptText("echo ok\n", true, 4); ok || !unsafe || text != "" {
		t.Fatal("over budget remote text accepted")
	}
	if text, ok, unsafe := sanitizeRemoteScriptText("echo ok\n", true, 8); !ok || unsafe || text != "echo ok\n" {
		t.Fatal("in budget remote text refused")
	}
}

func TestLifecycleLineBudgetFailsClosedBeforeTokenizing(t *testing.T) {
	setScanBudget(t, 0, 4, 0, 0, 0)
	p := filepath.Join(t.TempDir(), "many.sh")
	writeScript(t, p, strings.Repeat("echo ok\n", 10))
	if !guardScan("bash "+shQuote(p), "", nil) {
		t.Fatal("ten line script passed a four line budget")
	}
}

func TestCheckGatewayLifecycleShellScriptBudget(t *testing.T) {
	setScanBudget(t, 8, 0, 8, 0, 0)
	env := &Env{Home: t.TempDir()}
	p := filepath.Join(t.TempDir(), "long-line.sh")
	writeScript(t, p, strings.Repeat("x", 7))
	if err := CheckGatewayLifecycle(env, "", p); err != nil {
		t.Fatal(err)
	}
	writeScript(t, p, strings.Repeat("x", 9))
	if _, ok := CheckGatewayLifecycle(env, "", p).(*GatewayLifecycleBlocked); !ok {
		t.Fatal("over budget script allowed")
	}
}

func TestCheckGatewayLifecyclePythonPathChargesMasker(t *testing.T) {
	setScanBudget(t, 0, 0, 16, 0, 0)
	env := &Env{Home: t.TempDir()}
	dir := t.TempDir()
	small := filepath.Join(dir, "small.py")
	writeScript(t, small, "x = 1\n")
	if err := CheckGatewayLifecycle(env, "run report", small); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(dir, "long.py")
	writeScript(t, long, "x = 1\n"+strings.Repeat("y", 40)+"\n")
	if _, ok := CheckGatewayLifecycle(env, "run report", long).(*GatewayLifecycleBlocked); !ok {
		t.Fatal("over budget python line allowed")
	}
}

func TestDefaultBudgetAdmitsWideBenignWrapperGraph(t *testing.T) {
	dir := t.TempDir()
	var hub strings.Builder
	for i := 0; i < 200; i++ {
		child := filepath.Join(dir, "c"+strconv.Itoa(i)+".sh")
		writeScript(t, child, strings.Repeat("echo step && ls -la /tmp\n", 20))
		hub.WriteString("bash " + shQuote(child) + "\n")
	}
	hubPath := filepath.Join(dir, "hub.sh")
	writeScript(t, hubPath, hub.String())
	if guardScan("bash "+shQuote(hubPath), "", nil) {
		t.Fatal("benign graph blocked")
	}
	evil := filepath.Join(dir, "evil.sh")
	writeScript(t, evil, "icebath gateway restart\n")
	writeScript(t, hubPath, hub.String()+"bash "+shQuote(evil)+"\n")
	if !guardScan("bash "+shQuote(hubPath), "", nil) {
		t.Fatal("lifecycle command behind benign scripts missed")
	}
}

// test_lifecycle_guard_heredoc_walk.py
func bigFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "big_blob.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{0}, 2*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInertHeredocBodyPathNotWalkedAsScript(t *testing.T) {
	dir := t.TempDir()
	big := bigFile(t, dir)
	if guardScan("python3 - <<'PY'\nfrom pathlib import Path\nprint(Path('"+big+"').stat().st_size)\nPY", dir, nil) {
		t.Fatal("inert body path walked")
	}
	if guardScan("cd "+dir+" && python3 - <<'PY'\nfrom pathlib import Path\nprint(Path('"+big+"').stat().st_size)\nPY", dir, nil) {
		t.Fatal("prefixed inert body path walked")
	}
}

func TestAllowlistedNameFunctionHeredocStaysVisible(t *testing.T) {
	if !guardScan("python3() { bash; }; python3 <<'PY'\nicebath gateway restart\nPY", t.TempDir(), nil) {
		t.Fatal("shadowed python3 heredoc hidden")
	}
}

func TestUnquotedHeredocBodyPathStillWalked(t *testing.T) {
	dir := t.TempDir()
	big := bigFile(t, dir)
	if !guardScan("cat > /tmp/x <<EOF\n"+big+"\nEOF", dir, nil) {
		t.Fatal("expandable body not walked")
	}
}

func TestInertHeredocBodyScriptPathStillRead(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "restart.sh")
	writeScript(t, script, "#!/bin/sh\nicebath gateway restart\n")
	if !guardScan("python3 - <<'PY'\nimport os\nos.system('"+script+"')\nPY", dir, nil) {
		t.Fatal("mentioned lifecycle script not read")
	}
}

func TestMentionedDataFileThatCannotBeScannedIsNotAVerdict(t *testing.T) {
	setScanBudget(t, 0, 0, 0, 0, 8)
	dir := t.TempDir()
	minified := filepath.Join(dir, "minified.json")
	writeScript(t, minified, "["+strings.Repeat("1,", 40000)+"1]")
	var notes strings.Builder
	for i := 0; i < 70; i++ {
		notes.WriteString("| /opt/frag" + strconv.Itoa(i) + "/tool-" + strconv.Itoa(i) + ".md | note |\n")
	}
	notesPath := filepath.Join(dir, "notes.md")
	writeScript(t, notesPath, notes.String())
	db := filepath.Join(dir, "state.db")
	writeScript(t, db, "db")
	state.TrackConnection(db)
	defer state.UntrackConnection(db)
	var misses []string
	remote := func(p string) (string, bool) {
		misses = append(misses, p)
		return "", false
	}
	for _, data := range []string{minified, notesPath, db} {
		command := "cd " + dir + " && python3 - <<'PY'\nt = open('" + data + "', encoding='utf-8').read()\nPY"
		if guardScan(command, dir, remote) {
			t.Fatalf("mention of %s became a verdict", filepath.Base(data))
		}
	}
	if len(misses) == 0 {
		t.Fatal("notes table was not walked")
	}
	if unsafe, refusal := ScanGatewayLifecycle("bash "+minified, "", nil); !unsafe || !strings.Contains(refusal, "budget") {
		t.Fatalf("executed minified: %v %q", unsafe, refusal)
	}
	if unsafe, refusal := ScanGatewayLifecycle("bash "+db, "", nil); !unsafe || !strings.Contains(refusal, "SQLite") {
		t.Fatalf("executed db: %v %q", unsafe, refusal)
	}
}

func TestMentionedScriptWithLifecycleCommandStillBlocks(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "restart.sh")
	writeScript(t, script, "#!/bin/sh\nicebath gateway restart\n")
	command := "cd " + dir + " && python3 - <<'PY'\nimport os\nos.system('" + script + "')\nPY"
	if unsafe, refusal := ScanGatewayLifecycle(command, dir, nil); !unsafe || refusal != "" {
		t.Fatalf("%v %q", unsafe, refusal)
	}
}

func TestUnscannableOwnCronScriptRaisesNamedRefusal(t *testing.T) {
	env := &Env{Home: t.TempDir()}
	dir := t.TempDir()
	big := bigFile(t, dir)
	db := filepath.Join(dir, "state.db")
	writeScript(t, db, "db")
	state.TrackConnection(db)
	defer state.UntrackConnection(db)
	for _, script := range []string{big, db} {
		err := CheckGatewayLifecycle(env, "run it", script)
		blocked, ok := err.(*GatewayLifecycleBlocked)
		if !ok {
			t.Fatalf("no refusal for %s", script)
		}
		if !strings.Contains(blocked.Message, "could not scan") || !strings.Contains(blocked.Message, script) || strings.Contains(blocked.Message, "contains a gateway lifecycle command") {
			t.Fatalf("message %q", blocked.Message)
		}
	}
}

// test_lifecycle_guard_live_db.py
func TestReferencedScriptReadRefusesLiveSqlite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writeScript(t, db, "db")
	state.TrackConnection(db)
	defer state.UntrackConnection(db)
	if text, ok, unsafe := readReferencedScript(db, -1); ok || text != "" || !unsafe {
		t.Fatal("live sqlite was read")
	}
}

func TestReferencedScriptReadPreservesNormalScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "script.sh")
	writeScript(t, p, "echo ok\n")
	if text, ok, unsafe := readReferencedScript(p, -1); !ok || text != "echo ok\n" || unsafe {
		t.Fatal("normal script read failed")
	}
}

func TestInvalidReferencedScriptPathTolerated(t *testing.T) {
	if text, ok, unsafe := readReferencedScript("/tmp/icebath\x00binary", -1); ok || text != "" || unsafe {
		t.Fatal("NUL path not tolerated")
	}
}

func TestReferencedScriptDirectoryIsNothingToScan(t *testing.T) {
	if _, ok, unsafe := readReferencedScript(t.TempDir(), -1); ok || unsafe {
		t.Fatal("directory must be nothing to scan")
	}
}

// test_lifecycle_guard_profile_scan.py
func TestLongProfileScanPreservesTargeting(t *testing.T) {
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profiles", "default"))
	for _, selector := range []string{"-p default", "--profile default", "--profile=default"} {
		for _, pair := range [][2]int{{58, 0}, {0, 58}, {58, 58}} {
			command := "icebath " + strings.Repeat("--quiet ", pair[0]) + selector + " " + strings.Repeat("--quiet ", pair[1]) + "gateway stop"
			if !ContainsGatewayLifecycleCommand(command) || !guardScan(command, "", nil) {
				t.Fatalf("missed %q", command)
			}
		}
	}
	for _, selector := range []string{"-p sibling", "--profile sibling", "--profile=sibling"} {
		command := "icebath " + strings.Repeat("--quiet ", 58) + selector + " gateway stop"
		if ContainsGatewayLifecycleCommand(command) || guardScan(command, "", nil) {
			t.Fatalf("sibling profile matched %q", command)
		}
	}
}

func TestNonmatchingFlagRunsFinish(t *testing.T) {
	for _, command := range []string{
		"rclone lsf $HOME/.icebath --recursive --files-only " + strings.Repeat("--exclude ", 58),
		"icebath " + strings.Repeat("--quiet ", 1000),
		"icebath -p default " + strings.Repeat("--quiet ", 1000) + "gateway status",
		"icebath " + strings.Repeat("--option"+strings.Repeat(" ", 100)+"value ", 58),
	} {
		if ContainsGatewayLifecycleCommand(command) {
			t.Fatalf("false positive %q", command[:40])
		}
	}
}
