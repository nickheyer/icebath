package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	CodexModelsCatalogEndpoint = "https://chatgpt.com/backend-api/codex/models"
	CodexNewestClientVersion   = "99.0.0"
	CodexUngatedClientVersion  = "0.0.0"
	codexContextTTL            = time.Hour
	codexContextNegativeTTL    = 300 * time.Second
	codexCacheMax              = 128
)

type ContextSource string

const (
	SourceLive       ContextSource = "live"
	SourceMemory     ContextSource = "memory"
	SourceFallback   ContextSource = "fallback"
	SourcePortal     ContextSource = "portal"
	SourceOpenRouter ContextSource = "openrouter"
)

type codexMemo struct {
	windows map[string]int
	at      time.Time
}

type codexListing struct {
	entries []wire.Value
	at      time.Time
}

func CodexCatalogURLs(baseURL string) []string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint := CodexModelsCatalogEndpoint
	if base != "" {
		endpoint = base + "/models"
	}
	return []string{endpoint + "?client_version=" + CodexNewestClientVersion, endpoint + "?client_version=" + CodexUngatedClientVersion}
}

func CodexCatalogProbeAllowed(accessToken auth.Secret, baseURL string) bool {
	token := accessToken.Reveal()
	if token == "" {
		return false
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = CodexModelsCatalogEndpoint
	}
	if !providers.HostMatches(base, "chatgpt.com") {
		return true
	}
	claims, ok := auth.JWTClaims(token)
	if !ok {
		return false
	}
	parsed, err := wire.ParseValue(claims)
	return err == nil && parsed.IsObject() && len(parsed.Fields()) > 0
}

func CodexTokenFingerprint(accessToken auth.Secret, baseURL string) string {
	sum := sha256.Sum256([]byte(accessToken.Reveal() + "\n" + strings.TrimRight(strings.TrimSpace(baseURL), "/")))
	return hex.EncodeToString(sum[:])[:16]
}

func (c *Catalog) FetchCodexCatalogEntries(ctx context.Context, header http.Header, baseURL string) ([]wire.Value, int, error) {
	return c.fetchCodexCatalogEntries(c.scoped(ctx), header, baseURL)
}

func (c *Catalog) fetchCodexCatalogEntries(ctx context.Context, header http.Header, baseURL string) ([]wire.Value, int, error) {
	status := 0
	for _, target := range CodexCatalogURLs(baseURL) {
		resp, err := c.get(ctx, target, header, "", fetchPhases)
		if err != nil {
			return nil, status, err
		}
		status = resp.status
		if status != http.StatusOK {
			continue
		}
		data, err := resp.value()
		if err != nil {
			return nil, status, err
		}
		if entries := data.Get("models").Elems(); data.IsObject() && len(entries) > 0 {
			return entries, status, nil
		}
	}
	return nil, status, nil
}

func (c *Catalog) boundCodexCaches(key string) {
	if _, ok := c.codex[key]; ok || len(c.codex) < codexCacheMax {
		return
	}
	oldest, first := "", true
	for k, memo := range c.codex {
		if first || memo.at.Before(c.codex[oldest].at) {
			oldest, first = k, false
		}
	}
	delete(c.codex, oldest)
	delete(c.codexMax, oldest)
}

func (c *Catalog) rememberNoCodexCatalog(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if current, ok := c.codex[key]; ok && len(current.windows) > 0 && now.Sub(current.at) < codexContextTTL {
		return
	}
	c.boundCodexCaches(key)
	c.codex[key] = codexMemo{windows: map[string]int{}, at: now.Add(-codexContextTTL + codexContextNegativeTTL)}
}

func (c *Catalog) rememberCodexListing(key string, entries []wire.Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.codexRaw[key]; !ok && len(c.codexRaw) >= codexCacheMax {
		oldest, first := "", true
		for k, listing := range c.codexRaw {
			if first || listing.at.Before(c.codexRaw[oldest].at) {
				oldest, first = k, false
			}
		}
		delete(c.codexRaw, oldest)
	}
	c.codexRaw[key] = codexListing{entries: entries, at: time.Now()}
}

