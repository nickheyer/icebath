package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// ProbeIntervalSeconds spaces dispatch write retries and idle re probes of a degraded store
const ProbeIntervalSeconds = 60.0

// LowFreeBytes is the free space below which doctor warns before the store starts failing
const LowFreeBytes = 100 << 20

const storeFixHint = "free disk space, remount it read-write, or fix permissions on %s"

var storeDispatchSites = map[string]bool{"advance": true, "claim": true}

// Distinct job id and scheduled instant pair an outage kept from running
type runKey struct {
	ID        string
	NextRunAt string
	HasNext   bool
}

// StoreDegraded is one cron store that stopped accepting writes and what that cost
type StoreDegraded struct {
	Store       string
	Since       float64
	Error       string
	RecoveredAt *float64
	Sites       map[string]bool
	Skipped     map[runKey]bool
	lastProbe   *float64
}

// SkippedRuns counts the distinct scheduled runs the outage skipped
func (r *StoreDegraded) SkippedRuns() int { return len(r.Skipped) }

// NoticeFields renders the store, error, start time, fix hint and skipped count for notices
func (r *StoreDegraded) NoticeFields() map[string]any {
	since := r.Since
	out := storeReportFields(r.Store, r.Error, &since)
	out["skipped"] = r.SkippedRuns()
	return out
}

func (r *StoreDegraded) snapshot() *StoreDegraded {
	c := *r
	c.Sites, c.Skipped = map[string]bool{}, map[runKey]bool{}
	for k := range r.Sites {
		c.Sites[k] = true
	}
	for k := range r.Skipped {
		c.Skipped[k] = true
	}
	return &c
}

var storeHealth = struct {
	sync.Mutex
	degraded  map[string]*StoreDegraded
	recovered map[string]*StoreDegraded
	listener  func(event string, record *StoreDegraded)
}{degraded: map[string]*StoreDegraded{}, recovered: map[string]*StoreDegraded{}}

// Wall and monotonic clocks plus the write probe, swapped by tests driving an outage
var storeHealthHooks = struct {
	wall, mono func() float64
	probe      func(cronDir string) error
}{wall: func() float64 { return epochNow() }, mono: monoNow, probe: func(dir string) error { return ProbeStore(dir) }}

var monoStart = time.Now()

func monoNow() float64 { return time.Since(monoStart).Seconds() }

// DescribeStoreError renders an OSError as errno name and strerror, its plain text without an errno
func DescribeStoreError(err error) string {
	code, _ := pypath.ErrnoName(err)
	text := ""
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text = pypath.Strerror(errno)
	}
	if text == "" {
		text = pypath.OSErrorText(err)
	}
	if code != "" {
		return code + ": " + text
	}
	return text
}

func storeReportFields(store, errText string, since *float64) map[string]any {
	sinceText := "unknown"
	if since != nil {
		sec := int64(*since)
		at := time.Unix(sec, int64((*since-float64(sec))*1e9)).Local()
		sinceText = at.Format("2006-01-02T15:04:05-07:00")
	}
	return map[string]any{"store": store, "error": errText, "since": sinceText, "fix": fmt.Sprintf(storeFixHint, store)}
}

// SetStoreTransitionListener installs the receiver of unwritable and recovered transitions, nil removes it
func SetStoreTransitionListener(fn func(event string, record *StoreDegraded)) {
	storeHealth.Lock()
	storeHealth.listener = fn
	storeHealth.Unlock()
}

// Hands a transition to the listener, a failing listener logged and never breaking the tick
func notifyStoreTransition(log *slog.Logger, event string, record *StoreDegraded) {
	storeHealth.Lock()
	listener := storeHealth.listener
	storeHealth.Unlock()
	if listener == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("Cron store %s transition listener failed", event), "error", r)
		}
	}()
	listener(event, record.snapshot())
}

func storeHealthKey(cronDir string) string {
	resolved, err := pypath.Realpath(cronDir, pypath.Lax)
	if err != nil {
		return filepath.Clean(cronDir)
	}
	return resolved
}

func runKeys(jobs []*Job) []runKey {
	keys := make([]runKey, 0, len(jobs))
	for _, j := range jobs {
		keys = append(keys, runKey{ID: j.ID, NextRunAt: deref(j.NextRunAt), HasNext: j.NextRunAt != nil})
	}
	return keys
}

