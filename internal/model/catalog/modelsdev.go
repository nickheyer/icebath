package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	ModelsDevURL        = "https://models.dev/api.json"
	modelsDevTTL        = 4 * time.Hour
	modelsDevRetryDelay = 300 * time.Second
)

var (
	errNotModified     = errors.New("catalog: the models.dev registry is unchanged")
	errInvalidRegistry = errors.New("catalog: models.dev returned an empty or invalid registry")
)

type mdModel struct {
	id               string
	name             string
	family           string
	knowledge        string
	releaseDate      string
	status           string
	limits           map[string]float64
	flags            map[string]bool
	hasModalities    bool
	inputIsList      bool
	inputMods        []string
	outputMods       []string
	cost             map[string]float64
	interleaved      bool
	interleavedField string
}

type mdProvider struct {
	id        string
	valid     bool
	name      string
	env       []string
	api       string
	doc       string
	hasModels bool
	order     []string
	models    map[string]*mdModel
}

type mdRegistry struct {
	order     []string
	providers map[string]*mdProvider
}

type modelsDev struct {
	state      sync.Mutex
	cache      *mdRegistry
	cacheTime  time.Time
	retryAfter time.Time
	inFlight   bool
	fetch      sync.Mutex
}

var modelFlagNames = []string{"reasoning", "tool_call", "attachment", "temperature", "structured_output", "open_weights"}

func stringList(v wire.Value) []string {
	var out []string
	for _, item := range v.Elems() {
		if text, ok := item.Str(); ok {
			out = append(out, text)
		}
	}
	return out
}

func parseModelEntry(id string, raw wire.Value) *mdModel {
	m := &mdModel{
		id: id, name: raw.Get("name").Text(), family: raw.Get("family").Text(), knowledge: raw.Get("knowledge").Text(),
		releaseDate: raw.Get("release_date").Text(), status: raw.Get("status").Text(),
		limits: map[string]float64{}, flags: map[string]bool{}, cost: map[string]float64{},
	}
	for _, field := range raw.Get("limit").Fields() {
		if n, ok := field.Value.Float(); ok {
			m.limits[field.Key] = n
		}
	}
	for _, name := range modelFlagNames {
		if flag := raw.Get(name); flag.Exists() {
			m.flags[name] = flag.Truthy()
		}
	}
	if mods := raw.Get("modalities"); mods.Exists() {
		m.hasModalities = true
		if input := mods.Get("input"); input.IsArray() {
			m.inputIsList, m.inputMods = true, stringList(input)
		}
		m.outputMods = stringList(mods.Get("output"))
	}
	for _, field := range raw.Get("cost").Fields() {
		if rate, ok := scalarFloat(field.Value); ok {
			m.cost[field.Key] = rate
		}
	}
	if inter := raw.Get("interleaved"); inter.Truthy() {
		m.interleaved = true
		m.interleavedField = inter.Get("field").Text()
	}
	return m
}

func parseRegistry(data []byte) (*mdRegistry, error) {
	root, err := wire.ParseValue(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, err
	}
	fields := root.Fields()
	if !root.IsObject() || len(fields) == 0 {
		return nil, errInvalidRegistry
	}
	reg := &mdRegistry{providers: make(map[string]*mdProvider, len(fields))}
	for _, field := range fields {
		p := &mdProvider{id: field.Key, models: map[string]*mdModel{}}
		if _, seen := reg.providers[field.Key]; !seen {
			reg.order = append(reg.order, field.Key)
		}
		reg.providers[field.Key] = p
		raw := field.Value
		if !raw.IsObject() {
			continue
		}
		p.valid = true
		p.name, p.api, p.doc = raw.Get("name").Text(), raw.Get("api").Text(), raw.Get("doc").Text()
		p.env = stringList(raw.Get("env"))
		models := raw.Get("models")
		p.hasModels = !models.Exists() || models.IsObject()
		for _, entry := range models.Fields() {
			if _, seen := p.models[entry.Key]; !seen {
				p.order = append(p.order, entry.Key)
			}
			if entry.Value.IsObject() {
				p.models[entry.Key] = parseModelEntry(entry.Key, entry.Value)
			} else {
				p.models[entry.Key] = nil
			}
		}
	}
	return reg, nil
}

func (r *mdRegistry) modelCount() int {
	total := 0
	for _, p := range r.providers {
		total += len(p.order)
	}
	return total
}

