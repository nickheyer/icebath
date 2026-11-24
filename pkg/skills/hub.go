package skills

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Seconds a shared index cache entry stays fresh
const IndexCacheTTL = 3600.0

const (
	maxSkillFetchRedirects = 5
	defaultHubHTTPTimeout  = 20 * time.Second
)

var hubRedirectStatus = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// Skills hub state paths and network seams of one scope, each override standing in for a patched module path
type Hub struct {
	S                                                                                                             *Scope
	HubDirOverride, LockFileOverride, QuarantineDirOverride, AuditLogOverride, TapsFileOverride, IndexDirOverride string
	io                                                                                                            hubIO
}

// Replaceable hub IO, nil fields run the real implementation
type hubIO struct {
	httpGet         func(ctx context.Context, rawURL string, o hubGet) (*httputil.Response, error)
	ssrfGet         func(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string) (*httputil.Response, error)
	guardedGet      func(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string) *httputil.Response
	isSafeURL       func(rawURL string) bool
	siteAccess      func(rawURL string) *tools.WebsiteBlock
	readIndexCache  func(key string) (any, error)
	writeIndexCache func(key string, data any) error
	streamClient    func(timeout time.Duration) *http.Client
	shippedIndex    func() any
}

// Options of one plain hub GET
type hubGet struct {
	Timeout         time.Duration
	Params          url.Values
	Headers         map[string]string
	FollowRedirects bool
}

// Hub over a scope with live path resolution
func NewHub(s *Scope) *Hub { return &Hub{S: s} }

func (h *Hub) toolsEnv() *tools.Env {
	if h.S.Env != nil {
		return h.S.Env
	}
	return &tools.Env{Home: h.S.Home, Config: h.S.Config, Secrets: h.S.Secrets, Platform: h.S.Platform, Cwd: h.S.Cwd, Log: h.S.Log}
}

func hubPathOr(override, parent, leaf string) string {
	if override != "" {
		return override
	}
	return filepath.Join(parent, leaf)
}

// Hub state directory under the skills dir
func (h *Hub) HubDir() string { return hubPathOr(h.HubDirOverride, h.S.SkillsDir(), ".hub") }

// Provenance lock file of installed hub skills
func (h *Hub) LockFilePath() string { return hubPathOr(h.LockFileOverride, h.HubDir(), "lock.json") }

// Directory downloaded bundles wait in before install
func (h *Hub) QuarantineDir() string {
	return hubPathOr(h.QuarantineDirOverride, h.HubDir(), "quarantine")
}

// Append only audit log of hub actions
func (h *Hub) AuditLogPath() string { return hubPathOr(h.AuditLogOverride, h.HubDir(), "audit.log") }

// Custom GitHub tap list file
func (h *Hub) TapsFilePath() string { return hubPathOr(h.TapsFileOverride, h.HubDir(), "taps.json") }

// Shared index cache directory of every adapter
func (h *Hub) IndexCacheDir() string { return hubPathOr(h.IndexDirOverride, h.HubDir(), "index-cache") }

type hubSessionKey struct{}

type hubSession struct{ noFollow, follow *http.Client }

// Context reusing one SSRF safe client for a single hub resolution, release closes it
func (h *Hub) HTTPSession(ctx context.Context) (context.Context, func()) {
	if ctx.Value(hubSessionKey{}) != nil {
		return ctx, func() {}
	}
	base := tools.NewSSRFSafeClient(h.toolsEnv(), 0, false)
	follow := *base
	follow.CheckRedirect = httputil.RedirectPolicy(true)
	sess := &hubSession{noFollow: base, follow: &follow}
	return context.WithValue(ctx, hubSessionKey{}, sess), base.CloseIdleConnections
}

// Request URL that cannot be built, raised past the transport error handling of every hub fetch helper
type hubInvalidURLError struct{ err error }

