// Remote Config backend serving each profile config from the config plane, polled in memory and written back with compare and swap
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

const Kind = "remote"

const (
	pollInterval   = 300 * time.Second
	maxRetryWait   = 20 * time.Second
	maxPostprocess = 8
	maxPrepare     = 3
	versionKey     = "_config_version"
)

var bootRetryDelays = []time.Duration{10 * time.Second, 20 * time.Second}

var _ config.Backend = (*Backend)(nil)

var (
	ErrUnattached = &config.BackendUnavailableError{Message: "Remote Config: the remote config backend is not attached to a config store"}
	ErrNoResolver = &config.BackendUnavailableError{Message: "Remote Config: the remote config backend has no home resolver"}
	errNoPortal   = errors.New("the remote config backend has no Icebath Portal credential")
)

type Options struct {
	Homes    *home.Resolver
	Portal   PortalCredential
	OnNotice func(ctx context.Context, text string)
}

type Backend struct {
	homes  *home.Resolver
	portal PortalCredential
	notice func(ctx context.Context, text string)
	store  atomic.Pointer[config.Store]

	mu            sync.Mutex
	states        map[string]*profileState
	fetchLocks    map[string]*sync.Mutex
	poller        sync.Once
	managedWarned atomic.Bool
	unknownMu     sync.Mutex
	unknownWarned map[string]bool
	serial        atomic.Int64

	clientMu sync.Mutex
	client   *http.Client

	idpMu    sync.Mutex
	idpCache map[idpKey]idpToken
}

type writerStamp struct {
	known bool
	null  bool
	value int64
}

type profileState struct {
	home    string
	profile string

	mu             sync.Mutex
	server         *yaml.Node
	doc            *yaml.Node
	etag           string
	profileVersion int64
	profileWriter  writerStamp
	otherWriters   []int64
	locks          []lockEntry
	changedNS      int64
	gen            int64
	installed      int64
	base           *yaml.Node
	postprocessed  bool
	fetchedAt      time.Time
	lastError      string
	nextPoll       time.Time
}

type private struct {
	st     *profileState
	serial int64
	mu     sync.Mutex
	doc    *yaml.Node
	gen    int64
}

type (
	privateKey  struct{}
	preserveKey struct{}
	bootingKey  struct{ key string }
)

type note struct {
	debug bool
	text  string
}

type fetchFailed struct {
	detail     string
	retryable  bool
	retryAfter time.Duration
	status     int
}

type intent struct {
	sets   changeSet
	unsets [][]string
	pinned [][]string
}

type migrationCheck struct {
	stored *yaml.Node
	after  int
}

func New(o Options) *Backend {
	return &Backend{homes: o.Homes, portal: o.Portal, notice: o.OnNotice, states: map[string]*profileState{},
		fetchLocks: map[string]*sync.Mutex{}, unknownWarned: map[string]bool{}}
}

func (b *Backend) Attach(s *config.Store) { b.store.Store(s) }

func (b *Backend) notify(ctx context.Context, text string) {
	if b.notice == nil {
		slog.Warn(text)
		return
	}
	b.notice(ctx, text)
}

func logNotes(notes []note) {
	for _, n := range notes {
		if n.debug {
			slog.Debug(n.text)
			continue
		}
		slog.Warn(n.text)
	}
}

func unavailable(msg string) error { return &config.BackendUnavailableError{Message: msg} }

func jitter() time.Duration {
	return time.Duration(float64(pollInterval) * (0.9 + 0.2*rand.Float64()))
}

func privateFor(ctx context.Context, st *profileState) *private {
	if m, ok := ctx.Value(privateKey{}).(*private); ok && m.st == st {
		return m
	}
	return nil
}

func docVersion(doc *yaml.Node) int {
	if v, ok := child(doc, versionKey); ok && v.Tag == "!!int" {
		if n, err := strconv.Atoi(v.Value); err == nil {
			return n
		}
	}
	return 0
}