func (c *Catalog) modelsDevCachePath() string { return home.ModelsDevCache.In(c.home) }

func (c *Catalog) modelsDevETagPath() string { return home.ModelsDevCacheETag.In(c.home) }

func (c *Catalog) loadModelsDevETag() string {
	data, err := os.ReadFile(c.modelsDevETagPath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("failed to load the models.dev ETag", "error", err)
		}
		return ""
	}
	return strings.TrimSpace(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
}

func (c *Catalog) clearModelsDevETag() {
	if err := os.Remove(c.modelsDevETagPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("failed to clear the models.dev ETag", "error", err)
	}
}

func (c *Catalog) quarantineModelsDevCache() {
	if err := os.Rename(c.modelsDevCachePath(), home.ModelsDevCacheCorrupt.In(c.home)); err != nil {
		slog.Debug("could not quarantine the corrupt models.dev cache", "error", err)
	}
	c.clearModelsDevETag()
}

func (c *Catalog) loadModelsDevDisk() *mdRegistry {
	data, err := os.ReadFile(c.modelsDevCachePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		slog.Warn("failed to load the models.dev disk cache, quarantining", "error", err)
		c.quarantineModelsDevCache()
		return nil
	}
	reg, err := parseRegistry(data)
	if err != nil {
		if errors.Is(err, errInvalidRegistry) {
			slog.Warn("models.dev disk cache is corrupt or empty, quarantining (will fetch again from the network)")
		} else {
			slog.Warn("failed to load the models.dev disk cache, quarantining", "error", err)
		}
		c.quarantineModelsDevCache()
		return nil
	}
	return reg
}

func (c *Catalog) modelsDevDiskAge() (time.Duration, bool) {
	info, err := os.Stat(c.modelsDevCachePath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("failed to stat the models.dev disk cache", "error", err)
		}
		return 0, false
	}
	age := time.Since(info.ModTime())
	return age, age >= 0
}

func (c *Catalog) saveModelsDevDisk(ctx context.Context, raw []byte, etag string) {
	if err := fileutil.WriteJSON(ctx, c.modelsDevCachePath(), json.RawMessage(raw), fileutil.JSONStyle{Tight: true}); err != nil {
		slog.Debug("failed to save the models.dev disk cache", "error", err)
	}
	if etag == "" {
		return
	}
	if err := fileutil.WriteText(ctx, c.modelsDevETagPath(), etag); err != nil {
		slog.Debug("failed to save the models.dev ETag", "error", err)
	}
}

func (c *Catalog) modelsDevURL(ctx context.Context) string {
	if cfg := c.config(ctx); cfg != nil {
		if override := strings.TrimSpace(cfg.ModelsDev.URL); override != "" {
			return override
		}
	}
	return ModelsDevURL
}

func (c *Catalog) fetchModelsDev(ctx context.Context, conditional bool) (*mdRegistry, []byte, string, error) {
	header := http.Header{}
	if conditional {
		if etag := c.loadModelsDevETag(); etag != "" {
			header.Set("If-None-Match", etag)
		}
	}
	resp, err := c.get(ctx, c.modelsDevURL(ctx), header, "", fetchPhases)
	if err != nil {
		return nil, nil, "", err
	}
	if resp.status == http.StatusNotModified {
		return nil, nil, "", errNotModified
	}
	if !resp.success() {
		return nil, nil, "", &httpclient.StatusError{Code: resp.status, Status: http.StatusText(resp.status), Method: http.MethodGet, Host: "models.dev"}
	}
	body := bytes.TrimPrefix(resp.body, []byte("\xef\xbb\xbf"))
	reg, err := parseRegistry(body)
	if err != nil {
		return nil, nil, "", err
	}
	return reg, body, resp.header.Get("ETag"), nil
}

