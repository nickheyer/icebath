package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	// TickerIntervalSeconds is the scheduler loop period
	TickerIntervalSeconds = 60
	oneshotGraceSeconds   = 120
	cronOutputDefaultKeep = 50
	saveJobsMergeAttempts = 5
)

// Store is one profile home's cron job store under <home>/cron
type Store struct {
	home       string
	cronDir    string
	jobsFile   string
	outputDir  string
	clk        *clock
	log        *slog.Logger
	knobs      *knobWarner
	loadConfig func() (config.Tree, error)
	// Executions database path a test points elsewhere, the cron dir default when empty
	executionsFile string
	// Notepad database path a test points elsewhere, the cron dir default when empty
	notepadFile string
}

// Held jobs lock section carrying the jobs.json stamp captured by the last load inside it
type jobsTx struct {
	loadStamp *fileStamp
}

func (tx *jobsTx) recordLoadStamp(st *fileStamp) {
	if tx != nil {
		tx.loadStamp = st
	}
}

type fileStamp struct {
	mtimeNS int64
	size    int64
	ino     uint64
}

var (
	jobsDirLocksMu sync.Mutex
	jobsDirLocks   = map[string]*sync.Mutex{}
	fireFenceMu    sync.Mutex
	fireFenceLocks = map[string]chan struct{}{}
	epochNow       = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

// Store rooted at home with now as its clock source and the zone read from config and environment
func openCronStore(home string, now func() time.Time) *Store {
	resolved, err := pypath.Realpath(pypath.Expanduser(home), pypath.Lax)
	if err != nil {
		resolved = filepath.Clean(pypath.Expanduser(home))
	}
	s := &Store{home: resolved, log: slog.Default()}
	s.cronDir = filepath.Join(resolved, "cron")
	s.jobsFile = filepath.Join(s.cronDir, "jobs.json")
	s.outputDir = filepath.Join(s.cronDir, "output")
	s.loadConfig = func() (config.Tree, error) { return config.Load(resolved, nil) }
	s.knobs = newKnobWarner(s.log)
	s.clk = &clock{nowFn: now, zoneFn: func() *time.Location {
		cfg, _ := s.loadConfig()
		loc, _ := ResolveZone(cfg, nil)
		return loc
	}}
	return s
}

// Home is the profile home this store belongs to
func (s *Store) Home() string { return s.home }

// CronDir is the directory holding jobs.json, locks and markers
func (s *Store) CronDir() string { return s.cronDir }

// JobsFile is the jobs.json path
func (s *Store) JobsFile() string { return s.jobsFile }

// OutputDir is where per job run output lives
func (s *Store) OutputDir() string { return s.outputDir }

// Serializes a load modify save section in process and across processes through cron/.jobs.lock
func (s *Store) withJobsLock(fn func(tx *jobsTx) error) error {
	jobsDirLocksMu.Lock()
	mu := jobsDirLocks[s.cronDir]
	if mu == nil {
		mu = &sync.Mutex{}
		jobsDirLocks[s.cronDir] = mu
	}
	jobsDirLocksMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	lockFile, err := s.openJobsLock()
	if err != nil {
		s.log.Warn(fmt.Sprintf("jobs.json cross-process lock unavailable (%s); proceeding with in-process lock only", pypath.OSErrorText(err)))
	}
	if lockFile != nil {
		defer releaseFlock(lockFile)
	}
	return fn(&jobsTx{})
}

// Opens and flocks .jobs.lock, nil without error when the bounded wait timed out
func (s *Store) openJobsLock() (*os.File, error) {
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.cronDir, ".jobs.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	ok, err := acquireFlock(f, jobsLockTimeout)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		s.log.Error(fmt.Sprintf("Timed out after %.0fs waiting for the cron jobs lock (%s) — another process is holding it. Proceeding with in-process locking only so the scheduler stays alive (#60703).", jobsLockTimeout.Seconds(), path))
		f.Close()
		return nil, nil
	}
	return f, nil
}

// Bounded wait for the jobs lock and each fire fence, shortened by tests
var jobsLockTimeout = 30 * time.Second

type fireFenceHeldKey struct{}

// Runs fn holding one job's fire fence, acquired false when the local or cross process lock timed out
func (s *Store) fireJobLock(ctx context.Context, jobID string, fn func(ctx context.Context, acquired bool) error) error {
	key := s.resolvedCronDir() + "::" + jobID
	held, _ := ctx.Value(fireFenceHeldKey{}).(map[string]bool)
	if acquired, ok := held[key]; ok {
		return fn(ctx, acquired)
	}
	fireFenceMu.Lock()
	local := fireFenceLocks[key]
	if local == nil {
		local = make(chan struct{}, 1)
		fireFenceLocks[key] = local
	}
	fireFenceMu.Unlock()
	select {
	case local <- struct{}{}:
	case <-time.After(jobsLockTimeout):
		s.log.Error(fmt.Sprintf("Timed out waiting for local fire fence %s; failing closed", key))
		return fn(ctx, false)
	}
	defer func() { <-local }()
	acquired := false
	var lockFile *os.File
	if err := s.ensureDirs(); err != nil {
		s.log.Error(fmt.Sprintf("Cron fire fence unavailable for %s: %s", jobID, pypath.OSErrorText(err)))
	} else {
		path := filepath.Join(s.cronDir, ".fire-"+uuid5URLHex(key)+".lock")
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			s.log.Error(fmt.Sprintf("Cron fire fence unavailable for %s: %s", jobID, pypath.OSErrorText(err)))
		} else {
			lockFile = f
			ok, err := acquireFlock(f, jobsLockTimeout)
			switch {
			case err != nil:
				s.log.Error(fmt.Sprintf("Cron fire fence unavailable for %s: %s", jobID, pypath.OSErrorText(err)))
			case !ok:
				s.log.Error(fmt.Sprintf("Timed out waiting for fire fence %s; failing closed", path))
			default:
				acquired = true
			}
		}
	}
	defer func() {
		if lockFile == nil {
			return
		}
		if acquired {
			releaseFlock(lockFile)
		} else {
			lockFile.Close()
		}
	}()
	next := make(map[string]bool, len(held)+1)
	for k, v := range held {
		next[k] = v
	}
	next[key] = acquired
	return fn(context.WithValue(ctx, fireFenceHeldKey{}, next), acquired)
}