func (e *hubInvalidURLError) Error() string { return e.err.Error() }
func (e *hubInvalidURLError) Unwrap() error { return e.err }

func hubRequest(ctx context.Context, rawURL string, o hubGet) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httputil.MergeQuery(rawURL, o.Params), nil)
	if err != nil {
		return nil, &hubInvalidURLError{err}
	}
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// GET through the active session client, or a one shot plain request outside a session
func (h *Hub) skillsHubHTTPGet(ctx context.Context, rawURL string, o hubGet) (*httputil.Response, error) {
	if h.io.httpGet != nil {
		return h.io.httpGet(ctx, rawURL, o)
	}
	client := httputil.Client(o.FollowRedirects)
	if sess, ok := ctx.Value(hubSessionKey{}).(*hubSession); ok {
		client = sess.noFollow
		if o.FollowRedirects {
			client = sess.follow
		}
	}
	req, err := hubRequest(ctx, rawURL, o)
	if err != nil {
		return nil, err
	}
	return httputil.Do(client, req, httputil.EachPhase(o.Timeout))
}

// One URL fetched with connect time SSRF validation and no automatic redirects
func (h *Hub) ssrfSafeHTTPGet(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string) (*httputil.Response, error) {
	if h.io.ssrfGet != nil {
		return h.io.ssrfGet(ctx, rawURL, timeout, headers)
	}
	ctx, release := h.HTTPSession(ctx)
	defer release()
	sess := ctx.Value(hubSessionKey{}).(*hubSession)
	req, err := hubRequest(ctx, rawURL, hubGet{Headers: headers})
	if err != nil {
		return nil, err
	}
	return httputil.Do(sess.noFollow, req, httputil.EachPhase(timeout))
}

func (h *Hub) urlIsSafe(rawURL string) bool {
	if h.io.isSafeURL != nil {
		return h.io.isSafeURL(rawURL)
	}
	return tools.IsSafeURL(h.toolsEnv(), rawURL)
}

func (h *Hub) websiteBlock(rawURL string) *tools.WebsiteBlock {
	if h.io.siteAccess != nil {
		return h.io.siteAccess(rawURL)
	}
	block, _ := tools.CheckWebsiteAccess(h.toolsEnv(), rawURL, "")
	return block
}

// Hop is allowed by the SSRF and website policies, logging the refusal otherwise
func (h *Hub) hopAllowed(rawURL string) bool {
	if !h.urlIsSafe(rawURL) {
		h.S.logger().Warn(fmt.Sprintf("Blocked unsafe Skills Hub URL: %s", rawURL))
		return false
	}
	if blocked := h.websiteBlock(rawURL); blocked != nil {
		h.S.logger().Info(fmt.Sprintf("Blocked Skills Hub fetch for %s by rule %s", blocked.Host, blocked.Rule))
		return false
	}
	return true
}

// GET with SSRF and website policy checks on every redirect hop, nil when blocked or failed, invalid URLs raised through hubRaise
func (h *Hub) guardedHTTPGet(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string) *httputil.Response {
	if h.io.guardedGet != nil {
		return h.io.guardedGet(ctx, rawURL, timeout, headers)
	}
	current := rawURL
	for range maxSkillFetchRedirects + 1 {
		if !h.hopAllowed(current) {
			return nil
		}
		resp, err := h.ssrfSafeHTTPGet(ctx, current, timeout, headers)
		if err != nil {
			var invalid *hubInvalidURLError
			if errors.As(err, &invalid) {
				hubRaise(err)
			}
			h.S.logger().Debug(fmt.Sprintf("Skills Hub fetch failed for %s: %s", current, err))
			return nil
		}
		if hubRedirectStatus[resp.StatusCode] {
			location := resp.Header.Get("Location")
			if location == "" {
				return nil
			}
			current = resolveRedirect(current, location)
			continue
		}
		return resp
	}
	h.S.logger().Warn(fmt.Sprintf("Skills Hub fetch exceeded redirect limit for %s", rawURL))
	return nil
}

