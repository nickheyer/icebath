package configplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	Token         = "plane-token-1"
	NousToken     = "nous-access-token-1"
	ClientID      = "agent-client"
	ClientSecret  = "agent-secret"
	Instance      = "inst-0001"
	MaxValueDepth = 32
)

// One request the plane received on its config route
type Request struct {
	Method, Profile, Auth, Instance, IfNoneMatch string
	Body                                         map[string]any
}

// Profile layer the plane stores with its version and the writer config version last sent
type Profile struct {
	Values  map[string]any
	Version int
	Writer  any
}

// Status and body the plane answers every PATCH with instead of applying it
type Refusal struct {
	Status int
	Body   map[string]any
}

// Lock path and the level that holds it
type Lock struct {
	Path  []string
	Level string
}

// Agent side helpers a write is checked against, the same ones the remote backend uses
type Rules struct {
	Decode        func(encoded string) ([]string, error)
	Encode        func(segments []string) string
	WriteCheck    func(setPaths [][]string, setValues []any, unsets [][]string, locks []Lock) (Lock, bool)
	SecretLiteral func(path []string, value any) bool
}

// Contract faithful config plane double with one upper document, per profile values and an IdP token endpoint
type Plane struct {
	Mu              sync.Mutex
	Upper           map[string]any
	UpperLocks      []map[string]any
	Groups          []map[string]any
	GroupProvenance map[string]any
	Profiles        map[string]*Profile
	FailStatus      int
	EtagOverride    string
	Requests        []Request
	TokenRequests   int
	NousRefreshes   int
	Home            string
	URL             string
	OnGet           func()
	RefusePatch     *Refusal
	rules           Rules
}

// Starts the plane on loopback and closes it when the test ends
func New(t testing.TB, rules Rules) *Plane {
	p := &Plane{Upper: map[string]any{}, Profiles: map[string]*Profile{}, rules: rules}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	p.URL = srv.URL
	t.Cleanup(srv.Close)
	return p
}

// Merges over into base, lists replace, null over a mapping is ignored and null over anything else sets null
func DeepMerge(base, over map[string]any) map[string]any {
	out := cloneDoc(base)
	for _, k := range config.MappingKeys(over) {
		v := over[k]
		cur, overObj := out[k].(map[string]any)
		if vm, ok := v.(map[string]any); overObj && ok {
			setKey(out, k, DeepMerge(cur, vm))
		} else if !(overObj && v == nil) {
			setKey(out, k, config.Clone(v))
		}
	}
	return out
}

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

func depth(v any) int {
	best := 0
	switch c := v.(type) {
	case map[string]any:
		for _, e := range c {
			best = max(best, depth(e))
		}
		return 1 + best
	case []any:
		for _, e := range c {
			best = max(best, depth(e))
		}
		return 1 + best
	}
	return 0
}

// Parses a JSON body into plain maps with key order kept and integers as int
func loadJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid utf-8")
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	return plain(v), nil
}

func plain(v any) any {
	switch c := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, c.Len())
		for _, k := range c.Keys {
			out[k] = plain(c.Vals[k])
		}
		config.SetMappingKeys(out, c.Keys)
		return out
	case []any:
		out := make([]any, len(c))
		for i, e := range c {
			out[i] = plain(e)
		}
		return out
	case int64:
		return int(c)
	case *big.Int:
		if c.IsInt64() {
			return int(c.Int64())
		}
		return c
	}
	return v
}

// Profile record by name, created empty at version zero on first use
func (p *Plane) Profile(name string) *Profile {
	prof := p.Profiles[name]
	if prof == nil {
		prof = &Profile{Values: map[string]any{}}
		p.Profiles[name] = prof
	}
	return prof
}

