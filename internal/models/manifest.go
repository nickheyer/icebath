package models

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Online catalog as icebath ships it, what every read of it falls back to
//
//go:embed model-catalog.json
var shippedManifest []byte

const (
	manifestCache = "model_catalog.json"
	// Newest version of the catalog's shape this build reads
	manifestVersion = 1
	// Time an address that failed to answer is not asked again in this process, while nothing is kept on disk
	manifestRetry = 5 * time.Minute
	manifestWait  = 8 * time.Second
)

// One model of a provider's block of the online catalog
type ManifestModel struct {
	ID          string
	Description string
	// The model a route of the provider lands on when none is picked
	Default bool
}

// Catalog of the profile: a fresh copy kept on disk as it is, a stale one while a refresh runs in the background, else fetched now, the
// shipped one when none of that works; empty while the online catalog is turned off
func (m *Models) manifest(cfg *config.Config, refresh bool) map[string]any {
	if !cfg.GetBool(modelsconfig.CatalogEnabled) {
		return map[string]any{}
	}
	s := m.of(cfg)
	address := strings.TrimSpace(cfg.GetString(modelsconfig.CatalogURL))
	if address == "" {
		address = modelsconfig.DefaultCatalogURL
	}
	ttl := m.RefreshInterval(cfg)
	kept, at := s.readManifest()
	if !refresh && kept != nil {
		s.mu.Lock()
		held, heldAt := s.manifest, s.manifestAt
		s.mu.Unlock()
		if time.Since(at) < ttl && held != nil && heldAt.Equal(at) {
			return held
		}
		if time.Since(at) >= ttl {
			s.background("manifest", func() {
				if fetched := s.fetchManifest(address); fetched != nil {
					s.writeManifest(fetched)
				}
			})
		}
		return s.holdManifest(kept, at)
	}
	s.mu.Lock()
	waiting := time.Now().Before(s.manifestRetry[address])
	s.mu.Unlock()
	if !refresh && kept == nil && waiting {
		return shipped()
	}
	fetched := s.fetchManifest(address)
	s.mu.Lock()
	if fetched == nil {
		s.manifestRetry[address] = time.Now().Add(manifestRetry)
	} else {
		delete(s.manifestRetry, address)
	}
	s.mu.Unlock()
	if fetched != nil {
		s.writeManifest(fetched)
		if written, at := s.readManifest(); written != nil {
			return s.holdManifest(written, at)
		}
		return s.holdManifest(fetched, time.Now())
	}
	if kept != nil {
		return s.holdManifest(kept, at)
	}
	return shipped()
}

func (s *state) holdManifest(data map[string]any, at time.Time) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifest, s.manifestAt = data, at
	return data
}

// Copy of the catalog on disk and when it was written, nil when missing or of a shape this build can't read
func (s *state) readManifest() (map[string]any, time.Time) {
	path := s.file(manifestCache)
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	data, ok := decodeManifest(raw)
	if !ok {
		return nil, time.Time{}
	}
	return data, info.ModTime()
}

func (s *state) writeManifest(data map[string]any) {
	if err := writeJSON(s.file(manifestCache), data); err != nil {
		slog.Info("model catalog cache write failed", "error", err)
	}
}

// Catalog at the address, else at the mirror of the shipped one; nil when neither gives a valid one
func (s *state) fetchManifest(address string) map[string]any {
	if data := s.getManifest(address); data != nil {
		return data
	}
	if address != modelsconfig.MirrorCatalogURL {
		if data := s.getManifest(modelsconfig.MirrorCatalogURL); data != nil {
			slog.Info("model catalog primary URL failed, using fallback", "url", modelsconfig.MirrorCatalogURL)
			return data
		}
	}
	return nil
}

// Catalog one address gives, nil when it doesn't answer with a valid one
func (s *state) getManifest(target string) map[string]any {
	reply, err := send(context.Background(), "GET", target, map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}, nil, manifestWait, true)
	if err != nil {
		slog.Info("model catalog fetch failed", "url", target, "error", err)
		return nil
	}
	if reply.status >= 400 {
		slog.Info("model catalog fetch failed", "url", target, "status", reply.status)
		return nil
	}
	data, ok := decodeManifest(reply.body)
	if !ok {
		slog.Info("model catalog failed schema validation", "url", target)
		return nil
	}
	return data
}

