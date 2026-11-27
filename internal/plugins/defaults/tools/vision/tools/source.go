package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	visionconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
)

const (
	// Most bytes of an image or video read from anywhere
	maxMedia = 50 << 20
	// Agent some sites want before they hand out media
	browser = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// Extensions of the videos the tool reads, with the type each is sent as
var videoTypes = map[string]string{".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/mov", ".avi": "video/mp4", ".mkv": "video/mp4",
	".mpeg": "video/mpeg", ".mpg": "video/mpeg"}

// Bytes and type of the image a source names: a data: URL, an http(s) URL, or a file path or file:// URI on this machine
func resolve(ctx context.Context, call *tools.Call, source string) ([]byte, string, error) {
	source = strings.TrimSpace(source)
	var data []byte
	switch {
	case source == "":
		return nil, "", &prepError{"image_url is required"}
	case strings.HasPrefix(source, "data:"):
		var err error
		if data, err = dataURL(source); err != nil {
			return nil, "", &prepError{err.Error()}
		}
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		if !web.SafeURL(call.Env.Config, source) {
			return nil, "", &prepError{"blocked: unsafe or private URL"}
		}
		if block := web.Blocked(call.Env.Config, source); block != nil {
			return nil, "", &prepError{block.Message}
		}
		var err error
		if data, err = download(ctx, call, source, images); err != nil {
			return nil, "", err
		}
	case scheme.MatchString(source) && !strings.HasPrefix(strings.ToLower(source), "file://"):
		return nil, "", &prepError{"Unrecognized image source scheme. Use an http(s) URL, a local file path, a file:// URI, or a data: URL."}
	default:
		var err error
		if _, data, err = file(call, source); err != nil {
			return nil, "", err
		}
	}
	if len(data) > maxMedia {
		return nil, "", &prepError{"media exceeds size limit"}
	}
	if mime := sniff(data); mime != "" {
		return data, mime, nil
	}
	if svg(data) {
		return data, "image/svg+xml", nil
	}
	return nil, "", &prepError{"source is not a recognized image"}
}

// Bytes of a base64 data: URL, whatever type it claims
func dataURL(source string) ([]byte, error) {
	header, payload, _ := strings.Cut(source, ",")
	if !strings.Contains(header, ";base64") {
		return nil, errors.New("data: URL must be base64-encoded")
	}
	if len(payload)*3/4 > maxMedia {
		return nil, errors.New("data: URL exceeds size limit")
	}
	if at := strings.IndexAny(payload, "\r\n"); at >= 0 {
		return nil, fmt.Errorf("invalid base64 in data: URL: Only base64 data is allowed (non-alphabet character at offset %d)", at)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 in data: URL: %w", err)
	}
	return data, nil
}

// Path and bytes of a file on this machine, ~ expanded and relative to the working directory
func file(call *tools.Call, source string) (string, []byte, error) {
	if len(source) >= 7 && strings.EqualFold(source[:7], "file://") {
		source = source[7:]
	}
	expanded := home(source)
	path := expanded
	if !filepath.IsAbs(path) {
		path = filepath.Join(call.Env.Cwd, path)
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, &prepError{fmt.Sprintf("media file not found: '%s'", filepath.Clean(expanded))}
	}
	if info.Size() > maxMedia {
		return "", nil, &prepError{"media exceeds size limit"}
	}
	data, err := os.ReadFile(path)
	return path, data, err
}

// Path with a leading ~ or ~user turned into that home folder
func home(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	name, rest, _ := strings.Cut(path[1:], string(filepath.Separator))
	var dir string
	if name == "" {
		dir, _ = os.UserHomeDir()
	} else if account, err := user.Lookup(name); err == nil {
		dir = account.HomeDir
	}
	if dir == "" {
		return path
	}
	return filepath.Join(dir, rest)
}

// Kind of media a download fetches
type kind struct {
	label  string
	accept string
	// Retries every failure, not only those a later try may get past
	stubborn bool
}

var (
	images = kind{label: "Image", accept: "image/*,*/*;q=0.8"}
	videos = kind{label: "Video", accept: "video/*,*/*;q=0.8", stubborn: true}
)

// Refusal of a download a later try would get no further with
type refusal struct{ text string }

func (e *refusal) Error() string { return e.text }

// Failed status of a download
type status struct {
	code int
	url  string
}

func (e *status) Error() string {
	class := "Client error"
	switch {
	case e.code < 200:
		class = "Informational response"
	case e.code < 400:
		class = "Redirect response"
	case e.code >= 500:
		class = "Server error"
	}
	return fmt.Sprintf("%s '%d %s' for url '%s'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", class, e.code, http.StatusText(e.code), e.url, e.code)
}

// Media at an address, tried up to three times. An image tries again only after a failure that may pass: the network, a rate limit or the
// server; a video after any
func download(ctx context.Context, call *tools.Call, address string, media kind) ([]byte, error) {
	limit := 60 * time.Second
	if media == images {
		limit = time.Duration(max(call.Env.Config.GetInt(auxiliary.Task{Name: visionconfig.Task}.Key(visionconfig.DownloadTimeout)), 1)) * time.Second
	}
	var last error
	for attempt := range 3 {
		data, err := fetch(ctx, call, address, media, limit)
		if err == nil {
			return data, nil
		}
		last = err
		var refused *refusal
		var failed *status
		final := errors.As(err, &refused) || errors.As(err, &failed) && failed.code >= 400 && failed.code < 500 && failed.code != 429
		if attempt == 2 || !media.stubborn && final {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(2<<attempt) * time.Second):
		}
	}
	return nil, last
}