func (c *Catalog) codexContextLengths(ctx context.Context, accessToken auth.Secret, baseURL string) (map[string]int, bool) {
	if !CodexCatalogProbeAllowed(accessToken, baseURL) {
		return nil, false
	}
	now := time.Now()
	key := CodexTokenFingerprint(accessToken, baseURL)
	c.mu.Lock()
	cached, ok := c.codex[key]
	c.mu.Unlock()
	if ok && now.Sub(cached.at) < codexContextTTL {
		return cached.windows, false
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+accessToken.Reveal())
	for name, value := range auth.CodexAccountHeaders(accessToken.Reveal()) {
		header.Set(name, value)
	}
	entries, status, err := c.fetchCodexCatalogEntries(ctx, header, baseURL)
	if err != nil {
		slog.Debug("Codex /models probe failed", "error", httpclient.Message(err))
		c.rememberNoCodexCatalog(key)
		return nil, false
	}
	if status != http.StatusOK {
		slog.Debug("Codex /models probe was refused, using the built-in windows", "status", status)
		c.rememberNoCodexCatalog(key)
		return nil, false
	}
	c.rememberCodexListing(key, entries)
	windows, ceilings := map[string]int{}, map[string]int{}
	for _, item := range entries {
		slug, isText := item.Get("slug").Str()
		window, hasWindow := jsonInt(item.Get("context_window"))
		if !item.IsObject() || !isText || !hasWindow || window <= 0 {
			continue
		}
		slug = strings.TrimSpace(slug)
		windows[slug] = window
		if ceiling, ok := jsonInt(item.Get("max_context_window")); ok && ceiling > 0 {
			ceilings[slug] = ceiling
		}
	}
	if len(windows) == 0 {
		c.rememberNoCodexCatalog(key)
		return nil, false
	}
	c.mu.Lock()
	c.boundCodexCaches(key)
	c.codexMax[key] = ceilings
	c.codex[key] = codexMemo{windows: windows, at: now}
	c.mu.Unlock()
	return windows, true
}

func (c *Catalog) CodexContextLengths(ctx context.Context, accessToken auth.Secret, baseURL string) (windows map[string]int, fresh bool) {
	return c.codexContextLengths(c.scoped(ctx), accessToken, baseURL)
}

func (c *Catalog) CodexCatalogListing(accessToken auth.Secret, baseURL string) (entries []wire.Value, fetched time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	listing, ok := c.codexRaw[CodexTokenFingerprint(accessToken, baseURL)]
	return listing.entries, listing.at, ok
}

func codexCatalogKey(windows map[string]int, slug string) (string, bool) {
	if _, ok := windows[slug]; ok {
		return slug, true
	}
	for key := range windows {
		if strings.EqualFold(key, slug) {
			return key, true
		}
	}
	return "", false
}

func (c *Catalog) cachedCodexCatalogMax(accessToken auth.Secret, baseURL, slug string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	published := c.codexMax[CodexTokenFingerprint(accessToken, baseURL)]
	key, ok := codexCatalogKey(published, slug)
	if !ok {
		return 0, false
	}
	return published[key], true
}

func (c *Catalog) codexOAuthContextLength(ctx context.Context, model string, accessToken auth.Secret, baseURL string) (int, ContextSource, bool) {
	modelBare := strings.TrimSpace(c.StripProviderPrefix(ctx, model))
	if modelBare == "" {
		return 0, "", false
	}
	bump := func(window int, source ContextSource, ceiling int) (int, ContextSource, bool) {
		verified, ok := verifiedCodexWindow(modelBare)
		if ok && window == codexStaleAdvertised {
			if ceiling > 0 {
				verified = min(verified, ceiling)
			}
			slog.Debug("Codex OAuth context raised from the advertised window to the verified one", "model", modelBare, "advertised", window, "verified", verified)
			return verified, source, true
		}
		return window, source, true
	}
	lookup := BareCodexSlug(StripCodexContextVariantSuffix(modelBare))
	ceiling := 0
	if !accessToken.Empty() {
		live, fresh := c.codexContextLengths(ctx, accessToken, baseURL)
		ceiling, _ = c.cachedCodexCatalogMax(accessToken, baseURL, lookup)
		if slug, ok := codexCatalogKey(live, lookup); ok {
			source := SourceMemory
			if fresh {
				source = SourceLive
			}
			return bump(live[slug], source, ceiling)
		}
	}
	if hit, ok := longestKeyMatch(codexOAuthFallback, strings.ToLower(lookup)); ok {
		return bump(hit.window, SourceFallback, ceiling)
	}
	return 0, "", false
}