// Marks the store degraded after a failed write, announcing entry once and each new failing site
func storeHealthNoteUnwritable(s *Store, err error, consequence, site string, skippedJobs ...*Job) {
	noteUnwritableAt(s.log, s.cronDir, err, consequence, site, skippedJobs...)
}

func noteUnwritableAt(log *slog.Logger, cronDir string, err error, consequence, site string, skippedJobs ...*Job) {
	store := storeHealthKey(cronDir)
	storeHealth.Lock()
	record := storeHealth.degraded[store]
	entered := record == nil
	if entered {
		since := storeHealthHooks.wall()
		if ended := storeHealth.recovered[store]; ended != nil {
			since = ended.Since
			delete(storeHealth.recovered, store)
		}
		record = &StoreDegraded{Store: store, Since: since, Error: DescribeStoreError(err), Sites: map[string]bool{}, Skipped: map[runKey]bool{}}
		storeHealth.degraded[store] = record
	}
	newSite := !record.Sites[site]
	record.Sites[site] = true
	if storeDispatchSites[site] || record.lastProbe != nil {
		mono := storeHealthHooks.mono()
		record.lastProbe = &mono
	}
	record.Error = DescribeStoreError(err)
	for _, k := range runKeys(skippedJobs) {
		record.Skipped[k] = true
	}
	snap := record.snapshot()
	storeHealth.Unlock()
	switch {
	case entered:
		log.Warn(fmt.Sprintf("Cron store %s is unwritable (%s); %s. Scheduled jobs are skipped until it accepts writes "+
			"again, then each due job fires once. Fix: %s.", store, pypath.OSErrorText(err), consequence, fmt.Sprintf(storeFixHint, store)))
		notifyStoreTransition(log, "unwritable", snap)
	case newSite:
		log.Info(fmt.Sprintf("Cron store %s still unwritable (%s); %s", store, pypath.OSErrorText(err), consequence))
	}
}

// Ends the outage of the store a write just landed in, keeping it until a due scan save sees it
func storeHealthNoteWritable(s *Store, cronDir string) {
	storeHealth.Lock()
	if len(storeHealth.degraded) == 0 {
		storeHealth.Unlock()
		return
	}
	key := storeHealthKey(cronDir)
	record := storeHealth.degraded[key]
	var snap *StoreDegraded
	if record != nil {
		delete(storeHealth.degraded, key)
		at := storeHealthHooks.wall()
		record.RecoveredAt = &at
		storeHealth.recovered[record.Store] = record
		snap = record.snapshot()
	}
	storeHealth.Unlock()
	if snap == nil {
		return
	}
	s.log.Warn(fmt.Sprintf("Cron store %s is writable again; %d skipped run(s), catching up once per job", snap.Store, snap.SkippedRuns()))
	notifyStoreTransition(s.log, "recovered", snap)
}

// DegradedRecord is the live outage of the store under cronDir, nil while it accepts writes
func DegradedRecord(cronDir string) *StoreDegraded {
	storeHealth.Lock()
	defer storeHealth.Unlock()
	if r := storeHealth.degraded[storeHealthKey(cronDir)]; r != nil {
		return r.snapshot()
	}
	return nil
}

// OutageCovers reports whether a run due at dueAt within grace fell inside the live or just ended outage
func OutageCovers(cronDir string, dueAt, grace float64) bool {
	key := storeHealthKey(cronDir)
	storeHealth.Lock()
	defer storeHealth.Unlock()
	for _, r := range []*StoreDegraded{storeHealth.degraded[key], storeHealth.recovered[key]} {
		if r != nil && r.Since <= dueAt+grace && (r.RecoveredAt == nil || dueAt <= *r.RecoveredAt) {
			return true
		}
	}
	return false
}

// StoreKey is the health registry key of the cron store under a profile home
func StoreKey(home string) string { return storeHealthKey(filepath.Join(home, "cron")) }

// ForgetStores drops live and just ended outages of the given store keys
func ForgetStores(stores []string) {
	storeHealth.Lock()
	defer storeHealth.Unlock()
	for _, store := range stores {
		delete(storeHealth.degraded, store)
		delete(storeHealth.recovered, store)
	}
}

// DegradedRecords lists every store currently refusing writes
func DegradedRecords() []*StoreDegraded {
	storeHealth.Lock()
	defer storeHealth.Unlock()
	out := make([]*StoreDegraded, 0, len(storeHealth.degraded))
	for _, r := range storeHealth.degraded {
		out = append(out, r.snapshot())
	}
	return out
}