// Location of a redirect resolved against the URL that answered it, an unparsable URL raised through hubRaise
func resolveRedirect(current, location string) string {
	base, err := url.Parse(current)
	if err != nil {
		hubRaise(err)
	}
	ref, err := base.Parse(location)
	if err != nil {
		hubRaise(err)
	}
	return ref.String()
}

// Streams one response with policy checked redirects, nil when blocked or failed, invalid URLs raised through hubRaise
func (h *Hub) guardedHTTPStream(ctx context.Context, rawURL string, params url.Values, timeout time.Duration) *httputil.StreamResponse {
	current, currentParams := rawURL, params
	for range maxSkillFetchRedirects + 1 {
		if !h.hopAllowed(current) {
			return nil
		}
		var client *http.Client
		if h.io.streamClient != nil {
			client = h.io.streamClient(timeout)
		} else {
			client = tools.NewSSRFSafeClient(h.toolsEnv(), 0, false)
		}
		req, err := hubRequest(ctx, current, hubGet{Params: currentParams})
		var resp *httputil.StreamResponse
		if err == nil {
			resp, err = httputil.Stream(client, req, httputil.EachPhase(timeout))
		}
		if err != nil {
			var invalid *hubInvalidURLError
			if errors.As(err, &invalid) {
				hubRaise(err)
			}
			h.S.logger().Debug(fmt.Sprintf("Skills Hub stream failed for %s: %s", current, err))
			return nil
		}
		if !hubRedirectStatus[resp.StatusCode] {
			return resp
		}
		resp.Body.Close()
		location := resp.Header.Get("Location")
		if location == "" {
			return nil
		}
		current = resolveRedirect(current, location)
		currentParams = nil
	}
	h.S.logger().Warn(fmt.Sprintf("Skills Hub fetch exceeded redirect limit for %s", rawURL))
	return nil
}

// Parsed JSON of a file younger than ttl seconds, nil when missing, stale or invalid
func readJSONIfFresh(path string, ttl float64) (any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if float64(time.Now().UnixNano()-st.ModTime().UnixNano())/1e9 > ttl {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

// Fresh shared index cache entry of a key, nil on miss or expiry
func (h *Hub) readIndexCache(key string) (any, error) {
	if h.io.readIndexCache != nil {
		return h.io.readIndexCache(key)
	}
	return readJSONIfFresh(filepath.Join(h.IndexCacheDir(), key+".json"), IndexCacheTTL)
}

// Writes a shared index cache entry and the .ignore that keeps search tools out of the hub dir
func (h *Hub) writeIndexCache(key string, data any) error {
	if h.io.writeIndexCache != nil {
		return h.io.writeIndexCache(key, data)
	}
	dir := h.IndexCacheDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	ignore := filepath.Join(h.HubDir(), ".ignore")
	if _, err := os.Stat(ignore); err != nil {
		_ = os.WriteFile(ignore, []byte("# Exclude hub internals from search tools\n*\n"), 0o666)
	}
	text, err := jsonutil.Encode(data, jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), []byte(text), 0o666); err != nil {
		h.S.logger().Debug(fmt.Sprintf("Could not write cache: %s", err))
	}
	return nil
}

// JSON state file under the hub dir with a fixed empty shape, rewritten under a cross process lock
type jsonStateFile struct {
	Path   string
	empty  func() *jsonutil.Object
	holder *tools.FileLockHolder
	log    func() interface{ Warn(string, ...any) }
}

var hubLockHolder, tapsLockHolder tools.FileLockHolder

// Decoded file, a fresh empty shape when missing or not JSON, the decode error on bad utf-8
func (f *jsonStateFile) read() (any, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return f.empty(), nil
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return f.empty(), nil
	}
	return v, nil
}