func (c *Catalog) refreshModelsDevLocked(ctx context.Context, where string) (*mdRegistry, bool) {
	md := &c.md
	md.state.Lock()
	held := md.cache
	md.state.Unlock()
	reg, raw, etag, err := c.fetchModelsDev(ctx, held != nil)
	now := time.Now()
	switch {
	case err == nil:
		c.saveModelsDevDisk(ctx, raw, etag)
		md.state.Lock()
		md.cache, md.cacheTime, md.retryAfter = reg, now, time.Time{}
		md.state.Unlock()
		slog.Debug("refreshed the models.dev registry", "where", where, "providers", len(reg.order), "models", reg.modelCount())
		return reg, true
	case errors.Is(err, errNotModified):
		md.state.Lock()
		defer md.state.Unlock()
		if md.cache == nil {
			c.clearModelsDevETag()
			md.retryAfter = now.Add(modelsDevRetryDelay)
			slog.Warn("models.dev answered 304 but no cached registry is held, cleared the ETag sidecar and will fetch again unconditionally", "where", where)
			return nil, true
		}
		md.cacheTime, md.retryAfter = now, time.Time{}
		slog.Debug("models.dev registry unchanged (304 Not Modified), cache confirmed fresh again", "where", where)
		return md.cache, true
	default:
		md.state.Lock()
		md.retryAfter = now.Add(modelsDevRetryDelay)
		md.state.Unlock()
		slog.Debug("models.dev refresh failed, retry suppressed", "where", where, "seconds", modelsDevRetryDelay.Seconds(), "error", httpclient.Message(err))
		return nil, false
	}
}

func (c *Catalog) startModelsDevRefresh() {
	md := &c.md
	md.state.Lock()
	if time.Now().Before(md.retryAfter) || md.inFlight {
		md.state.Unlock()
		return
	}
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if closed {
		md.state.Unlock()
		return
	}
	md.inFlight = true
	md.state.Unlock()
	go func() {
		defer c.wg.Done()
		md.fetch.Lock()
		c.refreshModelsDevLocked(c.bg, "background")
		md.fetch.Unlock()
		md.state.Lock()
		md.inFlight = false
		md.state.Unlock()
	}()
}

func (c *Catalog) serveStaleModelsDev(message string, age time.Duration) *mdRegistry {
	md := &c.md
	md.state.Lock()
	if grace := time.Now().Add(-modelsDevTTL + modelsDevRetryDelay); grace.After(md.cacheTime) {
		md.cacheTime = grace
	}
	held := md.cache
	md.state.Unlock()
	c.startModelsDevRefresh()
	slog.Debug(message, "age_seconds", age.Seconds())
	return held
}

func (c *Catalog) modelsDevRegistry(ctx context.Context, allowNetwork bool) *mdRegistry {
	md := &c.md
	if !allowNetwork {
		md.state.Lock()
		defer md.state.Unlock()
		if md.cache == nil {
			if disk := c.loadModelsDevDisk(); disk != nil {
				md.cache, md.cacheTime = disk, time.Time{}
				if age, ok := c.modelsDevDiskAge(); ok {
					md.cacheTime = time.Now().Add(-age)
				}
			}
		}
		return md.cache
	}
	md.state.Lock()
	if md.cache != nil && time.Since(md.cacheTime) < modelsDevTTL {
		held := md.cache
		md.state.Unlock()
		return held
	}
	if md.cache != nil {
		md.state.Unlock()
		return c.serveStaleModelsDev("using the stale in-memory models.dev cache, refreshing in background", 0)
	}
	if age, ok := c.modelsDevDiskAge(); ok {
		if disk := c.loadModelsDevDisk(); disk != nil {
			md.cache = disk
			if age >= modelsDevTTL {
				md.state.Unlock()
				return c.serveStaleModelsDev("using the stale models.dev disk cache, refreshing in background", age)
			}
			md.cacheTime = time.Now().Add(-age)
			md.state.Unlock()
			slog.Debug("loaded models.dev from the fresh disk cache", "providers", len(disk.order), "age_seconds", age.Seconds())
			return disk
		}
	}
	if time.Now().Before(md.retryAfter) {
		held := md.cache
		md.state.Unlock()
		return held
	}
	md.state.Unlock()
	md.fetch.Lock()
	defer md.fetch.Unlock()
	md.state.Lock()
	if md.cache != nil || time.Now().Before(md.retryAfter) {
		held := md.cache
		md.state.Unlock()
		return held
	}
	md.state.Unlock()
	if served, ok := c.refreshModelsDevLocked(ctx, "foreground"); ok {
		return served
	}
	md.state.Lock()
	defer md.state.Unlock()
	if md.cache == nil {
		md.cache, md.cacheTime = c.loadModelsDevDisk(), time.Time{}
		if md.cache != nil {
			slog.Debug("loaded the stale models.dev disk cache", "providers", len(md.cache.order))
		}
	}
	return md.cache
}