// Hex of uuid5 in the URL namespace, the fire fence lock file name for one key
func uuid5URLHex(name string) string {
	return strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String(), "-", "")
}

func (s *Store) resolvedCronDir() string {
	if r, err := pypath.Realpath(s.cronDir, pypath.Lax); err == nil {
		return r
	}
	return s.cronDir
}

// Runs fn under the job's fire fence, false without calling it when the fence is unavailable
func (s *Store) underFireFence(ctx context.Context, jobID string, fn func(ctx context.Context) (bool, error)) (bool, error) {
	var out bool
	err := s.fireJobLock(ctx, jobID, func(ctx context.Context, acquired bool) error {
		if !acquired {
			return nil
		}
		var err error
		out, err = fn(ctx)
		return err
	})
	return out, err
}

// Holds the fire fence while fn performs a side effect, ownsClaim true when expectedOwner holds the fire claim
func (s *Store) fireClaimFence(ctx context.Context, jobID, expectedOwner string, fn func(ctx context.Context, ownsClaim bool) error) error {
	return s.fireJobLock(ctx, jobID, func(ctx context.Context, acquired bool) error {
		if !acquired {
			return fn(ctx, false)
		}
		owns := false
		err := s.withJobsLock(func(tx *jobsTx) error {
			jobs, err := s.loadJobs(tx)
			if err != nil {
				return err
			}
			var job *Job
			for _, j := range jobs {
				if j.ID == jobID {
					job = j
					break
				}
			}
			if job == nil {
				owns, err = s.selfRemovalDeliveryAllowed(ctx, tx, jobID)
				return err
			}
			owns = job.FireClaim != nil && job.FireClaim.By == expectedOwner
			return nil
		})
		if err != nil {
			return err
		}
		return fn(ctx, owns)
	})
}

// Run local marker recording that a run deleted its own job record
type selfRemovalDelivery struct {
	jobID   string
	removed bool
}

type selfRemovalKey struct{}

// Context permitting this run's final delivery after it removes its own job record
func selfRemovalDeliveryScope(ctx context.Context, jobID string) (context.Context, *selfRemovalDelivery) {
	marker := &selfRemovalDelivery{jobID: jobID}
	return context.WithValue(ctx, selfRemovalKey{}, marker), marker
}

