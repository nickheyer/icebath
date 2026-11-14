package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/vault"
)

const (
	AuthTypeAPIKey          = "api_key"
	AuthTypeOAuth           = "oauth"
	AuthTypeOAuthDeviceCode = "oauth_device_code"
	AuthTypeOAuthExternal   = "oauth_external"
	AuthTypeExternalProcess = "external_process"
	AuthTypeAWSSDK          = "aws_sdk"
)

type Directory interface {
	Spec(ctx context.Context, provider string) (SeedSpec, bool)
	Specs(ctx context.Context) []SeedSpec
	ClearModelsCache(ctx context.Context, provider string)
	InstallHint(ctx context.Context, provider string) string
}

type StoreOptions struct {
	Home      string
	Version   string
	Config    *config.Store
	Homes     *home.Resolver
	Keychain  *vault.Keychain
	HTTP      *http.Client
	Directory Directory
	ChildEnv  func(ctx context.Context) []string
}

type Store struct {
	home     string
	root     string
	authPath string
	rootAuth string
	cfg      *config.Store
	env      *config.Env
	homes    *home.Resolver
	keychain *vault.Keychain
	http     *http.Client
	dir      Directory
	childEnv func(ctx context.Context) []string

	version string

	mu                 sync.Mutex
	rootMemo           *rootMemo
	healMarks          map[string]string
	healNotices        []string
	noticed            bool
	refreshers         map[string]Refresher
	observers          map[string][]func(ctx context.Context, previous, next json.RawMessage) error
	statuses           map[string]func(ctx context.Context) (Status, error)
	codexProbes        map[string]codexProbe
	spentRotations     []string
	deadRefreshTokens  []string
	envIngestionWarned []string
}

type rootMemo struct {
	key   string
	store *doc
}

func Open(o StoreOptions) (*Store, error) {
	if strings.TrimSpace(o.Home) == "" {
		return nil, errors.New("auth: store needs a profile home")
	}
	if o.Config == nil || o.Homes == nil || o.HTTP == nil || o.Directory == nil || o.ChildEnv == nil || o.Version == "" {
		return nil, errors.New("auth: store needs config, home resolver, HTTP client, provider directory, child environment and product version")
	}
	root, err := home.RootOf(o.Home)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve the install root of %s: %w", o.Home, err)
	}
	keychain := o.Keychain
	if keychain == nil {
		keychain = vault.NewKeychain()
	}
	s := &Store{
		home:        o.Home,
		root:        root,
		authPath:    home.AuthFile.In(o.Home),
		cfg:         o.Config,
		env:         o.Config.Env(),
		homes:       o.Homes,
		keychain:    keychain,
		http:        o.HTTP,
		dir:         o.Directory,
		childEnv:    o.ChildEnv,
		healMarks:   map[string]string{},
		refreshers:  map[string]Refresher{},
		observers:   map[string][]func(ctx context.Context, previous, next json.RawMessage) error{},
		statuses:    map[string]func(ctx context.Context) (Status, error){},
		codexProbes: map[string]codexProbe{},
		version:     o.Version,
	}
	if !samePath(o.Home, root) {
		s.rootAuth = home.AuthFile.In(root)
	}
	return s, nil
}

func (s *Store) Home() string { return s.home }

func (s *Store) Path() string { return s.authPath }

func (s *Store) bind(ctx context.Context) context.Context { return home.WithOverride(ctx, s.home) }

func (s *Store) RegisterRefresher(provider string, r Refresher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshers[provider] = r
}

func (s *Store) refresher(provider string) Refresher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshers[provider]
}

func (s *Store) ObserveStateWrites(provider string, fn func(ctx context.Context, previous, next json.RawMessage) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers[provider] = append(s.observers[provider], fn)
}

func (s *Store) RegisterStatus(provider string, fn func(ctx context.Context) (Status, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[provider] = fn
}

func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs {
		return abs
	}
	return filepath.Join(resolvePath(dir), base)
}

func samePath(left, right string) bool { return resolvePath(left) == resolvePath(right) }

