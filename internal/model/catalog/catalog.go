// Model lists, context windows, capabilities and pricing for one profile home
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	probeDeadline = 15 * time.Second
	maxReplyBytes = 256 << 20
	maxErrorBytes = 1 << 20
	maxRedirects  = 20
)

var ErrOptions = errors.New("catalog: options need a home, a config store, a provider registry, a provider directory, a local runtime, a secret scope, a route resolver, a dialect lister, a credential store, a portal account and a plan usage source")

var ErrClosed = errors.New("catalog: closed")

type Route struct {
	Provider string
	BaseURL  string
	Key      auth.Credential
}

type RouteResolver interface {
	ResolveRoute(ctx context.Context, provider, model string) (Route, error)
}

type Options struct {
	Home      string
	Config    *config.Store
	HTTP      httpclient.Options
	Registry  *providers.Registry
	Directory *providers.Directory
	Local     *local.Runtime
	Secrets   *secret.Scope
	Routes    RouteResolver
	Dialects  func(ctx context.Context) []wire.Mode
	Auth      *auth.Store
	Portal    *portal.Account
	Version   string
	Plan      PlanUsage
}

type phases struct {
	connect time.Duration
	read    time.Duration
	total   time.Duration
}

var (
	fetchPhases   = phases{connect: 5 * time.Second, read: 10 * time.Second, total: probeDeadline}
	propsPhases   = phases{connect: 5 * time.Second, read: 5 * time.Second, total: probeDeadline}
	detectPhases  = phases{connect: 2 * time.Second, read: 2 * time.Second, total: probeDeadline}
	localPhases   = phases{connect: 3 * time.Second, read: 3 * time.Second, total: probeDeadline}
	controlPhases = phases{connect: 60 * time.Second, read: 60 * time.Second, total: 120 * time.Second}
)

type transportKey struct {
	ca       string
	insecure bool
	phases   phases
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r *reply) success() bool { return r.status >= 200 && r.status < 300 }

func (r *reply) failed() bool { return r.status >= 400 }

func (r *reply) value() (wire.Value, error) { return wire.ParseValue(r.body) }

type Catalog struct {
	home    string
	cfg     *config.Store
	reg     *providers.Registry
	dir     *providers.Directory
	local   *local.Runtime
	secrets *secret.Scope
	routes  RouteResolver
	http    httpclient.Options

	dialects func(ctx context.Context) []wire.Mode

	auth    *auth.Store
	portal  *portal.Account
	version string
	plan    PlanUsage
	st      *state

	bg     context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu           sync.Mutex
	closed       bool
	transports   map[transportKey]*httpclient.Transport
	blackholes   map[string]time.Time
	verdicts     map[string]serverVerdict
	localProbes  map[localProbeKey]localProbe
	bedrockMiss  map[bedrockProbeKey]time.Time
	warned       map[string]bool
	openRouter   *ModelSet
	openRouterAt time.Time
	endpoints    map[endpointKey]endpointMemo
	codex        map[string]codexMemo
	codexMax     map[string]map[string]int
	codexRaw     map[string]codexListing
	generation   map[string]bool
	discovery    map[string]bedrockDiscovery
	profileARNs  map[string]string

	md modelsDev
}

func New(o Options) (*Catalog, error) {
	if o.Home == "" || o.Config == nil || o.Registry == nil || o.Directory == nil || o.Local == nil || o.Secrets == nil || o.Routes == nil || o.Dialects == nil ||
		o.Auth == nil || o.Portal == nil || o.Plan == nil {
		return nil, ErrOptions
	}
	bg, cancel := context.WithCancel(home.WithOverride(context.Background(), o.Home))
	return &Catalog{
		home: o.Home, cfg: o.Config, reg: o.Registry, dir: o.Directory, local: o.Local, secrets: o.Secrets, routes: o.Routes, http: o.HTTP,
		dialects: o.Dialects,
		auth:     o.Auth, portal: o.Portal, version: o.Version, plan: o.Plan, st: newState(),
		bg: bg, cancel: cancel,
		transports: map[transportKey]*httpclient.Transport{}, blackholes: map[string]time.Time{},
		verdicts: map[string]serverVerdict{}, localProbes: map[localProbeKey]localProbe{},
		bedrockMiss: map[bedrockProbeKey]time.Time{}, warned: map[string]bool{},
		endpoints: map[endpointKey]endpointMemo{}, codex: map[string]codexMemo{}, codexMax: map[string]map[string]int{},
		codexRaw: map[string]codexListing{}, generation: map[string]bool{}, discovery: map[string]bedrockDiscovery{},
		profileARNs: map[string]string{},
	}, nil
}

func (c *Catalog) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
	c.mu.Lock()
	for _, t := range c.transports {
		t.CloseIdleConnections()
	}
	c.mu.Unlock()
	return nil
}

func (c *Catalog) Home() string { return c.home }

func (c *Catalog) scoped(ctx context.Context) context.Context {
	return home.WithOverride(ctx, c.home)
}

func (c *Catalog) config(ctx context.Context) *config.Config {
	cfg, err := c.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug("config is unreadable for a catalog lookup", "error", err)
		return nil
	}
	return cfg
}

func (c *Catalog) once(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warned[key] {
		return false
	}
	c.warned[key] = true
	return true
}