// Effective response for a profile, the etag a digest of the body
func (p *Plane) Effective(name string) map[string]any {
	prof := p.Profile(name)
	lvls := []any{map[string]any{"kind": "tenant", "version": 1, "writerConfigVersion": nil}}
	for _, g := range p.Groups {
		lv := map[string]any{"kind": "group", "writerConfigVersion": nil}
		for k, v := range g {
			lv[k] = config.Clone(v)
		}
		lvls = append(lvls, lv)
	}
	lvls = append(lvls, map[string]any{"kind": "profile", "version": prof.Version, "writerConfigVersion": prof.Writer})
	locks := make([]any, len(p.UpperLocks))
	for i, l := range p.UpperLocks {
		locks[i] = config.Clone(l)
	}
	body := map[string]any{
		"instanceId": Instance, "profile": name, "config": DeepMerge(p.Upper, prof.Values),
		"locks": locks, "provenance": map[string]any{}, "levels": lvls,
		"profileVersion": prof.Version, "recordVersion": 1,
	}
	if p.GroupProvenance != nil {
		body["groupProvenance"] = config.Clone(p.GroupProvenance)
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	body["etag"] = `"` + hex.EncodeToString(sum[:]) + `"`
	if p.EtagOverride != "" {
		body["etag"] = p.EtagOverride
	}
	return body
}

func (p *Plane) byMethod(method string) []Request {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var out []Request
	for _, r := range p.Requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// PATCH requests received so far
func (p *Plane) Patches() []Request { return p.byMethod("PATCH") }

// GET requests received so far
func (p *Plane) Gets() []Request { return p.byMethod("GET") }

func send(w http.ResponseWriter, status int, body map[string]any, etag string) {
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

func (p *Plane) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPost {
		p.serveToken(w, r, raw)
		return
	}
	if r.URL.Path != "/v1/config/self" {
		send(w, 404, map[string]any{"error": "not_found"}, "")
		return
	}
	if r.Method == "GET" && p.OnGet != nil {
		p.OnGet()
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
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Requests = append(p.Requests, Request{Method: r.Method, Profile: profile, Body: body,
		Auth: r.Header.Get("Authorization"), Instance: r.Header.Get("X-Hermes-Instance-Id"), IfNoneMatch: r.Header.Get("If-None-Match")})
	if p.FailStatus != 0 {
		send(w, p.FailStatus, map[string]any{"error": "unavailable", "message": "stub outage"}, "")
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "Bearer "+Token && auth != "Bearer "+NousToken {
		send(w, 401, map[string]any{"error": "unauthorized", "message": "bad token"}, "")
		return
	}
	if r.Header.Get("X-Hermes-Instance-Id") != Instance {
		send(w, 403, map[string]any{"error": "config_agent_unknown", "message": "unknown instance"}, "")
		return
	}
	if r.Method == "GET" {
		eff := p.Effective(profile)
		if r.Header.Get("If-None-Match") == eff["etag"] {
			send(w, 304, nil, eff["etag"].(string))
			return
		}
		send(w, 200, eff, eff["etag"].(string))
		return
	}
	if p.RefusePatch != nil {
		send(w, p.RefusePatch.Status, p.RefusePatch.Body, "")
		return
	}
	if body == nil {
		body = map[string]any{}
	}
	p.patch(w, profile, body)
}

func (p *Plane) serveToken(w http.ResponseWriter, r *http.Request, raw []byte) {
	if r.URL.Path == "/api/oauth/token" {
		p.Mu.Lock()
		p.NousRefreshes++
		p.Mu.Unlock()
		send(w, 200, map[string]any{"access_token": NousToken, "refresh_token": "nous-refresh-2", "token_type": "Bearer", "expires_in": 3600}, "")
		return
	}
	if r.URL.Path != "/token" {
		send(w, 404, map[string]any{"error": "not_found"}, "")
		return
	}
	form, _ := url.ParseQuery(string(raw))
	p.Mu.Lock()
	p.TokenRequests++
	p.Mu.Unlock()
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != ClientID || form.Get("client_secret") != ClientSecret {
		send(w, 401, map[string]any{"error": "invalid_client"}, "")
		return
	}
	send(w, 200, map[string]any{"access_token": Token, "token_type": "Bearer", "expires_in": 3600}, "")
}

func pyInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (p *Plane) patch(w http.ResponseWriter, profile string, body map[string]any) {
	prof := p.Profile(profile)
	if v, ok := pyInt(body["expectedVersion"]); !ok || v != prof.Version {
		send(w, 409, map[string]any{"error": "config_version_conflict", "message": "stale", "currentVersion": prof.Version}, "")
		return
	}
	var setPaths, unsets [][]string
	var setValues []any
	setBody, _ := body["set"].(map[string]any)
	for _, k := range config.MappingKeys(setBody) {
		path, _ := p.rules.Decode(k)
		setPaths, setValues = append(setPaths, path), append(setValues, setBody[k])
	}
	rawUnsets, _ := body["unset"].([]any)
	for _, u := range rawUnsets {
		path, _ := p.rules.Decode(u.(string))
		unsets = append(unsets, path)
	}
	var locks []Lock
	for _, l := range p.UpperLocks {
		path, _ := p.rules.Decode(l["path"].(string))
		locks = append(locks, Lock{Path: path, Level: l["level"].(string)})
	}
	if lock, refused := p.rules.WriteCheck(setPaths, setValues, unsets, locks); refused {
		path := p.rules.Encode(lock.Path)
		out := map[string]any{"error": "config_key_locked", "path": path, "lockedBy": lock.Level, "message": path + " is locked by " + lock.Level}
		for _, l := range p.UpperLocks {
			if l["path"] == path && l["level"] == lock.Level {
				if g, ok := l["groupId"].(string); ok && g != "" {
					out["lockedByGroupId"] = g
				}
				break
			}
		}
		send(w, 403, out, "")
		return
	}
	for i, v := range setValues {
		if depth(v) > MaxValueDepth {
			path := p.rules.Encode(setPaths[i])
			send(w, 400, map[string]any{"error": "config_value_invalid", "path": path, "reason": "too_deep",
				"message": fmt.Sprintf("%s: value nesting exceeds %d", path, MaxValueDepth)}, "")
			return
		}
	}
	for i, v := range setValues {
		if p.rules.SecretLiteral(setPaths[i], v) {
			send(w, 400, map[string]any{"error": "config_secret_literal", "message": "secret literal"}, "")
			return
		}
	}
	for i, path := range setPaths {
		node := prof.Values
		for _, seg := range path[:len(path)-1] {
			next, ok := node[seg].(map[string]any)
			if !ok {
				next = map[string]any{}
				setKey(node, seg, next)
			}
			node = next
		}
		setKey(node, path[len(path)-1], config.Clone(setValues[i]))
	}
	for _, path := range unsets {
		var node any = prof.Values
		for _, seg := range path[:len(path)-1] {
			m, _ := node.(map[string]any)
			node = nil
			if m != nil {
				node = m[seg]
			}
		}
		if m, ok := node.(map[string]any); ok {
			delete(m, path[len(path)-1])
		}
	}
	prof.Version++
	if w2, ok := body["writerConfigVersion"]; ok {
		prof.Writer = w2
	}
	eff := p.Effective(profile)
	send(w, 200, eff, eff["etag"].(string))
}
