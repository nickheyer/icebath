package activesessions

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
)

var leaseLog = logging.GetLogger("hermes_cli.active_sessions")

// Machine readable refusal reasons, capacity and ownership calling for different operator action
const (
	SessionNotOwned                = "SESSION_NOT_OWNED"
	MaxConcurrentSessions          = "MAX_CONCURRENT_SESSIONS"
	SessionCoordinationUnavailable = "SESSION_COORDINATION_UNAVAILABLE"
)

// Exclusive per session submit the gateway advertises, held by the atomic acquisition check
const PerSessionExclusiveSubmit = true

// Waits on a registry lock with no deadline
const NoLockTimeout time.Duration = -1

// Grace during which a fresh lease of this process is never read as an orphan
const selfOrphanGraceSeconds = 30.0

// Poll interval of a registry lock wait with a deadline
const lockPollInterval = 50 * time.Millisecond

// Refusal of an acquisition, the message for people and the reason as the contract
type Refusal struct {
	Message string
	Reason  string
}

// Message of the refusal
func (r *Refusal) String() string { return r.Message }

// Message the refusal encodes as wherever a string is expected
func (r *Refusal) PyStr() string { return r.Message }

// Refusal text prefixed with its reason line so the contract survives a subprocess boundary
func FormatRefusalStderr(message *Refusal) string {
	if message.Reason != "" {
		return fmt.Sprintf("icebath-refusal-reason: %s\n%s", message.Reason, message.Message)
	}
	return message.Message
}

// Compact holders phrase like desktop x4, cli with the age of the oldest lease
func SummarizeHolders(entries []*pyjson.Object) string {
	if len(entries) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, e := range entries {
		surface := "unknown"
		if v := e.GetOr("surface", nil); pyTruthy(v) {
			surface = pyrepr.Str(v)
		}
		counts[surface]++
	}
	surfaces := slices.Collect(maps.Keys(counts))
	sort.Slice(surfaces, func(i, j int) bool {
		if counts[surfaces[i]] != counts[surfaces[j]] {
			return counts[surfaces[i]] > counts[surfaces[j]]
		}
		return surfaces[i] < surfaces[j]
	})
	parts := make([]string, len(surfaces))
	for i, surface := range surfaces {
		parts[i] = surface
		if n := counts[surface]; n > 1 {
			parts[i] = fmt.Sprintf("%s x%d", surface, n)
		}
	}
	held := strings.Join(parts, ", ")
	oldest, found := 0.0, false
	for _, e := range entries {
		if t, ok := optionalFloat(e.GetOr("started_at", nil)); ok && t != 0 && (!found || t < oldest) {
			oldest, found = t, true
		}
	}
	if found {
		held += fmt.Sprintf(", oldest %s ago", FormatAge(nowSeconds()-oldest))
	}
	return held
}

// Capacity refusal text naming the surfaces that hold the slots
func ActiveSessionLimitMessage(activeCount int, maxSessions *big.Int, entries []*pyjson.Object) string {
	detail := ""
	if held := SummarizeHolders(entries); held != "" {
		detail = fmt.Sprintf(" Held by: %s.", held)
	}
	return fmt.Sprintf("Icebath is at the active session limit (%d/%s).%s Try again when another session finishes.", activeCount, maxSessions.String(), detail)
}

// Refusal text for a session another live process holds, the plain sentence first and details second
func SessionAlreadyOwnedMessage(sessionID string, entry *pyjson.Object) string {
	return "This chat is open in another Icebath window/terminal. Use it there, or start a new chat here.\n" +
		SessionOwnerDetails(sessionID, entry)
}

// True when an existing lease belongs to this pid and the same live session re acquiring it
func isSameWriter(entry *pyjson.Object, metadata map[string]any) bool {
	raw := entry.GetOr("pid", nil)
	if !pyTruthy(raw) {
		raw = int64(-1)
	}
	pid, ok := pythonInt(raw)
	if !ok || pid != int64(os.Getpid()) {
		return false
	}
	existing := ""
	if meta, ok := entry.GetOr("metadata", nil).(*pyjson.Object); ok && meta != nil {
		existing = pyrepr.Str(orEmpty(meta.GetOr("live_session_id", nil)))
	}
	incoming := ""
	if metadata != nil {
		incoming = pyrepr.Str(orEmpty(metadata["live_session_id"]))
	}
	return existing != "" && incoming != "" && existing == incoming
}