func sameStore(left, right string) bool {
	if samePath(left, right) {
		return true
	}
	a, err := os.Stat(left)
	if err != nil {
		return false
	}
	b, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

func lockPathOf(authPath string) string {
	return strings.TrimSuffix(authPath, filepath.Ext(authPath)) + ".lock"
}

func (s *Store) lock(ctx context.Context, authPath string, timeout time.Duration) (context.Context, func(), error) {
	if timeout < time.Second {
		timeout = time.Second
	}
	lockPath := lockPathOf(authPath)
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lk, err := fileutil.OpenLock(wait, lockPath, fileutil.RecordHolder(), fileutil.Range(0, 1),
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for auth store lock (%s)", lockPath)))
	if err != nil {
		return ctx, nil, err
	}
	return lk.Context(ctx), func() { _ = lk.Release() }, nil
}

func emptyStore() *doc {
	d := newDoc()
	d.set("version", intNum(StoreVersion))
	d.set("providers", newDoc())
	return d
}

func (s *Store) load(path string) (*doc, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return emptyStore(), nil
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("auth: could not read %s, leaving the store on disk untouched rather than degrading to an empty one", path), "error", err)
		return nil, err
	}
	v, perr := parseJSON(bytes.TrimPrefix(data, utf8BOM))
	if perr != nil {
		corrupt := home.AuthCorrupt.In(filepath.Dir(path))
		note := "Corrupt file preserved at"
		if cerr := fileutil.CopyFile(path, corrupt); cerr != nil {
			note = "A copy could NOT be preserved at"
			slog.Debug(fmt.Sprintf("auth: could not preserve a copy of the corrupt store at %s", corrupt), "error", cerr)
		}
		slog.Warn(fmt.Sprintf("auth: failed to parse %s (%v), starting with empty store. %s %s", path, perr, note, corrupt))
		return emptyStore(), nil
	}
	raw, _ := v.(*doc)
	if raw != nil && (raw.obj("providers") != nil || raw.obj("credential_pool") != nil) {
		if !raw.has("providers") {
			raw.set("providers", newDoc())
		}
		return raw, nil
	}
	if systems := raw.obj("systems"); systems != nil {
		store := emptyStore()
		if systems.has(ProviderPortal) {
			store.obj("providers").set(ProviderPortal, systems.value(ProviderPortal))
			store.set("active_provider", ProviderPortal)
		} else {
			store.set("active_provider", nil)
		}
		return store, nil
	}
	return emptyStore(), nil
}

func (s *Store) savePrivateJSON(ctx context.Context, target string, data any, syncDir bool) error {
	if err := s.homes.Mkdir(ctx, filepath.Dir(target)); err != nil {
		return err
	}
	if err := home.SecureParentDir(target); err != nil {
		return err
	}
	base := filepath.Base(target)
	opts := []fileutil.WriteOption{fileutil.Private(), fileutil.TempPrefix("." + strings.TrimSuffix(base, filepath.Ext(base)) + "_")}
	if syncDir {
		opts = append(opts, fileutil.SyncDir())
	}
	return fileutil.WriteJSON(ctx, target, data, fileutil.JSONStyle{Indent: 2}, opts...)
}

func (s *Store) save(ctx context.Context, store *doc, path string) error {
	store.set("version", intNum(StoreVersion))
	store.set("updated_at", isoStamp(time.Now()))
	if err := s.savePrivateJSON(ctx, path, store, true); err != nil {
		return err
	}
	if path != s.authPath {
		s.mu.Lock()
		s.rootMemo = nil
		s.mu.Unlock()
	}
	return nil
}

