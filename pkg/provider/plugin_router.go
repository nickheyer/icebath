package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Error the upstream disk mirror hit raises, its TTL name is never defined in the router module
var errRouterDiskTTLUndefined = errors.New("name '_DISK_TTL_SECONDS' is not defined")

// Efforts map and once only flags for one home, nil efforts meaning unknown
type routerCacheState struct {
	efforts     map[string][]string
	warmStarted bool
	diskChecked bool
}

var (
	routerMu          sync.Mutex
	routerModuleState = &routerCacheState{}
	routerStateByHome = map[string]*routerCacheState{}
	routerWarmSpawn   = func(refresh func()) { go refresh() }
	// Test seam standing in for the upstream pytest guard, consulted before the once only flag
	routerWarmSuppressed = func() bool { return false }
)

// Profiles the bundled router plugin registers
func routerPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "router", display: "Ramp Router", mode: ModeCodexResponses,
		desc:   "Ramp Router (router.com) \u2014 routes each request to the cheapest model that clears your quality bar",
		signup: "https://app.router.com/keys", keys: []string{"RAMP_ROUTER_API_KEY", "ROUTER_API_KEY", "RAMP_ROUTER_BASE_URL"},
		base: routerBaseURL(context.Background()), authType: AuthTypeAPIKey, vision: true,
	}, "ramp-router", "ramp", "router.com")
	p.LazyHeaders = NewLazyHeaders(HeaderEntry{Key: "User-Agent", Fn: func() string { return UserAgent("Icebath-Agent") }})
	p.DefaultAuxModel = "gpt-5.4-mini"
	p.FetchModelsHook = routerFetchModels
	p.SupportedReasoningEffortsHook = routerSupportedEfforts
	return []Profile{p}
}

// Module slot without a home override, else the active home's slot
func routerState(ctx context.Context) *routerCacheState {
	if home.Override(ctx) == "" {
		return routerModuleState
	}
	routerMu.Lock()
	defer routerMu.Unlock()
	key := home.Key(home.Dir(ctx))
	st := routerStateByHome[key]
	if st == nil {
		st = &routerCacheState{}
		routerStateByHome[key] = st
	}
	return st
}

// First non empty value of the variables from the home dotenv, then the process environment
func routerEnvValue(ctx context.Context, vars ...string) string {
	h := home.Dir(ctx)
	for _, resolve := range []func(string) string{
		func(k string) string { v, _ := config.GetEnvValuePreferDotenv(ctx, h, k); return v },
		os.Getenv,
	} {
		for _, k := range vars {
			if v := strings.TrimSpace(resolve(k)); v != "" {
				return v
			}
		}
	}
	return ""
}

// Router base URL from the profile dotenv then the environment, the public endpoint otherwise
func routerBaseURL(ctx context.Context) string {
	if v := strings.TrimRight(routerEnvValue(ctx, "RAMP_ROUTER_BASE_URL"), "/"); v != "" {
		return v
	}
	return RouterBaseURL
}

func routerAPIKey(ctx context.Context) string {
	return routerEnvValue(ctx, "RAMP_ROUTER_API_KEY", "ROUTER_API_KEY")
}