// Python int() of a registry pid value, false where it raises
func pythonInt(v any) (int64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return x, true
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return 0, false
		}
		return int64(math.Trunc(x)), true
	case string:
		n, ok := pyIntText(x)
		return int64(n), ok
	case *big.Int:
		return x.Int64(), x.IsInt64()
	}
	return 0, false
}

// Current wall clock in epoch seconds
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Registry lock that could not be taken, the cause wrapped
type LockUnavailableError struct {
	Path string
	Err  error
}

// Message naming the lock file
func (e *LockUnavailableError) Error() string { return "file lock unavailable: " + e.Path }

// Cause of the lock failure
func (e *LockUnavailableError) Unwrap() error { return e.Err }

// Registry lock entry every lease operation takes, the exclusive lock with no deadline
var enterFileLock = func(path string) (func(), error) { return FileLock(path, NoLockTimeout) }

// Takes the exclusive lock on path, polling up to timeout when it is not NoLockTimeout
func FileLock(path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	if err := flockWithTimeout(fh, timeout); err != nil {
		_ = fh.Close()
		return nil, &LockUnavailableError{Path: path, Err: err}
	}
	return func() {
		_ = pyfs.UnlockFile(fh)
		_ = fh.Close()
	}, nil
}

// Blocks on the whole file lock, or retries only while another holder has it until the timeout
func flockWithTimeout(fh *os.File, timeout time.Duration) error {
	if timeout < 0 {
		return pyfs.LockFile(fh)
	}
	deadline := time.Now().Add(timeout)
	for {
		held, err := pyfs.TryLockFile(fh)
		if err != nil {
			return err
		}
		if held {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("file lock still held after %ss", pyrepr.Repr(timeout.Seconds()))
		}
		time.Sleep(lockPollInterval)
	}
}

// Active session slot recorded in a registry until it is released
type Lease struct {
	LeaseID       string
	Surface       string
	Enabled       bool
	StatePath     string
	LockPath      string
	TrackLiveness bool

	mu        sync.Mutex
	sessionID string
	released  bool
}

// Session the lease currently holds
func (l *Lease) SessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID
}

// True once the registry drop of the lease has landed
func (l *Lease) Released() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

func (l *Lease) setSessionID(id string) {
	l.mu.Lock()
	l.sessionID = id
	l.mu.Unlock()
}

func (l *Lease) setReleased() {
	l.mu.Lock()
	l.released = true
	l.mu.Unlock()
}

// Drops the lease from its registry once, a disabled lease never touching the file
func (l *Lease) Release() error {
	if l.Released() || !l.Enabled {
		return nil
	}
	return ReleaseActiveSession(l)
}

// Registry and lock path of the lease when pinned, else of the registry home or the active home
func leaseFiles(ctx context.Context, lease *Lease, registryHome string) (string, string) {
	if lease != nil && lease.StatePath != "" && lease.LockPath != "" {
		return lease.StatePath, lease.LockPath
	}
	return leasePaths(ctx, registryHome)
}

var (
	queuedReleasesMu sync.Mutex
	queuedReleases   = map[string]map[string]bool{}
)

// Normalised real path keying the queued releases of one registry file
func registryKey(path string) string {
	real, err := pypath.Realpath(path, pypath.Lax)
	if err != nil {
		real = path
	}
	return pypath.Normcase(real)
}

// Retires a lease from this process view of the registry now, its file write coming later
func MarkReleaseQueued(lease any) {
	l, ok := lease.(*Lease)
	if !ok || l == nil || !l.Enabled || l.Released() {
		return
	}
	statePath, _ := leaseFiles(context.Background(), l, "")
	key := registryKey(statePath)
	queuedReleasesMu.Lock()
	defer queuedReleasesMu.Unlock()
	if queuedReleases[key] == nil {
		queuedReleases[key] = map[string]bool{}
	}
	queuedReleases[key][l.LeaseID] = true
}

// Makes a lease visible again to the caller holding the registry lock, which drops it
func unmarkReleaseQueued(statePath, leaseID string) {
	key := registryKey(statePath)
	queuedReleasesMu.Lock()
	defer queuedReleasesMu.Unlock()
	if queued, ok := queuedReleases[key]; ok {
		delete(queued, leaseID)
		if len(queued) == 0 {
			delete(queuedReleases, key)
		}
	}
}

