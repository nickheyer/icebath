package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
)

const generatedSubdir = "generated"

var redirectStatusCodes = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

const maxSaveURLRedirects = 5

// Cache directory for one media kind under the home, parents created
func cacheDir(env *tools.Env, kind string) (string, error) {
	p := filepath.Join(homeOf(env), "cache", filepath.FromSlash(kind))
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// Timestamped and uuid suffixed cache file path for one media kind
func cachePath(env *tools.Env, kind, prefix, extension string) (string, error) {
	dir, err := cacheDir(env, kind)
	if err != nil {
		return "", err
	}
	ts := time.Now().Format("20060102_150405")
	return filepath.Join(dir, fmt.Sprintf("%s_%s_%s.%s", prefix, ts, randomHex(4), extension)), nil
}

// Writes raw bytes into the media cache and returns the absolute path
func saveBytes(env *tools.Env, kind string, raw []byte, prefix, extension string) (string, error) {
	path, err := cachePath(env, kind, prefix, extension)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Decodes base64 data into the media cache and returns the absolute path
func saveB64(env *tools.Env, kind, b64, prefix, extension string) (string, error) {
	raw, err := pybase64.B64Decode(b64)
	if err != nil {
		return "", err
	}
	return saveBytes(env, kind, raw, prefix, extension)
}

// Download policy of one saveURL call
type saveURLOptions struct {
	Prefix                  string
	Timeout                 time.Duration
	MaxBytes                int64
	ChunkSize               int
	ContentTypes            map[string]string
	URLExtensions           []string
	DefaultExtension        string
	Label                   string
	EmptyError              string
	Headers                 map[string]string
	RequireKnownContentType bool
	// API endpoint the profile is configured to call for the provider that produced the URL, empty when the URL has no such endpoint
	ProviderBase string
}

// Scheme, host and port a URL is served from, the scheme's own port filled in, false for anything that is not an http URL with a host
func urlOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	switch {
	case scheme != "http" && scheme != "https":
		return "", false
	case port == "" && scheme == "http":
		port = "80"
	case port == "":
		port = "443"
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), true
}

// Whether a download URL with an unambiguous authority is served from the very endpoint the provider's API was called on
func sameProviderOrigin(rawURL, providerBase string) bool {
	base, ok := urlOrigin(providerBase)
	if !ok {
		return false
	}
	if reason, err := tools.AuthorityAmbiguity(rawURL, false); err != nil || reason != "" {
		return false
	}
	target, ok := urlOrigin(rawURL)
	return ok && target == base
}

// Streams a provider URL into the media cache with no partial file left, every hop SSRF checked except one on the provider's own configured endpoint, where only cloud metadata addresses stay refused and the only place the request headers are sent
func saveURL(ctx context.Context, env *tools.Env, kind, rawURL string, o saveURLOptions) (string, error) {
	current, hopHeaders := rawURL, o.Headers
	for range maxSaveURLRedirects + 1 {
		var client *http.Client
		onProvider := sameProviderOrigin(current, o.ProviderBase)
		if onProvider {
			if tools.IsAlwaysBlockedURL(env, current) {
				return "", fmt.Errorf("%s URL targets an always-blocked address: %s", o.Label, current)
			}
			client = httputil.Client(false)
			client.Timeout = o.Timeout
		} else {
			if !tools.IsSafeURL(env, current) {
				return "", fmt.Errorf("%s URL failed the SSRF safety check: %s", o.Label, current)
			}
			client = tools.NewSSRFSafeClient(env, o.Timeout, false)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		if onProvider {
			for k, v := range hopHeaders {
				req.Header.Set(k, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		if redirectStatusCodes[resp.StatusCode] {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if location == "" {
				return "", fmt.Errorf("%s download redirected without a Location: %s", o.Label, current)
			}
			next, err := pyurl.Join(current, location)
			if err != nil {
				return "", err
			}
			current, hopHeaders = next, nil
			continue
		}
		path, err := writeSavedURLBody(env, kind, rawURL, current, resp, o)
		resp.Body.Close()
		return path, err
	}
	return "", fmt.Errorf("%s download exceeded %d redirects: %s", o.Label, maxSaveURLRedirects, rawURL)
}

// Picks the extension from the content type then the URL suffix and streams the capped body to disk
func writeSavedURLBody(env *tools.Env, kind, original, current string, resp *http.Response, o saveURLOptions) (string, error) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &httputil.StatusError{StatusCode: resp.StatusCode, URL: current, Location: resp.Header.Get("Location")}
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	extension, known := o.ContentTypes[contentType]
	if o.RequireKnownContentType && !known {
		shown := contentType
		if shown == "" {
			shown = "(missing)"
		}
		return "", fmt.Errorf("%s download returned unexpected Content-Type %s", o.Label, shown)
	}
	if !known {
		extension = o.DefaultExtension
		urlPath := strings.ToLower(strings.SplitN(current, "?", 2)[0])
		for _, ext := range o.URLExtensions {
			if strings.HasSuffix(urlPath, "."+ext) {
				extension = ext
				if ext == "jpeg" {
					extension = "jpg"
				}
				break
			}
		}
	}
	path, err := cachePath(env, kind, o.Prefix, extension)
	if err != nil {
		return "", err
	}
	fh, err := os.Create(path)
	if err != nil {
		return "", err
	}
	chunk := o.ChunkSize
	if chunk <= 0 {
		chunk = 64 * 1024
	}
	buf := make([]byte, chunk)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > o.MaxBytes {
				fh.Close()
				removeQuietly(path)
				return "", fmt.Errorf("%s at %s exceeds %dMB cap; refusing to cache.", o.Label, original, o.MaxBytes/(1024*1024))
			}
			if _, werr := fh.Write(buf[:n]); werr != nil {
				fh.Close()
				removeQuietly(path)
				return "", werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			fh.Close()
			removeQuietly(path)
			return "", rerr
		}
	}
	if err := fh.Close(); err != nil {
		removeQuietly(path)
		return "", err
	}
	if written == 0 {
		removeQuietly(path)
		return "", errors.New(strings.ReplaceAll(o.EmptyError, "{url}", original))
	}
	return path, nil
}