func (b *Backend) key(h string) (string, error) {
	if b.homes == nil {
		return "", ErrNoResolver
	}
	key, err := b.homes.Key(h)
	if err != nil {
		return "", unavailable(fmt.Sprintf("Remote Config: cannot resolve the profile home %s: %v", h, err))
	}
	return key, nil
}

func profileFor(h string) (string, error) {
	name, ok := home.ProfileName(h)
	if !ok || name == "" || !(name == home.DefaultProfile || home.ValidProfileID(name)) {
		return "", unavailable(fmt.Sprintf("Remote Config: %s is not an icebath profile home, so it has no remote config "+
			"(%s=remote serves profile homes only).", h, config.EnvConfigBackend))
	}
	return name, nil
}

func (b *Backend) state(ctx context.Context, h string) (*profileState, error) {
	key, err := b.key(h)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	st := b.states[key]
	b.mu.Unlock()
	if st != nil {
		b.startPoller()
		return st, nil
	}
	if ctx.Value(bootingKey{key}) != nil {
		return nil, unavailable(fmt.Sprintf("Remote Config: a config read for %s re-entered the load of that profile's config "+
			"on the same thread (code on the boot path read config before it was loaded). This is an icebath bug; "+
			"refusing the read instead of deadlocking.", h))
	}
	b.mu.Lock()
	fl := b.fetchLocks[key]
	if fl == nil {
		fl = &sync.Mutex{}
		b.fetchLocks[key] = fl
	}
	b.mu.Unlock()
	st, err = b.bootFetch(context.WithValue(ctx, bootingKey{key}, true), h, key, fl)
	if err != nil {
		return nil, err
	}
	b.startPoller()
	return st, nil
}

func (b *Backend) roster() []*profileState {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*profileState, 0, len(b.states))
	for _, st := range b.states {
		out = append(out, st)
	}
	return out
}

func (b *Backend) fetchBearer(ctx context.Context, h string) (string, *fetchFailed) {
	token, err := b.bearer(ctx, h)
	if err != nil {
		var ce *credentialError
		if errors.As(err, &ce) {
			return "", &fetchFailed{detail: ce.msg, retryable: ce.retryable}
		}
		return "", &fetchFailed{detail: err.Error(), retryable: true}
	}
	return token, nil
}

func (b *Backend) bootFetch(ctx context.Context, h, key string, fl *sync.Mutex) (*profileState, error) {
	profile, err := profileFor(h)
	if err != nil {
		return nil, err
	}
	if instanceID() == "" {
		return nil, unavailable(fmt.Sprintf("Remote Config: %s is not set, so icebath cannot identify this agent "+
			"to the config plane. icebath does not start without its remote config.", EnvInstance))
	}
	delays := slices.Clone(bootRetryDelays)
	for {
		var notes []note
		st, failure := b.bootAttempt(ctx, h, key, profile, fl, &notes)
		logNotes(notes)
		if failure == nil {
			return st, nil
		}
		final := unavailable(fmt.Sprintf("Remote Config: cannot load the config for profile %s from %s: %s. icebath does "+
			"not start without its remote config (%s=remote has no local fallback).", quote(profile), shownURL(),
			failure.detail, config.EnvConfigBackend))
		if !failure.retryable || len(delays) == 0 {
			return nil, final
		}
		wait := min(max(failure.retryAfter, delays[0]), maxRetryWait)
		delays = delays[1:]
		slog.Warn(fmt.Sprintf("Remote Config: fetch for profile %s failed (%s); retrying in %.0fs", quote(profile),
			failure.detail, wait.Seconds()))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, final
		case <-timer.C:
		}
	}
}

func (b *Backend) bootAttempt(ctx context.Context, h, key, profile string, fl *sync.Mutex, notes *[]note) (*profileState, *fetchFailed) {
	token, failure := b.fetchBearer(ctx, h)
	if failure != nil {
		return nil, failure
	}
	fl.Lock()
	defer fl.Unlock()
	b.mu.Lock()
	st := b.states[key]
	b.mu.Unlock()
	if st != nil {
		return st, nil
	}
	st = &profileState{home: h, profile: profile}
	if _, failure := b.fetchInto(ctx, st, false, token, notes); failure != nil {
		return nil, failure
	}
	b.mu.Lock()
	b.states[key] = st
	b.mu.Unlock()
	return st, nil
}

