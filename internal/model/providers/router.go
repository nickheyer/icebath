package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	RouterDefaultBaseURL = "https://api.router.com/v1"
	routerDiskTTL        = 24 * time.Hour
)

type routerHomeState struct {
	efforts     map[string][]string
	warmStarted bool
	diskChecked bool
}

type routerProfile struct {
	base
	d      Deps
	mu     sync.Mutex
	states map[string]*routerHomeState
}

func newRouter(d Deps) Profile {
	return &routerProfile{d: d, states: map[string]*routerHomeState{}, base: base{spec: Spec{
		Name: "router", Aliases: []string{"ramp-router", "ramp", "router.com"}, Mode: wire.ModeResponses, DisplayName: "Ramp Router",
		Description: "Ramp Router (router.com) — routes each request to the cheapest model that clears your quality bar",
		SignupURL:   "https://app.router.com/keys", EnvVars: []string{"RAMP_ROUTER_API_KEY", "ROUTER_API_KEY", "RAMP_ROUTER_BASE_URL"},
		BaseURL: routerStartupBaseURL(d), AuthType: authTypeAPIKey,
		DefaultHeaders: map[string]string{"User-Agent": userAgent(d, "icebath")}, Vision: true, DefaultAuxModel: "gpt-5.4-mini",
		FallbackModels: []string{},
	}}}
}

func routerStartupBaseURL(d Deps) string {
	return routerBaseURL(context.Background(), d)
}

func routerBaseURL(ctx context.Context, d Deps) string {
	if v := strings.TrimRight(strings.TrimSpace(d.envPreferFile(ctx, "RAMP_ROUTER_BASE_URL")), "/"); v != "" {
		return v
	}
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("RAMP_ROUTER_BASE_URL")), "/"); v != "" {
		return v
	}
	return RouterDefaultBaseURL
}

func (p *routerProfile) baseURL(ctx context.Context) string {
	return routerBaseURL(ctx, p.d)
}

