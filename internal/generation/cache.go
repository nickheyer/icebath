package generation

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Folder of the profile what the services make is kept in, under the caches a sandbox is handed
const Generated = "cache/generated"

// Most redirects one download follows
const maxRedirects = 5

// How the files of a kind are kept and fetched
type store struct {
	folder string
	// Extensions of the content types it takes, and of the URL endings that name one when the type names none
	types      map[string]string
	extensions []string
	fallback   string
	label      string
	// Text of a download that brought nothing, {url} standing for the address
	empty   string
	timeout time.Duration
	max     int64
}

var stores = map[Kind]store{
	Image: {folder: "images", types: map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/webp": "webp", "image/gif": "gif"},
		extensions: []string{"png", "jpg", "jpeg", "webp", "gif"}, fallback: "png", label: "Image", empty: "Image at {url} returned 0 bytes; refusing to cache.",
		timeout: 60 * time.Second, max: 25 << 20},
	Video: {folder: "videos", types: map[string]string{"video/mp4": "mp4", "video/webm": "webm", "video/quicktime": "mov", "video/x-matroska": "mkv"},
		extensions: []string{"mp4", "webm", "mov", "mkv"}, fallback: "mp4", label: "Video", empty: "Video at {url} was empty (0 bytes).",
		timeout: 180 * time.Second, max: 200 << 20},
}

// Folder the profile keeps what was made of the kind in, made when missing
func Dir(cfg *config.Config, kind Kind) (string, error) {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), filepath.FromSlash(Generated), stores[kind].folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// New file of the cache of the kind, named for what made it and when
func (j *Job) path(kind Kind, prefix, extension string) (string, error) {
	dir, err := Dir(j.Config, kind)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s_%s_%s.%s", safe(prefix), time.Now().Format("20060102_150405"), strs.Random(4), extension)), nil
}

// Prefix fit for a file name: letters, digits, dashes, dots and underscores, anything else an underscore
func safe(prefix string) string {
	prefix = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, prefix)
	if strings.Trim(prefix, "._") == "" {
		return "media"
	}
	return prefix
}

// Saves the bytes into the cache of the kind, answering the path
func (j *Job) Save(kind Kind, data []byte, prefix, extension string) (string, error) {
	path, err := j.path(kind, prefix, extension)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Saves base64 data into the cache of the kind, answering the path
func (j *Job) Unpack(kind Kind, b64, prefix, extension string) (string, error) {
	data, err := Base64(b64)
	if err != nil {
		return "", err
	}
	return j.Save(kind, data, prefix, extension)
}

// Bytes of base64 in either alphabet, padded or not, whitespace ignored
func Base64(text string) ([]byte, error) {
	text = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return -1
		case r == '-':
			return '+'
		case r == '_':
			return '/'
		}
		return r
	}, text)
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(text, "="))
}

// Picture an images API answered with kept in the cache: its base64, else its URL downloaded, the URL itself when the download fails; a
// failure of the call when it carried neither or the base64 could not be saved. Label names the service in what the model reads
func (j *Job) Picture(ctx context.Context, call Call, b64, address, prefix, label, origin string) (string, *Result) {
	switch {
	case b64 != "":
		path, err := j.Unpack(Image, b64, prefix, "png")
		if err != nil {
			failure := call.Fail(fmt.Sprintf("Could not save image to cache: %v", err), "io_error")
			return "", &failure
		}
		return path, nil
	case address != "":
		path, err := j.Download(ctx, Image, address, Fetch{Prefix: prefix, Origin: origin})
		if err != nil && strings.HasPrefix(address, "data:") {
			failure := call.Fail(fmt.Sprintf("Could not save image to cache: %v", err), "io_error")
			return "", &failure
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("%s image URL %s could not be cached (%v); falling back to bare URL.", label, address, err))
			return address, nil
		}
		return path, nil
	}
	failure := call.Fail(label+" response contained neither b64_json nor URL", "empty_response")
	return "", &failure
}

// How one download goes, zero values taking the defaults of the kind
type Fetch struct {
	Prefix  string
	Timeout time.Duration
	Max     int64
	// Headers sent on the hops to the service's own endpoint, nowhere else
	Headers map[string]string
	// Refuses a reply whose content type is not one of the kind
	Strict bool
	// Address of the API the service was called on: a download from that very endpoint skips the private address checks, only cloud
	// metadata staying refused, and is the only place the headers go
	Origin string
}