func (f *jsonStateFile) readObject() (*jsonutil.Object, error) {
	v, err := f.read()
	if err != nil {
		return nil, err
	}
	o, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("%s holds %s where a mapping was expected", f.Path, errchain.Kind(v))
	}
	return o, nil
}

// Atomic indent two rewrite keeping the existing file mode
func (f *jsonStateFile) write(ctx context.Context, data any, ensureASCII bool) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o777); err != nil {
		return err
	}
	opts := []jsonutil.Option{jsonutil.Indent(2)}
	if ensureASCII {
		opts = append(opts, jsonutil.ASCII())
	}
	text, err := jsonutil.Encode(data, opts...)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(ctx, f.Path, []byte(text+"\n"), fsutil.WriteOptions{KeepOwner: true, NativeLineEnds: true})
}

// Re reads, mutates and saves under the ten second lock, unlocked with a warning when the lock is unusable
func (f *jsonStateFile) update(ctx context.Context, mutate func(*jsonutil.Object) (bool, error), ensureASCII bool) (bool, error) {
	release, err := tools.AcquireFileLock(f.Path+".lock", f.holder, 10*time.Second,
		fmt.Sprintf("Timed out waiting for the skills hub state lock (%s)", f.Path))
	if err != nil {
		f.log().Warn(fmt.Sprintf("Updating %s without its lock", f.Path), "error", err)
	} else {
		defer release()
	}
	data, err := f.readObject()
	if err != nil {
		return false, err
	}
	changed, err := mutate(data)
	if err != nil || !changed {
		return changed, err
	}
	return true, f.write(ctx, data, ensureASCII)
}

// Lock file of installed hub skill provenance
type HubLockFile struct{ jsonStateFile }

// Lock file at path, the hub lock path when empty
func (h *Hub) LockFile(path string) *HubLockFile {
	if path == "" {
		path = h.LockFilePath()
	}
	return &HubLockFile{jsonStateFile{Path: path, holder: &hubLockHolder, log: h.warnLog, empty: func() *jsonutil.Object {
		o := jsonutil.NewObject()
		o.Set("version", int64(1))
		o.Set("installed", jsonutil.NewObject())
		return o
	}}}
}

func (h *Hub) warnLog() interface{ Warn(string, ...any) } { return h.S.logger() }

// Whole lock document
func (l *HubLockFile) Load() (*jsonutil.Object, error) { return l.readObject() }

// Rewrites the lock document keeping non ASCII text
func (l *HubLockFile) Save(ctx context.Context, data *jsonutil.Object) error {
	return l.write(ctx, data, false)
}

func lockInstalled(data *jsonutil.Object, create bool) (*jsonutil.Object, error) {
	v, ok := data.Get("installed")
	if !ok {
		if !create {
			return nil, errors.New("the skills hub lock has no installed table")
		}
		o := jsonutil.NewObject()
		data.Set("installed", o)
		return o, nil
	}
	o, isObj := v.(*jsonutil.Object)
	if !isObj {
		return nil, fmt.Errorf("skills hub lock installed table is %s where a mapping was expected", errchain.Kind(v))
	}
	return o, nil
}

// Records one install after validating the name and install path shape a later uninstall trusts
func (l *HubLockFile) RecordInstall(ctx context.Context, name, source, identifier, trustLevel, scanVerdict, skillHash, installPath string,
	files []string, metadata, scanProvenance *jsonutil.Object) error {
	safeName, err := validateHubSkillName(name)
	if err != nil {
		return err
	}
	safePath, err := normalizeLockInstallPath(installPath, safeName)
	if err != nil {
		return err
	}
	_, err = l.update(ctx, func(data *jsonutil.Object) (bool, error) {
		installed, err := lockInstalled(data, true)
		if err != nil {
			return false, err
		}
		now := time.Now().UTC().Format(timeutil.RFC3339Micro)
		fileList := make([]any, len(files))
		for i, f := range files {
			fileList[i] = f
		}
		if metadata == nil || metadata.Len() == 0 {
			metadata = jsonutil.NewObject()
		}
		if scanProvenance == nil || scanProvenance.Len() == 0 {
			scanProvenance = jsonutil.NewObject()
		}
		e := jsonutil.NewObject()
		for _, kv := range [][2]any{{"source", source}, {"identifier", identifier}, {"trust_level", trustLevel},
			{"scan_verdict", scanVerdict}, {"content_hash", skillHash}, {"install_path", safePath}, {"files", fileList},
			{"metadata", metadata}, {"scan_provenance", scanProvenance}, {"installed_at", now}, {"updated_at", now}} {
			e.Set(kv[0].(string), kv[1])
		}
		installed.Set(safeName, e)
		return true, nil
	}, false)
	return err
}