func (c *Catalog) tlsFor(ctx context.Context, baseURL string) (string, bool) {
	if baseURL == "" {
		return "", false
	}
	entries := c.RouteEntries(ctx, baseURL, nil)
	if len(entries) == 0 {
		return "", false
	}
	ca, verify := entries[0].TLS()
	if verify != nil && !*verify {
		return "", true
	}
	if ca == "" {
		return "", false
	}
	path := home.ExpandUser(ca)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		if abs, err := filepath.Abs(path); err == nil {
			return abs, false
		}
		return path, false
	}
	if c.once("ca\x00" + ca) {
		slog.Warn("ssl_ca_cert path does not exist, using the OS trust store instead", "path", ca)
	}
	return "", false
}

func (c *Catalog) client(ctx context.Context, tlsURL string, p phases) (*http.Client, error) {
	ca, insecure := c.tlsFor(ctx, tlsURL)
	key := transportKey{ca: ca, insecure: insecure, phases: p}
	c.mu.Lock()
	t, ok := c.transports[key]
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if ok {
		return t.Client(), nil
	}
	o := c.http
	if insecure {
		o.TLS.Insecure = true
		slog.Warn("TLS certificate verification DISABLED (ssl_verify: false), intended for local development only and unsafe on any network you do not fully control", "endpoint", endpointLabel(tlsURL))
	} else if ca != "" {
		o.TLS.RootCAFile = ca
	}
	o.Timeouts = httpclient.Timeouts{Connect: p.connect, TLS: p.connect, Write: p.read, Header: p.read, Idle: p.read, Pool: p.connect, Exchange: p.total}
	o.MaxRedirects = maxRedirects
	o.ForwardHeaders = false
	built, err := httpclient.New(o)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if existing, ok := c.transports[key]; ok {
		c.mu.Unlock()
		built.CloseIdleConnections()
		return existing.Client(), nil
	}
	c.transports[key] = built
	c.mu.Unlock()
	return built.Client(), nil
}

func endpointLabel(baseURL string) string {
	if key, ok := endpointHostKey(baseURL); ok {
		return key
	}
	return "a custom provider endpoint"
}

func send(ctx context.Context, cl *http.Client, method, rawURL string, header http.Header, body []byte, sign wire.Authorizer) (*reply, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		req.Header[name] = slices.Clone(values)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if sign != nil {
		if err := sign.Authorize(ctx, req, body); err != nil {
			return nil, err
		}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := int64(maxErrorBytes)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		limit = maxReplyBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func (c *Catalog) get(ctx context.Context, rawURL string, header http.Header, tlsURL string, p phases) (*reply, error) {
	cl, err := c.client(ctx, tlsURL, p)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, p.total)
	defer cancel()
	return send(bounded, cl, http.MethodGet, rawURL, header, nil, nil)
}

func isConnectTimeout(err error) bool {
	var he *httpclient.Error
	return errors.As(err, &he) && he.Phase == httpclient.PhaseConnect && he.Timeout()
}

func bearerHeader(ctx context.Context, key auth.Credential) http.Header {
	h := http.Header{}
	if token := key.ProbeKey(ctx).Reveal(); token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}

func readJSONObject(path string) map[string]json.RawMessage {
	var data map[string]json.RawMessage
	if err := fileutil.ReadJSON(path, &data); err != nil {
		return map[string]json.RawMessage{}
	}
	if data == nil {
		return map[string]json.RawMessage{}
	}
	return data
}

func memoStamp(raw json.RawMessage) (float64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, true
	}
	var number float64
	if err := json.Unmarshal(trimmed, &number); err == nil {
		return number, true
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if parsed, err := strconv.ParseFloat(text, 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func memoGet(path, key string, ttl time.Duration, stampKey, valueKey string) (json.RawMessage, bool) {
	var entry map[string]json.RawMessage
	raw, ok := readJSONObject(path)[key]
	if !ok || json.Unmarshal(raw, &entry) != nil || entry == nil {
		return nil, false
	}
	stampRaw, ok := entry[stampKey]
	if !ok {
		return nil, false
	}
	stamp, ok := memoStamp(stampRaw)
	if !ok || unixSeconds(time.Now())-stamp >= ttl.Seconds() {
		return nil, false
	}
	value, ok := entry[valueKey]
	if !ok {
		return nil, false
	}
	return value, true
}

func memoPut(ctx context.Context, path, key string, value any, ttl time.Duration, stampKey, valueKey, what string) {
	now := unixSeconds(time.Now())
	kept := map[string]json.RawMessage{}
	for name, raw := range readJSONObject(path) {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			continue
		}
		stamp, ok := memoStamp(entry[stampKey])
		if !ok {
			slog.Debug("failed to save "+what, "error", "an entry carries a stamp that is not a number")
			return
		}
		if now-stamp < ttl.Seconds() {
			kept[name] = raw
		}
	}
	encodedValue, err := json.Marshal(value)
	if err != nil {
		slog.Debug("failed to save "+what, "error", err)
		return
	}
	encoded, err := json.Marshal(map[string]json.RawMessage{stampKey: json.RawMessage(strconv.FormatFloat(now, 'f', -1, 64)), valueKey: encodedValue})
	if err != nil {
		slog.Debug("failed to save "+what, "error", err)
		return
	}
	kept[key] = encoded
	if err := fileutil.WriteJSON(ctx, path, kept, fileutil.JSONStyle{Lines: true, Tight: true}); err != nil {
		slog.Debug("failed to save "+what, "error", err)
	}
}

func (c *Catalog) localProbeGet(kind, key string) (json.RawMessage, bool) {
	return memoGet(home.LocalEndpointProbes.In(c.home), kind+":"+key, localProbeDiskTTL, "ts", "value")
}

func (c *Catalog) localProbePut(ctx context.Context, kind, key string, value any) {
	memoPut(ctx, home.LocalEndpointProbes.In(c.home), kind+":"+key, value, localProbeDiskTTL, "ts", "value", "local probe disk cache")
}
