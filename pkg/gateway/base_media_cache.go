package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Audio extensions the gateway delivers natively with their MIME types
var audioMIMETypes = map[string]string{
	".ogg": "audio/ogg", ".opus": "audio/opus", ".mp3": "audio/mpeg", ".m2a": "audio/mpeg",
	".wav": "audio/wav", ".m4a": "audio/m4a", ".flac": "audio/flac",
}

// Image extensions platforms deliver as documents, routed to the image cache
var SupportedImageDocumentTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif",
}

// Extension order of the document, video and image tables as hermes declares them
var (
	documentTypeOrder = []string{".pdf", ".md", ".txt", ".csv", ".log", ".json", ".xml", ".yaml", ".yml", ".toml", ".ini",
		".cfg", ".zip", ".doc", ".xls", ".ppt", ".docx", ".xlsx", ".pptx", ".ts", ".py", ".sh"}
	videoTypeOrder         = []string{".mp4", ".mov", ".webm", ".mkv", ".avi"}
	imageDocumentTypeOrder = []string{".jpg", ".jpeg", ".png", ".webp", ".gif"}
)

// MIME to extension lookup where the earliest extension of each table wins
var mimeToExt = func() map[string]string {
	out := map[string]string{}
	add := func(table map[string]string, order []string) {
		for i := len(order) - 1; i >= 0; i-- {
			out[table[order[i]]] = order[i]
		}
	}
	add(mediacache.SupportedDocumentTypes, documentTypeOrder)
	add(mediacache.SupportedVideoTypes, videoTypeOrder)
	add(SupportedImageDocumentTypes, imageDocumentTypeOrder)
	return out
}()

// Byte cache writers under the active profile home, swappable the way hermes module globals are
var (
	cacheImageFromBytes = func(ctx context.Context, data []byte, ext string) (string, error) {
		return mediacache.CacheImageFromBytes(ctx, home.Dir(ctx), data, ext)
	}
	cacheAudioFromBytes = func(ctx context.Context, data []byte, ext string) (string, error) {
		return mediacache.CacheAudioFromBytes(ctx, home.Dir(ctx), data, ext)
	}
	cacheVideoFromBytes = func(ctx context.Context, data []byte, ext string) (string, error) {
		return mediacache.CacheVideoFromBytes(ctx, home.Dir(ctx), data, ext)
	}
	cacheDocumentFromBytes = func(ctx context.Context, data []byte, filename string) (string, error) {
		return mediacache.CacheDocumentFromBytes(home.Dir(ctx), data, filename)
	}
	cacheMediaBytesFn = CacheMediaBytes
)

// Runs a blocking cache write on its own goroutine and returns its result or the context error
func runOffLoop[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Caches image bytes without blocking the caller
func CacheImageFromBytesAsync(ctx context.Context, data []byte, ext string) (string, error) {
	fn := cacheImageFromBytes
	return runOffLoop(ctx, func() (string, error) { return fn(ctx, data, ext) })
}

// Caches audio bytes without blocking the caller
func CacheAudioFromBytesAsync(ctx context.Context, data []byte, ext string) (string, error) {
	fn := cacheAudioFromBytes
	return runOffLoop(ctx, func() (string, error) { return fn(ctx, data, ext) })
}

// Caches video bytes without blocking the caller
func CacheVideoFromBytesAsync(ctx context.Context, data []byte, ext string) (string, error) {
	fn := cacheVideoFromBytes
	return runOffLoop(ctx, func() (string, error) { return fn(ctx, data, ext) })
}

// Caches document bytes without blocking the caller
func CacheDocumentFromBytesAsync(ctx context.Context, data []byte, filename string) (string, error) {
	fn := cacheDocumentFromBytes
	return runOffLoop(ctx, func() (string, error) { return fn(ctx, data, filename) })
}

// One cached attachment with its host path, recorded MIME, kind and transcript name
type CachedMedia struct {
	Path        string
	MediaType   string
	Kind        string
	DisplayName string
}

// Transcript line pointing the agent at the file through the sandbox visible path of the active scope
func (m *CachedMedia) ContextNote(ctx context.Context) string {
	return fmt.Sprintf("[%s '%s' saved at: %s]", m.Kind, m.DisplayName, tools.ToAgentVisibleCachePath(mediaToolsEnv(ctx), m.Path))
}

// File extension from the filename, else from the MIME table
func resolveMediaExt(filename, mimeType string) string {
	ext := ""
	if filename != "" {
		_, e := pypath.Splitext(filename)
		ext = pystr.Lower(e)
	}
	if ext != "" {
		return ext
	}
	return mimeToExt[pystr.Lower(mimeType)]
}

// Filename with every character outside word characters, dot, dash and space replaced by underscore
func mediaDisplayName(filename string) string {
	var b strings.Builder
	for _, r := range filename {
		if pystr.IsAlnum(r) || r == '_' || r == '.' || r == '-' || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Classifies attachment bytes by extension, MIME and default kind and caches them, nil only for an image failing validation
func CacheMediaBytes(ctx context.Context, data []byte, filename, mimeType, defaultKind string) (*CachedMedia, error) {
	ext := resolveMediaExt(filename, mimeType)
	mime := pystr.Lower(mimeType)
	display := strings.TrimLeft(ext, ".")
	if filename != "" {
		display = mediaDisplayName(filename)
	} else if display == "" {
		display = "file"
	}
	kinds := []struct {
		kind, defaultExt string
		table            map[string]string
		cache            func(context.Context, []byte, string) (string, error)
		passthrough      bool
	}{
		{"image", ".jpg", SupportedImageDocumentTypes, cacheImageFromBytes, true},
		{"video", ".mp4", mediacache.SupportedVideoTypes, cacheVideoFromBytes, false},
		{"audio", ".ogg", audioMIMETypes, cacheAudioFromBytes, true},
	}
	for _, k := range kinds {
		_, extKnown := k.table[ext]
		if !(strings.HasPrefix(mime, k.kind+"/") || extKnown || defaultKind == k.kind) {
			continue
		}
		kindExt := k.defaultExt
		if extKnown {
			kindExt = ext
		}
		path, err := k.cache(ctx, data, kindExt)
		if err != nil {
			var valueErr *mediacache.ValueError
			if k.kind == "image" && errors.As(err, &valueErr) {
				return nil, nil
			}
			return nil, err
		}
		outMime := k.table[kindExt]
		if k.passthrough && strings.HasPrefix(mime, k.kind+"/") {
			outMime = mime
		}
		return &CachedMedia{Path: path, MediaType: outMime, Kind: k.kind, DisplayName: display}, nil
	}
	fallbackName := filename
	if fallbackName == "" {
		fallbackName = "document.bin"
		if ext != "" {
			fallbackName = "document" + ext
		}
	}
	path, err := cacheDocumentFromBytes(ctx, data, fallbackName)
	if err != nil {
		return nil, err
	}
	outMime := mediacache.SupportedDocumentTypes[ext]
	if outMime == "" {
		outMime = mime
	}
	if outMime == "" {
		outMime = "application/octet-stream"
	}
	if display == "" {
		display = fallbackName
	}
	return &CachedMedia{Path: path, MediaType: outMime, Kind: "document", DisplayName: display}, nil
}

// Classifies and caches attachment bytes without blocking the caller
func CacheMediaBytesAsync(ctx context.Context, data []byte, filename, mimeType, defaultKind string) (*CachedMedia, error) {
	fn := cacheMediaBytesFn
	return runOffLoop(ctx, func() (*CachedMedia, error) { return fn(ctx, data, filename, mimeType, defaultKind) })
}
