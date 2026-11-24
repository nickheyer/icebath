package pet

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Public petdex manifest and the fetch, cache and download bounds of the store
const (
	ManifestURL     = "https://petdex.dev/api/manifest"
	manifestTimeout = 10 * time.Second
	manifestTTL     = 300 * time.Second
	downloadTimeout = 60 * time.Second
	petdexUserAgent = "icebath-agent-petdex"
)

// One pet row of the public petdex manifest
type ManifestEntry struct {
	Slug, DisplayName, Kind, SubmittedBy, SpritesheetURL, PetJSONURL, ZipURL string
}

// Manifest row from its dict with the display name, kind and URLs defaulted
func ManifestEntryFromDict(d *jsonutil.Object) ManifestEntry {
	text := func(key string) string { return anyutil.String(d.ValueOr(key, "")) }
	orText := func(key string, fallback any) string {
		v := d.ValueOr(key, "")
		if !anyutil.IsTrue(v) {
			v = fallback
		}
		return anyutil.String(v)
	}
	return ManifestEntry{Slug: strings.TrimSpace(text("slug")), DisplayName: orText("displayName", d.ValueOr("slug", "")),
		Kind: orText("kind", "pet"), SubmittedBy: orText("submittedBy", ""), SpritesheetURL: orText("spritesheetUrl", ""),
		PetJSONURL: orText("petJsonUrl", ""), ZipURL: orText("zipUrl", "")}
}

// Raised when the manifest cannot be fetched or parsed
type ManifestError struct{ Msg string }

func (e *ManifestError) Error() string     { return e.Msg }
func (e *ManifestError) ErrorKind() string { return "ManifestError" }

// Short lived manifest cache and the single prefetch in flight
var manifestCache struct {
	sync.Mutex
	at          time.Time
	entries     []ManifestEntry
	warm        bool
	prefetching bool
}

// Whether the cache holds a manifest younger than the TTL, the caller holding the lock
func cacheIsWarmLocked() bool {
	return manifestCache.warm && time.Since(manifestCache.at) < manifestTTL
}

// Warms the manifest cache on its own goroutine, idempotent and never blocking
func Prefetch(ctx context.Context, env *tools.Env) {
	manifestCache.Lock()
	if cacheIsWarmLocked() || manifestCache.prefetching {
		manifestCache.Unlock()
		return
	}
	manifestCache.prefetching = true
	manifestCache.Unlock()
	go func() {
		defer func() {
			manifestCache.Lock()
			manifestCache.prefetching = false
			manifestCache.Unlock()
		}()
		if _, err := FetchManifest(context.WithoutCancel(ctx), env, false); err != nil {
			pkgLog().Debug("petdex manifest prefetch failed: " + err.Error())
		}
	}()
}

// SSRF vetted GET following redirects with the petdex user agent, raising on HTTP errors
func httpGet(ctx context.Context, env *tools.Env, rawURL string, timeout time.Duration) (*httputil.Response, error) {
	if !tools.IsSafeURL(env, rawURL) {
		return nil, errors.New("Pet store URL failed the SSRF safety check: " + rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", petdexUserAgent)
	resp, err := httputil.Do(tools.NewSSRFSafeClient(env, timeout, true), req, httputil.EachPhase(timeout))
	if err != nil {
		return nil, err
	}
	if err := resp.CheckStatus(); err != nil {
		return nil, err
	}
	return resp, nil
}

// str of a fetch failure, the httpx text for transport errors
func fetchErrText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return err.Error()
	}
	return err.Error()
}

// Every approved pet of the public manifest, cached for five minutes unless forced
func FetchManifest(ctx context.Context, env *tools.Env, force bool) ([]ManifestEntry, error) {
	manifestCache.Lock()
	if !force && cacheIsWarmLocked() {
		entries := manifestCache.entries
		manifestCache.Unlock()
		return entries, nil
	}
	manifestCache.Unlock()
	payload, err := func() (any, error) {
		if !tools.IsSafeURL(env, ManifestURL) {
			return nil, &ManifestError{"Pet manifest URL failed the SSRF safety check: " + ManifestURL}
		}
		resp, err := httpGet(ctx, env, ManifestURL, manifestTimeout)
		if err != nil {
			return nil, err
		}
		return resp.JSON()
	}()
	if err != nil {
		return nil, &ManifestError{"could not fetch petdex manifest: " + fetchErrText(err)}
	}
	var pets []any
	if obj, ok := payload.(*jsonutil.Object); ok {
		pets, _ = obj.ValueOr("pets", nil).([]any)
	}
	if pets == nil {
		return nil, &ManifestError{"petdex manifest had no 'pets' array"}
	}
	entries := []ManifestEntry{}
	for _, raw := range pets {
		if d, ok := raw.(*jsonutil.Object); ok {
			if e := ManifestEntryFromDict(d); e.Slug != "" && e.SpritesheetURL != "" {
				entries = append(entries, e)
			}
		}
	}
	manifestCache.Lock()
	manifestCache.at, manifestCache.entries, manifestCache.warm = time.Now(), entries, true
	manifestCache.Unlock()
	return entries, nil
}

// Manifest entry of a slug compared case insensitively, nil when not listed
func FindEntry(ctx context.Context, env *tools.Env, slug string) (*ManifestEntry, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	entries, err := FetchManifest(ctx, env, false)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if strings.ToLower(entries[i].Slug) == slug {
			return &entries[i], nil
		}
	}
	return nil, nil
}