// Active run deleted exactly its own record and no record has since taken that id
func (s *Store) selfRemovalDeliveryAllowed(ctx context.Context, tx *jobsTx, jobID string) (bool, error) {
	marker, _ := ctx.Value(selfRemovalKey{}).(*selfRemovalDelivery)
	if marker == nil || marker.jobID != jobID || !marker.removed {
		return false, nil
	}
	jobs, err := s.loadJobs(tx)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return false, nil
		}
	}
	return true, nil
}

// Output directory of one job, refusing ids that are not a single safe path component
func (s *Store) jobOutputDir(jobID string) (string, error) {
	text := pystr.Strip(jobID)
	if text == "" || text == "." || text == ".." || strings.ContainsAny(text, "/\\") || filepath.IsAbs(text) || filepath.VolumeName(text) != "" {
		return "", fmt.Errorf("Invalid cron job id for output path: %s", pyrepr.Repr(jobID))
	}
	return filepath.Join(s.outputDir, text), nil
}

// Path sits under a named profile, by its resolved or raw components
func isNamedProfilePath(path string) bool {
	if r, err := pypath.Realpath(path, pypath.Lax); err == nil && hasPart(r, "profiles") {
		return true
	}
	return hasPart(path, "profiles")
}

func hasPart(path, part string) bool {
	for _, p := range strings.Split(filepath.ToSlash(path), "/") {
		if p == part {
			return true
		}
	}
	return false
}

// Creates a cron directory, never recreating a deleted named profile home
func ensureCronDir(dir string) error {
	if !isNamedProfilePath(dir) {
		return os.MkdirAll(dir, 0o777)
	}
	err := os.Mkdir(dir, 0o777)
	if err != nil && errors.Is(err, fs.ErrExist) {
		if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
			return nil
		}
	}
	return err
}

// Cron and output directories exist with owner only permissions
func (s *Store) ensureDirs() error {
	if err := ensureCronDir(s.cronDir); err != nil {
		return err
	}
	if err := ensureCronDir(s.outputDir); err != nil {
		return err
	}
	config.SecureDir(s.home, s.cronDir)
	config.SecureDir(s.home, s.outputDir)
	return nil
}

// Owner only file through the shared config rules
func (s *Store) secureFile(path string) { config.SecureFile(s.home, path) }

// Process identity and chown calls ownership restoration uses, swapped by tests posing as root
var ownership = struct {
	owner      func(os.FileInfo) (int, int, bool)
	euid, egid func() int
	chown      func(path string, uid, gid int) error
}{owner: fileOwner, euid: os.Geteuid, egid: os.Getegid, chown: os.Chown}

// Gives a rewritten file back to its previous owner when root rewrote someone else's file
func (s *Store) preserveFileOwnership(path string, before os.FileInfo) {
	if before == nil {
		return
	}
	uid, gid, ok := ownership.owner(before)
	if !ok {
		return
	}
	euid, egid := ownership.euid(), ownership.egid()
	if euid != 0 || (uid == euid && gid == egid) {
		return
	}
	if err := ownership.chown(path, uid, gid); err != nil {
		s.log.Warn(fmt.Sprintf("Could not restore ownership of %s to uid=%d gid=%d after rewrite: %s — if the gateway runs as a different user, its cron ticker may now be locked out (see issue #68483).", path, uid, gid, pypath.OSErrorText(err)))
	}
}

func statStamp(path string) *fileStamp {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &fileStamp{mtimeNS: st.ModTime().UnixNano(), size: st.Size(), ino: fileInode(path, st)}
}

func (a *fileStamp) equal(b *fileStamp) bool {
	return a != nil && b != nil && *a == *b
}

// Reports an OSError shaped failure as opposed to a decode or logic error
func isOSError(err error) bool {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &errno)
}

// Jobs file decoder every load goes through, swapped by tests that race a write against a load
var jobsFileParser = parseJobsFile

// Reads jobs.json as utf-8-sig text and decodes it, retrying without strict control character rules
func parseJobsFile(path string) (any, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	if !utf8.Valid(raw) {
		return nil, false, fmt.Errorf("'utf-8' codec can't decode jobs.json: invalid continuation byte")
	}
	v, err := pyjson.Loads(string(raw))
	if err == nil {
		return v, false, nil
	}
	var de *pyjson.DecodeError
	if !errors.As(err, &de) {
		return nil, false, err
	}
	v, err = pyjson.LoadsLenient(string(raw))
	return v, true, err
}

func entryWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Loads every job, auto repairing malformed shapes under the jobs lock and persisting the repair
func (s *Store) loadJobs(tx *jobsTx) ([]*Job, error) {
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	pre := statStamp(s.jobsFile)
	if _, err := os.Stat(s.jobsFile); err != nil {
		tx.recordLoadStamp(nil)
		return []*Job{}, nil
	}
	data, lenient, err := jobsFileParser(s.jobsFile)
	if err != nil {
		if isOSError(err) {
			s.log.Error(fmt.Sprintf("IOError reading jobs.json: %s", pypath.OSErrorText(err)))
			return nil, fmt.Errorf("Failed to read cron database: %s", pypath.OSErrorText(err))
		}
		s.log.Error(fmt.Sprintf("Failed to auto-repair jobs.json: %s", err))
		return nil, fmt.Errorf("Cron database corrupted and unrepairable: %s", err)
	}
	repair := ""
	if lenient {
		repair = "had invalid control characters"
	}
	var notes []string
	unmergeable := false
	var entries []any
	switch d := data.(type) {
	case *pyjson.Object:
		jobsVal, ok := d.Get("jobs")
		if !ok {
			jobsVal = []any{}
		}
		switch jv := jobsVal.(type) {
		case *pyjson.Object:
			var skipped []string
			for _, k := range jv.Keys {
				v, isObj := jv.Vals[k].(*pyjson.Object)
				if !isObj {
					skipped = append(skipped, pyrepr.Repr(k))
					continue
				}
				flat := &pyjson.Object{Keys: append([]string(nil), v.Keys...), Vals: map[string]any{}}
				for kk, vv := range v.Vals {
					flat.Vals[kk] = vv
				}
				id, _ := v.Get("id")
				if !pyrepr.Truthy(id) {
					id = k
				}
				flat.Set("id", id)
				entries = append(entries, flat)
			}
			if len(skipped) > 0 {
				notes = append(notes, fmt.Sprintf("Skipping %d non-dict entr%s in id-keyed jobs map: %s", len(skipped), entryWord(len(skipped), "y", "ies"), strings.Join(skipped, ", ")))
			}
			repair, unmergeable = "id-keyed jobs map flattened to list", true
		case []any:
			entries = jv
		default:
			notes = append(notes, fmt.Sprintf("Replacing invalid jobs.json 'jobs' field (%s) with an empty list", pyrepr.TypeName(jobsVal)))
			repair, unmergeable = "invalid jobs field replaced with list", true
		}
	case []any:
		entries = d
		repair = "bare list wrapped as dict"
	default:
		return nil, fmt.Errorf("Cron database corrupted: expected {'jobs': [...]}, got %s", pyrepr.TypeName(data))
	}
	var objs []*pyjson.Object
	junkTypes := map[string]bool{}
	junk := 0
	for _, e := range entries {
		if o, ok := e.(*pyjson.Object); ok {
			objs = append(objs, o)
			continue
		}
		junk++
		junkTypes[pyrepr.TypeName(e)] = true
	}
	if junk > 0 {
		types := make([]string, 0, len(junkTypes))
		for t := range junkTypes {
			types = append(types, t)
		}
		sort.Strings(types)
		notes = append(notes, fmt.Sprintf("Skipping %d non-object entr%s in jobs.json (types: %s)", junk, entryWord(junk, "y", "ies"), strings.Join(types, ", ")))
		if repair == "" {
			repair = "non-object entries dropped"
		}
	}
	for _, o := range objs {
		if normalizeRepeatCompleted(o) && repair == "" {
			repair = "invalid repeat.completed normalized"
		}
	}
	jobs := make([]*Job, 0, len(objs))
	for _, o := range objs {
		j, err := jobFromObject(o)
		if err != nil {
			return nil, fmt.Errorf("Cron database corrupted and unrepairable: %s", err)
		}
		jobs = append(jobs, j)
	}
	if repair != "" {
		if tx == nil {
			var out []*Job
			err := s.withJobsLock(func(tx *jobsTx) error {
				var err error
				out, err = s.loadJobs(tx)
				return err
			})
			return out, err
		}
		for _, n := range notes {
			s.log.Warn(n)
		}
		replace := false
		if unmergeable {
			_, readable := s.peekJobsUnlocked()
			replace = !readable
		}
		if err := s.saveJobsLocked(tx, jobs, nil, replace); err != nil {
			if !isOSError(err) {
				return nil, err
			}
			storeHealthNoteUnwritable(s, err, fmt.Sprintf("jobs.json repair (%s) not persisted", repair), "load")
		} else {
			s.log.Warn(fmt.Sprintf("Auto-repaired jobs.json (%s)", repair))
		}
	}
	tx.recordLoadStamp(pre)
	return jobs, nil
}