func effective(resp *response) (*yaml.Node, bool) {
	raw, ok := resp.body["config"]
	if !ok {
		return nil, false
	}
	cfg, err := config.WireNode(raw)
	if err != nil || !isMap(cfg) {
		return nil, false
	}
	if _, ok := rawInt(resp.body["profileVersion"]); !ok {
		return nil, false
	}
	var locks []json.RawMessage
	if json.Unmarshal(resp.body["locks"], &locks) != nil || locks == nil {
		return nil, false
	}
	return cfg, true
}

func (b *Backend) fetchInto(ctx context.Context, st *profileState, conditional bool, token string, notes *[]note) (bool, *fetchFailed) {
	st.mu.Lock()
	started, etag := st.installed, st.etag
	st.mu.Unlock()
	if !conditional {
		etag = ""
	}
	resp, err := b.request(ctx, http.MethodGet, st.home, st.profile, etag, nil, token)
	if err != nil {
		var ce *credentialError
		if errors.As(err, &ce) {
			return false, &fetchFailed{detail: ce.msg, retryable: ce.retryable}
		}
		return false, &fetchFailed{detail: err.Error(), retryable: true}
	}
	if resp.status == http.StatusNotModified && conditional {
		st.mu.Lock()
		st.fetchedAt, st.lastError = time.Now(), ""
		st.mu.Unlock()
		return false, nil
	}
	if cfg, ok := effective(resp); resp.status == http.StatusOK && ok {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.installed != started {
			*notes = append(*notes, note{debug: true, text: fmt.Sprintf("Remote Config: dropped a fetch for profile %s that a write overtook", quote(st.profile))})
			return false, nil
		}
		*notes = append(*notes, install(st, resp, cfg)...)
		return true, nil
	}
	if resp.status == http.StatusOK {
		return false, &fetchFailed{detail: "the plane returned a malformed response", retryable: true}
	}
	return false, &fetchFailed{detail: fmt.Sprintf("HTTP %d %s: %s", resp.status, resp.code(), resp.message()),
		retryable: resp.status >= 500 || resp.status == http.StatusTooManyRequests, retryAfter: resp.retryAfter, status: resp.status}
}

func levelsOf(resp *response) []map[string]json.RawMessage {
	var raws []json.RawMessage
	if json.Unmarshal(resp.body["levels"], &raws) != nil {
		return nil
	}
	var out []map[string]json.RawMessage
	for _, r := range raws {
		var lv map[string]json.RawMessage
		if json.Unmarshal(r, &lv) == nil && lv != nil {
			out = append(out, lv)
		}
	}
	return out
}

func install(st *profileState, resp *response, cfg *yaml.Node) []note {
	var notes []note
	doc := clone(cfg)
	dropChild(doc, versionKey)
	levels := levelsOf(resp)
	var writers, others []int64
	stamp := writerStamp{}
	sawProfile := false
	for _, lv := range levels {
		kind, _ := rawString(lv["kind"])
		w, isInt := rawInt(lv["writerConfigVersion"])
		if isInt {
			writers = append(writers, w)
			if kind != "profile" {
				others = append(others, w)
			}
		}
		if kind == "profile" && !sawProfile {
			sawProfile = true
			raw, present := lv["writerConfigVersion"]
			switch {
			case !present || strings.TrimSpace(string(raw)) == "null":
				stamp = writerStamp{known: true, null: true}
			case isInt:
				stamp = writerStamp{known: true, value: w}
			}
		}
	}
	version := int64(config.Version)
	if len(writers) > 0 {
		version = slices.Min(writers)
	}
	putChild(doc, versionKey, intValue(version))
	var locks []lockEntry
	var entries []json.RawMessage
	_ = json.Unmarshal(resp.body["locks"], &entries)
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		var p []string
		var err error
		path, isText := "", false
		if json.Unmarshal(raw, &entry) == nil && entry != nil {
			path, isText = rawString(entry["path"])
		}
		level, hasLevel := entry["level"]
		if isText && hasLevel {
			p, err = decodePath(path)
		}
		if !isText || !hasLevel || err != nil {
			notes = append(notes, note{text: "Remote Config: ignoring malformed lock entry " + strings.TrimSpace(string(raw))})
			continue
		}
		locks = append(locks, lockEntry{path: p, level: rawText(level)})
	}
	pv, _ := rawInt(resp.body["profileVersion"])
	st.server = cfg
	st.doc = doc
	st.etag = resp.etag
	if st.etag == "" {
		st.etag = resp.field("etag")
	}
	st.profileVersion = pv
	st.profileWriter = stamp
	st.otherWriters = others
	st.locks = locks
	st.changedNS = time.Now().UnixNano()
	st.gen++
	st.installed++
	st.base = nil
	st.postprocessed = false
	st.fetchedAt, st.lastError = time.Now(), ""
	st.nextPoll = time.Now().Add(jitter())
	return notes
}

