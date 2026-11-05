package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	stubToken        = "plane-token-1"
	stubNousToken    = "nous-access-token-1"
	stubClientID     = "agent-client"
	stubClientSecret = "agent-secret"
	stubInstance     = "inst-0001"
	maxValueDepth    = 32
)

type stubRequest struct {
	method, profile, auth, instance, ifNoneMatch string
	body                                         map[string]any
}

type stubProfile struct {
	values  map[string]any
	version int
	writer  any
}

// Contract faithful config plane double with one upper document, per profile values and an IdP token endpoint
type stubPlane struct {
	mu              sync.Mutex
	upper           map[string]any
	upperLocks      []map[string]any
	groups          []map[string]any
	groupProvenance map[string]any
	profiles        map[string]*stubProfile
	failStatus      int
	etagOverride    string
	requests        []stubRequest
	tokenRequests   int
	nousRefreshes   int
	home            string
	url             string
	srv             *httptest.Server
}

func stubDeepMerge(base, over map[string]any) map[string]any {
	out := cloneDoc(base)
	for _, k := range config.MappingKeys(over) {
		v := over[k]
		cur, overObj := out[k].(map[string]any)
		if vm, ok := v.(map[string]any); overObj && ok {
			setKey(out, k, stubDeepMerge(cur, vm))
		} else if !(overObj && v == nil) {
			setKey(out, k, config.Clone(v))
		}
	}
	return out
}

func stubDepth(v any) int {
	best := 0
	switch c := v.(type) {
	case map[string]any:
		for _, e := range c {
			best = max(best, stubDepth(e))
		}
		return 1 + best
	case []any:
		for _, e := range c {
			best = max(best, stubDepth(e))
		}
		return 1 + best
	}
	return 0
}

