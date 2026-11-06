package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Seams over the lock probes and the unscoped runtime fallback that hermes tests patch
var (
	statusIsGatewayRuntimeLockActive       = isGatewayRuntimeLockActiveLive
	statusIsGatewayRuntimeLockActiveStrict = isGatewayRuntimeLockActiveStrictLive
	statusUnscopedRuntimeStatusRunningPid  = func() (int, bool, error) { return GetRuntimeStatusRunningPid(nil, "") }
)

// Reserves the launch profile home, false when another owner holds it or the lock cannot be opened
func AcquireGatewayRuntimeLock() bool {
	if err := ProcessOwnership.Reserve([]string{processHermesHome()}, ""); err != nil {
		return false
	}
	clearRunningPidCache()
	return true
}

// Releases only this process's profile reservations, never their lock files
func ReleaseGatewayRuntimeLock() {
	ProcessOwnership.Close()
	clearRunningPidCache()
}

// Whether this process holds the runtime reservation of its launch home
func OwnsGatewayRuntimeLock() bool { return ProcessOwnership.Owns(processHermesHome()) }

// Whether another process holds the lock behind f, a won probe released and f always closed
func probeLockFile(f *os.File) bool {
	defer f.Close()
	held := !tryAcquireFileLock(f)
	if !held {
		releaseFileLock(f)
	}
	return held
}

// Whether some process owns the gateway runtime lock at lockPath, the process lock when empty
func IsGatewayRuntimeLockActive(lockPath string) (bool, error) {
	return statusIsGatewayRuntimeLockActive(lockPath)
}

func isGatewayRuntimeLockActiveLive(lockPath string) (bool, error) {
	resolved := lockPath
	if resolved == "" {
		resolved = statusGatewayLockPath("")
	}
	if OwnsGatewayRuntimeLock() && resolved == statusGatewayLockPath("") {
		return true, nil
	}
	if _, err := os.Stat(resolved); err != nil {
		return false, nil
	}
	f, err := os.OpenFile(resolved, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if errors.Is(err, fs.ErrPermission) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return probeLockFile(f), nil
}

// Whether path exists, an error instead of false when its metadata cannot be inspected
func strictPathExists(path, label string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%s metadata is not inspectable: %w", label, err)
	}
	return true, nil
}

func isGatewayRuntimeLockActiveStrictLive(lockPath string) (bool, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gateway runtime lock is not inspectable: %w", err)
	}
	return probeLockFile(f), nil
}

// Writes this process pid record with O_EXCL so a racing gateway sees fs.ErrExist
func WritePidFile() error {
	path := pidPath()
	if _, err := home.MakeUserDir(filepath.Dir(path), 0o777, true, true); err != nil {
		return err
	}
	if err := writeJSONExcl(path, statusBuildPidRecord()); err != nil {
		return err
	}
	clearRunningPidCache()
	return nil
}

// Creates path exclusively and dumps record with the json.dump defaults, unlinking on a failed write
func writeJSONExcl(path string, record *pyjson.Object) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o777)
	if err != nil {
		return err
	}
	text, err := pyjson.Dumps(record)
	if err == nil {
		_, err = f.WriteString(text)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		unlinkQuietly(path)
		return err
	}
	return nil
}

// Live gateway pid of a runtime status record proven against the process and its home, false otherwise
func GetRuntimeStatusRunningPid(runtime *pyjson.Object, expectedHome string) (int, bool, error) {
	payload := runtime
	if payload == nil {
		payload = ReadRuntimeStatus("")
	}
	if payload == nil {
		return 0, false, nil
	}
	state := recGet(payload, "gateway_state")
	if state == nil || pyEq(state, "stopped") || pyEq(state, "startup_failed") {
		return 0, false, nil
	}
	pid, ok := liveFromRecord(payload)
	if !ok {
		return 0, false, nil
	}
	if expectedHome == "" {
		belongs, err := pidRecordBelongsToCurrentProfile(payload)
		if err != nil {
			return 0, false, err
		}
		if !belongs {
			return 0, false, nil
		}
	}
	if expectedHome != "" && RecordedGatewayHomeConflicts(payload, expectedHome) {
		return 0, false, nil
	}
	if !recordMatchesLiveGatewayPid(payload, pid, expectedHome) {
		return 0, false, nil
	}
	return pid, true, nil
}