func (s *Store) loadRoot() *doc {
	if s.rootAuth == "" {
		return newDoc()
	}
	fi, err := os.Stat(s.rootAuth)
	if err != nil {
		s.mu.Lock()
		s.rootMemo = nil
		s.mu.Unlock()
		return newDoc()
	}
	ino, ctime := statIdentity(fi)
	key := fmt.Sprintf("%s|%d|%d|%d|%d", resolvePath(s.rootAuth), fi.ModTime().UnixNano(), fi.Size(), ino, ctime)
	settled := time.Since(fi.ModTime()) >= 2*time.Second
	s.mu.Lock()
	memo := s.rootMemo
	s.mu.Unlock()
	if settled && memo != nil && memo.key == key {
		return deepCopy(memo.store).(*doc)
	}
	store, err := s.load(s.rootAuth)
	if err != nil {
		s.mu.Lock()
		s.rootMemo = nil
		s.mu.Unlock()
		return newDoc()
	}
	s.mu.Lock()
	s.rootMemo = &rootMemo{key: key, store: store}
	s.mu.Unlock()
	return deepCopy(store).(*doc)
}

func providerStateIn(store *doc, provider string) *doc {
	return store.obj("providers").obj(provider).clone()
}

func (s *Store) stateWithSource(store *doc, provider string) (*doc, string) {
	if state := providerStateIn(store, provider); state != nil {
		return state, s.authPath
	}
	if state := providerStateIn(s.loadRoot(), provider); state != nil {
		return state, s.rootAuth
	}
	return nil, ""
}

func (s *Store) loadState(store *doc, provider string) *doc {
	state, _ := s.stateWithSource(store, provider)
	return state
}

type stateTx struct {
	s        *Store
	ctx      context.Context
	provider string
	store    *doc
	state    *doc
	source   string
}

func (s *Store) transact(ctx context.Context, provider string, timeout time.Duration, fn func(tx *stateTx) error) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, timeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	state, source := s.stateWithSource(store, provider)
	if source == "" || samePath(source, s.authPath) {
		return fn(&stateTx{s: s, ctx: ctx, provider: provider, store: store, state: state, source: source})
	}
	inner, unlockRoot, err := s.lock(ctx, source, timeout)
	if err != nil {
		return err
	}
	defer unlockRoot()
	rootStore, err := s.load(source)
	if err != nil {
		return err
	}
	return fn(&stateTx{s: s, ctx: inner, provider: provider, store: store, state: providerStateIn(rootStore, provider), source: source})
}

func (s *Store) noteStoreWrite(ctx context.Context, store *doc, provider string, state *doc) error {
	s.mu.Lock()
	fns := slices.Clone(s.observers[provider])
	s.mu.Unlock()
	if len(fns) == 0 {
		return nil
	}
	previous := rawOf(s.loadState(store, provider))
	next := rawOf(state)
	for _, fn := range fns {
		if err := fn(ctx, previous, next); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) storeProviderState(ctx context.Context, store *doc, provider string, state *doc, setActive bool) error {
	if err := s.noteStoreWrite(ctx, store, provider, state); err != nil {
		return err
	}
	store.section("providers").set(provider, state)
	if setActive {
		store.set("active_provider", provider)
	}
	return nil
}

func (s *Store) saveActiveState(ctx context.Context, provider string, state *doc) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	if err := s.storeProviderState(ctx, store, provider, state, true); err != nil {
		return err
	}
	return s.save(ctx, store, s.authPath)
}

