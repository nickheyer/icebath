package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

const (
	maxPostprocess = 8
	maxPrepare     = 3
)

var (
	pollSeconds               = 300.0
	minPollSeconds            = 30.0
	BootRetryDelays           = []float64{10, 20}
	maxRetryWait              = 20.0
	sleep                     = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	request                   = Request
	complete                  = (*remoteBackend).completed
	stderr          io.Writer = os.Stderr
	profileIDRE               = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\n?$`)
)

func pollInterval() float64 { return max(pollSeconds, minPollSeconds) }

func jitteredPoll() time.Time {
	return time.Now().Add(time.Duration(pollInterval() * (0.9 + 0.2*rand.Float64()) * float64(time.Second)))
}

// Logs messages gathered while a backend lock was held, a debug prefix logging at debug level
func logNotes(notes []string) {
	for _, note := range notes {
		if rest, ok := strings.CutPrefix(note, "debug:"); ok {
			slog.Debug(rest)
		} else {
			slog.Warn(note)
		}
	}
}

// One failed fetch, its retry hints and the HTTP status when there was a response
type fetchFailed struct {
	detail        string
	retryable     bool
	retryAfter    float64
	hasRetryAfter bool
	status        int
}

func (e *fetchFailed) Error() string { return e.detail }

// In memory user layer of one profile home and the plane state it was installed from
type profileState struct {
	mu             sync.Mutex
	home, profile  string
	serverConfig   map[string]any
	doc            map[string]any
	etag           string
	profileVersion int
	locks          []Lock
	changedNS      int64
	gen, installed int
	base           map[string]any
	postprocessed  bool
	fetchedAt      time.Time
	lastError      string
	nextPoll       time.Time
}

// Diff base each profile had when one goroutine last read it, the map replaced and never mutated so a held one stays as it was
type readScope struct {
	seen map[*profileState]map[string]any
}

type bootingKey struct{}

func booting(ctx context.Context) map[string]bool {
	keys, _ := ctx.Value(bootingKey{}).(map[string]bool)
	return keys
}

func withBooting(ctx context.Context, key string) context.Context {
	keys := map[string]bool{key: true}
	for k := range booting(ctx) {
		keys[k] = true
	}
	return context.WithValue(ctx, bootingKey{}, keys)
}

var valueRefusals = map[[2]string]bool{
	{"400", "config_secret_literal"}: true, {"400", "config_value_invalid"}: true, {"400", "config_path_invalid"}: true,
	{"400", "config_path_reserved"}: true, {"413", "config_level_too_large"}: true, {"413", "config_body_too_large"}: true,
}

// Profile version a plane answer carries: an integer of any Go kind or a whole float within the int range; false for true, false and anything else
func profileVersionOf(v any) (int, bool) {
	if f, isFloat := v.(float64); isFloat {
		if f != math.Trunc(f) || f < math.MinInt || f >= math.MaxInt {
			return 0, false
		}
		return int(f), true
	}
	n, ok := anyutil.Int(v)
	if !ok || n < math.MinInt || n > math.MaxInt {
		return 0, false
	}
	return int(n), true
}

// What makes an effective config answer unusable, empty when it holds a config mapping, a whole number profileVersion and a lock list
func effectiveProblem(body map[string]any) string {
	if _, ok := body["config"].(map[string]any); !ok {
		return fmt.Sprintf("its config is %s where a mapping was expected", errchain.Kind(body["config"]))
	}
	if _, ok := profileVersionOf(body["profileVersion"]); !ok {
		return fmt.Sprintf("its profileVersion is %s where a whole number was expected", errchain.Kind(body["profileVersion"]))
	}
	if _, ok := body["locks"].([]any); !ok {
		return fmt.Sprintf("its locks are %s where a list was expected", errchain.Kind(body["locks"]))
	}
	return ""
}

// Whether an effective config answer holds a config mapping, a whole number profileVersion and a lock list
func effectiveOK(body map[string]any) bool { return effectiveProblem(body) == "" }

func setKey(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		config.AppendMappingKey(m, key)
	}
	m[key] = value
}

func cloneDoc(m map[string]any) map[string]any {
	out, _ := config.Clone(m).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// Remote config backend serving each profile user layer from the config plane
type remoteBackend struct {
	mu            sync.Mutex
	states        map[string]*profileState
	fetchLocks    map[string]*sync.Mutex
	pollerDone    chan struct{}
	stop          chan struct{}
	stopOnce      sync.Once
	managedWarned bool
	scopes        syncutil.GoroutineLocal[any]
}

func newRemoteBackend() config.ConfigBackend {
	return &remoteBackend{
		states: map[string]*profileState{}, fetchLocks: map[string]*sync.Mutex{}, stop: make(chan struct{}),
	}
}

func (b *remoteBackend) Name() string { return "remote" }

func profileFor(h string) (string, error) {
	name := home.ProfileNameFor(h)
	if name == "" || !profileIDRE.MatchString(name) {
		return "", &config.ConfigBackendUnavailable{Message: fmt.Sprintf("Remote Config: %s is not an Icebath profile home, so it has no remote config "+
			"(ICEBATH_CONFIG_BACKEND=remote serves profile homes only).", h)}
	}
	return name, nil
}

// Profile state of home, fetched once at first use with the fail closed boot retry
func (b *remoteBackend) state(ctx context.Context, h string) (*profileState, error) {
	key := home.Key(h)
	b.mu.Lock()
	st := b.states[key]
	b.mu.Unlock()
	if st != nil {
		if !b.pollerAlive() {
			b.ensurePoller()
		}
		return st, nil
	}
	if booting(ctx)[key] {
		return nil, &config.ConfigBackendUnavailable{Message: fmt.Sprintf("Remote Config: a config read for %s re-entered the load of that profile's config "+
			"on the same thread (code on the boot path read config before it was loaded). This is "+
			"an Icebath bug; refusing the read instead of deadlocking.", h)}
	}
	b.mu.Lock()
	fetchLock := b.fetchLocks[key]
	if fetchLock == nil {
		fetchLock = &sync.Mutex{}
		b.fetchLocks[key] = fetchLock
	}
	b.mu.Unlock()
	st, err := b.bootFetch(withBooting(ctx, key), h, key, fetchLock)
	if err != nil {
		return nil, err
	}
	b.ensurePoller()
	return st, nil
}

func (b *remoteBackend) roster() []*profileState {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*profileState, 0, len(b.states))
	for _, st := range b.states {
		out = append(out, st)
	}
	return out
}

// Boot fetch with bounded retry that fails closed, the credential resolved before the fetch lock
func (b *remoteBackend) bootFetch(ctx context.Context, h, key string, fetchLock *sync.Mutex) (*profileState, error) {
	profile, err := profileFor(h)
	if err != nil {
		return nil, err
	}
	if InstanceID() == "" {
		return nil, &config.ConfigBackendUnavailable{Message: "Remote Config: " + InstanceEnv + " is not set, so Icebath cannot identify this agent " +
			"to the config plane. Icebath does not start without its remote config."}
	}
	delays := slices.Clone(BootRetryDelays)
	for {
		var notes []string
		st, err := func() (*profileState, error) {
			token, err := bearerForFetch(ctx, h)
			if err != nil {
				return nil, err
			}
			fetchLock.Lock()
			defer fetchLock.Unlock()
			b.mu.Lock()
			st := b.states[key]
			b.mu.Unlock()
			if st == nil {
				st = &profileState{home: h, profile: profile, serverConfig: map[string]any{}, doc: map[string]any{}}
				if _, err := b.fetchInto(ctx, st, false, token, &notes, false); err != nil {
					return nil, err
				}
				b.mu.Lock()
				b.states[key] = st
				b.mu.Unlock()
			}
			return st, nil
		}()
		logNotes(notes)
		if err == nil {
			return st, nil
		}
		var ff *fetchFailed
		if !errors.As(err, &ff) {
			return nil, err
		}
		if ff.retryable && len(delays) > 0 {
			wait := delays[0]
			if ff.hasRetryAfter && ff.retryAfter > wait {
				wait = ff.retryAfter
			}
			wait = min(wait, maxRetryWait)
			delays = delays[1:]
			slog.Warn(fmt.Sprintf("Remote Config: fetch for profile %s failed (%s); retrying in %.0fs", strconv.Quote(profile), ff, wait))
			sleep(wait)
			continue
		}
		return nil, &config.ConfigBackendUnavailable{Message: fmt.Sprintf("Remote Config: cannot load the config for profile %s from "+
			"%s: %s. Icebath does not start without its remote config "+
			"(ICEBATH_CONFIG_BACKEND=remote has no local fallback).", strconv.Quote(profile), ShownURL(), ff)}
	}
}

func bearerForFetch(ctx context.Context, h string) (string, error) {
	token, err := Bearer(ctx)
	if err != nil {
		var pce *PlaneCredentialError
		if errors.As(err, &pce) {
			return "", &fetchFailed{detail: err.Error(), retryable: pce.Retryable}
		}
		return "", err
	}
	return token, nil
}

// GET of the self route into st, true when the document changed, held when the caller holds st.mu
func (b *remoteBackend) fetchInto(ctx context.Context, st *profileState, conditional bool, token string, notes *[]string, held bool) (bool, error) {
	if notes == nil {
		var local []string
		defer func() { logNotes(local) }()
		notes = &local
	}
	lock := func() func() {
		if held {
			return func() {}
		}
		st.mu.Lock()
		return st.mu.Unlock
	}
	unlock := lock()
	started := st.installed
	etag := ""
	if conditional {
		etag = st.etag
	}
	unlock()
	resp, err := request(ctx, "GET", st.profile, etag, nil, token)
	if err != nil {
		var pce *PlaneCredentialError
		var te *TransportError
		switch {
		case errors.As(err, &pce):
			return false, &fetchFailed{detail: err.Error(), retryable: pce.Retryable}
		case errors.As(err, &te):
			return false, &fetchFailed{detail: err.Error(), retryable: true}
		}
		return false, err
	}
	if resp.Status == 304 && conditional {
		unlock := lock()
		st.fetchedAt, st.lastError = time.Now(), ""
		unlock()
		return false, nil
	}
	if resp.Status == 200 && effectiveOK(resp.Body) {
		unlock := lock()
		defer unlock()
		if st.installed != started {
			*notes = append(*notes, fmt.Sprintf("debug:Remote Config: dropped a fetch for profile %s that a write overtook", strconv.Quote(st.profile)))
			return false, nil
		}
		*notes = append(*notes, b.install(st, resp.Body, resp.ETag)...)
		return true, nil
	}
	if resp.Status == 200 {
		return false, &fetchFailed{detail: "the plane returned a malformed response: " + effectiveProblem(resp.Body), retryable: true}
	}
	return false, &fetchFailed{detail: fmt.Sprintf("HTTP %d %s: %s", resp.Status, resp.Error(), resp.Message()),
		retryable: resp.Status >= 500 || resp.Status == 429, retryAfter: resp.RetryAfter, hasRetryAfter: resp.HasRetryAfter, status: resp.Status}
}

// Installs a fetched document under st.mu and returns the warnings to log later
func (b *remoteBackend) install(st *profileState, body map[string]any, etag string) []string {
	var notes []string
	cfg := body["config"].(map[string]any)
	doc := cloneDoc(cfg)
	var locks []Lock
	rawLocks, _ := body["locks"].([]any)
	for _, entry := range rawLocks {
		lock, ok := decodeLock(entry)
		if !ok {
			notes = append(notes, "Remote Config: ignoring malformed lock entry "+anyutil.Quote(wireObject(entry)))
			continue
		}
		locks = append(locks, lock)
	}
	st.serverConfig = cfg
	st.doc = doc
	st.etag = etag
	if st.etag == "" {
		st.etag = truthyText(body["etag"])
	}
	st.profileVersion, _ = profileVersionOf(body["profileVersion"])
	st.locks = locks
	st.changedNS = time.Now().UnixNano()
	st.gen++
	st.installed++
	st.base = nil
	st.postprocessed = false
	st.fetchedAt, st.lastError = time.Now(), ""
	st.nextPoll = jitteredPoll()
	return notes
}

// Lock one entry of the plane's lock list names, false without a text path or a level key; a null level reads as the upper level
func decodeLock(entry any) (Lock, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return Lock{}, false
	}
	rawPath, ok := m["path"].(string)
	if !ok {
		return Lock{}, false
	}
	level, ok := m["level"]
	if !ok {
		return Lock{}, false
	}
	path, err := Decode(rawPath)
	if err != nil {
		return Lock{}, false
	}
	name := "upper"
	if level != nil {
		name = anyutil.String(level)
	}
	return Lock{Path: path, Level: name}, true
}

// Brings the published doc to its completed postprocess output
func (b *remoteBackend) completed(ctx context.Context, st *profileState) error {
	for range maxPostprocess {
		b.postprocess(ctx, st)
		st.mu.Lock()
		done := st.postprocessed
		st.mu.Unlock()
		if done {
			return nil
		}
	}
	return config.NewConfigWriteError("Remote Config: the profile's config kept changing while it was migrated in memory", "config_version_conflict")
}

// Marks the published doc completed
func (b *remoteBackend) postprocess(ctx context.Context, st *profileState) {
	st.mu.Lock()
	st.postprocessed = true
	st.mu.Unlock()
}

func publishLocked(st *profileState, doc, base map[string]any) {
	st.doc = doc
	st.base = base
	st.changedNS = time.Now().UnixNano()
	st.gen++
}

// Runs take under st.mu once the published doc is completed output or this context's own migration copy
func (b *remoteBackend) snapshot(ctx context.Context, st *profileState, take func()) error {
	for range maxPostprocess {
		if err := complete(b, ctx, st); err != nil {
			return err
		}
		st.mu.Lock()
		if st.postprocessed {
			take()
			st.mu.Unlock()
			return nil
		}
		st.mu.Unlock()
	}
	return config.NewConfigWriteError("Remote Config: the profile's config kept changing while it was read", "config_version_conflict")
}

// Read scope of the calling goroutine, each goroutine keeping its own the way each thread and task starts with its own context
func (b *remoteBackend) scope() *readScope {
	if held, ok := b.scopes.Get(); ok {
		return held.(*readScope)
	}
	s := &readScope{}
	b.scopes.Set(s)
	return s
}

// Records under st.mu the base the calling goroutine just read
func (b *remoteBackend) rememberRead(st *profileState) {
	s := b.scope()
	base := st.base
	if base == nil {
		base = st.serverConfig
	}
	seen := make(map[*profileState]map[string]any, len(s.seen)+1)
	for k, v := range s.seen {
		seen[k] = v
	}
	seen[st] = base
	s.seen = seen
}

// Base the calling goroutine last read st at, nil when it never read it
func (b *remoteBackend) readBaseFor(st *profileState) map[string]any {
	held := b.scope().seen[st]
	if held == nil {
		return nil
	}
	base := cloneDoc(held)
	return base
}

// Doc, version and locks of the profile user layer of home
func (b *remoteBackend) ReadUserLayer(ctx context.Context, h string) (config.UserLayer, error) {
	st, err := b.state(ctx, h)
	if err != nil {
		return config.UserLayer{}, err
	}
	var out config.UserLayer
	err = b.snapshot(ctx, st, func() {
		doc := st.doc
		b.rememberRead(st)
		out.Doc = cloneDoc(doc)
		out.Version = versionOf(ctx, st)
		out.Locks = make(map[string]string, len(st.locks))
		for _, l := range st.locks {
			out.Locks[Encode(l.Path)] = l.Level
		}
	})
	if err != nil {
		return config.UserLayer{}, err
	}
	out.Provenance = fmt.Sprintf("remote:%s profile=%s", ShownURL(), st.profile)
	return out, nil
}

func (b *remoteBackend) ReadUserDoc(ctx context.Context, h string) (any, error) {
	layer, err := b.ReadUserLayer(ctx, h)
	if err != nil {
		return nil, err
	}
	return layer.Doc, nil
}

// Readable once the layer of home is fetched, an unreachable plane failing as an unavailable backend
func (b *remoteBackend) ProbeReadable(ctx context.Context, h string) error {
	_, err := b.state(ctx, h)
	return err
}

func (b *remoteBackend) ReadUserDocReadonly(ctx context.Context, h string) (any, error) {
	st, err := b.state(ctx, h)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	err = b.snapshot(ctx, st, func() {
		b.rememberRead(st)
		doc = st.doc
	})
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// Version leading with a nanosecond time, a migration copy getting a negative serial lead of its own
func versionOf(ctx context.Context, st *profileState) []any {
	return []any{st.changedNS, st.etag, st.gen}
}

// Version of home, remembered as the calling goroutine's read base when the doc is completed output
func (b *remoteBackend) Version(ctx context.Context, h string) ([]any, error) {
	st, err := b.state(ctx, h)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.postprocessed {
		b.rememberRead(st)
	}
	return versionOf(ctx, st), nil
}

func (b *remoteBackend) CacheVersion(_ context.Context, _ string, version []any) ([]any, error) {
	return version, nil
}

// Always true since the plane resolves a document for every profile
func (b *remoteBackend) Exists(context.Context, string) (bool, error) { return true, nil }

// Level locking dotted or one of its ancestors
func (b *remoteBackend) Locked(ctx context.Context, h, dotted string) (string, bool, error) {
	path := FromDotted(dotted)
	st, err := b.state(ctx, h)
	if err != nil {
		return "", false, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.locks {
		if len(l.Path) <= len(path) && slices.Equal(path[:len(l.Path)], l.Path) {
			return l.Level, true, nil
		}
	}
	return "", false, nil
}

// Sends changes as one PATCH with CAS, re-reading and retrying once on a conflict
func (b *remoteBackend) WriteChanges(ctx context.Context, h string, changes config.Changes) error {
	st, err := b.state(ctx, h)
	if err != nil {
		return err
	}
	token, err := Bearer(ctx)
	if err != nil {
		var pce *PlaneCredentialError
		if errors.As(err, &pce) {
			return config.NewConfigWriteError("Remote Config write failed: "+err.Error(), "config_plane_unreachable")
		}
		return err
	}
	var notes []string
	defer func() { logNotes(notes) }()
	return b.sendChanges(ctx, st, changes, token, &notes)
}

// Key level edit a write means, fixed once against the doc the caller read
type intent struct {
	sets   *PathMap
	unsets []Path
	pinned []Path
}

func (b *remoteBackend) sendChanges(ctx context.Context, st *profileState, changes config.Changes, token string, notes *[]string) error {
	var in *intent
	for attempt := 1; attempt <= 2; attempt++ {
		retried := false
		for range maxPrepare {
			if err := complete(b, ctx, st); err != nil {
				return err
			}
			st.mu.Lock()
			if !st.postprocessed {
				st.mu.Unlock()
				continue
			}
			seen := st.installed
			if in == nil {
				var err error
				if in, err = b.intent(ctx, st, changes); err != nil {
					st.mu.Unlock()
					return err
				}
			}
			body, _, unsets, err := b.patchBody(st, in)
			if err != nil || body == nil {
				st.mu.Unlock()
				return err
			}
			if st.installed != seen {
				st.mu.Unlock()
				continue
			}
			resp, err := request(ctx, "PATCH", st.profile, "", body, token)
			if err != nil {
				st.mu.Unlock()
				var pce *PlaneCredentialError
				var te *TransportError
				if errors.As(err, &pce) || errors.As(err, &te) {
					return config.NewConfigWriteError("Remote Config write failed: "+err.Error(), "config_plane_unreachable")
				}
				return err
			}
			if resp.Status == 200 && effectiveOK(resp.Body) {
				*notes = append(*notes, b.install(st, resp.Body, resp.ETag)...)
				b.rememberRead(st)
				*notes = append(*notes, inheritedNotes(st, unsets)...)
				st.mu.Unlock()
				return nil
			}
			if resp.Status == 409 && attempt == 1 {
				_, err := b.fetchInto(ctx, st, false, token, notes, true)
				st.mu.Unlock()
				if err != nil {
					var ff *fetchFailed
					if errors.As(err, &ff) {
						return config.NewConfigWriteError("Remote Config write conflict and re-read failed: "+err.Error(), "config_version_conflict")
					}
					return err
				}
				retried = true
				break
			}
			st.mu.Unlock()
			return writeError(resp)
		}
		if !retried {
			return config.NewConfigWriteError("Remote Config write not sent: the profile's config kept changing "+
				"while the write was prepared", "config_version_conflict")
		}
	}
	return nil
}

func diffBase(st *profileState) map[string]any {
	base := st.base
	if base == nil {
		base = st.serverConfig
	}
	out := cloneDoc(base)
	return out
}

func lockedError(lock Lock) error {
	path := Encode(lock.Path)
	return config.NewConfigLockedError(path, lock.Level, fmt.Sprintf("%s is locked by the %s level of Remote Config", path, lock.Level))
}

// Key level edit changes makes to the last read doc, plus the leaf paths a keyed set or pin names
func (b *remoteBackend) intent(ctx context.Context, st *profileState, changes config.Changes) (*intent, error) {
	base := diffBase(st)
	var next map[string]any
	if changes.Document != nil {
		if read := b.readBaseFor(st); read != nil {
			base = read
		}
		wire, err := ToWire(config.Clone(changes.Document), nil)
		if err != nil {
			return nil, err
		}
		next = wire.(map[string]any)
	} else {
		next = cloneDoc(base)
	}
	for _, kv := range changes.Set {
		path := FromDotted(kv.Key)
		wire, err := ToWire(kv.Value, path)
		if err != nil {
			return nil, err
		}
		setPath(next, path, wire)
	}
	for _, key := range changes.Unset {
		popPath(next, FromDotted(key))
	}
	if changes.Document != nil {
		var dropped []Path
		base, next, dropped = StripLocked(base, next, st.locks)
		for _, prefix := range config.RefusedLockedOmissionPrefixes(ctx) {
			whole := FromDotted(prefix)
			for _, path := range dropped {
				if Covers(whole, path) || Covers(path, whole) {
					level := "upper"
					for _, l := range st.locks {
						if slices.Equal(l.Path, path) {
							level = l.Level
							break
						}
					}
					return nil, lockedError(Lock{Path: path, Level: level})
				}
			}
		}
		if len(dropped) > 0 {
			names := make([]string, len(dropped))
			for i, p := range dropped {
				names[i] = Encode(p)
			}
			sort.Strings(names)
			fmt.Fprintf(stderr, "Note: %d setting(s) locked by Remote Config were not saved: %s\n", len(dropped), strings.Join(names, ", "))
		}
	}
	sets, unsets := IntentDiff(base, next, nil)
	var pinned []Path
	pinnedSeen := map[string]bool{}
	var named []string
	for _, kv := range changes.Set {
		named = append(named, kv.Key)
	}
	if changes.Document != nil {
		named = append(named, config.PinnedKeys(ctx)...)
	}
	for _, key := range named {
		path := FromDotted(key)
		wire, found := getPath(next, path)
		if !found {
			continue
		}
		leaves := &PathMap{}
		if m, ok := wire.(map[string]any); ok && len(m) > 0 {
			leaves, _ = IntentDiff(map[string]any{}, m, path)
		} else {
			leaves.Set(path, wire)
		}
		for _, leaf := range leaves.Entries() {
			sets.SetDefault(leaf.Path, leaf.Value)
			if k := pathKey(leaf.Path); !pinnedSeen[k] {
				pinnedSeen[k] = true
				pinned = append(pinned, leaf.Path)
			}
		}
	}
	return &intent{sets: sets, unsets: unsets, pinned: pinned}, nil
}

// PATCH body applying the intent to the current doc, nil when that is no change
func (b *remoteBackend) patchBody(st *profileState, in *intent) (*jsonutil.Object, *PathMap, []Path, error) {
	base := diffBase(st)
	next := cloneDoc(base)
	ApplyIntent(next, in.sets, in.unsets)
	sets, unsets := Diff(base, next, nil)
	for _, path := range in.pinned {
		value, _ := in.sets.Get(path)
		if m, ok := value.(map[string]any); ok && len(m) == 0 {
			continue
		}
		if _, locked := CoveringLock(st.locks, path); sets.Has(path) || locked {
			continue
		}
		if slices.ContainsFunc(sets.Entries(), func(e PathValue) bool { return Covers(e.Path, path) }) {
			continue
		}
		sets.Set(path, config.Clone(value))
	}
	if lock, refused := WriteCheck(sets, unsets, st.locks); refused {
		return nil, nil, nil, lockedError(lock)
	}
	for _, e := range sets.Entries() {
		if bad, ok := SecretLiteralPath(e.Path, e.Value); ok {
			return nil, nil, nil, config.NewConfigValueError(Encode(bad)+" is a secret-shaped key: Remote Config stores only a ${VAR} reference "+
				"there, never the secret itself. Put the value in .env and set a ${VAR} reference.", "config_secret_literal")
		}
	}
	if sets.Len() == 0 && len(unsets) == 0 {
		return nil, nil, nil, nil
	}
	setBody, unsetBody := EncodeChanges(sets, unsets)
	body := jsonutil.NewObject()
	body.Set("expectedVersion", st.profileVersion)
	if setBody.Len() > 0 {
		body.Set("set", setBody)
	}
	if len(unsetBody) > 0 {
		unsetList := make([]any, len(unsetBody))
		for i, u := range unsetBody {
			unsetList[i] = u
		}
		body.Set("unset", unsetList)
	}
	return body, sets, unsets, nil
}

// Write refusal for a plane answer, a lock or value refusal keeping the plane code
func writeError(resp Response) error {
	if resp.Status == 403 && resp.Error() == "config_key_locked" {
		path := truthyText(resp.Body["path"])
		lockedBy := truthyText(resp.Body["lockedBy"])
		if lockedBy == "" {
			lockedBy = "upper"
		}
		return config.NewConfigLockedError(path, lockedBy, "Remote Config refused the write: "+resp.Message())
	}
	if valueRefusals[[2]string{strconv.Itoa(resp.Status), resp.Error()}] {
		detail := resp.Message()
		var extras []string
		for _, kv := range [][2]string{{"path", "path"}, {"reason", "reason"}} {
			if v, ok := resp.Body[kv[1]].(string); ok && v != "" && !strings.Contains(detail, v) {
				extras = append(extras, kv[0]+" "+v)
			}
		}
		if len(extras) > 0 {
			detail = fmt.Sprintf("%s (%s)", detail, strings.Join(extras, ", "))
		}
		return config.NewConfigValueError("Remote Config refused the write: "+detail, resp.Error())
	}
	return config.NewConfigWriteError(fmt.Sprintf("Remote Config refused the write (%d %s): %s", resp.Status, resp.Error(), resp.Message()), resp.Error())
}

// Warns for every unset path an upper level still supplies
func inheritedNotes(st *profileState, unsets []Path) []string {
	var notes []string
	for _, p := range unsets {
		if _, found := getPath(st.serverConfig, p); found {
			notes = append(notes, "Remote Config: "+Encode(p)+" is still set by an upper level; removing it from this "+
				"profile cannot remove an inherited value (set it to null for 'no value')")
		}
	}
	return notes
}

// Lands doc in the running migration copy, or publishes it with its diff base outside a migration
func (b *remoteBackend) ApplyInMemory(ctx context.Context, h string, doc map[string]any) error {
	st, err := b.state(ctx, h)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	d := cloneDoc(doc)
	base := cloneDoc(d)
	publishLocked(st, d, base)
	return nil
}

func (b *remoteBackend) SupportsFileTooling() bool { return false }

func (b *remoteBackend) HonorsManagedConfig() bool { return false }

func (b *remoteBackend) ProtectedEnvNames() []string { return slices.Clone(PlaneCredentialEnvNames) }

func (b *remoteBackend) DeploymentEnvNames() []string {
	return []string{config.BackendEnv, URLEnv, InstanceEnv}
}

func (b *remoteBackend) BootstrapEnvNames() []string { return slices.Clone(PlaneCredentialEnvNames) }

// Restores the read bases the calling goroutine held, so a guard read never becomes the base of a later save
func (b *remoteBackend) PreserveReadBase(context.Context) func() {
	s := b.scope()
	held := s.seen
	return func() {
		if held != nil {
			s.seen = held
		}
	}
}

func (b *remoteBackend) DoctorWarnings(context.Context, string) []config.DoctorWarning {
	managed := managedConfigFile()
	if managed == "" {
		return nil
	}
	return []config.DoctorWarning{{
		Message: managed + " exists but is ignored in remote config mode",
		Hint:    "(remove it)",
		Issue:   "Remove " + managed + ": remote config mode takes config and locks only from Remote Config",
	}}
}

// Fetches the layer of home failing closed and warns once about an ignored managed config file
func (b *remoteBackend) Boot(ctx context.Context, h string) error {
	if _, err := b.state(ctx, h); err != nil {
		return err
	}
	b.mu.Lock()
	first := !b.managedWarned
	b.managedWarned = true
	b.mu.Unlock()
	if first {
		if path := managedConfigFile(); path != "" {
			msg := "WARNING: " + path + " exists but is IGNORED: ICEBATH_CONFIG_BACKEND=remote takes config " +
				"and locks only from Remote Config (D18). Remove the file."
			slog.Warn(msg)
			fmt.Fprintln(stderr, msg)
		}
	}
	return nil
}

func (b *remoteBackend) Destination(_ context.Context, h string) (string, error) {
	profile, err := profileFor(h)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Remote Config (profile %s level)", strconv.Quote(profile)), nil
}

func (b *remoteBackend) Describe(_ context.Context, h string) string {
	b.mu.Lock()
	st := b.states[home.Key(h)]
	b.mu.Unlock()
	instance := InstanceID()
	if instance == "" {
		instance = "<unset>"
	}
	head := fmt.Sprintf("Remote Config %s (instance %s)", ShownURL(), instance)
	if st == nil {
		return head + ": profile not fetched yet"
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	age := -1
	if !st.fetchedAt.IsZero() {
		age = int(time.Since(st.fetchedAt).Seconds())
	}
	tail := ""
	if st.lastError != "" {
		tail = ", last poll failed: " + st.lastError
	}
	return fmt.Sprintf("%s: profile %s, profile level v%d, %d lock(s), fetched %ds ago%s",
		head, strconv.Quote(st.profile), st.profileVersion, len(st.locks), age, tail)
}

func (b *remoteBackend) pollerAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pollerDone == nil {
		return false
	}
	select {
	case <-b.pollerDone:
		return false
	default:
		return true
	}
}

// Starts the poll goroutine unless one is running
func (b *remoteBackend) ensurePoller() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pollerDone != nil {
		select {
		case <-b.pollerDone:
		default:
			return
		}
	}
	done := make(chan struct{})
	b.pollerDone = done
	go b.pollLoop(done)
}

func (b *remoteBackend) pollLoop(done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-b.stop:
			return
		default:
		}
		wake := b.pollTick()
		timer := time.NewTimer(max(time.Second, time.Until(wake)))
		select {
		case <-b.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Polls every due profile and returns when the next one is due, a panic retrying after the minimum interval
func (b *remoteBackend) pollTick() (wake time.Time) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Remote Config: poll loop error; retrying", "error", r)
			wake = time.Now().Add(time.Duration(minPollSeconds * float64(time.Second)))
		}
	}()
	now := time.Now()
	for _, st := range b.roster() {
		st.mu.Lock()
		due := !st.nextPoll.After(now)
		st.mu.Unlock()
		if due {
			b.pollOne(st)
		}
	}
	wake = now.Add(time.Duration(pollInterval() * float64(time.Second)))
	for i, st := range b.roster() {
		st.mu.Lock()
		if i == 0 || st.nextPoll.Before(wake) {
			wake = st.nextPoll
		}
		st.mu.Unlock()
	}
	return wake
}

// One conditional GET that never fails, an error keeping the in memory document until the next tick
func (b *remoteBackend) pollOne(st *profileState) bool {
	changed, err := b.fetchInto(context.Background(), st, true, "", nil, false)
	if err != nil {
		st.mu.Lock()
		st.lastError = err.Error()
		st.mu.Unlock()
		var ff *fetchFailed
		if errors.As(err, &ff) {
			msg := fmt.Sprintf("Remote Config: poll for profile %s failed (%s); keeping the loaded config", strconv.Quote(st.profile), err)
			if ff.status == 401 || ff.status == 403 {
				slog.Error(msg)
			} else {
				slog.Warn(msg)
			}
		} else {
			slog.Warn(fmt.Sprintf("Remote Config: poll for profile %s failed", strconv.Quote(st.profile)), "error", err)
		}
		changed = false
	}
	st.mu.Lock()
	st.nextPoll = jitteredPoll()
	version := st.profileVersion
	st.mu.Unlock()
	if changed {
		slog.Info(fmt.Sprintf("Remote Config: profile %s changed (profile level v%d)", strconv.Quote(st.profile), version))
	}
	return changed
}

// Polls every profile the backend holds once, installing each changed document
func (b *remoteBackend) PollAll() {
	for _, st := range b.roster() {
		b.pollOne(st)
	}
}

// Managed scope config.yaml when it exists, empty otherwise
func managedConfigFile() string {
	dir := config.ManagedDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// First path at or under path where the read back value differs from what was sent
func firstChange(path Path, sent any, found bool, got any) Path {
	if !found {
		return path
	}
	sentMap, sentIsMap := sent.(map[string]any)
	gotMap, gotIsMap := got.(map[string]any)
	if sentIsMap && gotIsMap {
		for _, key := range config.MappingKeys(sentMap) {
			value, in := gotMap[key]
			if changed := firstChange(extend(path, key), sentMap[key], in, value); changed != nil {
				return changed
			}
		}
		return nil
	}
	wire, err := ToWire(config.Clone(got), path)
	if err != nil || !JSONEqual(wire, sent) {
		return path
	}
	return nil
}