// Entries with the releases this process has queued for the registry hidden
func withoutQueuedReleases(path string, entries []*pyjson.Object) []*pyjson.Object {
	queuedReleasesMu.Lock()
	empty := len(queuedReleases) == 0
	queuedReleasesMu.Unlock()
	if empty {
		return entries
	}
	key := registryKey(path)
	queuedReleasesMu.Lock()
	queued := maps.Clone(queuedReleases[key])
	queuedReleasesMu.Unlock()
	if len(queued) == 0 {
		return entries
	}
	var kept []*pyjson.Object
	for _, e := range entries {
		if !queued[leaseIDOf(e)] {
			kept = append(kept, e)
		}
	}
	return kept
}

// Metadata as a JSON object keyed in sorted order
func cleanMetadata(metadata map[string]any) *pyjson.Object {
	obj := pyjson.NewObject()
	for _, k := range slices.Sorted(maps.Keys(metadata)) {
		obj.Set(k, metadata[k])
	}
	return obj
}

// Entries without the lease, the registry written only when it was present
func dropLease(statePath string, entries []*pyjson.Object, leaseID string) ([]*pyjson.Object, error) {
	var kept []*pyjson.Object
	for _, e := range entries {
		if leaseIDOf(e) != leaseID {
			kept = append(kept, e)
		}
	}
	if len(kept) != len(entries) {
		if err := writeEntries(statePath, kept); err != nil {
			return nil, err
		}
	}
	return kept, nil
}

// True when a non empty session id has an entry among the leases
func holdsSession(entries []*pyjson.Object, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for _, e := range entries {
		if pyrepr.Str(orEmpty(e.GetOr("session_id", nil))) == sessionID {
			return true
		}
	}
	return false
}

// Raw and pruned registry entries, false with the warning logged when unreadable and untracked
func readLiveEntries(statePath string, trackLiveness bool, warn string, targetSessionID *string, targetPid *int) ([]*pyjson.Object, []*pyjson.Object, bool, error) {
	raw, err := readEntries(statePath, true)
	if err == nil {
		var live []*pyjson.Object
		live, err = pruneDead(raw, trackLiveness, targetSessionID, targetPid)
		if err == nil {
			return raw, live, true, nil
		}
	}
	var regErr *RegistryError
	if !errors.As(err, &regErr) || trackLiveness {
		return nil, nil, false, err
	}
	leaseLog.Warning(warn)
	return nil, nil, false, nil
}

// Registry entry of a new lease owned by this process
func leaseEntry(leaseID, sessionID, surface string, metadata map[string]any, trackLiveness bool) *pyjson.Object {
	now := nowSeconds()
	var start any
	if s, ok := ownStartTime(); ok {
		start = s
	}
	entry := pyjson.ObjectOf(
		"lease_id", leaseID,
		"session_id", sessionID,
		"surface", surface,
		"pid", int64(os.Getpid()),
		"process_start_time", start,
		"started_at", now,
		"updated_at", now,
	)
	if trackLiveness {
		entry.Set("track_liveness", true)
	}
	if len(metadata) > 0 {
		entry.Set("metadata", cleanMetadata(metadata))
	}
	return entry
}