func (s *Store) persistStateTo(ctx context.Context, provider string, state *doc, target string, setActive bool) error {
	ctx, unlock, err := s.lock(ctx, target, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(target)
	if err != nil {
		return err
	}
	if err := s.storeProviderState(ctx, store, provider, state.clone(), setActive); err != nil {
		return err
	}
	return s.save(ctx, store, target)
}

func (s *Store) saveStateToSource(ctx context.Context, store *doc, provider string, state *doc, source string) error {
	if source == "" || samePath(source, s.authPath) {
		if err := s.storeProviderState(ctx, store, provider, state, false); err != nil {
			return err
		}
		return s.save(ctx, store, s.authPath)
	}
	return s.persistStateTo(ctx, provider, state, source, false)
}

func (t *stateTx) save(state *doc) error {
	return t.s.saveStateToSource(t.ctx, t.store, t.provider, state, t.source)
}

func (t *stateTx) saveLocal(state *doc) error {
	if err := t.s.storeProviderState(t.ctx, t.store, t.provider, state, false); err != nil {
		return err
	}
	return t.s.save(t.ctx, t.store, t.s.authPath)
}

func (t *stateTx) saveActive(state *doc) error {
	if err := t.s.storeProviderState(t.ctx, t.store, t.provider, state, true); err != nil {
		return err
	}
	return t.s.save(t.ctx, t.store, t.s.authPath)
}

type Tx struct{ tx *stateTx }

func (t *Tx) Context() context.Context { return t.tx.ctx }

func (t *Tx) State() (json.RawMessage, bool) {
	if t.tx.state == nil {
		return nil, false
	}
	return rawOf(t.tx.state), true
}

func (t *Tx) Source() string { return t.tx.source }

func (t *Tx) FromRoot() bool {
	return t.tx.source != "" && !samePath(t.tx.source, t.tx.s.authPath)
}

func (t *Tx) Save(state json.RawMessage) error {
	d, err := parseObject(state)
	if err != nil {
		return err
	}
	return t.tx.save(d)
}

func (t *Tx) SaveLocal(state json.RawMessage) error {
	d, err := parseObject(state)
	if err != nil {
		return err
	}
	return t.tx.saveLocal(d)
}

func (t *Tx) SaveActive(state json.RawMessage) error {
	d, err := parseObject(state)
	if err != nil {
		return err
	}
	return t.tx.saveActive(d)
}

func (t *Tx) SaveWithActive(state json.RawMessage, active string) error {
	d, err := parseObject(state)
	if err != nil {
		return err
	}
	if err := t.tx.s.storeProviderState(t.tx.ctx, t.tx.store, t.tx.provider, d, true); err != nil {
		return err
	}
	if active != "" {
		t.tx.store.set("active_provider", active)
	} else {
		t.tx.store.del("active_provider")
	}
	return t.tx.s.save(t.tx.ctx, t.tx.store, t.tx.s.authPath)
}

func (t *Tx) DropPoolSources(provider string, sources ...string) bool {
	pool := t.tx.store.obj("credential_pool")
	entries, ok := pool.value(provider).([]any)
	if !ok {
		return false
	}
	retained := make([]any, 0, len(entries))
	for _, item := range entries {
		if row, ok := item.(*doc); ok && row != nil {
			if source, ok := row.value("source").(string); ok && slices.Contains(sources, source) {
				continue
			}
		}
		retained = append(retained, item)
	}
	if len(retained) == len(entries) {
		return false
	}
	pool.set(provider, retained)
	return true
}

func (s *Store) Transact(ctx context.Context, provider string, timeout time.Duration, fn func(tx *Tx) error) error {
	return s.transact(ctx, provider, timeout, func(tx *stateTx) error { return fn(&Tx{tx: tx}) })
}

func (s *Store) Update(ctx context.Context, provider string, fn func(state json.RawMessage) (json.RawMessage, error)) error {
	return s.Transact(ctx, provider, LockTimeout, func(tx *Tx) error {
		current, _ := tx.State()
		next, err := fn(current)
		if err != nil {
			return err
		}
		return tx.Save(next)
	})
}

func (s *Store) SaveActive(ctx context.Context, provider string, state json.RawMessage) error {
	d, err := parseObject(state)
	if err != nil {
		return err
	}
	return s.saveActiveState(ctx, provider, d)
}

func (s *Store) State(ctx context.Context, provider string) (json.RawMessage, bool, error) {
	state, err := s.providerState(ctx, provider)
	if err != nil || state == nil {
		return nil, false, err
	}
	return rawOf(state), true, nil
}

func (s *Store) providerState(_ context.Context, provider string) (*doc, error) {
	store, err := s.load(s.authPath)
	if err != nil {
		return nil, err
	}
	return s.loadState(store, provider), nil
}

func (s *Store) PortalTokenHasBillingScope(ctx context.Context) bool {
	state, err := s.providerState(ctx, ProviderPortal)
	if err != nil || state == nil {
		return false
	}
	scope, ok := state.value("scope").(string)
	return ok && slices.Contains(strings.Fields(scope), PortalBillingManageScope)
}

func (s *Store) Active(_ context.Context) (string, error) {
	store, err := s.load(s.authPath)
	if err != nil {
		return "", err
	}
	return store.str("active_provider"), nil
}

func (s *Store) MarkActiveIfUnset(ctx context.Context, provider string) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(store.str("active_provider")) != "" {
		return nil
	}
	store.set("active_provider", provider)
	return s.save(ctx, store, s.authPath)
}