// Streams a URL a service answered with into the cache of the kind, no partial file left behind, every hop checked against the private
// address rules except one on the service's own endpoint; a data URL is decoded into it
func (j *Job) Download(ctx context.Context, kind Kind, raw string, f Fetch) (string, error) {
	s := stores[kind]
	if f.Prefix == "" {
		f.Prefix = string(kind)
	}
	if f.Timeout <= 0 {
		f.Timeout = s.timeout
	}
	if f.Max <= 0 {
		f.Max = s.max
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
		data, declared, err := Inline(raw)
		if err != nil {
			return "", err
		}
		if int64(len(data)) > f.Max {
			return "", fmt.Errorf("%s data URL exceeds %dMB cap; refusing to cache.", s.label, f.Max>>20)
		}
		extension, ok := s.types[declared]
		if !ok {
			extension = s.fallback
		}
		return j.Save(kind, data, f.Prefix, extension)
	}
	current, headers := raw, f.Headers
	for range maxRedirects + 1 {
		var client *http.Client
		own := sameOrigin(current, f.Origin)
		if own {
			if web.AlwaysBlocked(current) {
				return "", fmt.Errorf("%s URL targets an always-blocked address: %s", s.label, current)
			}
			client = &http.Client{Timeout: f.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		} else {
			if !web.SafeURL(j.Config, current) {
				return "", fmt.Errorf("%s URL failed the SSRF safety check: %s", s.label, current)
			}
			client = web.Client(j.Config, f.Timeout, false)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		if own {
			for name, value := range headers {
				request.Header.Set(name, value)
			}
		}
		response, err := client.Do(request)
		if err != nil {
			return "", err
		}
		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
			location := response.Header.Get("Location")
			response.Body.Close()
			if location == "" {
				return "", fmt.Errorf("%s download redirected without a Location: %s", s.label, current)
			}
			next, err := response.Request.URL.Parse(location)
			if err != nil {
				return "", err
			}
			current, headers = next.String(), nil
			continue
		}
		path, err := j.keep(kind, s, f, raw, current, response)
		response.Body.Close()
		return path, err
	}
	return "", fmt.Errorf("%s download exceeded %d redirects: %s", s.label, maxRedirects, raw)
}

// Writes the body of a reply into the cache, its extension from the content type else the URL's ending, under the size cap
func (j *Job) keep(kind Kind, s store, f Fetch, original, current string, response *http.Response) (string, error) {
	if response.StatusCode < 200 || response.StatusCode > 299 {
		io.Copy(io.Discard, response.Body)
		return "", Status(response.StatusCode, current)
	}
	mime, _, _ := strings.Cut(response.Header.Get("Content-Type"), ";")
	mime = strings.ToLower(strings.TrimSpace(mime))
	extension, known := s.types[mime]
	if f.Strict && !known {
		return "", fmt.Errorf("%s download returned unexpected Content-Type %s", s.label, strs.Filled(mime, "(missing)"))
	}
	if !known {
		extension = s.fallback
		at, _, _ := strings.Cut(strings.ToLower(current), "?")
		for _, candidate := range s.extensions {
			if strings.HasSuffix(at, "."+candidate) {
				extension = strings.Replace(candidate, "jpeg", "jpg", 1)
				break
			}
		}
	}
	path, err := j.path(kind, f.Prefix, extension)
	if err != nil {
		return "", err
	}
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	written, err := io.Copy(file, io.LimitReader(response.Body, f.Max+1))
	if closed := file.Close(); err == nil {
		err = closed
	}
	switch {
	case err != nil:
	case written > f.Max:
		err = fmt.Errorf("%s at %s exceeds %dMB cap; refusing to cache.", s.label, original, f.Max>>20)
	case written == 0:
		err = errors.New(strings.ReplaceAll(s.empty, "{url}", original))
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// Failed status of a reply, in the words of the requests library the model knows
func Status(code int, address string) error {
	class := "unexpected status"
	switch code / 100 {
	case 3:
		class = "redirect"
	case 4:
		class = "client error"
	case 5:
		class = "server error"
	}
	if u, err := url.Parse(address); err == nil {
		address = u.Redacted()
	}
	return fmt.Errorf("%s %d %s for %s", class, code, http.StatusText(code), address)
}

// Whether the URL is served from the very scheme, host and port of the API at base, its authority plain enough that no client reads
// another host from it
func sameOrigin(raw, base string) bool {
	a, ok := origin(base)
	if !ok {
		return false
	}
	b, ok := origin(raw)
	return ok && a == b
}

// Scheme, host and port of an http URL, the scheme's own port filled in; false for any other URL or an authority with user info, escapes,
// backslashes, spaces or controls in it
func origin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", false
	}
	_, authority, _ := strings.Cut(raw, "://")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if strings.ContainsAny(authority, `@%\`) || strings.ContainsFunc(authority, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", false
	}
	scheme, port := strings.ToLower(u.Scheme), u.Port()
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