// Acquires an active session slot, refusing a second live owner of the session or a full configured cap
func TryAcquire(ctx context.Context, sessionID, surface string, cfg any, metadata map[string]any, registryHome string, trackLiveness bool) (*Lease, *Refusal, error) {
	maxSessions := ResolveMaxConcurrentSessions(cfg)
	leaseID := strings.ReplaceAll(uuid.NewString(), "-", "")
	key := sessionID
	if key == "" && !trackLiveness {
		return &Lease{LeaseID: leaseID, sessionID: key, Surface: surface}, nil, nil
	}
	entry := leaseEntry(leaseID, key, surface, metadata, trackLiveness)
	statePath, lockPath := leaseFiles(ctx, nil, registryHome)
	lease := &Lease{LeaseID: leaseID, sessionID: key, Surface: surface, Enabled: true, StatePath: statePath, LockPath: lockPath, TrackLiveness: trackLiveness}
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	raw, entries, loaded, err := readLiveEntries(statePath, trackLiveness,
		"Active-session registry is unavailable; refusing the session rather than risking a concurrent writer", &key, nil)
	if err != nil {
		return nil, nil, err
	}
	if !loaded {
		return nil, &Refusal{
			Message: fmt.Sprintf("Icebath could not read the active-session registry at %s, so it cannot prove this session has no other live owner. Fix or remove that file and try again.", statePath),
			Reason:  SessionCoordinationUnavailable,
		}, nil
	}
	if pruned := len(raw) - len(entries); pruned != 0 {
		leaseLog.Info(fmt.Sprintf("Pruned %d stale active session lease(s)", pruned))
	}
	refuse := func(message, reason, log string) (*Lease, *Refusal, error) {
		if err := writeEntries(statePath, entries); err != nil {
			return nil, nil, err
		}
		leaseLog.Info(log)
		return nil, &Refusal{Message: message, Reason: reason}, nil
	}
	if key != "" {
		for index, existing := range entries {
			if pyrepr.Str(orEmpty(existing.GetOr("session_id", nil))) != key {
				continue
			}
			if isSameWriter(existing, metadata) {
				entries[index] = entry
				if err := writeEntries(statePath, entries); err != nil {
					return nil, nil, err
				}
				return lease, nil, nil
			}
			return refuse(SessionAlreadyOwnedMessage(key, existing), SessionNotOwned,
				fmt.Sprintf("Refused active session %s: already held by pid=%s surface=%s", key, pyrepr.Str(existing.GetOr("pid", nil)), pyrepr.Str(existing.GetOr("surface", nil))))
		}
	}
	if maxSessions != nil && big.NewInt(int64(len(entries))).Cmp(maxSessions) >= 0 {
		return refuse(ActiveSessionLimitMessage(len(entries), maxSessions, entries), MaxConcurrentSessions,
			fmt.Sprintf("Active session limit reached: active=%d max=%s surface=%s", len(entries), maxSessions.String(), surface))
	}
	entries = append(entries, entry)
	if err := writeEntries(statePath, entries); err != nil {
		return nil, nil, err
	}
	return lease, nil, nil
}

// Drops the lease from the registry it was acquired against, an unreadable untracked registry preserved
func ReleaseActiveSession(lease *Lease) error {
	statePath, lockPath := leaseFiles(context.Background(), lease, "")
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	unmarkReleaseQueued(statePath, lease.LeaseID)
	if lease.Released() {
		return nil
	}
	target := lease.SessionID()
	_, live, loaded, err := readLiveEntries(statePath, lease.TrackLiveness,
		"Active-session registry is unavailable; preserving it while releasing an untracked lease", &target, nil)
	if err != nil {
		return err
	}
	if loaded {
		if _, err := dropLease(statePath, live, lease.LeaseID); err != nil {
			return err
		}
	}
	lease.setReleased()
	return nil
}

// Moves a lease to a new session id without dropping its slot, false when it cannot be moved
func TransferActiveSession(lease *Lease, sessionID string, metadata map[string]any) (bool, error) {
	if sessionID == "" || lease.Released() {
		return false, nil
	}
	if !lease.Enabled {
		lease.setSessionID(sessionID)
		return true, nil
	}
	statePath, lockPath := leaseFiles(context.Background(), lease, "")
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return false, err
	}
	defer unlock()
	if lease.Released() {
		return false, nil
	}
	target := lease.SessionID()
	_, entries, loaded, err := readLiveEntries(statePath, lease.TrackLiveness,
		"Active-session registry is unavailable; refusing to overwrite it during lease transfer", &target, nil)
	if err != nil || !loaded {
		return false, err
	}
	var own *pyjson.Object
	for _, e := range entries {
		if leaseIDOf(e) == lease.LeaseID {
			own = e
			break
		}
	}
	switch {
	case own != nil:
		own.Set("session_id", sessionID)
		own.Set("updated_at", nowSeconds())
		if len(metadata) > 0 {
			own.Set("metadata", cleanMetadata(metadata))
		}
	case lease.TrackLiveness:
		entries = append(entries, leaseEntry(lease.LeaseID, sessionID, lease.Surface, metadata, true))
	default:
		return false, nil
	}
	if err := writeEntries(statePath, entries); err != nil {
		return false, err
	}
	lease.setSessionID(sessionID)
	return true, nil
}