func newStubPlane(t *testing.T) *stubPlane {
	p := &stubPlane{upper: map[string]any{}, profiles: map[string]*stubProfile{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	p.url = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

func (p *stubPlane) profile(name string) *stubProfile {
	prof := p.profiles[name]
	if prof == nil {
		prof = &stubProfile{values: map[string]any{}}
		p.profiles[name] = prof
	}
	return prof
}

// Effective response for a profile, the etag a digest of the body
func (p *stubPlane) effective(name string) map[string]any {
	prof := p.profile(name)
	lvls := []any{map[string]any{"kind": "tenant", "version": 1, "writerConfigVersion": nil}}
	for _, g := range p.groups {
		lv := map[string]any{"kind": "group", "writerConfigVersion": nil}
		for k, v := range g {
			lv[k] = config.Clone(v)
		}
		lvls = append(lvls, lv)
	}
	lvls = append(lvls, map[string]any{"kind": "profile", "version": prof.version, "writerConfigVersion": prof.writer})
	locks := make([]any, len(p.upperLocks))
	for i, l := range p.upperLocks {
		locks[i] = config.Clone(l)
	}
	body := map[string]any{
		"instanceId": stubInstance, "profile": name, "config": stubDeepMerge(p.upper, prof.values),
		"locks": locks, "provenance": map[string]any{}, "levels": lvls,
		"profileVersion": prof.version, "recordVersion": 1,
	}
	if p.groupProvenance != nil {
		body["groupProvenance"] = config.Clone(p.groupProvenance)
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	body["etag"] = `"` + hex.EncodeToString(sum[:]) + `"`
	if p.etagOverride != "" {
		body["etag"] = p.etagOverride
	}
	return body
}

func (p *stubPlane) patches() []stubRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []stubRequest
	for _, r := range p.requests {
		if r.method == "PATCH" {
			out = append(out, r)
		}
	}
	return out
}

func (p *stubPlane) gets() []stubRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []stubRequest
	for _, r := range p.requests {
		if r.method == "GET" {
			out = append(out, r)
		}
	}
	return out
}

func stubSend(w http.ResponseWriter, status int, body map[string]any, etag string) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(status)
	w.Write(raw)
}

func (p *stubPlane) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPost {
		p.serveToken(w, r, raw)
		return
	}
	if r.URL.Path != "/v1/config/self" {
		stubSend(w, 404, map[string]any{"error": "not_found"}, "")
		return
	}
	profile := "default"
	if v := r.URL.Query()["profile"]; len(v) > 0 {
		profile = v[0]
	}
	var body map[string]any
	if r.Method == "PATCH" {
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		parsed, _ := loadJSON(raw)
		body, _ = parsed.(map[string]any)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, stubRequest{method: r.Method, profile: profile, body: body,
		auth: r.Header.Get("Authorization"), instance: r.Header.Get("X-Hermes-Instance-Id"), ifNoneMatch: r.Header.Get("If-None-Match")})
	if p.failStatus != 0 {
		stubSend(w, p.failStatus, map[string]any{"error": "unavailable", "message": "stub outage"}, "")
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "Bearer "+stubToken && auth != "Bearer "+stubNousToken {
		stubSend(w, 401, map[string]any{"error": "unauthorized", "message": "bad token"}, "")
		return
	}
	if r.Header.Get("X-Hermes-Instance-Id") != stubInstance {
		stubSend(w, 403, map[string]any{"error": "config_agent_unknown", "message": "unknown instance"}, "")
		return
	}
	if r.Method == "GET" {
		eff := p.effective(profile)
		if r.Header.Get("If-None-Match") == eff["etag"] {
			stubSend(w, 304, nil, eff["etag"].(string))
			return
		}
		stubSend(w, 200, eff, eff["etag"].(string))
		return
	}
	if body == nil {
		body = map[string]any{}
	}
	p.patch(w, profile, body)
}

func (p *stubPlane) serveToken(w http.ResponseWriter, r *http.Request, raw []byte) {
	if r.URL.Path == "/api/oauth/token" {
		p.mu.Lock()
		p.nousRefreshes++
		p.mu.Unlock()
		stubSend(w, 200, map[string]any{"access_token": stubNousToken, "refresh_token": "nous-refresh-2", "token_type": "Bearer", "expires_in": 3600}, "")
		return
	}
	if r.URL.Path != "/token" {
		stubSend(w, 404, map[string]any{"error": "not_found"}, "")
		return
	}
	form, _ := url.ParseQuery(string(raw))
	p.mu.Lock()
	p.tokenRequests++
	p.mu.Unlock()
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != stubClientID || form.Get("client_secret") != stubClientSecret {
		stubSend(w, 401, map[string]any{"error": "invalid_client"}, "")
		return
	}
	stubSend(w, 200, map[string]any{"access_token": stubToken, "token_type": "Bearer", "expires_in": 3600}, "")
}

func (p *stubPlane) patch(w http.ResponseWriter, profile string, body map[string]any) {
	prof := p.profile(profile)
	if v, ok := pyInt(body["expectedVersion"]); !ok || v != prof.version {
		stubSend(w, 409, map[string]any{"error": "config_version_conflict", "message": "stale", "currentVersion": prof.version}, "")
		return
	}
	sets := &PathMap{}
	setBody, _ := body["set"].(map[string]any)
	for _, k := range config.MappingKeys(setBody) {
		path, _ := Decode(k)
		sets.Set(path, setBody[k])
	}
	var unsets []Path
	rawUnsets, _ := body["unset"].([]any)
	for _, u := range rawUnsets {
		path, _ := Decode(u.(string))
		unsets = append(unsets, path)
	}
	var locks []Lock
	for _, l := range p.upperLocks {
		path, _ := Decode(l["path"].(string))
		locks = append(locks, Lock{Path: path, Level: l["level"].(string)})
	}
	if lock, refused := WriteCheck(sets, unsets, locks); refused {
		path := Encode(lock.Path)
		out := map[string]any{"error": "config_key_locked", "path": path, "lockedBy": lock.Level, "message": path + " is locked by " + lock.Level}
		for _, l := range p.upperLocks {
			if l["path"] == path && l["level"] == lock.Level {
				if g, ok := l["groupId"].(string); ok && g != "" {
					out["lockedByGroupId"] = g
				}
				break
			}
		}
		stubSend(w, 403, out, "")
		return
	}
	for _, e := range sets.Entries() {
		if stubDepth(e.Value) > maxValueDepth {
			path := Encode(e.Path)
			stubSend(w, 400, map[string]any{"error": "config_value_invalid", "path": path, "reason": "too_deep",
				"message": fmt.Sprintf("%s: value nesting exceeds %d", path, maxValueDepth)}, "")
			return
		}
	}
	for _, e := range sets.Entries() {
		if _, bad := SecretLiteralPath(e.Path, e.Value); bad {
			stubSend(w, 400, map[string]any{"error": "config_secret_literal", "message": "secret literal"}, "")
			return
		}
	}
	for _, e := range sets.Entries() {
		setPath(prof.values, e.Path, config.Clone(e.Value))
	}
	for _, u := range unsets {
		popPath(prof.values, u)
	}
	prof.version++
	if w2, ok := body["writerConfigVersion"]; ok {
		prof.writer = w2
	}
	eff := p.effective(profile)
	stubSend(w, 200, eff, eff["etag"].(string))
}

// Remote deployment env pointing at the stub with the IdP client credentials
func remoteEnv(t *testing.T, p *stubPlane) {
	t.Setenv(config.BackendEnv, "remote")
	t.Setenv(URLEnv, p.url)
	t.Setenv(InstanceEnv, stubInstance)
	t.Setenv(idpEnvPrefix+"TOKEN_URL", p.url+"/token")
	t.Setenv(idpEnvPrefix+"CLIENT_ID", stubClientID)
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", stubClientSecret)
}

func clearIDPCache() {
	idpMu.Lock()
	idpCache = map[idpCacheKey]idpCached{}
	idpMu.Unlock()
}

// Fixture of the Python plane fixture with a default home, the stub env, zero boot delays and a fresh backend
func setupPlane(t *testing.T) *stubPlane {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	p := newStubPlane(t)
	remoteEnv(t, p)
	saved := BootRetryDelays
	BootRetryDelays = []float64{0, 0}
	ResetForTests()
	clearIDPCache()
	p.home = h
	t.Cleanup(func() {
		ResetForTests()
		clearIDPCache()
		BootRetryDelays = saved
	})
	return p
}

func testBackend() *remoteBackend { return GetRemoteBackend().(*remoteBackend) }