// One try at the media, stopped when no data comes for the time limit
func fetch(ctx context.Context, call *tools.Call, address string, media kind, limit time.Duration) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	late := false
	idle := time.AfterFunc(limit, func() {
		mu.Lock()
		late = true
		mu.Unlock()
		cancel()
	})
	defer idle.Stop()
	timedOut := func(err error) error {
		mu.Lock()
		defer mu.Unlock()
		if late {
			return fmt.Errorf("%s download timed out after %.0fs: %w", media.label, limit.Seconds(), err)
		}
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", browser)
	request.Header.Set("Accept", media.accept)
	response, err := web.Client(call.Env.Config, 0, true).Do(request)
	if err != nil {
		var blocked *web.BlockedError
		if errors.As(err, &blocked) {
			return nil, &refusal{blocked.Message}
		}
		return nil, timedOut(err)
	}
	defer response.Body.Close()
	idle.Reset(limit)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &status{code: response.StatusCode, url: response.Request.URL.String()}
	}
	if declared, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64); err == nil && declared > maxMedia {
		return nil, &refusal{fmt.Sprintf("%s too large (%d bytes, max %d)", media.label, declared, maxMedia)}
	}
	if block := web.Blocked(call.Env.Config, response.Request.URL.String()); block != nil {
		return nil, &refusal{block.Message}
	}
	var data []byte
	buffer := make([]byte, 64<<10)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			idle.Reset(limit)
			if data = append(data, buffer[:n]...); len(data) > maxMedia {
				return nil, &refusal{fmt.Sprintf("%s too large (%d bytes, max %d)", media.label, len(data), maxMedia)}
			}
		}
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return nil, timedOut(err)
		}
	}
}

// Path of the video a source names, and its type: a file on this machine, or one downloaded from an http(s) URL
func materialize(ctx context.Context, call *tools.Call, source string) ([]byte, string, error) {
	invalid := errors.New("Invalid video source. Provide an HTTP/HTTPS URL or a valid local file path.")
	if path, data, err := file(call, source); err == nil {
		mime := videoTypes[strings.ToLower(filepath.Ext(path))]
		if mime == "" {
			return nil, "", unsupported(filepath.Ext(path))
		}
		return data, mime, nil
	} else if !errors.As(err, new(*prepError)) {
		return nil, "", err
	}
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") || !web.SafeURL(call.Env.Config, source) {
		return nil, "", invalid
	}
	if block := web.Blocked(call.Env.Config, source); block != nil {
		return nil, "", &refusal{block.Message}
	}
	data, err := download(ctx, call, source, videos)
	return data, "video/mp4", err
}

func unsupported(extension string) error {
	return fmt.Errorf("Unsupported video format: '%s'. Supported: .avi, .mkv, .mov, .mp4, .mpeg, .mpg, .webm", extension)
}