// Drops leases of this process its caller cannot vouch for, sparing foreign and freshly written ones
func dropSelfOrphans(entries []*pyjson.Object, ownLiveLeaseIDs map[string]bool) []*pyjson.Object {
	if ownLiveLeaseIDs == nil {
		return entries
	}
	pid := int64(os.Getpid())
	cutoff := nowSeconds() - selfOrphanGraceSeconds
	var kept []*pyjson.Object
	for _, e := range entries {
		started, _ := optionalFloat(e.GetOr("started_at", nil))
		if !pyjson.Equal(e.GetOr("pid", nil), pid) || ownLiveLeaseIDs[leaseIDOf(e)] || started > cutoff {
			kept = append(kept, e)
		}
	}
	return kept
}

// Count of orphaned leases of this process dropped from the registry of one home
func releaseOrphanedLeasesInHome(registryHome string, liveLeaseIDs map[string]bool) (int, error) {
	statePath, lockPath := leasePaths(context.Background(), registryHome)
	if info, err := pypath.PathlibStat(statePath); err != nil || info == nil {
		return 0, err
	}
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return 0, err
	}
	defer unlock()
	pid := os.Getpid()
	_, entries, loaded, err := readLiveEntries(statePath, false,
		"Active-session registry is unavailable; skipping orphaned-lease sweep", nil, &pid)
	if err != nil || !loaded {
		return 0, err
	}
	kept := dropSelfOrphans(entries, liveLeaseIDs)
	dropped := len(entries) - len(kept)
	if dropped != 0 {
		if err := writeEntries(statePath, kept); err != nil {
			return 0, err
		}
	}
	return dropped, nil
}

// Drops the registry entries of this process no live session owns across the root and every profile home
func ReleaseOrphanedLeases(liveLeaseIDs map[string]bool) (int, error) {
	root := home.Root()
	homes := []string{root}
	if dirents, err := os.ReadDir(filepath.Join(root, "profiles")); err == nil {
		for _, d := range dirents {
			if p := filepath.Join(root, "profiles", d.Name()); home.ProfileLive(p) {
				homes = append(homes, p)
			}
		}
	}
	dropped := 0
	for _, h := range homes {
		n, err := releaseOrphanedLeasesInHome(h, liveLeaseIDs)
		var lockErr *LockUnavailableError
		if errors.As(err, &lockErr) {
			return dropped, err
		}
		if err != nil {
			leaseLog.Debug(fmt.Sprintf("orphaned-lease sweep failed for %s: %s", h, err))
			continue
		}
		dropped += n
	}
	return dropped, nil
}

// Runs body holding the registry lock with whether the session is leased, so no claim lands between check and write
func LivenessGuard(ctx context.Context, sessionID, registryHome string, ownLiveLeaseIDs map[string]bool, body func(active bool) error) error {
	statePath, lockPath := leasePaths(ctx, registryHome)
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	raw, err := readEntries(statePath, true)
	if err != nil {
		return err
	}
	entries, err := pruneDead(raw, true, &sessionID, nil)
	if err != nil {
		return err
	}
	entries = dropSelfOrphans(entries, ownLiveLeaseIDs)
	if err := writeEntries(statePath, entries); err != nil {
		return err
	}
	return body(holdsSession(entries, sessionID))
}

// Releases the lease and runs body under its registry lock with whether a sibling still leases the session
func ReleaseLivenessGuard(ctx context.Context, lease *Lease, sessionID string, ownLiveLeaseIDs map[string]bool, body func(active bool) error) error {
	if !lease.Enabled || lease.Released() {
		registryHome := ""
		if lease.StatePath != "" {
			registryHome = filepath.Dir(filepath.Dir(lease.StatePath))
		}
		return LivenessGuard(ctx, sessionID, registryHome, ownLiveLeaseIDs, body)
	}
	statePath, lockPath := leaseFiles(ctx, lease, "")
	unlock, err := enterFileLock(lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	unmarkReleaseQueued(statePath, lease.LeaseID)
	raw, err := readEntries(statePath, true)
	if err != nil {
		return err
	}
	entries, err := pruneDead(raw, true, &sessionID, nil)
	if err != nil {
		return err
	}
	var kept []*pyjson.Object
	for _, e := range entries {
		if leaseIDOf(e) != lease.LeaseID {
			kept = append(kept, e)
		}
	}
	kept = dropSelfOrphans(kept, ownLiveLeaseIDs)
	if len(kept) != len(entries) {
		if err := writeEntries(statePath, kept); err != nil {
			return err
		}
	}
	lease.setReleased()
	return body(holdsSession(kept, sessionID))
}
