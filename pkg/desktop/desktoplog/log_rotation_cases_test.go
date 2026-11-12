package desktoplog

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

type fakeActiveIO struct {
	size      int64
	exists    bool
	truncated []string
}

func (f *fakeActiveIO) Size(string) (int64, bool) { return f.size, f.exists }
func (f *fakeActiveIO) Truncate(file string) error {
	f.truncated = append(f.truncated, file)
	return nil
}

type realActiveIO struct{}

func (realActiveIO) Size(file string) (int64, bool) {
	st, err := os.Stat(file)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}
func (realActiveIO) Truncate(file string) error { return os.Truncate(file, 0) }

func TestALogUnderTheCapIsLeftAloneWhateverItsPath(t *testing.T) {
	if ops := PlanLogRotation(LogMaxBytes-1, "/logs/desktop-chromium.log"); len(ops) != 0 {
		t.Fatalf("ops %v", ops)
	}
}

func TestAnOversizedLogCascadesToBackupsInsteadOfGrowingForever(t *testing.T) {
	base := "/logs/desktop-chromium.log"
	ops := PlanLogRotation(LogMaxBytes, base)
	moved := false
	for _, op := range ops {
		if op.Op == "mv" && op.Src == base && op.Dst == LogBackupPath(base, 1) {
			moved = true
		}
		if !strings.HasPrefix(op.Src, base) || (op.Dst != "" && !strings.HasPrefix(op.Dst, base)) {
			t.Fatalf("op outside the log %v", op)
		}
	}
	if !moved || ops[0] != (RotationOp{Op: "rm", Src: LogBackupPath(base, 3)}) {
		t.Fatalf("ops %v", ops)
	}
}

func TestABootLoopLogPastTheDiscardCeilingIsReclaimedNotStrandedIn1(t *testing.T) {
	base := "/logs/desktop-chromium.log"
	ops := PlanLogRotation(LogDiscardBytes+1, base)
	removesBase := false
	for _, op := range ops {
		if op.Op != "rm" {
			t.Fatalf("op %v", op)
		}
		removesBase = removesBase || op.Src == base
	}
	if !removesBase {
		t.Fatalf("ops %v", ops)
	}
}

func TestALogALiveProcessKeepsAppendingToIsReclaimedInPlaceNotRenamed(t *testing.T) {
	io := &fakeActiveIO{size: LogMaxBytes, exists: true}
	if ok, err := ReclaimActiveLogIfOversized("/logs/desktop-chromium.log", io); !ok || err != nil {
		t.Fatalf("reclaim %v %v", ok, err)
	}
	if len(io.truncated) != 1 || io.truncated[0] != "/logs/desktop-chromium.log" {
		t.Fatalf("truncated %v", io.truncated)
	}
}

func TestAnUnderCapOrAbsentActiveLogIsLeftAlone(t *testing.T) {
	under := &fakeActiveIO{size: LogMaxBytes - 1, exists: true}
	absent := &fakeActiveIO{}
	for _, io := range []*fakeActiveIO{under, absent} {
		if ok, _ := ReclaimActiveLogIfOversized("/logs/x.log", io); ok || len(io.truncated) != 0 {
			t.Fatalf("touched %v", io.truncated)
		}
	}
}

func TestTruncationReallyFreesTheFileAndAnAppendModeWriterRestartsAt0(t *testing.T) {
	file := filepath.Join(t.TempDir(), "desktop-chromium.log")
	handle, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.Truncate(LogMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := ReclaimActiveLogIfOversized(file, realActiveIO{}); !ok || err != nil {
		t.Fatalf("reclaim %v %v", ok, err)
	}
	if _, err := handle.WriteString("FATAL:after\n"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "FATAL:after\n" {
		t.Fatalf("file %q", raw)
	}
}

func TestStampsLinesWithTheLocalWallClockTimeInTheShapePythonLogsUse(t *testing.T) {
	moment := time.Date(2026, 9, 3, 7, 5, 9, 42e6, time.Local)
	line := FormatDesktopLogLine("[boot] Resolving Icebath backend", FormatLogStamp(moment))
	m := regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2}),(\d{3}) \[icebath\] (.*)$`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("line %q", line)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	got := time.Date(n(1), time.Month(n(2)), n(3), n(4), n(5), n(6), n(7)*1e6, time.Local)
	if !got.Equal(moment) || m[8] != "[boot] Resolving Icebath backend" {
		t.Fatalf("line %q", line)
	}
}

func TestRememberedChunksShareOneStampKeepTheLast300LinesAndReachDesktopLog(t *testing.T) {
	file := filepath.Join(t.TempDir(), "logs", "desktop.log")
	moment := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.Local)
	l := New(file, func() time.Time { return moment })
	l.Remember("  first\r\nsecond  ")
	for i := range 310 {
		l.Remember("line " + strconv.Itoa(i))
	}
	recent := l.Recent(300)
	if len(recent) != 300 || recent[299] != "2026-01-02 03:04:05,006 [icebath] line 309" {
		t.Fatalf("recent %d %q", len(recent), recent[len(recent)-1])
	}
	l.Drain()
	raw, err := os.ReadFile(file)
	if err != nil || !strings.HasPrefix(string(raw), "2026-01-02 03:04:05,006 [icebath] first\n2026-01-02 03:04:05,006 [icebath] second\n") {
		t.Fatalf("file %q %v", string(raw)[:80], err)
	}
}