// Verified pid of the gateway owning home from its pid file and lock, then its runtime record
func LiveGatewayPidForHome(h string) (int, bool, error) {
	pid, ok, err := GetRunningPidCached(pypath.PathJoin(h, "gateway.pid"), false, gatewayRunningPidCacheTTL)
	if err != nil || ok {
		return pid, ok, err
	}
	return GetRuntimeStatusRunningPid(ReadRuntimeStatus(pypath.PathJoin(h, runtimeStatusFile)), h)
}

// Removes the pid file only when it names this process
func RemovePidFile() {
	path := pidPath()
	if filePid, ok := pidFromRecord(readJSONFile(path, false), "pid"); ok && filePid != statusGetpid() {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
	clearRunningPidCache()
}

// Whether pid is in a stopped or tracing stop state
func processIsStopped(pid int) bool {
	data, err := statusProcRead(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			fields := strings.Fields(line)
			return len(fields) > 1 && (fields[1] == "T" || fields[1] == "t")
		}
	}
	return false
}

// Machine local lock keyed by scope and identity, false plus the holder record when another owner has it
func AcquireScopedLock(scope, identity string, metadata *pyjson.Object) (bool, *pyjson.Object, error) {
	lockPath := scopeLockPath(scope, identity)
	acquired, existing, err := acquireScopedLockAt(lockPath, scope, identity, metadata)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return false, nil, unusableLockDir(filepath.Dir(lockPath), err)
		}
		return false, nil, err
	}
	unusableLockDirRecovered(filepath.Dir(lockPath))
	return acquired, existing, nil
}

func acquireScopedLockAt(lockPath, scope, identity string, metadata *pyjson.Object) (bool, *pyjson.Object, error) {
	if _, err := home.MakeUserDir(filepath.Dir(lockPath), 0o777, true, true); err != nil {
		return false, nil, err
	}
	if f, err := os.Open(lockPath); err == nil {
		f.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, nil, err
	}
	record := pyjson.NewObject()
	base := statusBuildPidRecord()
	for _, k := range base.Keys {
		record.Set(k, recGet(base, k))
	}
	record.Set("scope", scope)
	record.Set("identity_hash", scopeHash(identity))
	if metadata == nil || metadata.Len() == 0 {
		metadata = pyjson.NewObject()
	}
	record.Set("metadata", metadata)
	record.Set("updated_at", utcNowISO())
	if profile := statusProfileLabelForHome(processHermesHome()); profile != "" {
		record.Set("profile", profile)
	}
	existing := readJSONFile(lockPath, false)
	if existing == nil {
		if _, err := os.Stat(lockPath); err == nil {
			unlinkQuietly(lockPath)
		}
	}
	if existing != nil && existing.Len() > 0 {
		existingPid, hasPid := pidFromRecord(existing, "pid")
		if hasPid && existingPid == statusGetpid() {
			if err := writeJSONFile(lockPath, record); err != nil {
				return false, nil, err
			}
			return true, existing, nil
		}
		if !scopedLockRecordIsStale(existing, existingPid, hasPid) {
			return false, existing, nil
		}
		tombstone := lockPath + ".stale"
		if err := statusReplace(lockPath, tombstone); err == nil {
			unlinkQuietly(tombstone)
		}
	}
	if err := writeJSONExcl(lockPath, record); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, readJSONFile(lockPath, false), nil
		}
		return false, nil, err
	}
	return true, nil, nil
}

// Whether a scoped lock owner is dead, reused or stopped, gateway owners also proven by their command line
func scopedLockRecordIsStale(existing *pyjson.Object, existingPid int, hasPid bool) bool {
	if !hasPid || !statusPidExists(existingPid) {
		return true
	}
	recordedStart := recGet(existing, "start_time")
	currentStart := statusProcessStartTime(existingPid)
	if startTimesConflict(recordedStart, currentStart) {
		return true
	}
	meta, _ := recGet(existing, "metadata").(*pyjson.Object)
	processOwner := meta != nil && pyEq(recGet(meta, "owner_kind"), "process")
	if !processOwner && !statusLooksLikeGatewayProcess(existingPid) {
		if _, ok := statusReadProcessCmdline(existingPid); ok {
			return true
		}
		if (recordedStart == nil || currentStart == nil) && !recordLooksLikeGateway(existing) {
			return true
		}
	}
	return processIsStopped(existingPid)
}

