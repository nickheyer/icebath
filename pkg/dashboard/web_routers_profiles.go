package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	profileReadWarnedMu sync.Mutex
	profileReadWarned   = map[[2]string]bool{}
)

// Logs a per profile session read failure once per profile and message
func warnProfileReadError(profile string, err error) {
	key := [2]string{profile, pyErrText(err)}
	profileReadWarnedMu.Lock()
	seen := profileReadWarned[key]
	profileReadWarned[key] = true
	profileReadWarnedMu.Unlock()
	if seen {
		return
	}
	webServerLog().Warn(fmt.Sprintf("profile session read failed for %s (reported only in the response errors array): %s",
		pyrepr.StrRepr(profile), pyErrText(err)))
}

// Python str of a raised error, the OSError text for a filesystem error and the bare message otherwise
func pyErrText(err error) string {
	text := pypath.OSErrorText(err)
	if text == err.Error() {
		return pyrepr.ExceptionMessage(err)
	}
	return text
}

// Shell command that configures a profile in the CLI
func profileSetupCommand(name string) (string, error) {
	if _, err := resolveProfileDir(name); err != nil {
		return "", err
	}
	if name == "default" {
		return "icebath setup", nil
	}
	return name + " setup", nil
}

// Path resolved strictly when possible, else made absolute
func resolvedPath(p string) string {
	if r, err := pypath.Resolve(p); err == nil {
		return r
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// Query name the config scope expects for a profile directory, empty for the process home
func scopeProfileName(path string) string {
	resolved := resolvedPath(path)
	if resolved == resolvedPath(home.Process()) {
		return ""
	}
	if resolved == resolvedPath(home.Root()) {
		return "default"
	}
	return filepath.Base(path)
}

// Writes the main model into a profile config after validating it against validateIn or the profile itself
func writeProfileModel(ctx context.Context, profileDir, providerName, model, validateIn, baseURL string) error {
	if validateIn == "" {
		validateIn = profileDir
	}
	vctx, _, err := configProfileScope(ctx, scopeProfileName(validateIn))
	if err != nil {
		return err
	}
	providerName, model = provider.NormalizeMainModelAssignment(vctx, providerName, model)
	cfg, err := webLoadConfig(vctx)
	if err != nil {
		return err
	}
	result, err := provider.ValidatedMainModelSelection(vctx, cfg, providerName, model, baseURL, "", nil)
	if err != nil {
		return asHTTPError(err)
	}
	wctx, _, err := configProfileScope(ctx, scopeProfileName(profileDir))
	if err != nil {
		return err
	}
	lctx, unlock := provider.LockConfigMutation(wctx)
	defer unlock()
	cfg, err = webLoadConfig(lctx)
	if err != nil {
		return err
	}
	current, held := cfg["model"]
	if !held {
		current = map[string]any{}
	}
	modelCfg, err := provider.ApplyMainModelAssignment(lctx, current, result, "")
	if err != nil {
		return asHTTPError(err)
	}
	cfg["model"] = modelCfg
	return webSaveConfig(config.RefusingLockedOmissions(lctx, "model"), cfg, config.SaveOptions{})
}

// Disables every installed skill of the profile outside keep and returns how many were newly disabled
func disableUnselectedSkills(ctx context.Context, profileDir string, keep []string) (int, error) {
	keepSet := map[string]bool{}
	for _, s := range keep {
		if t := pystr.Strip(s); t != "" {
			keepSet[t] = true
		}
	}
	hctx, unlock := provider.LockConfigMutation(home.WithOverride(ctx, profileDir))
	defer unlock()
	skillsRoot := filepath.Join(profileDir, "skills")
	var installed []string
	if isDirPath(skillsRoot) {
		err := filepath.WalkDir(skillsRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == skillsRoot {
					return err
				}
				return nil
			}
			if d.Name() == "SKILL.md" && p != skillsRoot {
				installed = append(installed, filepath.Base(filepath.Dir(p)))
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	cfg, err := webLoadConfig(hctx)
	if err != nil {
		return 0, err
	}
	disabled := skills.GetDisabledSkills(cfg, "")
	newly := 0
	for _, name := range installed {
		if !keepSet[name] && !disabled[name] {
			disabled[name] = true
			newly++
		}
	}
	if newly > 0 {
		if err := skills.SaveDisabledSkills(hctx, profileDir, cfg, disabled, ""); err != nil {
			return 0, err
		}
	}
	return newly, nil
}

// Python exception classes a profile route maps to an HTTP status
type profileExc int

const (
	excFileNotFound profileExc = iota
	excFileExists
	excValue
)

// Whether err is an instance of the Python class c stands for
func (c profileExc) matches(err error) bool {
	switch c {
	case excFileNotFound:
		return errors.Is(err, fs.ErrNotExist) || pypath.OSErrorClass(err) == "FileNotFoundError"
	case excFileExists:
		return errors.Is(err, fs.ErrExist)
	}
	switch pyrepr.ExceptionName(err) {
	case "ValueError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError", "JSONDecodeError", "EnvValueError":
		return true
	}
	return false
}

var (
	profileNotFoundDefault   = []profileExc{excFileNotFound}
	profileBadRequestDefault = []profileExc{excValue}
)

// HTTP error of a profile operation by exception class, settlement and HTTP errors raised as is, else a logged 500
func profileErrors(err error, logMsg string, notFound, badRequest []profileExc) error {
	if err == nil {
		return nil
	}
	if h, ok := asHTTPException(err); ok {
		return h
	}
	for _, c := range notFound {
		if c.matches(err) {
			return NewHTTPException(404, pyErrText(err))
		}
	}
	for _, c := range badRequest {
		if c.matches(err) {
			return NewHTTPException(400, pyErrText(err))
		}
	}
	var pending *manage.ProfileIdentitySettlementPending
	if errors.As(err, &pending) {
		return pending
	}
	webServerLog().Error(logMsg, "exc_info", err)
	return NewHTTPException(500, pyErrText(err))
}

// Result of fn or a logged default when it fails, the parent operation having already succeeded
func profileBestEffort[T any](logMsg string, def T, fn func() (T, error)) (out T) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Error(logMsg, "exc_info", fmt.Errorf("%v", r))
			out = def
		}
	}()
	v, err := fn()
	if err != nil {
		webServerLog().Error(logMsg, "exc_info", err)
		return def
	}
	return v
}

// Name and home of every served profile including standalone and parked ones, default alone when none enumerate
func profileTargets(ctx context.Context, logLabel string) ([]profiles.ServedProfile, error) {
	targets, err := profiles.ProfilesToServe(ctx, true, true, true)
	if err != nil {
		webServerLog().Error(logLabel+": profile enumeration failed", "exc_info", err)
		targets = nil
	}
	if len(targets) == 0 {
		dir, err := profiles.GetProfileDir("default")
		if err != nil {
			return nil, err
		}
		targets = append(targets, profiles.ServedProfile{Name: "default", Home: dir})
	}
	return targets, nil
}

// Every profile target or only the one profile names
func scopedProfileTargets(ctx context.Context, logLabel, profile string) ([]profiles.ServedProfile, error) {
	scope := profile
	if scope == "" {
		scope = "all"
	}
	if scope = pystr.Strip(scope); scope == "" {
		scope = "all"
	}
	targets, err := profileTargets(ctx, logLabel)
	if err != nil || scope == "all" {
		return targets, err
	}
	var out []profiles.ServedProfile
	for _, t := range targets {
		if t.Name == scope {
			out = append(out, t)
		}
	}
	return out, nil
}

// Stamps session rows with their profile, the 300 second active flag and boolean archive and pin flags
func tagRows(rows []*pyjson.Object, name string, now float64) ([]*pyjson.Object, error) {
	for _, s := range rows {
		s.Set("profile", name)
		s.Set("is_default_profile", name == "default")
		active, err := sessionRowIsActive(s, now)
		if err != nil {
			return nil, err
		}
		s.Set("is_active", active)
		s.Set("archived", pyrepr.Truthy(s.GetOr("archived", nil)))
		s.Set("pinned", pyrepr.Truthy(s.GetOr("pinned", nil)))
	}
	return rows, nil
}

// Rows offset to offset plus cap with every pinned row past the window appended
func pinnedWindow(rows []*pyjson.Object, offset, limit int) []*pyjson.Object {
	start := min(offset, len(rows))
	end := min(offset+limit, len(rows))
	window := append([]*pyjson.Object{}, rows[start:end]...)
	if len(rows) > offset+limit {
		seen := map[*pyjson.Object]bool{}
		for _, s := range window {
			seen[s] = true
		}
		for _, s := range rows[offset+limit:] {
			if pyrepr.Truthy(s.GetOr("pinned", nil)) && !seen[s] {
				window = append(window, s)
			}
		}
	}
	return window
}

// First truthy value of the keys, else int zero
func firstTruthy(get func(string) any, keys ...string) any {
	for _, k := range keys {
		if v := get(k); pyrepr.Truthy(v) {
			return v
		}
	}
	return int64(0)
}

// Recency sort value of a session row, last activity then start time
func recency(s *pyjson.Object) float64 {
	return counterFloat(firstTruthy(func(k string) any { return s.GetOr(k, nil) }, "last_active", "started_at"))
}

// Recency sort value of a project tree session map
func recencyMap(s map[string]any) float64 {
	return counterFloat(firstTruthy(func(k string) any { return s[k] }, "last_active", "started_at"))
}

// Error row of one failed profile read
func profileErrorRow(name string, err error) *pyjson.Object {
	return pyjson.ObjectOf("profile", name, "error", pyErrText(err))
}

// Result of fn against the profile's read only state.db, false when it is missing or the open or fn fails
func readProfileDB[T any](ctx context.Context, name, profileHome string, errs *[]*pyjson.Object, fn func(db *state.DB) (T, error)) (T, bool) {
	var zero T
	dbPath := filepath.Join(profileHome, "state.db")
	if !pathExistsFollow(dbPath) {
		return zero, false
	}
	fail := func(err error) (T, bool) {
		state.NoteStorageError(dbPath, err)
		warnProfileReadError(name, err)
		if errs != nil {
			*errs = append(*errs, profileErrorRow(name, err))
		}
		return zero, false
	}
	db, err := openSessionDBAtPath(ctx, dbPath, true)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	out, err := fn(db)
	if err != nil {
		return fail(err)
	}
	return out, true
}

// Whether a path exists with symlinks followed, the pathlib exists
func pathExistsFollow(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Profile to corrupt map of every scanned profile whose state.db is latched as structurally corrupt
func corruptProfileStores(targets []profiles.ServedProfile) *pyjson.Object {
	out := pyjson.NewObject()
	for _, t := range targets {
		if state.StorageState(filepath.Join(t.Home, "state.db")) == state.StorageCorrupt {
			out.Set(t.Name, state.StorageCorrupt)
		}
	}
	return out
}

// Sidebar scan cache bounds, the response TTL and the response and per profile entry caps
var sidebarCacheTTL = 5 * time.Second

const (
	sidebarCacheMaxEntries        = 32
	sidebarProfileCacheMaxEntries = 256
)

// Identity and mutation metadata of a file without opening it, empty when it cannot be stat'ed
func statFingerprint(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "None"
	}
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	dev, ino, err := pyfs.LstatIdentity(target)
	if err != nil {
		return "None"
	}
	return fmt.Sprintf("(%d, %d, %d, %d)", dev, ino, st.Size(), st.ModTime().UnixNano())
}

// Fingerprint of a SQLite store through its main file and WAL
func sidebarDBFingerprint(dbPath string) string {
	return statFingerprint(dbPath) + "|" + statFingerprint(dbPath+"-wal")
}

// Per profile sidebar slice cache key, the store path and fingerprint first
type sidebarProfileKey struct {
	dbPath, fingerprint, rest string
}

var (
	sidebarProfileCacheMu   sync.Mutex
	sidebarProfileCacheKeys []sidebarProfileKey
	sidebarProfileCacheVals = map[sidebarProfileKey]*pyjson.Object{}
)

// Moves a cached key to the most recent end of the LRU order
func sidebarProfileTouch(key sidebarProfileKey) {
	for i, k := range sidebarProfileCacheKeys {
		if k == key {
			sidebarProfileCacheKeys = append(sidebarProfileCacheKeys[:i], sidebarProfileCacheKeys[i+1:]...)
			break
		}
	}
	sidebarProfileCacheKeys = append(sidebarProfileCacheKeys, key)
}

// Copy of the cached slices of a key, nil on a miss
func sidebarProfileCacheGet(key sidebarProfileKey) *pyjson.Object {
	sidebarProfileCacheMu.Lock()
	defer sidebarProfileCacheMu.Unlock()
	value, ok := sidebarProfileCacheVals[key]
	if !ok || value == nil {
		return nil
	}
	sidebarProfileTouch(key)
	return pycopy.Of(value)
}

// Stores a copy of the slices, dropping older fingerprints of the same store and the least recent overflow
func sidebarProfileCachePut(key sidebarProfileKey, value *pyjson.Object) {
	snapshot := pycopy.Of(value)
	sidebarProfileCacheMu.Lock()
	defer sidebarProfileCacheMu.Unlock()
	kept := sidebarProfileCacheKeys[:0]
	for _, k := range sidebarProfileCacheKeys {
		if k.dbPath == key.dbPath && k.fingerprint != key.fingerprint {
			delete(sidebarProfileCacheVals, k)
			continue
		}
		kept = append(kept, k)
	}
	sidebarProfileCacheKeys = kept
	sidebarProfileCacheVals[key] = snapshot
	sidebarProfileTouch(key)
	for len(sidebarProfileCacheKeys) > sidebarProfileCacheMaxEntries {
		delete(sidebarProfileCacheVals, sidebarProfileCacheKeys[0])
		sidebarProfileCacheKeys = sidebarProfileCacheKeys[1:]
	}
}

// Empties the per profile sidebar slice cache
func sidebarProfileCacheClear() {
	sidebarProfileCacheMu.Lock()
	defer sidebarProfileCacheMu.Unlock()
	sidebarProfileCacheKeys = nil
	sidebarProfileCacheVals = map[sidebarProfileKey]*pyjson.Object{}
}

// Whether any sidebar slice holds rows
func sliceHasRows(slices *pyjson.Object) bool {
	for _, key := range []string{"recents", "cron", "messaging"} {
		if pyrepr.Truthy(slices.GetOr(key, nil)) {
			return true
		}
	}
	return false
}

// Copy of an error row
func copyErrorRow(err *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range err.Keys {
		out.Set(k, err.Vals[k])
	}
	return out
}

// Copies of the scan failures a retry can re-attempt, latched corrupt stores left out
func retryableProfileErrors(errs []*pyjson.Object, scanned []profiles.ServedProfile) []*pyjson.Object {
	corrupt := corruptProfileStores(scanned)
	out := []*pyjson.Object{}
	for _, e := range errs {
		if p, ok := e.GetOr("profile", nil).(string); ok {
			if _, bad := corrupt.Get(p); bad {
				continue
			}
		}
		out = append(out, copyErrorRow(e))
	}
	return out
}

// Copies of error rows as a JSON list
func errorRowList(errs []*pyjson.Object) []any {
	out := make([]any, len(errs))
	for i, e := range errs {
		out[i] = copyErrorRow(e)
	}
	return out
}

// Failed load slice without a sessions key, extra keys appended in order
func failedLoadSlice(errs []*pyjson.Object, extra ...any) *pyjson.Object {
	out := pyjson.ObjectOf("failed", true, "retry", true, "errors", errorRowList(errs))
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return out
}

// Failed load marker of each errored profile
func profilesFailed(errs []*pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, e := range errs {
		p, _ := e.GetOr("profile", nil).(string)
		if p == "" {
			continue
		}
		msg, ok := e.Get("error")
		if !ok {
			msg = ""
		}
		out.Set(p, pyjson.ObjectOf("failed", true, "retry", true, "error", msg))
	}
	return out
}

// Cached response with its expiry
type sidebarCacheItem struct {
	expires time.Time
	value   *pyjson.Object
}

// Single flight scan with a short TTL response cache, failed scans never cached
type sidebarSingleflight struct {
	fn        func(ctx context.Context, args ...any) (*pyjson.Object, error)
	cacheMu   sync.Mutex
	keys      []string
	items     map[string]sidebarCacheItem
	refreshMu sync.Mutex
}

// Single flight cache around a sidebar scan
func newSidebarSingleflight(fn func(ctx context.Context, args ...any) (*pyjson.Object, error)) *sidebarSingleflight {
	return &sidebarSingleflight{fn: fn, items: map[string]sidebarCacheItem{}}
}

// Moves a key to the most recent end of the cache order
func (s *sidebarSingleflight) touch(key string) {
	for i, k := range s.keys {
		if k == key {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			break
		}
	}
	s.keys = append(s.keys, key)
}

// Copy of a live cached response, false on a miss or an expired entry
func (s *sidebarSingleflight) lookup(key string) (*pyjson.Object, bool) {
	now := time.Now()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	item, ok := s.items[key]
	if !ok || !now.Before(item.expires) {
		delete(s.items, key)
		for i, k := range s.keys {
			if k == key {
				s.keys = append(s.keys[:i], s.keys[i+1:]...)
				break
			}
		}
		return nil, false
	}
	s.touch(key)
	return pycopy.Of(item.value), true
}

// Response for these arguments from cache or one scan at a time
func (s *sidebarSingleflight) call(ctx context.Context, args ...any) (*pyjson.Object, error) {
	ttl := sidebarCacheTTL
	if ttl <= 0 {
		return s.fn(ctx, args...)
	}
	key := freezeArgs(args)
	if cached, ok := s.lookup(key); ok {
		return cached, nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if cached, ok := s.lookup(key); ok {
		return cached, nil
	}
	result, err := s.fn(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result != nil && pyrepr.Truthy(result.GetOr("errors", nil)) {
		return result, nil
	}
	snapshot := pycopy.Of(result)
	s.cacheMu.Lock()
	s.items[key] = sidebarCacheItem{expires: time.Now().Add(ttl), value: snapshot}
	s.touch(key)
	for len(s.keys) > sidebarCacheMaxEntries {
		delete(s.items, s.keys[0])
		s.keys = s.keys[1:]
	}
	s.cacheMu.Unlock()
	return result, nil
}

// Rows of a list as a JSON list
func rowList(rows []*pyjson.Object) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// Stable descending sort of session rows by a numeric key
func sortRowsDesc(rows []*pyjson.Object, key func(*pyjson.Object) float64) {
	sort.SliceStable(rows, func(i, j int) bool { return key(rows[i]) > key(rows[j]) })
}