func (s *Store) Clear(ctx context.Context, provider string) (bool, error) {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return false, err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return false, err
	}
	target := provider
	if target == "" {
		target = store.str("active_provider")
	}
	if target == "" {
		return false, nil
	}
	cleared := false
	for _, section := range []string{"providers", "credential_pool"} {
		if store.section(section).del(target) {
			cleared = true
		}
	}
	if store.str("active_provider") == target {
		store.set("active_provider", nil)
		cleared = true
	}
	if !cleared {
		return false, nil
	}
	return true, s.save(ctx, store, s.authPath)
}

func (s *Store) Deactivate(ctx context.Context) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	store.set("active_provider", nil)
	return s.save(ctx, store, s.authPath)
}

func (s *Store) RestoreActive(ctx context.Context, prior string) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	if prior != "" {
		store.set("active_provider", prior)
	} else {
		store.del("active_provider")
	}
	return s.save(ctx, store, s.authPath)
}

func (s *Store) IsKnownProvider(ctx context.Context, provider string) bool {
	_, ok := s.dir.Spec(s.bind(ctx), strings.ToLower(strings.TrimSpace(provider)))
	return ok
}

func (s *Store) ProviderDisplayName(ctx context.Context, provider string) string {
	if spec, ok := s.dir.Spec(s.bind(ctx), strings.ToLower(strings.TrimSpace(provider))); ok && spec.Name != "" {
		return spec.Name
	}
	return provider
}

func suppressedList(suppressed *doc, provider string) ([]any, bool) {
	switch raw := suppressed.value(provider).(type) {
	case []any:
		return raw, true
	case *doc:
		list := make([]any, 0, len(raw.keys))
		for _, k := range raw.keys {
			list = append(list, k)
		}
		suppressed.set(provider, list)
		return list, true
	}
	return nil, false
}

func listHas(list []any, value string) bool {
	return slices.ContainsFunc(list, func(v any) bool { s, ok := v.(string); return ok && s == value })
}

func (s *Store) Suppress(ctx context.Context, provider, source string) error {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	suppressed := store.section("suppressed_sources")
	list, _ := suppressedList(suppressed, provider)
	if !listHas(list, source) {
		list = append(list, source)
	}
	suppressed.set(provider, list)
	return s.save(ctx, store, s.authPath)
}

func (s *Store) IsSuppressed(_ context.Context, provider, source string) bool {
	store, err := s.load(s.authPath)
	if err != nil {
		return false
	}
	switch raw := store.obj("suppressed_sources").value(provider).(type) {
	case []any:
		return listHas(raw, source)
	case *doc:
		return raw.has(source)
	}
	return false
}

func (s *Store) Suppressed(_ context.Context, provider string) []string {
	store, err := s.load(s.authPath)
	if err != nil {
		return nil
	}
	switch raw := store.obj("suppressed_sources").value(provider).(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if str, ok := v.(string); ok {
				out = append(out, str)
			} else if text := textOf(v); text != "" {
				out = append(out, text)
			}
		}
		return out
	case *doc:
		return slices.Clone(raw.keys)
	}
	return nil
}

func (s *Store) Unsuppress(ctx context.Context, provider, source string) (bool, error) {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return false, err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return false, err
	}
	suppressed := store.obj("suppressed_sources")
	if suppressed == nil {
		return false, nil
	}
	list, ok := suppressedList(suppressed, provider)
	if !ok || !listHas(list, source) {
		return false, nil
	}
	list = slices.DeleteFunc(list, func(v any) bool { str, ok := v.(string); return ok && str == source })
	if len(list) == 0 {
		suppressed.del(provider)
	} else {
		suppressed.set(provider, list)
	}
	if suppressed.size() == 0 {
		store.del("suppressed_sources")
	}
	return true, s.save(ctx, store, s.authPath)
}