// Releases a scope lock owned by this pid whatever its recorded start time
func ReleaseScopedLock(scope, identity string) {
	lockPath := scopeLockPath(scope, identity)
	if pyEq(recGet(readJSONFile(lockPath, false), "pid"), int64(statusGetpid())) {
		unlinkQuietly(lockPath)
	}
}

// Removes scoped lock files, only those of ownerPid and ownerStartTime when given, returning the count
func ReleaseAllScopedLocks(ownerPid *int, ownerStartTime any) int {
	lockDir := GatewayLockDir()
	if _, err := os.Stat(lockDir); err != nil {
		return 0
	}
	matches, err := pypath.Glob(lockDir, "*.lock")
	if err != nil {
		return 0
	}
	removed := 0
	for _, lockFile := range matches {
		if ownerPid != nil {
			record := readJSONFile(lockFile, false)
			pid, ok := pidFromRecord(record, "pid")
			if !ok || pid != *ownerPid || (ownerStartTime != nil && !pyEq(recGet(record, "start_time"), ownerStartTime)) {
				continue
			}
		}
		if err := os.Remove(lockFile); err == nil || errors.Is(err, fs.ErrNotExist) {
			removed++
		}
	}
	return removed
}

// Pid of a running gateway proven by the held lock and its records, false when none runs
func GetRunningPid(pidPathArg string, cleanupStale bool) (int, bool, error) {
	resolvedPid := pidPathArg
	if resolvedPid == "" {
		resolvedPid = pidPath()
	}
	resolvedLock := statusGatewayLockPath(resolvedPid)
	active, err := statusIsGatewayRuntimeLockActive(resolvedLock)
	if err != nil {
		return 0, false, err
	}
	if active {
		expectedHome := ""
		if pidPathArg != "" {
			expectedHome = pypath.PathParent(pidPathArg)
		}
		sawLive := false
		for _, record := range []*pyjson.Object{readPidRecord(resolvedPid), readGatewayLockRecord(resolvedLock)} {
			pid, ok := liveFromRecord(record)
			if !ok {
				continue
			}
			var homeOK bool
			if expectedHome == "" {
				if homeOK, err = pidRecordBelongsToCurrentProfile(record); err != nil {
					return 0, false, err
				}
			} else {
				homeOK = !RecordedGatewayHomeConflicts(record, expectedHome)
			}
			if homeOK && recordMatchesLiveGatewayPid(record, pid, expectedHome) {
				return pid, true, nil
			}
			if homeOK || expectedHome != "" {
				sawLive = true
			}
		}
		if !sawLive {
			cleanupInvalidPidPath(resolvedPid, cleanupStale)
		}
		if pidPathArg == "" {
			return statusUnscopedRuntimeStatusRunningPid()
		}
		return 0, false, nil
	}
	runtimePid, ok := 0, false
	if pidPathArg == "" {
		if runtimePid, ok, err = statusUnscopedRuntimeStatusRunningPid(); err != nil {
			return 0, false, err
		}
	}
	if !ok || runtimePid == statusGetpid() {
		cleanupInvalidPidPath(resolvedPid, cleanupStale)
	}
	return runtimePid, ok, nil
}