// Drops one installed entry
func (l *HubLockFile) RecordUninstall(ctx context.Context, name string) error {
	_, err := l.update(ctx, func(data *jsonutil.Object) (bool, error) {
		installed, err := lockInstalled(data, true)
		if err != nil {
			return false, err
		}
		installed.Delete(name)
		return true, nil
	}, false)
	return err
}

// Installed entry of a skill name, nil when absent
func (l *HubLockFile) GetInstalled(name string) (any, error) {
	data, err := l.Load()
	if err != nil {
		return nil, err
	}
	installed, err := lockInstalled(data, false)
	if err != nil {
		return nil, err
	}
	v, _ := installed.Get(name)
	return v, nil
}

// Installed entries each led by its name
func (l *HubLockFile) ListInstalled() ([]*jsonutil.Object, error) {
	data, err := l.Load()
	if err != nil {
		return nil, err
	}
	installed, err := lockInstalled(data, false)
	if err != nil {
		return nil, err
	}
	out := make([]*jsonutil.Object, 0, installed.Len())
	for _, name := range installed.Keys() {
		entry, ok := installed.Value(name).(*jsonutil.Object)
		if !ok {
			return nil, fmt.Errorf("skills hub lock entry %s is %s where a mapping was expected", name, errchain.Kind(installed.Value(name)))
		}
		row := jsonutil.NewObject()
		row.Set("name", name)
		for _, k := range entry.Keys() {
			row.Set(k, entry.Value(k))
		}
		out = append(out, row)
	}
	return out, nil
}

// Installed entries keyed by identifier plus the clawhub/<slug> alias of each ClawHub row, exact ids winning
func (l *HubLockFile) ByIdentifier() (map[string]*jsonutil.Object, error) {
	rows, err := l.ListInstalled()
	if err != nil {
		return nil, err
	}
	installed := map[string]*jsonutil.Object{}
	var kept []*jsonutil.Object
	for _, entry := range rows {
		id, _ := entry.Get("identifier")
		if !anyutil.IsTrue(id) {
			continue
		}
		kept = append(kept, entry)
		if s, ok := id.(string); ok {
			installed[s] = entry
		}
	}
	for _, entry := range kept {
		if src, _ := entry.Get("source"); src == "clawhub" {
			id, _ := entry.Get("identifier")
			if alias, ok := clawhubCatalogIdentifier(id); ok {
				if _, exists := installed[alias]; !exists {
					installed[alias] = entry
				}
			}
		}
	}
	return installed, nil
}

// Tap list of custom GitHub repo sources
type TapsManager struct{ jsonStateFile }

// Tap list at path, the hub taps path when empty
func (h *Hub) Taps(path string) *TapsManager {
	if path == "" {
		path = h.TapsFilePath()
	}
	return &TapsManager{jsonStateFile{Path: path, holder: &tapsLockHolder, log: h.warnLog, empty: func() *jsonutil.Object {
		o := jsonutil.NewObject()
		o.Set("taps", []any{})
		return o
	}}}
}