func (b *Backend) completed(ctx context.Context, st *profileState) error {
	if privateFor(ctx, st) != nil {
		return nil
	}
	for range maxPostprocess {
		if err := b.postprocess(ctx, st); err != nil {
			return err
		}
		st.mu.Lock()
		done := st.postprocessed
		st.mu.Unlock()
		if done {
			return nil
		}
	}
	return &config.WriteError{Message: "Remote Config: the profile's config kept changing while it was migrated in memory",
		Code: "config_version_conflict"}
}

func (b *Backend) postprocess(ctx context.Context, st *profileState) error {
	if privateFor(ctx, st) != nil {
		return nil
	}
	st.mu.Lock()
	if st.postprocessed {
		st.mu.Unlock()
		return nil
	}
	seen, current, doc := st.installed, docVersion(st.doc), clone(st.doc)
	st.mu.Unlock()
	store := b.store.Load()
	if store == nil {
		return ErrUnattached
	}
	known, err := store.KnownRootKeys()
	if err != nil {
		return err
	}
	var unknown []string
	for _, k := range mapKeys(doc) {
		if k != versionKey && !slices.Contains(known, k) && !slices.Contains(unknown, k) {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		b.unknownMu.Lock()
		first := !b.unknownWarned[k]
		b.unknownWarned[k] = true
		b.unknownMu.Unlock()
		if first {
			slog.Warn(fmt.Sprintf("Remote Config: unknown config key %s is ignored by this icebath version", quote(k)))
		}
	}
	if config.SupportFloorVersion <= current && current < config.Version {
		return b.migrateInMemory(ctx, st, seen, current, doc)
	}
	if current < config.SupportFloorVersion {
		slog.Warn(fmt.Sprintf("Remote Config: profile %s was written by config version %d, below the migration floor %d; not migrated",
			quote(st.profile), current, config.SupportFloorVersion))
	}
	st.mu.Lock()
	if st.installed == seen {
		st.postprocessed = true
	}
	st.mu.Unlock()
	return nil
}

func (b *Backend) runPrivate(ctx context.Context, st *profileState, doc *yaml.Node, current int) (*yaml.Node, error) {
	store := b.store.Load()
	if store == nil {
		return nil, ErrUnattached
	}
	m := &private{st: st, doc: clone(doc), serial: b.serial.Add(1)}
	putChild(m.doc, versionKey, intValue(int64(current)))
	pctx := context.WithValue(home.WithOverride(ctx, st.home), privateKey{}, m)
	if _, err := store.MigrateInMemory(pctx, current, false); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doc, nil
}

func (b *Backend) migrateInMemory(ctx context.Context, st *profileState, started int64, current int, doc *yaml.Node) error {
	st.mu.Lock()
	stale := st.installed != started || st.postprocessed
	st.mu.Unlock()
	if stale {
		return nil
	}
	migrated, err := b.runPrivate(ctx, st, doc, current)
	if err != nil {
		return err
	}
	putChild(migrated, versionKey, intValue(int64(config.Version)))
	base := clone(migrated)
	dropChild(base, versionKey)
	st.mu.Lock()
	if st.installed != started || st.postprocessed {
		st.mu.Unlock()
		return nil
	}
	publishLocked(st, migrated, base)
	st.postprocessed = true
	st.mu.Unlock()
	slog.Info(fmt.Sprintf("Remote Config: migrated profile %s in memory from config version %d", quote(st.profile), current))
	return nil
}

func publishLocked(st *profileState, doc, base *yaml.Node) {
	st.doc = doc
	st.base = base
	st.changedNS = time.Now().UnixNano()
	st.gen++
}

func (b *Backend) snapshot(ctx context.Context, st *profileState, take func()) error {
	for range maxPostprocess {
		if err := b.completed(ctx, st); err != nil {
			return err
		}
		st.mu.Lock()
		if st.postprocessed || privateFor(ctx, st) != nil {
			take()
			st.mu.Unlock()
			return nil
		}
		st.mu.Unlock()
	}
	return &config.WriteError{Message: "Remote Config: the profile's config kept changing while it was read", Code: "config_version_conflict"}
}

func remember(ctx context.Context, st *profileState) {
	scope := config.ReadScopeOf(ctx)
	if scope == nil || (ctx.Value(preserveKey{}) != nil && !scope.Empty()) {
		return
	}
	base := st.base
	if base == nil {
		base = st.server
	}
	scope.Put(st, base)
}

func readBaseFor(ctx context.Context, st *profileState) *yaml.Node {
	scope := config.ReadScopeOf(ctx)
	if scope == nil {
		return nil
	}
	v, ok := scope.Get(st)
	held, _ := v.(*yaml.Node)
	if !ok || held == nil {
		return nil
	}
	base := clone(held)
	dropChild(base, versionKey)
	return base
}

func (b *Backend) Name() string { return Kind }

func (b *Backend) Read(ctx context.Context, h string) (*config.YAMLDocument, error) {
	st, err := b.state(ctx, h)
	if err != nil {
		return nil, err
	}
	var doc *config.YAMLDocument
	err = b.snapshot(ctx, st, func() {
		if m := privateFor(ctx, st); m != nil {
			m.mu.Lock()
			doc = config.DocumentOf(m.doc)
			m.mu.Unlock()
			return
		}
		remember(ctx, st)
		doc = config.DocumentOf(st.doc)
	})
	return doc, err
}

func (b *Backend) ReadReadonly(ctx context.Context, h string) (*config.YAMLDocument, error) {
	return b.Read(ctx, h)
}

func versionOf(st *profileState, m *private) config.Revision {
	if m != nil {
		m.mu.Lock()
		gen := m.gen
		m.mu.Unlock()
		return config.Revision{Stamp: -m.serial, Rest: fmt.Sprintf("%d|%d|%s|%d", gen, st.changedNS, st.etag, st.gen)}
	}
	return config.Revision{Stamp: st.changedNS, Rest: fmt.Sprintf("%s|%d", st.etag, st.gen)}
}

func (b *Backend) Version(ctx context.Context, h string) (config.Revision, error) {
	st, err := b.state(ctx, h)
	if err != nil {
		return config.Revision{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	m := privateFor(ctx, st)
	if m == nil && st.postprocessed {
		remember(ctx, st)
	}
	return versionOf(st, m), nil
}

func (b *Backend) CacheVersion(_ context.Context, _ string, v config.Revision) (config.Revision, error) {
	return v, nil
}

func (b *Backend) Exists(context.Context, string) (bool, error) { return true, nil }

func (b *Backend) ProbeReadable(ctx context.Context, h string) error {
	_, err := b.state(ctx, h)
	return err
}

func (b *Backend) Locked(ctx context.Context, h, dotted string) (string, bool) {
	st, err := b.state(ctx, h)
	if err != nil {
		return "", false
	}
	path := fromDotted(dotted)
	st.mu.Lock()
	defer st.mu.Unlock()
	if l, ok := coveringLock(st.locks, path); ok {
		return l.level, true
	}
	return "", false
}

func (b *Backend) SupportsFileTooling() bool { return false }

func (b *Backend) HonorsManagedConfig() bool { return false }

func (b *Backend) ProtectedEnvNames() []string { return credentialEnvNames() }

func (b *Backend) DeploymentEnvNames() []string {
	return []string{config.EnvConfigBackend, EnvURL, EnvInstance}
}

func (b *Backend) BootstrapEnvNames() []string { return credentialEnvNames() }

func (b *Backend) PreserveReadBase(ctx context.Context) context.Context {
	return context.WithValue(ctx, preserveKey{}, true)
}

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

func (b *Backend) DoctorWarnings(context.Context, string) []config.DoctorWarning {
	managed := managedConfigFile()
	if managed == "" {
		return nil
	}
	return []config.DoctorWarning{{
		Warning: managed + " exists but is ignored in remote config mode",
		Hint:    "(remove it)",
		Issue:   "Remove " + managed + ": remote config mode takes config and locks only from Remote Config",
	}}
}

func (b *Backend) Boot(ctx context.Context, h string) error {
	if _, err := b.state(ctx, h); err != nil {
		return err
	}
	if b.managedWarned.CompareAndSwap(false, true) {
		if path := managedConfigFile(); path != "" {
			msg := fmt.Sprintf("WARNING: %s exists but is IGNORED: %s=remote takes config and locks only from Remote Config "+
				"(D18). Remove the file.", path, config.EnvConfigBackend)
			slog.Warn(msg)
			b.notify(ctx, msg)
		}
	}
	return nil
}

func (b *Backend) Destination(h string) string {
	name, _ := home.ProfileName(h)
	return fmt.Sprintf("Remote Config (profile %s level)", quote(name))
}

func (b *Backend) Describe(_ context.Context, h string) string {
	id := instanceID()
	if id == "" {
		id = "<unset>"
	}
	head := fmt.Sprintf("Remote Config %s (instance %s)", shownURL(), id)
	var st *profileState
	if key, err := b.key(h); err == nil {
		b.mu.Lock()
		st = b.states[key]
		b.mu.Unlock()
	}
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
	return fmt.Sprintf("%s: profile %s, profile level v%d, %d lock(s), fetched %ds ago%s", head, quote(st.profile),
		st.profileVersion, len(st.locks), age, tail)
}

func (b *Backend) startPoller() {
	b.poller.Do(func() { go b.pollLoop() })
}

func (b *Backend) pollLoop() {
	for {
		now := time.Now()
		for _, st := range b.roster() {
			st.mu.Lock()
			due := !st.nextPoll.After(now)
			st.mu.Unlock()
			if due {
				b.pollOne(st)
			}
		}
		roster := b.roster()
		wake := now.Add(pollInterval)
		for i, st := range roster {
			st.mu.Lock()
			if i == 0 || st.nextPoll.Before(wake) {
				wake = st.nextPoll
			}
			st.mu.Unlock()
		}
		time.Sleep(max(time.Second, time.Until(wake)))
	}
}

func (b *Backend) pollOne(st *profileState) {
	ctx := home.WithOverride(context.Background(), st.home)
	var notes []note
	changed, failure := b.fetchInto(ctx, st, true, "", &notes)
	logNotes(notes)
	if failure != nil {
		st.mu.Lock()
		st.lastError = failure.detail
		st.mu.Unlock()
		msg := fmt.Sprintf("Remote Config: poll for profile %s failed (%s); keeping the loaded config", quote(st.profile), failure.detail)
		if failure.status == http.StatusUnauthorized || failure.status == http.StatusForbidden {
			slog.Error(msg)
		} else {
			slog.Warn(msg)
		}
	}
	st.mu.Lock()
	st.nextPoll = time.Now().Add(jitter())
	pv := st.profileVersion
	st.mu.Unlock()
	if changed {
		slog.Info(fmt.Sprintf("Remote Config: profile %s changed (profile level v%d)", quote(st.profile), pv))
	}
}