// Verified live gateway identity behind a held lock, an error for ambiguous or malformed runtime state
func GetRunningPidIdentityStrict(pidPathArg string) (int, float64, bool, error) {
	resolvedLock := statusGatewayLockPath(pidPathArg)
	pidExists, err := strictPathExists(pidPathArg, "gateway PID")
	if err != nil {
		return 0, 0, false, err
	}
	if exists, err := strictPathExists(resolvedLock, "gateway lock"); err != nil || !exists {
		return 0, 0, false, err
	}
	if active, err := statusIsGatewayRuntimeLockActiveStrict(resolvedLock); err != nil || !active {
		return 0, 0, false, err
	}
	lockRecord := readGatewayLockRecord(resolvedLock)
	records := strictIdentityRecords(pidPathArg, lockRecord, pidExists)
	for _, r := range records {
		if r == nil || r.Len() == 0 {
			return 0, 0, false, errors.New("gateway PID or lock metadata is malformed")
		}
	}
	pid, ok := pidFromRecord(records[0], "pid")
	if !ok || pid <= 0 {
		return 0, 0, false, errors.New("gateway PID and lock identities disagree")
	}
	for _, r := range records[1:] {
		if other, ok := pidFromRecord(r, "pid"); !ok || other != pid {
			return 0, 0, false, errors.New("gateway PID and lock identities disagree")
		}
	}
	if !statusPidExists(pid) {
		return 0, 0, false, errors.New("gateway identity is not live")
	}
	currentStart := statusProcessStartTime(pid)
	starts := make([]any, len(records))
	missing := currentStart == nil
	for i, r := range records {
		starts[i] = recGet(r, "start_time")
		missing = missing || starts[i] == nil
	}
	if missing {
		return 0, 0, false, errors.New("gateway creation time is unavailable")
	}
	agree, err := startTimesAgree(currentStart, starts...)
	if err != nil {
		return 0, 0, false, errors.New("gateway creation time is malformed")
	}
	if !agree {
		return 0, 0, false, errors.New("gateway process identity changed")
	}
	for _, r := range records {
		if !recordMatchesLiveGatewayPid(r, pid, "") {
			return 0, 0, false, errors.New("runtime metadata does not identify a live gateway")
		}
	}
	current, _ := pyFloat(currentStart)
	if !statusIsWindows {
		return pid, current, true, nil
	}
	exact, err := statusProcessCreateTime(pid)
	if err != nil {
		return 0, 0, false, fmt.Errorf("exact gateway creation time is unavailable: %w", err)
	}
	if pyRound(exact*100) != math.Trunc(current) {
		return 0, 0, false, errors.New("gateway process identity changed")
	}
	return pid, exact, true, nil
}

// Records the strict identity check must agree on, a previous gateway's pid file dropping out
func strictIdentityRecords(pidPathArg string, lockRecord *pyjson.Object, pidExists bool) []*pyjson.Object {
	if !pidExists {
		return []*pyjson.Object{lockRecord}
	}
	pidRecord := readPidRecord(pidPathArg)
	if pidRecord != nil && pidRecord.Len() > 0 && lockRecord != nil && lockRecord.Len() > 0 && namesAPreviousGateway(pidRecord, lockRecord) {
		return []*pyjson.Object{lockRecord}
	}
	return []*pyjson.Object{pidRecord, lockRecord}
}

// Whether a pid record is another instance than the lock holder and that instance is gone
func namesAPreviousGateway(pidRecord, lockRecord *pyjson.Object) bool {
	pid, ok := pidFromRecord(pidRecord, "pid")
	recorded, held := recGet(pidRecord, "start_time"), recGet(lockRecord, "start_time")
	if !ok || pid <= 0 || recorded == nil || held == nil {
		return false
	}
	agree, err := startTimesAgree(held, recorded)
	if err != nil {
		return false
	}
	if agree {
		return false
	}
	if !statusPidExists(pid) {
		return true
	}
	live := statusProcessStartTime(pid)
	if live == nil {
		return false
	}
	agree, err = startTimesAgree(live, recorded)
	return err == nil && !agree
}

// Running pid cached briefly for polling, invalidated whenever the pid, lock or status file changes
func GetRunningPidCached(pidPathArg string, cleanupStale bool, ttl time.Duration) (int, bool, error) {
	if ttl <= 0 {
		return GetRunningPid(pidPathArg, cleanupStale)
	}
	resolvedPid := pidPathArg
	if resolvedPid == "" {
		resolvedPid = pidPath()
	}
	includeStatus := pidPathArg == ""
	watched := []string{resolvedPid, statusGatewayLockPath(resolvedPid)}
	if includeStatus {
		watched = append(watched, runtimeStatusPath())
	}
	signature := func() []fileSig {
		out := make([]fileSig, len(watched))
		for i, p := range watched {
			out[i] = fileCacheSignature(p)
		}
		return out
	}
	sig := signature()
	key := runningPidCacheKey{resolvedPid, cleanupStale, includeStatus}
	runningPidCacheMu.Lock()
	cached, hit := runningPidCache[key]
	runningPidCacheMu.Unlock()
	if hit && time.Since(cached.at) <= ttl && sigEqual(cached.sig, sig) {
		return cached.pid, cached.ok, nil
	}
	pid, ok, err := GetRunningPid(pidPathArg, cleanupStale)
	if err != nil {
		return 0, false, err
	}
	refreshed := signature()
	runningPidCacheMu.Lock()
	runningPidCache[key] = runningPidCacheEntry{time.Now(), refreshed, pid, ok}
	runningPidCacheMu.Unlock()
	return pid, ok, nil
}

func sigEqual(a, b []fileSig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