func tapsList(data *jsonutil.Object, create bool) ([]any, error) {
	v, ok := data.Get("taps")
	if !ok {
		if create {
			data.Set("taps", []any{})
		}
		return []any{}, nil
	}
	list, isList := v.([]any)
	if !isList {
		return nil, fmt.Errorf("skills taps are %s where a list was expected", errchain.Kind(v))
	}
	return list, nil
}

func tapRepo(t any) (any, error) {
	o, ok := t.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("skills tap is %s where a mapping was expected", errchain.Kind(t))
	}
	v, ok := o.Get("repo")
	if !ok {
		return nil, errors.New("a tap entry has no repo")
	}
	return v, nil
}

// Taps in file order
func (m *TapsManager) Load() ([]any, error) {
	data, err := m.readObject()
	if err != nil {
		return nil, err
	}
	return tapsList(data, false)
}

// Taps in file order
func (m *TapsManager) ListTaps() ([]any, error) { return m.Load() }

// Rewrites the tap list
func (m *TapsManager) Save(ctx context.Context, taps []any) error {
	o := jsonutil.NewObject()
	o.Set("taps", taps)
	return m.write(ctx, o, true)
}

// Appends a tap, false when the repo is already tapped
func (m *TapsManager) Add(ctx context.Context, repo, path string) (bool, error) {
	return m.update(ctx, func(data *jsonutil.Object) (bool, error) {
		taps, err := tapsList(data, true)
		if err != nil {
			return false, err
		}
		for _, t := range taps {
			r, err := tapRepo(t)
			if err != nil {
				return false, err
			}
			if r == repo {
				return false, nil
			}
		}
		tap := jsonutil.NewObject()
		tap.Set("repo", repo)
		tap.Set("path", path)
		data.Set("taps", append(taps, tap))
		return true, nil
	}, true)
}

// Removes a tap by repo, false when it was not tapped
func (m *TapsManager) Remove(ctx context.Context, repo string) (bool, error) {
	return m.update(ctx, func(data *jsonutil.Object) (bool, error) {
		taps, err := tapsList(data, false)
		if err != nil {
			return false, err
		}
		kept := []any{}
		for _, t := range taps {
			r, err := tapRepo(t)
			if err != nil {
				return false, err
			}
			if r != repo {
				kept = append(kept, t)
			}
		}
		data.Set("taps", kept)
		return len(kept) != len(taps), nil
	}, true)
}

// Appends one space separated line to the audit log, a failed write only logged
func (h *Hub) AppendAuditLog(action, skillName, source, trustLevel, verdict, extra string) error {
	path := h.AuditLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	line := time.Now().UTC().Format("2006-01-02T15:04:05Z") + " " + action + " " + skillName + " " + source + ":" + trustLevel + " " + verdict
	if extra != "" {
		line += " " + extra
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err == nil {
		_, err = f.WriteString(line + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		h.S.logger().Debug(fmt.Sprintf("Could not write audit log: %s", err))
	}
	return nil
}

// Creates the hub dir, quarantine, index cache and empty state files when missing
func (h *Hub) EnsureHubDirs() error {
	if err := os.MkdirAll(h.HubDir(), 0o777); err != nil {
		return err
	}
	for _, dir := range []string{h.QuarantineDir(), h.IndexCacheDir()} {
		if err := os.Mkdir(dir, 0o777); err != nil {
			if st, serr := os.Stat(dir); !errors.Is(err, os.ErrExist) || serr != nil || !st.IsDir() {
				return err
			}
		}
	}
	for _, f := range [][2]string{{h.LockFilePath(), "{\"version\": 1, \"installed\": {}}\n"}, {h.AuditLogPath(), ""},
		{h.TapsFilePath(), "{\"taps\": []}\n"}} {
		if _, err := os.Stat(f[0]); err == nil {
			continue
		}
		if err := os.WriteFile(f[0], []byte(f[1]), 0o666); err != nil {
			return err
		}
	}
	return nil
}