// Clamps a non int or negative repeat.completed to a non negative int, reporting whether it changed
func normalizeRepeatCompleted(o *pyjson.Object) bool {
	rep, ok := o.Vals["repeat"].(*pyjson.Object)
	if !ok {
		return false
	}
	c, ok := rep.Get("completed")
	if !ok {
		return false
	}
	switch n := c.(type) {
	case int64:
		if n >= 0 {
			return false
		}
	case *big.Int:
		if n.Sign() >= 0 {
			return false
		}
	}
	v, err := pyint.Of(c)
	if err != nil || v.Sign() < 0 {
		rep.Set("completed", int64(0))
		return true
	}
	rep.Set("completed", pyint.Value(v))
	return true
}

// Repair free read under the lock, empty when missing and false when unreadable
func (s *Store) peekJobsUnlocked() ([]any, bool) {
	if _, err := os.Stat(s.jobsFile); err != nil {
		return []any{}, true
	}
	data, _, err := jobsFileParser(s.jobsFile)
	if err != nil {
		return nil, false
	}
	switch d := data.(type) {
	case *pyjson.Object:
		jobs, ok := d.Get("jobs")
		if !ok {
			return []any{}, true
		}
		list, isList := jobs.([]any)
		return list, isList
	case []any:
		return d, true
	}
	return nil, false
}

// Job record decoded from an ordered JSON object, keeping the object's key order for the next save
func jobFromObject(o *pyjson.Object) (*Job, error) {
	b, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	j := &Job{}
	if err := j.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	j.keyOrder = append([]string(nil), o.Keys...)
	return j, nil
}

// Ordered JSON object of a job record as jobs.json stores it
func jobToObject(j *Job) (any, error) {
	b, err := j.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return pyjson.LoadsBytes(b)
}

// On disk jobs missing from the payload and not deliberately removed, an error when disk is unreadable
func (s *Store) unmergedDiskJobs(tx *jobsTx, jobs []*Job, removedIDs []string) ([]*Job, error) {
	if tx != nil && tx.loadStamp != nil && tx.loadStamp.equal(statStamp(s.jobsFile)) {
		return nil, nil
	}
	disk, ok := s.peekJobsUnlocked()
	if !ok {
		return nil, fmt.Errorf("Cron database corrupted; refusing to overwrite %s", s.jobsFile)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.ID != "" {
			seen[j.ID] = true
		}
	}
	for _, id := range removedIDs {
		if id != "" {
			seen[id] = true
		}
	}
	var recovered []*Job
	for _, d := range disk {
		o, isObj := d.(*pyjson.Object)
		if !isObj {
			continue
		}
		id, _ := o.Get("id")
		if !pyrepr.Truthy(id) {
			continue
		}
		key := pyrepr.Str(id)
		if seen[key] {
			continue
		}
		j, err := jobFromObject(o)
		if err != nil {
			return nil, fmt.Errorf("Cron database corrupted; refusing to overwrite %s", s.jobsFile)
		}
		recovered = append(recovered, j)
		seen[key] = true
	}
	return recovered, nil
}

// Payload plus on disk jobs absent from it, never mutating the payload slice
func (s *Store) mergeUnexpectedDiskJobs(tx *jobsTx, jobs []*Job, removedIDs []string) ([]*Job, error) {
	recovered, err := s.unmergedDiskJobs(tx, jobs, removedIDs)
	if err != nil || len(recovered) == 0 {
		return jobs, err
	}
	ids := make([]any, len(recovered))
	for i, j := range recovered {
		ids[i] = j.ID
	}
	s.log.Warn(fmt.Sprintf("Preserved %d cron job(s) present on disk but missing from the in-memory save payload (concurrent create under degraded lock or stale writer) (#80624): %s", len(recovered), pyrepr.Repr(ids)))
	out := make([]*Job, 0, len(jobs)+len(recovered))
	return append(append(out, jobs...), recovered...), nil
}