func (p *routerProfile) apiKey(ctx context.Context) string {
	for _, name := range []string{"RAMP_ROUTER_API_KEY", "ROUTER_API_KEY"} {
		if v := strings.TrimSpace(p.d.envPreferFile(ctx, name)); v != "" {
			return v
		}
	}
	for _, name := range []string{"RAMP_ROUTER_API_KEY", "ROUTER_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func (p *routerProfile) state(ctx context.Context) (*routerHomeState, string) {
	homeDir := ""
	if p.d.Homes != nil {
		if h, err := p.d.Homes.Home(ctx); err == nil {
			homeDir = h
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.states[homeDir]
	if !ok {
		st = &routerHomeState{}
		p.states[homeDir] = st
	}
	return st, homeDir
}

func parseRouterEfforts(items []wire.Value) map[string][]string {
	ladder := wire.EffortLadder()
	out := map[string][]string{}
	for _, item := range items {
		mid := ""
		if id := item.Get("id"); id.Truthy() {
			mid = strings.TrimSpace(id.Display())
		}
		reasoning := item.Path("router", "capabilities", "reasoning")
		if !item.IsObject() || mid == "" || !reasoning.IsObject() {
			continue
		}
		if reasoning.Get("supported").False() {
			out[mid] = []string{}
			continue
		}
		levels, unknown := []string{}, []string{}
		for _, e := range reasoning.Get("efforts").Elems() {
			if !e.IsObject() {
				continue
			}
			if v := strings.TrimSpace(e.Get("value").Display()); v != "" && e.Get("value").Truthy() {
				if slices.Contains(ladder, v) {
					levels = append(levels, v)
				} else {
					unknown = append(unknown, v)
				}
			}
		}
		if len(unknown) > 0 {
			slog.Info(fmt.Sprintf("router: model %s publishes unrecognized reasoning effort level(s) %v; ignoring them", mid, unknown))
		}
		if len(levels) > 0 {
			out[mid] = levels
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (p *routerProfile) fetchItems(ctx context.Context, key, baseURL string, timeout time.Duration) ([]wire.Value, bool) {
	if baseURL == "" {
		baseURL = p.baseURL(ctx)
	}
	if key == "" {
		key = p.apiKey(ctx)
	}
	header := http.Header{}
	if key != "" {
		header.Set("Authorization", "Bearer "+key)
	}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", p.d.Net.UserAgent())
	data, err := p.d.Net.getJSON(ctx, strings.TrimRight(baseURL, "/")+"/models", timeout, header)
	if err != nil {
		slog.Debug(fmt.Sprintf("router: catalog fetch failed: %v", err))
		return nil, false
	}
	items := catalogItems(data)
	if !data.IsArray() && !data.Get("data").IsArray() {
		return nil, false
	}
	return items, true
}

func (p *routerProfile) seed(ctx context.Context, items []wire.Value) {
	parsed := parseRouterEfforts(items)
	if parsed == nil {
		return
	}
	st, homeDir := p.state(ctx)
	p.mu.Lock()
	st.efforts = parsed
	p.mu.Unlock()
	if homeDir == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"ts": float64(time.Now().UnixNano()) / 1e9, "efforts": parsed})
	path := home.RouterCatalog.In(homeDir)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("router: caps disk mirror write failed: %v", err))
	}
}

func (p *routerProfile) loadDisk(homeDir string) (map[string][]string, time.Duration, bool) {
	raw, err := os.ReadFile(home.RouterCatalog.In(homeDir))
	if err != nil {
		return nil, 0, false
	}
	v, err := wire.ParseValue([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")))
	if err != nil {
		return nil, 0, false
	}
	efforts := v.Get("efforts")
	if !efforts.IsObject() || len(efforts.Fields()) == 0 {
		return nil, 0, false
	}
	parsed := map[string][]string{}
	for _, f := range efforts.Fields() {
		if !f.Value.IsArray() {
			continue
		}
		levels := []string{}
		for _, lv := range f.Value.Elems() {
			levels = append(levels, lv.Display())
		}
		parsed[f.Key] = levels
	}
	age := routerDiskTTL
	if ts, ok := v.Get("ts").Float(); ok {
		age = max(0, time.Since(time.Unix(0, int64(ts*1e9))))
	}
	if len(parsed) == 0 {
		return nil, 0, false
	}
	return parsed, age, true
}

func (p *routerProfile) cached(ctx context.Context) map[string][]string {
	st, homeDir := p.state(ctx)
	p.mu.Lock()
	efforts, checked := st.efforts, st.diskChecked
	st.diskChecked = true
	p.mu.Unlock()
	if efforts != nil || checked || homeDir == "" {
		return efforts
	}
	parsed, age, ok := p.loadDisk(homeDir)
	if !ok {
		return nil
	}
	p.mu.Lock()
	if st.efforts == nil {
		st.efforts = parsed
	}
	efforts = st.efforts
	p.mu.Unlock()
	if age >= routerDiskTTL {
		p.warm(ctx)
	}
	return efforts
}

func (p *routerProfile) warm(ctx context.Context) {
	st, _ := p.state(ctx)
	p.mu.Lock()
	started := st.warmStarted
	st.warmStarted = true
	p.mu.Unlock()
	if started || p.apiKey(ctx) == "" {
		return
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		if items, ok := p.fetchItems(bg, "", "", defaultListTimeout); ok {
			p.seed(bg, items)
		}
	}()
}

func (p *routerProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	items, ok := p.fetchItems(ctx, in.APIKey.Reveal(), in.BaseURL, listTimeout(in))
	if !ok {
		return nil, false
	}
	p.seed(ctx, items)
	ids := []string{}
	for _, item := range items {
		if id := item.Get("id"); item.IsObject() && id.Truthy() && !slices.Contains(ids, id.Display()) {
			ids = append(ids, id.Display())
		}
	}
	return ids, len(ids) > 0
}

func (p *routerProfile) Efforts(ctx context.Context, model string) ([]string, bool) {
	mid := strings.TrimSpace(model)
	if mid == "" {
		return nil, false
	}
	efforts := p.cached(ctx)
	if efforts == nil {
		p.warm(ctx)
		return nil, false
	}
	levels, ok := efforts[mid]
	return slices.Clone(levels), ok
}