// Efforts map from a models data array, unsupported models empty and ladder unknown levels dropped
func routerParseEfforts(items any) map[string][]string {
	list, ok := items.([]any)
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for _, item := range list {
		obj, _ := item.(map[string]any)
		mid := ""
		if obj != nil && pyTruthy(obj["id"]) {
			mid = strings.TrimSpace(pyScalarStr(obj["id"]))
		}
		reasoning, isDict := digDict(obj, "router", "capabilities", "reasoning")
		if mid == "" || !isDict {
			continue
		}
		if reasoning["supported"] == false {
			out[mid] = []string{}
			continue
		}
		var levels []string
		efforts, _ := reasoning["efforts"].([]any)
		for _, e := range efforts {
			if em, ok := e.(map[string]any); ok {
				v := ""
				if pyTruthy(em["value"]) {
					v = strings.TrimSpace(pyScalarStr(em["value"]))
				}
				if v != "" {
					levels = append(levels, v)
				}
			}
		}
		var known, unknown []string
		for _, lv := range levels {
			if slices.Contains(effortLadder, lv) {
				known = append(known, lv)
			} else {
				unknown = append(unknown, lv)
			}
		}
		if len(unknown) > 0 {
			slog.Info(fmt.Sprintf("router: model %s publishes unrecognized reasoning effort level(s) %s; ignoring them "+
				"(update agent/reasoning_effort EFFORT_LADDER to adopt new vendor tiers)", mid, pyStrListRepr(unknown)))
		}
		if len(known) > 0 {
			out[mid] = known
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Nested dict lookup, false as soon as a level is missing or not a dict
func digDict(obj map[string]any, keys ...string) (map[string]any, bool) {
	cur := obj
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, cur != nil
}

func routerDiskPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "cache", "router_catalog.json")
}

// Writes the efforts mirror through a temp file so readers never see a torn file
func routerSaveDisk(ctx context.Context, efforts map[string][]string) {
	path := routerDiskPath(ctx)
	data, err := json.Marshal(map[string]any{"ts": float64(time.Now().UnixNano()) / 1e9, "efforts": efforts})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	if err == nil {
		err = os.WriteFile(tmp, data, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		slog.Debug("router: caps disk mirror write failed: " + err.Error())
	}
}

// Disk mirror efforts map, nil when unreadable, empty, or its stamp fails the upstream float parse
func routerLoadDisk(ctx context.Context) map[string][]string {
	raw, err := os.ReadFile(routerDiskPath(ctx))
	if err != nil {
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		return nil
	}
	decoded, _, err := pyJSONLoads(raw)
	if err != nil {
		return nil
	}
	data, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	efforts, ok := data["efforts"].(map[string]any)
	if !ok || len(efforts) == 0 {
		return nil
	}
	parsed := map[string][]string{}
	for mid, levels := range efforts {
		list, ok := levels.([]any)
		if !ok {
			continue
		}
		out := make([]string, 0, len(list))
		for _, lv := range list {
			out = append(out, pyStr(lv))
		}
		parsed[mid] = out
	}
	ts := data["ts"]
	if !pyTruthy(ts) {
		ts = 0.0
	}
	if _, err := catalogFloat(ts); err != nil || len(parsed) == 0 {
		return nil
	}
	return parsed
}

// Parses a models payload into the active home's memory cache and its disk mirror
func routerSeedEfforts(ctx context.Context, items any) map[string][]string {
	parsed := routerParseEfforts(items)
	if parsed != nil {
		st := routerState(ctx)
		routerMu.Lock()
		st.efforts = parsed
		routerMu.Unlock()
		routerSaveDisk(ctx, parsed)
	}
	return parsed
}

// Raw models data array from the Router catalog, false on any failure
func routerFetchCatalogItems(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]any, bool) {
	if baseURL == "" {
		baseURL = routerBaseURL(ctx)
	}
	if apiKey == "" {
		apiKey = routerAPIKey(ctx)
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": profileUserAgent()}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	data, err := modelsGetJSON(ctx, strings.TrimRight(baseURL, "/")+"/models", timeout, headers, nil)
	if err != nil {
		slog.Debug("router: catalog fetch failed: " + err.Error())
		return nil, false
	}
	items := data
	if obj, ok := data.(map[string]any); ok {
		if items, ok = obj["data"]; !ok {
			items = []any{}
		}
	}
	list, ok := items.([]any)
	return list, ok
}

// Memory cache, else the disk mirror read once per home, a mirror hit installing itself and failing like upstream
func routerEffortsCacheOnly(ctx context.Context) (map[string][]string, error) {
	st := routerState(ctx)
	routerMu.Lock()
	cached, checked := st.efforts, st.diskChecked
	if cached == nil && !checked {
		st.diskChecked = true
	}
	routerMu.Unlock()
	if cached != nil || checked {
		return cached, nil
	}
	parsed := routerLoadDisk(ctx)
	if parsed == nil {
		return nil, nil
	}
	routerMu.Lock()
	if st.efforts == nil {
		st.efforts = parsed
	}
	routerMu.Unlock()
	return nil, errRouterDiskTTLUndefined
}

// Refreshes the efforts cache in the background at most once per home and only with a key
func routerWarmEffortsAsync(ctx context.Context) {
	if routerWarmSuppressed() {
		return
	}
	st := routerState(ctx)
	routerMu.Lock()
	if st.warmStarted {
		routerMu.Unlock()
		return
	}
	st.warmStarted = true
	routerMu.Unlock()
	if routerAPIKey(ctx) == "" {
		return
	}
	bg := context.WithoutCancel(ctx)
	routerWarmSpawn(func() {
		if items, ok := routerFetchCatalogItems(bg, "", "", 8*time.Second); ok {
			routerSeedEfforts(bg, items)
		}
	})
}

// Live key scoped catalog in Router order deduplicated, the same payload seeding the efforts cache
func routerFetchModels(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	items, ok := routerFetchCatalogItems(ctx, apiKey, baseURL, timeout)
	if !ok {
		return nil, false
	}
	routerSeedEfforts(ctx, items)
	var ids []string
	for _, it := range items {
		if obj, ok := it.(map[string]any); ok && pyTruthy(obj["id"]) {
			if id := pyScalarStr(obj["id"]); !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids, len(ids) > 0
}

// Catalog declared effort vocabulary from the cache only, a cold cache starting the warmer
func routerSupportedEfforts(ctx context.Context, model string) ([]string, bool, error) {
	mid := strings.TrimSpace(model)
	if mid == "" {
		return nil, false, nil
	}
	efforts, err := routerEffortsCacheOnly(ctx)
	if err != nil {
		return nil, false, err
	}
	if efforts == nil {
		routerWarmEffortsAsync(ctx)
		return nil, false, nil
	}
	levels, ok := efforts[mid]
	if !ok {
		return nil, false, nil
	}
	return append([]string{}, levels...), true, nil
}