// Holds dispatch while a failed dispatch write is throttled or the re probe still fails, counting skipped runs
func (s *Store) dispatchBlocked(dueJobs []*Job) bool {
	key := storeHealthKey(s.cronDir)
	storeHealth.Lock()
	record := storeHealth.degraded[key]
	if record == nil || record.lastProbe == nil {
		storeHealth.Unlock()
		return false
	}
	last := *record.lastProbe
	storeHealth.Unlock()
	now := storeHealthHooks.mono()
	throttled := now-last < ProbeIntervalSeconds
	var probeErr error
	if !throttled {
		probeErr = storeHealthHooks.probe(s.cronDir)
	}
	storeHealth.Lock()
	defer storeHealth.Unlock()
	if !throttled {
		record.lastProbe = &now
	}
	if probeErr != nil {
		record.Error = DescribeStoreError(probeErr)
	}
	blocked := throttled || probeErr != nil
	if blocked {
		for _, k := range runKeys(dueJobs) {
			record.Skipped[k] = true
		}
	}
	return blocked
}

// Closes the just ended outage window once a due scan save has landed
func storeHealthEndRecoveryWindow(s *Store, cronDir string) {
	storeHealth.Lock()
	if len(storeHealth.recovered) > 0 {
		delete(storeHealth.recovered, storeHealthKey(cronDir))
	}
	storeHealth.Unlock()
}

// Re probes a degraded store on an idle tick and rewrites jobs.json once the probe passes to end the outage
func (s *Store) recheckIdle(ctx context.Context) error {
	key := storeHealthKey(s.cronDir)
	storeHealth.Lock()
	record := storeHealth.degraded[key]
	now := storeHealthHooks.mono()
	if record == nil || (record.lastProbe != nil && now-*record.lastProbe < ProbeIntervalSeconds) {
		storeHealth.Unlock()
		return nil
	}
	record.lastProbe = &now
	storeHealth.Unlock()
	probeErr := storeHealthHooks.probe(s.cronDir)
	if probeErr == nil {
		err := s.withJobsLock(home.WithOverride(ctx, s.home), func(tx *jobsTx) error {
			jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
			if err != nil {
				return err
			}
			return s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false)
		})
		if err == nil {
			return nil
		}
		if !isOSError(err) {
			return err
		}
		probeErr = err
	}
	storeHealth.Lock()
	record.Error = DescribeStoreError(probeErr)
	storeHealth.Unlock()
	return nil
}

// ProbeReport describes why the store under cronDir refuses writes, nil when the probe passes
func ProbeReport(cronDir string) map[string]any {
	probeErr := storeHealthHooks.probe(cronDir)
	if probeErr == nil {
		return nil
	}
	var since *float64
	if st, err := os.Stat(filepath.Join(cronDir, "jobs.json")); err == nil {
		at := float64(st.ModTime().UnixNano()) / 1e9
		since = &at
	}
	return storeReportFields(cronDir, DescribeStoreError(probeErr), since)
}

// FreeBytes is the space a writer can still use under path, root counting the reserved blocks
func FreeBytes(path string) (uint64, bool) { return freeBytes(path) }

// ProbeStore proves the store accepts a write as large as jobs.json, returning the OSError a save would hit
func ProbeStore(cronDir string) error {
	if st, err := os.Stat(cronDir); err != nil || !st.IsDir() {
		return nil
	}
	jobsFile := filepath.Join(cronDir, "jobs.json")
	size := int64(4096)
	if st, err := os.Stat(jobsFile); err == nil {
		size += st.Size()
	}
	resolved, err := pypath.Realpath(jobsFile, pypath.Lax)
	if err != nil {
		resolved = jobsFile
	}
	for _, target := range []string{resolved, filepath.Join(cronDir, ".tick.lock")} {
		if _, err := os.Stat(target); err == nil && !writable(target) {
			return &fs.PathError{Op: "access", Path: target, Err: syscall.EACCES}
		}
	}
	f, err := pyfs.MkstempBeside(jobsFile, ".probe_", "")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	chunk := make([]byte, 65536)
	for offset := int64(0); offset < size; offset += int64(len(chunk)) {
		n := min(int64(len(chunk)), size-offset)
		if _, err := f.Write(chunk[:n]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