// Catalog of a body when it has the shape: a version this build reads and providers whose models each have an id
func decodeManifest(raw []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	decoder.UseNumber()
	var data map[string]any
	if decoder.Decode(&data) != nil {
		return nil, false
	}
	version, ok := data["version"].(json.Number)
	if n, err := version.Int64(); !ok || err != nil || n > manifestVersion {
		return nil, false
	}
	providers, ok := data["providers"].(map[string]any)
	if !ok {
		return nil, false
	}
	for _, raw := range providers {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		models, ok := block["models"].([]any)
		if !ok {
			return nil, false
		}
		for _, item := range models {
			entry, ok := item.(map[string]any)
			if id, isText := entry["id"].(string); !ok || !isText || strings.TrimSpace(id) == "" {
				return nil, false
			}
		}
	}
	return data, true
}

// Catalog shipped with icebath, a fresh copy each time
func shipped() map[string]any {
	data, ok := decodeManifest(shippedManifest)
	if !ok {
		panic("models: the shipped model catalog is not a valid catalog")
	}
	return data
}

func block(manifest map[string]any, kind string) map[string]any {
	providers, _ := manifest["providers"].(map[string]any)
	b, _ := providers[kind].(map[string]any)
	return b
}

// Models the online catalog lists for the kind, from the catalog its models.catalog.providers entry names first; nil when it lists none
func (m *Models) Manifest(cfg *config.Config, kind string, refresh bool) []ManifestModel {
	b := m.override(cfg, kind)
	if b == nil {
		b = block(m.manifest(cfg, refresh), kind)
	}
	return manifestModels(b)
}

func manifestModels(b map[string]any) []ManifestModel {
	items, _ := b["models"].([]any)
	var out []ManifestModel
	for _, item := range items {
		entry, _ := item.(map[string]any)
		id := strings.TrimSpace(text(entry["id"]))
		if id == "" {
			continue
		}
		on, _ := entry["default"].(bool)
		out = append(out, ManifestModel{ID: id, Description: text(entry["description"]), Default: on})
	}
	return out
}

// Block of a catalog a provider's own entry of models.catalog.providers points at, nil when it names none or it can't be read
func (m *Models) override(cfg *config.Config, kind string) map[string]any {
	if !cfg.GetBool(modelsconfig.CatalogEnabled) {
		return nil
	}
	providers, _ := cfg.Get(modelsconfig.CatalogProviders).(map[string]any)
	entry, _ := providers[kind].(map[string]any)
	target := strings.TrimSpace(text(entry["url"]))
	if target == "" {
		return nil
	}
	s := m.of(cfg)
	s.mu.Lock()
	waiting := time.Now().Before(s.manifestRetry["override:"+target])
	s.mu.Unlock()
	if waiting {
		return nil
	}
	data := s.getManifest(target)
	s.mu.Lock()
	if data == nil {
		s.manifestRetry["override:"+target] = time.Now().Add(manifestRetry)
	} else {
		delete(s.manifestRetry, "override:"+target)
	}
	s.mu.Unlock()
	return block(data, kind)
}

// Model the online catalog labels the kind's default, from what is held or kept, the shipped catalog else; nothing is fetched
func (m *Models) ManifestDefault(cfg *config.Config, kind string) string {
	s := m.of(cfg)
	s.mu.Lock()
	held := s.manifest
	s.mu.Unlock()
	for _, source := range []func() map[string]any{
		func() map[string]any { return held },
		func() map[string]any { kept, _ := s.readManifest(); return kept },
		shipped,
	} {
		for _, model := range manifestModels(block(source(), kind)) {
			if model.Default {
				return model.ID
			}
		}
	}
	return ""
}

// Whether a provider is left out of the model pickers, by any of its names
func (m *Models) Hidden(cfg *config.Config, names ...string) bool {
	excluded := cfg.GetStringSlice(modelsconfig.CatalogExcluded)
	for _, name := range names {
		for _, hidden := range excluded {
			if name != "" && strings.EqualFold(strings.TrimSpace(hidden), name) {
				return true
			}
		}
	}
	return false
}

// How long a fetched online catalog is used before it is fetched again, at least a minute
func (m *Models) RefreshInterval(cfg *config.Config) time.Duration {
	return time.Duration(max(cfg.GetInt(modelsconfig.CatalogTTL), 1)) * time.Minute
}

// Fetches the online catalog again now, the addresses that failed asked again too; false while it is turned off
func (m *Models) RefreshCatalogs(cfg *config.Config) bool {
	if !cfg.GetBool(modelsconfig.CatalogEnabled) {
		return false
	}
	s := m.of(cfg)
	s.mu.Lock()
	clear(s.manifestRetry)
	s.mu.Unlock()
	return len(m.manifest(cfg, true)) > 0
}
