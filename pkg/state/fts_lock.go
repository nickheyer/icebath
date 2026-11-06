package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Admission wait a test may shorten
var ftsRebuildLockTimeoutSeconds = 120.0

const (
	ftsRebuildLockPollSeconds = 0.1
	lockBreakReacquireSeconds = 5.0
)

var isWindows = runtime.GOOS == "windows"

// Outcome of a bounded advisory lock acquire
type lockOutcome int

const (
	lockTimedOut lockOutcome = iota
	lockAcquired
	lockFailed
)

// Whether err means another process holds the lock, any other failure failing closed at once
func IsAdvisoryLockContention(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && lockContentionErrno(errno)
}

// Kernel start time of a process from its proc stat line, false off Linux or on any failure
func procStartTicks(pid int64) (int64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/stat")
	if err != nil {
		return 0, false
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	n, err := pyIntOf(fields[19])
	if err != nil || !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// Holder metadata object of a lock file, nil when absent, unreadable or not a JSON object
func readLockHolderRecord(f *os.File) pyObject {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	if n == 0 {
		return nil
	}
	v, err := pyLoads(pyDecodeUTF8Replace(buf[:n]))
	if err != nil {
		return nil
	}
	record, _ := v.(pyObject)
	return record
}

// Truncates the lock file and writes payload at offset zero, ignoring IO failures
func rewriteLockFile(f *os.File, payload []byte) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	if len(payload) > 0 {
		if _, err := f.Write(payload); err != nil {
			return
		}
	}
}

// Records this process as lock holder so contenders can tell an orphaned descriptor from a live holder
func writeLockHolderRecord(f *os.File) {
	pid := int64(os.Getpid())
	record := pyObject{{"pid", pid}, {"pidns", nil}, {"start_ticks", nil}, {"acquired_at", float64(time.Now().UnixNano()) / 1e9}}
	if ns := PidNamespaceID(); ns != "" {
		record[1].Value = ns
	}
	if ticks, ok := procStartTicks(pid); ok {
		record[2].Value = ticks
	}
	payload, err := pyDumps(record, pyJSON{sortKeys: true, ensureASCII: true, allowNaN: true, itemSep: ", ", keySep: ": "})
	if err != nil {
		return
	}
	rewriteLockFile(f, []byte(payload))
}

// Erases holder metadata before a normal release so a surviving record means an abnormal exit
func clearLockHolderRecord(f *os.File) { rewriteLockFile(f, nil) }

func (o pyObject) get(key string) (any, bool) {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// Whether an int equals a decoded JSON value under Python equality
func pyIntEquals(n int64, v any) bool {
	switch x := v.(type) {
	case json.Number:
		m, ok := new(big.Int).SetString(x.String(), 10)
		return ok && m.IsInt64() && m.Int64() == n
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0) && x == math.Trunc(x) && big.NewFloat(x).Cmp(new(big.Float).SetInt64(n)) == 0
	case bool:
		return (x && n == 1) || (!x && n == 0)
	case int64:
		return x == n
	case int:
		return int64(x) == n
	}
	return false
}

// True only when the recorded holder is provably dead or its pid recycled, the OverflowError int raises propagating
func lockHolderProvablyDead(record pyObject) (bool, error) {
	pidValue, ok := record.get("pid")
	if !ok {
		return false, nil
	}
	pid, err := pyIntOf(pidValue)
	if err != nil {
		var conv *pyConvError
		if errors.As(err, &conv) && conv.Class == "OverflowError" {
			return false, err
		}
		return false, nil
	}
	if pid.Sign() <= 0 {
		return false, nil
	}
	recorded, stamped := "", false
	if ns, present := record.get("pidns"); present && ns != nil {
		stamped = true
		recorded, _ = ns.(string)
	}
	if !PersistentRecordPidnsCheckable(recorded, stamped) {
		return false, nil
	}
	if pid.Cmp(big.NewInt(math.MaxInt32)) > 0 {
		return false, &pyConvError{"OverflowError", "signed integer is greater than maximum"}
	}
	alive, gone := signalZero(int(pid.Int64()))
	if gone {
		return true, nil
	}
	if !alive {
		return false, nil
	}
	recordedTicks, _ := record.get("start_ticks")
	if recordedTicks == nil {
		return false, nil
	}
	current, ok := procStartTicks(pid.Int64())
	return ok && !pyIntEquals(current, recordedTicks), nil
}

// Holder identity for deferral warnings, the OverflowError float raises propagating
func describeLockHolder(record pyObject) (string, error) {
	pid, ok := record.get("pid")
	if !ok {
		return "unknown (no holder record; pre-fix writer or non-icebath)", nil
	}
	age := ""
	if acquired, _ := record.get("acquired_at"); acquired != nil {
		at, err := pyFloatOf(acquired)
		var conv *pyConvError
		switch {
		case err == nil:
			age = ", acquired " + pyFixed0(float64(time.Now().UnixNano())/1e9-at) + "s ago"
		case errors.As(err, &conv) && conv.Class == "OverflowError":
			return "", err
		}
	}
	return "pid " + pyStr(pid) + age, nil
}

// Float formatted with zero decimals the way the .0f format spec renders it
func pyFixed0(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// Serializes full structural FTS rebuilds on a store across processes, fn learning whether this process holds the authority
func FTSRebuildAdmission(dbPath string, timeoutSeconds *float64, fn func(admitted bool) error) error {
	if dbPath == "" {
		return fn(true)
	}
	timeout := ftsRebuildLockTimeoutSeconds
	if timeoutSeconds != nil {
		timeout = math.Max(*timeoutSeconds, 0)
	}
	lockPath := dbPath + ".fts_rebuild.lock"
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not open FTS rebuild lock %s (%s) — deferring this rebuild "+
			"rather than running it without cross-process authority.", lockPath, err))
		return fn(false)
	}
	acquired := false
	defer func() {
		if acquired {
			releaseAdmissionLock(handle)
		}
		_ = handle.Close()
	}()
	outcome, handle, err := acquireAdmissionLock(lockPath, handle, timeout)
	if err != nil {
		return err
	}
	acquired = outcome == lockAcquired
	if outcome == lockTimedOut {
		var record pyObject
		if !isWindows {
			record = readLockHolderRecord(handle)
		}
		holder, err := describeLockHolder(record)
		if err != nil {
			return err
		}
		if timeout <= 0 {
			slog.Info(fmt.Sprintf("FTS rebuild lock %s is busy — deferring this retry "+
				"(the stale-FTS breadcrumb keeps it retryable). Recorded holder: %s.", lockPath, holder))
		} else {
			slog.Warn(fmt.Sprintf("FTS rebuild lock %s held by another process for more than %.0fs — deferring "+
				"this rebuild to avoid racing the holder (the stale-FTS breadcrumb keeps it "+
				"retryable). Recorded holder: %s.", lockPath, timeout, holder))
		}
	}
	return fn(acquired)
}