// Serializes the store payload to a synced temp file beside the resolved jobs.json
func (s *Store) stageJobsPayload(jobs []*Job) (string, error) {
	list := make([]any, len(jobs))
	for i, j := range jobs {
		o, err := jobToObject(j)
		if err != nil {
			return "", err
		}
		list[i] = o
	}
	payload := pyjson.NewObject()
	payload.Set("jobs", list)
	payload.Set("updated_at", s.clk.nowISO())
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return "", err
	}
	f, err := pyfs.MkstempBeside(s.jobsFile, ".jobs_", ".tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// Writes the rows each save hands over into a temp file beside jobs.json, the save_jobs attribute tests observe
var jobsPayloadStager = (*Store).stageJobsPayload

// Saves every job with the shrink merge guard unless replace, caller holding the jobs lock
func (s *Store) saveJobsLocked(tx *jobsTx, jobs []*Job, removedIDs []string, replace bool) error {
	if err := s.ensureDirs(); err != nil {
		return err
	}
	var before os.FileInfo
	for _, probe := range []string{s.jobsFile, filepath.Dir(s.jobsFile)} {
		if st, err := os.Stat(probe); err == nil {
			before = st
			break
		}
	}
	for attempt := 0; attempt <= saveJobsMergeAttempts; attempt++ {
		if !replace {
			merged, err := s.mergeUnexpectedDiskJobs(tx, jobs, removedIDs)
			if err != nil {
				return err
			}
			jobs = merged
		}
		tmp, err := jobsPayloadStager(s, jobs)
		if err != nil {
			return err
		}
		if !replace && attempt < saveJobsMergeAttempts {
			late, err := s.unmergedDiskJobs(tx, jobs, removedIDs)
			if err != nil {
				os.Remove(tmp)
				return err
			}
			if len(late) > 0 {
				os.Remove(tmp)
				continue
			}
		}
		replaced, err := pyfs.AtomicReplace(tmp, s.jobsFile)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		pyfs.FsyncDirectory(filepath.Dir(replaced))
		s.secureFile(s.jobsFile)
		s.preserveFileOwnership(s.jobsFile, before)
		tx.recordLoadStamp(nil)
		storeHealthNoteWritable(s, filepath.Dir(s.jobsFile))
		return nil
	}
	return nil
}

// LoadJobs returns every job, repairing malformed stores
func (s *Store) LoadJobs() ([]*Job, error) { return s.loadJobs(nil) }

// SaveJobs writes jobs under the lock, removedIDs naming deliberate deletes and replace skipping the merge
func (s *Store) SaveJobs(jobs []*Job, removedIDs []string, replace bool) error {
	return s.withJobsLock(func(tx *jobsTx) error { return s.saveJobsLocked(tx, jobs, removedIDs, replace) })
}

// Atomic owner only marker write whose failures are swallowed so markers never break a tick
func (s *Store) writeMarker(ctx context.Context, name, text, prefix string) {
	if s.ensureDirs() != nil {
		return
	}
	pyfs.AtomicWriteTextMode(home.WithOverride(ctx, s.home), filepath.Join(s.cronDir, name), text, prefix, 0o600)
}

// RecordTickerHeartbeat stamps loop liveness with the writer pid, plus the last success marker when success
func (s *Store) RecordTickerHeartbeat(ctx context.Context, success bool) {
	s.writeMarker(home.WithOverride(ctx, s.home), "ticker_heartbeat", fmt.Sprintf("%s %d", pyrepr.Float(epochNow()), os.Getpid()), ".hb_")
	if success {
		s.writeMarker(home.WithOverride(ctx, s.home), "ticker_last_success", pyrepr.Float(epochNow()), ".hb_")
	}
}

func (s *Store) readMarkerFields(name string) []string {
	text, ok := readUTF8Sig(filepath.Join(s.cronDir, name))
	if !ok {
		return nil
	}
	return pystr.SplitWhitespace(text, -1)
}

// Reads a text file as utf-8-sig with universal newlines, false on any read or decode failure
func readUTF8Sig(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(raw) {
		return "", false
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return text, true
}

// Seconds since the epoch stamp a marker holds, false when missing or unreadable
func (s *Store) epochFileAge(name string) (float64, bool) {
	fields := s.readMarkerFields(name)
	if len(fields) == 0 {
		return 0, false
	}
	stamp, err := pyint.ParseFloat(fields[0])
	if err != nil {
		return 0, false
	}
	age := epochNow() - stamp
	if !(age > 0) {
		return 0, true
	}
	return age, true
}

// TickerHeartbeatWriterAlive reports whether the pid that wrote the heartbeat still runs
func (s *Store) TickerHeartbeatWriterAlive() bool {
	fields := s.readMarkerFields("ticker_heartbeat")
	if len(fields) < 2 {
		return false
	}
	pid, ok := pyint.Parse(fields[1])
	if !ok || !pid.IsInt64() || pid.Int64() > math.MaxInt32 || pid.Int64() < math.MinInt32 {
		return false
	}
	return proc.Exists(int(pid.Int64()))
}

// TickerHeartbeatAge is the seconds since the loop last iterated, false when unknown
func (s *Store) TickerHeartbeatAge() (float64, bool) { return s.epochFileAge("ticker_heartbeat") }

// TickerSuccessAge is the seconds since the last tick that finished without error, false when unknown
func (s *Store) TickerSuccessAge() (float64, bool) { return s.epochFileAge("ticker_last_success") }

// RecordTickerError persists the latest tick failure for status readers in other processes
func (s *Store) RecordTickerError(ctx context.Context, message string) {
	s.writeMarker(home.WithOverride(ctx, s.home), "ticker_last_error", fmt.Sprintf("%s\n%s\n", pyrepr.Float(epochNow()), pystr.Strip(message)), ".terr_")
}

// ClearTickerError removes the last tick error marker, ignoring failures
func (s *Store) ClearTickerError() { os.Remove(filepath.Join(s.cronDir, "ticker_last_error")) }

// TickerLastError is the most recent recorded tick error, false when none
func (s *Store) TickerLastError() (string, bool) {
	raw, ok := readUTF8Sig(filepath.Join(s.cronDir, "ticker_last_error"))
	if !ok {
		return "", false
	}
	lines := pystr.Splitlines(raw, false)
	if len(lines) < 2 {
		return "", false
	}
	msg := pystr.Strip(strings.Join(lines[1:], "\n"))
	return msg, msg != ""
}

// Value of cron.<key> from config, the default when config or the key is unusable
func (s *Store) cronConfigValue(key string, def any) any {
	cfg, err := s.loadConfig()
	if err != nil || cfg == nil {
		return def
	}
	cron, ok := cfg["cron"].(map[string]any)
	if !ok {
		return def
	}
	if v, ok := cron[key]; ok {
		return v
	}
	return def
}

// Integer cron.<key> through Python int, the default when the value does not convert
func (s *Store) cronConfigInt(key string, def int64) int64 {
	v, err := pyint.Of(s.cronConfigValue(key, def))
	if err != nil || !v.IsInt64() {
		if err == nil && v.Sign() > 0 {
			return math.MaxInt64
		}
		if err == nil {
			return math.MinInt64
		}
		return def
	}
	return v.Int64()
}

// Float cron.<key> through Python float, the default when the value does not convert
func (s *Store) cronConfigFloat(key string, def float64) float64 {
	v, err := pyint.Float(s.cronConfigValue(key, def))
	if err != nil {
		return def
	}
	return v
}

// Per job output file cap from cron.output_retention
func (s *Store) cronOutputKeep() int64 {
	return s.cronConfigInt("output_retention", cronOutputDefaultKeep)
}

// Deletes the oldest timestamp named markdown outputs beyond keep, returning how many went
func (s *Store) pruneJobOutput(dir string, keep int64) int {
	if keep <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.Mode().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if int64(len(names)) <= keep {
		return 0
	}
	deleted := 0
	for _, name := range names[keep:] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			s.log.Debug(fmt.Sprintf("Failed to prune cron output %s: %s", name, pypath.OSErrorText(err)))
			continue
		}
		deleted++
	}
	return deleted
}

// SaveJobOutput writes one run's output as a timestamped markdown file and prunes older ones
func (s *Store) SaveJobOutput(ctx context.Context, jobID, output string) (string, error) {
	if err := s.ensureDirs(); err != nil {
		return "", err
	}
	dir, err := s.jobOutputDir(jobID)
	if err != nil {
		return "", err
	}
	if err := ensureCronDir(dir); err != nil {
		return "", err
	}
	config.SecureDir(s.home, dir)
	file := filepath.Join(dir, s.clk.now().Format("2006-01-02_15-04-05")+".md")
	if err := pyfs.AtomicWriteTextMode(home.WithOverride(ctx, s.home), file, output, ".output_", 0o600); err != nil {
		return "", err
	}
	s.secureFile(file)
	s.pruneJobOutput(dir, s.cronOutputKeep())
	return file, nil
}
