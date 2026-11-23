package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// AuthData is the parsed auth.json document in its exact on disk schema
type AuthData map[string]any

// Store persists auth.json for one icebath home with the global root as read only fallback
type Store struct {
	home       string
	path       string
	globalPath string
	cacheMu    sync.Mutex
	gcacheKey  string
	gcacheSig  string
	gcache     AuthData
	Log        *slog.Logger
}

// NewStore binds a store to the auth.json of the given icebath home
func NewStore(h string) *Store {
	if h == "" {
		h = home.Process()
	}
	s := &Store{home: h, path: home.AuthPath(h), Log: pkgLog()}
	root := home.RootOf(h)
	if !samePath(root, h) {
		s.globalPath = home.AuthPath(root)
	}
	return s
}

func (s *Store) Home() string { return s.home }

func (s *Store) Path() string { return s.path }

// GlobalPath is the global root auth.json or empty when the store is the global root itself
func (s *Store) GlobalPath() string { return s.globalPath }

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil {
		ra, _ = filepath.Abs(a)
	}
	if errB != nil {
		rb, _ = filepath.Abs(b)
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// SameAuthStore reports whether two paths name one store including hardlinks and bind mounts
func SameAuthStore(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

func resolvedKey(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return path
}

// Per path process lock that the holding goroutine may re-enter, a reentrant auth store lock
type storeLock struct {
	mu    sync.Mutex
	state sync.Mutex
	owner uint64
	depth int
}

var (
	storeLocksMu sync.Mutex
	storeLocks   = map[string]*storeLock{}
)

// Process wide lock of one resolved auth path shared by every Store instance
func (s *Store) processLock(path string) *storeLock {
	storeLocksMu.Lock()
	defer storeLocksMu.Unlock()
	key := resolvedKey(path)
	m, ok := storeLocks[key]
	if !ok {
		m = &storeLock{}
		storeLocks[key] = m
	}
	return m
}

// Numeric id of the calling goroutine from its stack header
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

// Re-enters the lock when this goroutine already holds it, returning the nested releaser
func (l *storeLock) reenter() (func(), bool) {
	gid := goroutineID()
	l.state.Lock()
	defer l.state.Unlock()
	if l.depth > 0 && l.owner == gid {
		l.depth++
		return func() {
			l.state.Lock()
			l.depth--
			l.state.Unlock()
		}, true
	}
	return nil, false
}

func (l *storeLock) acquire() {
	l.mu.Lock()
	l.state.Lock()
	l.owner, l.depth = goroutineID(), 1
	l.state.Unlock()
}

func (l *storeLock) release() {
	l.state.Lock()
	l.owner, l.depth = 0, 0
	l.state.Unlock()
	l.mu.Unlock()
}

// LockTimeoutError reports an auth store lock that could not be taken in time
type LockTimeoutError struct {
	// Lock waited for, the auth store lock when empty
	Lock     string
	LockPath string
	Hint     string
}

// Lead of the auth store lock timeout, read back from failure details by the TUI server
const AuthLockTimeoutLead = "Timed out waiting for auth store lock"

func (e *LockTimeoutError) Error() string {
	lead := AuthLockTimeoutLead
	if e.Lock != "" {
		lead = "Timed out waiting for " + e.Lock
	}
	msg := lead + " (" + e.LockPath + ")"
	if e.Hint != "" {
		return msg + "; " + e.Hint
	}
	return msg
}

func lockHolderHint(lockPath string) string {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(string(raw), "\ufeff"))
	if len(fields) == 0 {
		return ""
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	if !processAlive(pid) {
		return ""
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}

// fileLock takes the cross process advisory lock for an auth path and returns its releaser
func (s *Store) fileLock(authPath string, timeout time.Duration) (func(), error) {
	return s.takeFileLock(authPath, timeout, true)
}

// Lock holder busy reported by a lock attempt that does not wait
var errAuthLockBusy = errors.New("auth store lock is held")

// Takes the auth path lock, waiting up to timeout or failing at once with errAuthLockBusy when wait is off
func (s *Store) takeFileLock(authPath string, timeout time.Duration, wait bool) (func(), error) {
	lockPath := strings.TrimSuffix(authPath, filepath.Ext(authPath)) + ".lock"
	pl := s.processLock(authPath)
	if release, nested := pl.reenter(); nested {
		return release, nil
	}
	if wait {
		pl.acquire()
	} else if !pl.mu.TryLock() {
		return nil, errAuthLockBusy
	} else {
		pl.state.Lock()
		pl.owner, pl.depth = goroutineID(), 1
		pl.state.Unlock()
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		pl.release()
		return nil, err
	}
	f, err := openLockFile(lockPath)
	if err != nil {
		pl.release()
		return nil, err
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err := kernelLock(f, true)
		if err == nil {
			break
		}
		if !isLockContention(err) {
			f.Close()
			pl.release()
			return nil, err
		}
		if !wait {
			f.Close()
			pl.release()
			return nil, errAuthLockBusy
		}
		if time.Now().After(deadline) {
			f.Close()
			pl.release()
			return nil, &LockTimeoutError{LockPath: lockPath, Hint: lockHolderHint(lockPath)}
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.Truncate(0)
	f.Seek(0, io.SeekStart)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Sync()
	return func() {
		f.Truncate(0)
		f.Seek(0, io.SeekStart)
		if lockReleasePad != "" {
			f.WriteString(lockReleasePad)
		}
		f.Sync()
		kernelLock(f, false)
		f.Close()
		pl.release()
	}, nil
}

// LockPath takes the cross process lock of an explicit auth path like _auth_store_lock with target_path
func (s *Store) LockPath(authPath string, timeout time.Duration) (func(), error) {
	return s.fileLock(authPath, timeout)
}

// Lock takes the active store lock for one read modify write transaction
func (s *Store) Lock(timeout time.Duration) (func(), error) {
	return s.fileLock(s.path, timeout)
}

func emptyAuthData() AuthData {
	return AuthData{"version": float64(AuthStoreVersion), "providers": map[string]any{}}
}

// UnreadableStoreError is an auth.json in another format than the one this icebath reads
type UnreadableStoreError struct {
	Path    string
	Version any
}

func (e *UnreadableStoreError) Error() string {
	return fmt.Sprintf("%s is not in the format this icebath reads (format %v, expected %d). Move it aside and sign in again with `icebath auth add`.",
		e.Path, e.Version, AuthStoreVersion)
}

// LoadFile parses one auth.json, quarantining corrupt content and failing on unreadable files and on another format
func (s *Store) LoadFile(path string) (AuthData, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyAuthData(), nil
	}
	if err != nil {
		s.Log.Warn("auth: could not read store, leaving the store on disk untouched rather than degrading to an empty one", "path", path, "err", err)
		return nil, err
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		corrupt := strings.TrimSuffix(path, filepath.Ext(path)) + ".json.corrupt"
		preserved := copyFile(path, corrupt) == nil
		verb := "Corrupt file preserved at"
		if !preserved {
			verb = "A copy could NOT be preserved at"
		}
		s.Log.Warn("auth: failed to parse store, starting with empty store", "path", path, "err", err, "detail", verb+" "+corrupt)
		return emptyAuthData(), nil
	}
	obj, _ := doc.(map[string]any)
	if obj == nil {
		return emptyAuthData(), nil
	}
	if version, _ := anyFloat(obj["version"]); version != AuthStoreVersion {
		return nil, &UnreadableStoreError{Path: path, Version: obj["version"]}
	}
	_, hasProviders := obj["providers"].(map[string]any)
	_, hasPool := obj["credential_pool"].(map[string]any)
	if hasProviders || hasPool {
		if _, ok := obj["providers"]; !ok {
			obj["providers"] = map[string]any{}
		}
		return AuthData(obj), nil
	}
	return emptyAuthData(), nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, st.Mode().Perm())
}

// Load parses the active auth.json
func (s *Store) Load() (AuthData, error) { return s.LoadFile(s.path) }

func fileSignature(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

// LoadGlobal returns the global root store as a memoised read only fallback or an empty map
func (s *Store) LoadGlobal() AuthData {
	if s.globalPath == "" {
		return AuthData{}
	}
	if _, err := os.Stat(s.globalPath); err != nil {
		s.cacheMu.Lock()
		s.gcache = nil
		s.cacheMu.Unlock()
		return AuthData{}
	}
	key := resolvedKey(s.globalPath)
	sig := fileSignature(s.globalPath)
	s.cacheMu.Lock()
	if s.gcache != nil && s.gcacheKey == key && s.gcacheSig == sig {
		cached := s.gcache
		s.cacheMu.Unlock()
		return cached
	}
	s.cacheMu.Unlock()
	data, err := s.LoadFile(s.globalPath)
	if err != nil {
		s.cacheMu.Lock()
		s.gcache = nil
		s.cacheMu.Unlock()
		return AuthData{}
	}
	s.cacheMu.Lock()
	s.gcache, s.gcacheKey, s.gcacheSig = data, key, sig
	s.cacheMu.Unlock()
	return data
}

// Restricts the resolved parent to 0700 except the root, top level dirs and the install tree
func secureParentDir(target string) { home.SecureParentDir(target) }

// savePrivateJSON writes indented JSON at mode 0600 under a 0700 parent and fsyncs the directory
func savePrivateJSON(target string, data any) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	secureParentDir(target)
	raw, err := marshalIndented(data)
	if err != nil {
		return err
	}
	if err := config.AtomicWrite(target, raw, 0o600); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

func marshalIndented(data any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

// SaveTo stamps the store format, updated_at and the pool rows it drops, then atomically writes it to a specific path
func (s *Store) SaveTo(data AuthData, target string) error {
	data["version"] = float64(AuthStoreVersion)
	recordPoolTombstones(data, target)
	data["updated_at"] = utcNowISO()
	if err := savePrivateJSON(target, data); err != nil {
		return err
	}
	if !samePath(target, s.path) {
		s.cacheMu.Lock()
		s.gcache = nil
		s.cacheMu.Unlock()
	}
	return nil
}

// Save writes the document to the active store
func (s *Store) Save(data AuthData) error { return s.SaveTo(data, s.path) }

// section returns the named map replacing any missing or non map value in place
func section(data map[string]any, key string) map[string]any {
	m, ok := data[key].(map[string]any)
	if !ok {
		m = map[string]any{}
		data[key] = m
	}
	return m
}

// ProviderStateIn returns a shallow copy of one provider entry
func ProviderStateIn(data AuthData, providerID string) map[string]any {
	if data == nil {
		return nil
	}
	providers, ok := data["providers"].(map[string]any)
	if !ok {
		return nil
	}
	st, ok := providers[providerID].(map[string]any)
	if !ok {
		return nil
	}
	return cloneMap(st)
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ProviderStateWithSource returns provider state and the path it came from, profile first then global root
func (s *Store) ProviderStateWithSource(data AuthData, providerID string) (map[string]any, string) {
	if st := ProviderStateIn(data, providerID); st != nil {
		return st, s.path
	}
	if st := ProviderStateIn(s.LoadGlobal(), providerID); st != nil {
		return st, s.globalPath
	}
	return nil, ""
}

// ProviderState returns provider state with the global root fallback
func (s *Store) ProviderState(data AuthData, providerID string) map[string]any {
	st, _ := s.ProviderStateWithSource(data, providerID)
	return st
}

// StoreProviderState writes state under providers
func (s *Store) StoreProviderState(data AuthData, providerID string, state map[string]any) {
	section(data, "providers")[providerID] = state
}

// Update runs fn under the store lock and saves when fn reports a change
func (s *Store) Update(fn func(data AuthData) (bool, error)) error {
	unlock, err := s.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := s.Load()
	if err != nil {
		return err
	}
	changed, err := fn(data)
	if err != nil || !changed {
		return err
	}
	return s.Save(data)
}

func poolEntries(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func poolMap(data AuthData) map[string]any {
	m, _ := data["credential_pool"].(map[string]any)
	return m
}

// ReadCredentialPool returns one provider's pool rows with the global root as fallback when the profile has none
func (s *Store) ReadCredentialPool(providerID string) ([]map[string]any, error) {
	data, err := s.Load()
	if err != nil {
		return nil, err
	}
	if entries := poolEntries(poolMap(data)[providerID]); len(entries) > 0 {
		return entries, nil
	}
	return poolEntries(poolMap(s.LoadGlobal())[providerID]), nil
}

// ReadCredentialPools returns every provider's rows with per provider global fallback
func (s *Store) ReadCredentialPools() (map[string][]map[string]any, error) {
	data, err := s.Load()
	if err != nil {
		return nil, err
	}
	merged := map[string][]map[string]any{}
	for k, v := range poolMap(data) {
		merged[k] = poolEntries(v)
	}
	for k, v := range poolMap(s.LoadGlobal()) {
		ge := poolEntries(v)
		if len(ge) == 0 {
			continue
		}
		if len(merged[k]) == 0 {
			merged[k] = ge
		}
	}
	return merged, nil
}

var poolStatusFields = []string{
	"last_status", "last_status_at", "last_error_code", "last_error_reason", "last_error_message",
	"last_error_reset_at", "status_cleared_at",
}

var poolTokenGenerationFields = []string{
	"access_token", "refresh_token", "expires_at", "expires_at_ms", "expires_in", "obtained_at",
	"last_refresh", "agent_key", "agent_key_expires_at", "agent_key_expires_in", "agent_key_id",
	"agent_key_obtained_at", "agent_key_reused", "scope", "inference_base_url",
}

// TokenPair is the access and refresh token generation of one pool row
type TokenPair struct{ Access, Refresh any }

func credentialTokenPair(row map[string]any) TokenPair {
	if row == nil {
		return TokenPair{}
	}
	return TokenPair{Access: row["access_token"], Refresh: row["refresh_token"]}
}

func (p TokenPair) any() bool { return truthyAny(p.Access) || truthyAny(p.Refresh) }

func truthyAny(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func entryID(row map[string]any) string {
	id, _ := row["id"].(string)
	return id
}

func entryIDs(entries []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range entries {
		if id := entryID(e); id != "" {
			out[id] = e
		}
	}
	return out
}

// TokenPairsByID snapshots the token generation of every row including blank pairs for token less rows
func TokenPairsByID(entries []map[string]any) map[string]TokenPair {
	out := map[string]TokenPair{}
	for id, row := range entryIDs(entries) {
		out[id] = credentialTokenPair(row)
	}
	return out
}

func mergePoolRowGeneration(entry, disk map[string]any, providerID string, base *TokenPair, statusCleared bool) map[string]any {
	return keepDiskLoginMeta(mergePoolRowTokens(entry, disk, providerID, base, statusCleared), disk)
}

// Merged row with the disk row's login name, account and rotation flag, which only disk edits change
func keepDiskLoginMeta(merged, disk map[string]any) map[string]any {
	if disk == nil {
		return merged
	}
	var out map[string]any
	for _, key := range loginMetaKeys {
		value, held := disk[key]
		if !held || sameAnyValue(merged[key], value) {
			continue
		}
		if out == nil {
			out = cloneMap(merged)
		}
		out[key] = value
	}
	if out == nil {
		return merged
	}
	return out
}

func mergePoolRowTokens(entry, disk map[string]any, providerID string, base *TokenPair, statusCleared bool) map[string]any {
	mergeDisk := disk
	if statusCleared {
		mergeDisk = nil
	}
	diskPair := credentialTokenPair(disk)
	if base == nil || !diskPair.any() || (diskPair.Access == base.Access && diskPair.Refresh == base.Refresh) {
		return mergeDiskCooldownState(entry, mergeDisk, providerID)
	}
	merged := cloneMap(entry)
	take := func(fields []string) {
		for _, f := range fields {
			if v, ok := disk[f]; ok {
				merged[f] = v
			} else {
				delete(merged, f)
			}
		}
	}
	take(poolTokenGenerationFields)
	if !statusCleared && entry["last_status"] == StatusDead {
		take(append(append([]string{}, poolStatusFields...), "failure_reason"))
	}
	return mergeDiskCooldownState(merged, mergeDisk, providerID)
}

func mergeDiskCooldownState(entry, disk map[string]any, providerID string) map[string]any {
	if disk == nil {
		return entry
	}
	mergedCooldowns := MergeModelCooldowns(disk["model_cooldowns"], entry["model_cooldowns"])
	merged := entry
	if len(mergedCooldowns) > 0 {
		merged = cloneMap(entry)
		merged["model_cooldowns"] = cooldownsToAny(mergedCooldowns)
	}
	diskStatus := map[string]any{}
	for _, f := range poolStatusFields {
		diskStatus[f] = disk[f]
	}
	withDisk := func() map[string]any {
		out := cloneMap(merged)
		for k, v := range diskStatus {
			out[k] = v
		}
		return out
	}
	memTS, _ := ParseAbsoluteTimestamp(entry["last_status_at"])
	clearedTS, _ := ParseAbsoluteTimestamp(disk["status_cleared_at"])
	if (entry["last_status"] == StatusDead || entry["last_status"] == StatusExhausted) && clearedTS > memTS {
		return withDisk()
	}
	ds := disk["last_status"]
	if ds != StatusDead && ds != StatusExhausted {
		return merged
	}
	memAccess, _ := entry["access_token"].(string)
	diskAccess, _ := disk["access_token"].(string)
	if memAccess != "" && diskAccess != "" && memAccess != diskAccess {
		return entry
	}
	diskTS, _ := ParseAbsoluteTimestamp(disk["last_status_at"])
	if diskTS <= memTS {
		return merged
	}
	if ds == StatusExhausted {
		until, ok := exhaustedUntil(PooledCredentialFromMap(providerID, disk), false)
		if !ok || until <= nowEpoch() {
			return merged
		}
	}
	return withDisk()
}

func cooldownsToAny(m map[string]float64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// WriteCredentialPool persists one provider's rows merging concurrent disk rows and newer disk cooldowns, never writing back a removed login
func (s *Store) WriteCredentialPool(providerID string, entries []map[string]any, removedIDs, statusClearedIDs []string, tokenBases map[string]TokenPair) ([]map[string]any, error) {
	removed := map[string]bool{}
	for _, id := range removedIDs {
		if id != "" {
			removed[id] = true
		}
	}
	cleared := map[string]bool{}
	for _, id := range statusClearedIDs {
		if id != "" {
			cleared[id] = true
		}
	}
	unlock, err := s.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := s.Load()
	if err != nil {
		return nil, err
	}
	pool := section(data, "credential_pool")
	existing := poolEntries(pool[providerID])
	existingByID := entryIDs(existing)
	entries = withoutTombstoned(entries, s.tombstonedIDs(data, providerID), existingByID)
	merged := make([]map[string]any, 0, len(entries)+len(existing))
	newIDs := map[string]bool{}
	for _, e := range entries {
		sanitized := SanitizeBorrowedCredentialPayload(e)
		id := entryID(sanitized)
		if id != "" {
			newIDs[id] = true
		}
		var base *TokenPair
		if b, ok := tokenBases[id]; ok {
			bb := b
			base = &bb
		}
		merged = append(merged, mergePoolRowGeneration(sanitized, existingByID[id], providerID, base, cleared[id]))
	}
	for _, disk := range existing {
		id := entryID(disk)
		if id != "" && !newIDs[id] && !removed[id] {
			merged = append(merged, SanitizeBorrowedCredentialPayload(disk))
		}
	}
	list := make([]any, len(merged))
	for i, m := range merged {
		list[i] = m
	}
	pool[providerID] = list
	if err := s.Save(data); err != nil {
		return nil, err
	}
	return merged, nil
}

func suppressedSourceList(suppressed map[string]any, providerID string) ([]any, bool) {
	switch raw := suppressed[providerID].(type) {
	case []any:
		return raw, true
	case map[string]any:
		list := make([]any, 0, len(raw))
		for _, k := range sortedMapKeys(raw) {
			list = append(list, k)
		}
		suppressed[providerID] = list
		return list, true
	}
	return nil, false
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SuppressCredentialSource records that a credential source must not be reseeded
func (s *Store) SuppressCredentialSource(providerID, source string) error {
	return s.Update(func(data AuthData) (bool, error) {
		suppressed := section(data, "suppressed_sources")
		list, ok := suppressedSourceList(suppressed, providerID)
		if !ok {
			list = []any{}
		}
		for _, v := range list {
			if v == source {
				suppressed[providerID] = list
				return true, nil
			}
		}
		suppressed[providerID] = append(list, source)
		return true, nil
	})
}

// IsSourceSuppressed reports whether the user suppressed a credential source
func (s *Store) IsSourceSuppressed(providerID, source string) bool {
	return s.SuppressedCredentialSources(providerID)[source]
}

// SuppressedCredentialSources returns every suppressed source for a provider
func (s *Store) SuppressedCredentialSources(providerID string) map[string]bool {
	out := map[string]bool{}
	data, err := s.Load()
	if err != nil {
		return out
	}
	sec, _ := data["suppressed_sources"].(map[string]any)
	switch listed := sec[providerID].(type) {
	case []any:
		for _, v := range listed {
			out[stringifyAny(v)] = true
		}
	case map[string]any:
		for k := range listed {
			out[k] = true
		}
	}
	return out
}

// UnsuppressCredentialSource clears a suppression marker and reports whether one existed
func (s *Store) UnsuppressCredentialSource(providerID, source string) (bool, error) {
	removed := false
	err := s.Update(func(data AuthData) (bool, error) {
		suppressed, ok := data["suppressed_sources"].(map[string]any)
		if !ok {
			return false, nil
		}
		list, ok := suppressedSourceList(suppressed, providerID)
		if !ok {
			return false, nil
		}
		idx := -1
		for i, v := range list {
			if v == source {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false, nil
		}
		list = append(list[:idx:idx], list[idx+1:]...)
		if len(list) == 0 {
			delete(suppressed, providerID)
		} else {
			suppressed[providerID] = list
		}
		if len(suppressed) == 0 {
			delete(data, "suppressed_sources")
		}
		removed = true
		return true, nil
	})
	return removed, err
}

// GetProviderAuthState returns persisted provider state profile first then global root
func (s *Store) GetProviderAuthState(providerID string) (map[string]any, error) {
	data, err := s.Load()
	if err != nil {
		return nil, err
	}
	return s.ProviderState(data, providerID), nil
}

// ClearProviderAuth deletes the provider state and the logins of one provider and reports whether anything changed
func (s *Store) ClearProviderAuth(providerID string) (bool, error) {
	cleared := false
	if providerID == "" {
		return false, nil
	}
	err := s.Update(func(data AuthData) (bool, error) {
		for _, sec := range []string{"providers", "credential_pool"} {
			entries := section(data, sec)
			if _, ok := entries[providerID]; ok {
				delete(entries, providerID)
				cleared = true
			}
		}
		return cleared, nil
	})
	return cleared, err
}

// lastAuthErrorMarker builds the record persisted when dead OAuth material is quarantined
func lastAuthErrorMarker(provider string, err *AuthError, reason, defaultCode string) map[string]any {
	code := err.Code
	if defaultCode != "" && code == "" {
		code = defaultCode
	}
	var codeAny any
	if code != "" || defaultCode != "" {
		codeAny = code
	}
	return map[string]any{
		"provider": provider, "message": err.Error(), "reason": reason, "relogin_required": true,
		"code": codeAny, "at": utcNowISO(),
	}
}

var flatOAuthTokenKeys = []string{"access_token", "refresh_token", "expires_at", "expires_in", "obtained_at"}

// quarantineFlatOAuthState strips dead tokens after a terminal refresh failure so later calls fail fast
func quarantineFlatOAuthState(state map[string]any, provider string, err *AuthError) {
	for _, k := range flatOAuthTokenKeys {
		delete(state, k)
	}
	state["last_auth_error"] = lastAuthErrorMarker(provider, err, "runtime_refresh_failure", "refresh_failed")
}

// coerceTTLSeconds converts any expires_in value into non negative whole seconds
func coerceTTLSeconds(v any) int {
	switch t := v.(type) {
	case float64:
		if t < 0 {
			return 0
		}
		return int(t)
	case int:
		if t < 0 {
			return 0
		}
		return t
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || n < 0 {
			return 0
		}
		return int(n)
	}
	return 0
}

// optionalBaseURL trims whitespace and trailing slashes returning empty for non strings
func optionalBaseURL(v any) string {
	s, _ := v.(string)
	return strings.TrimRight(strings.TrimSpace(s), "/")
}
