package pet

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Public petdex manifest and the fetch, cache and download bounds of the store
const (
	ManifestURL     = "https://petdex.dev/api/manifest"
	manifestTimeout = 10 * time.Second
	manifestTTL     = 300 * time.Second
	downloadTimeout = 60 * time.Second
	petdexUserAgent = "icebath-agent-petdex"
)

var manifestLog = slog.Default().With("logger", "agent.pet.manifest")

// One pet row of the public petdex manifest
type ManifestEntry struct {
	Slug, DisplayName, Kind, SubmittedBy, SpritesheetURL, PetJSONURL, ZipURL string
}

// Manifest row from its dict with the display name, kind and URLs defaulted
func ManifestEntryFromDict(d *pyjson.Object) ManifestEntry {
	text := func(key string) string { return pyrepr.Str(d.GetOr(key, "")) }
	orText := func(key string, fallback any) string {
		v := d.GetOr(key, "")
		if !pyrepr.Truthy(v) {
			v = fallback
		}
		return pyrepr.Str(v)
	}
	return ManifestEntry{Slug: pystr.Strip(text("slug")), DisplayName: orText("displayName", d.GetOr("slug", "")),
		Kind: orText("kind", "pet"), SubmittedBy: orText("submittedBy", ""), SpritesheetURL: orText("spritesheetUrl", ""),
		PetJSONURL: orText("petJsonUrl", ""), ZipURL: orText("zipUrl", "")}
}

// Raised when the manifest cannot be fetched or parsed
type ManifestError struct{ Msg string }

func (e *ManifestError) Error() string      { return e.Msg }
func (e *ManifestError) PyTypeName() string { return "ManifestError" }

// Short lived manifest cache and the single prefetch in flight
var manifestCache struct {
	sync.Mutex
	at          time.Time
	entries     []ManifestEntry
	warm        bool
	prefetching bool
}

// Drops the cached manifest so the next fetch hits the network
func ClearManifestCache() {
	manifestCache.Lock()
	defer manifestCache.Unlock()
	manifestCache.warm, manifestCache.entries = false, nil
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
			manifestLog.Debug("petdex manifest prefetch failed: " + err.Error())
		}
	}()
}

// SSRF vetted GET following redirects with the petdex user agent, raising on HTTP errors
func httpGet(ctx context.Context, env *tools.Env, rawURL string, timeout time.Duration) (*pyhttpx.Response, error) {
	if !tools.IsSafeURL(env, rawURL) {
		return nil, errors.New("Pet store URL failed the SSRF safety check: " + rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", petdexUserAgent)
	resp, err := pyhttpx.Do(tools.NewSSRFSafeClient(env, timeout, true), req, timeout)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(resp.StatusErrorText(rawURL))
	}
	return resp, nil
}

// str of a fetch failure, the httpx text for transport errors
func fetchErrText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return pyhttpx.ErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
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
	if obj, ok := payload.(*pyjson.Object); ok {
		pets, _ = obj.GetOr("pets", nil).([]any)
	}
	if pets == nil {
		return nil, &ManifestError{"petdex manifest had no 'pets' array"}
	}
	entries := []ManifestEntry{}
	for _, raw := range pets {
		if d, ok := raw.(*pyjson.Object); ok {
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
	slug = pystr.Lower(pystr.Strip(slug))
	entries, err := FetchManifest(ctx, env, false)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if pystr.Lower(entries[i].Slug) == slug {
			return &entries[i], nil
		}
	}
	return nil, nil
}
