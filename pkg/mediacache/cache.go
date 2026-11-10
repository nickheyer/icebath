package mediacache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inbound media cap applied when gateway.max_inbound_media_bytes is absent or unparseable
const DefaultInboundMediaMaxBytes int64 = 128 * 1024 * 1024

// Age in hours past which the cleanup sweeps delete cached media
const MediaCacheMaxAgeHours = 24

// Video extensions the gateway accepts with their MIME types
var SupportedVideoTypes = map[string]string{
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
}

// Document extensions the gateway accepts with their MIME types
var SupportedDocumentTypes = map[string]string{
	".pdf": "application/pdf", ".md": "text/markdown", ".txt": "text/plain", ".csv": "text/csv",
	".log": "text/plain", ".json": "application/json", ".xml": "application/xml",
	".yaml": "application/yaml", ".yml": "application/yaml", ".toml": "application/toml",
	".ini": "text/plain", ".cfg": "text/plain", ".zip": "application/zip",
	".doc": "application/msword", ".xls": "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".ts":   "text/plain", ".py": "text/plain", ".sh": "text/plain",
}

// Error the Python cache functions raise as ValueError
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *ValueError) PyTypeName() string { return "ValueError" }

// Inbound media byte cap from the home config, the default when the key is missing or not an integer
func InboundMediaMaxBytes(ctx context.Context, h string) int64 {
	tree, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return DefaultInboundMediaMaxBytes
	}
	section, ok := map[string]any(tree)["gateway"].(map[string]any)
	if !ok {
		return DefaultInboundMediaMaxBytes
	}
	raw, ok := section["max_inbound_media_bytes"]
	if !ok {
		return DefaultInboundMediaMaxBytes
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return DefaultInboundMediaMaxBytes
	}
	if !n.IsInt64() {
		if n.Sign() > 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return n.Int64()
}

// Rejects a payload over the cap, nil maxBytes reading the home config and a zero cap disabling the check
func ValidateInboundMediaSize(ctx context.Context, h string, size int64, mediaType string, maxBytes *int64) error {
	var limit int64
	if maxBytes == nil {
		limit = InboundMediaMaxBytes(ctx, h)
	} else {
		limit = *maxBytes
	}
	if limit != 0 && size > limit {
		return &ValueError{Msg: fmt.Sprintf("Inbound %s payload is too large (%d bytes > %d bytes)", mediaType, size, limit)}
	}
	return nil
}

// One media cache directory, the populated legacy name winning over the new subpath
type cacheKind struct{ newSubpath, oldName string }

var (
	imageCache      = cacheKind{"cache/images", "image_cache"}
	audioCache      = cacheKind{"cache/audio", "audio_cache"}
	videoCache      = cacheKind{"cache/videos", "video_cache"}
	documentCache   = cacheKind{"cache/documents", "document_cache"}
	screenshotCache = cacheKind{"cache/screenshots", "browser_screenshots"}
)

// Resolves and creates the kind's directory under the home, owner only unless the install is managed
func (k cacheKind) dir(ctx context.Context, h string) (string, error) {
	d := home.HermesDir(h, k.newSubpath, k.oldName)
	if err := secureMediaCacheDir(ctx, h, d); err != nil {
		return "", err
	}
	return d, nil
}

// Deletes the kind's files older than the age and returns the count
func (k cacheKind) cleanup(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	d, err := k.dir(ctx, h)
	if err != nil {
		return 0, err
	}
	return pyfs.UnlinkFilesOlderThan(d, "*", maxAgeHours*3600, nil), nil
}

// Image cache directory of the home, created on demand
func ImageCacheDir(ctx context.Context, h string) (string, error) { return imageCache.dir(ctx, h) }

// Audio cache directory of the home, created on demand
func AudioCacheDir(ctx context.Context, h string) (string, error) { return audioCache.dir(ctx, h) }

// Video cache directory of the home, created on demand
func VideoCacheDir(ctx context.Context, h string) (string, error) { return videoCache.dir(ctx, h) }

// Document cache directory of the home, created on demand
func DocumentCacheDir(ctx context.Context, h string) (string, error) {
	return documentCache.dir(ctx, h)
}

// Browser screenshot cache directory of the home, created on demand
func ScreenshotCacheDir(ctx context.Context, h string) (string, error) {
	return screenshotCache.dir(ctx, h)
}

// Deletes cached images older than the age in hours and returns the count
func CleanupImageCache(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	return imageCache.cleanup(ctx, h, maxAgeHours)
}

// Deletes cached audio older than the age in hours and returns the count
func CleanupAudioCache(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	return audioCache.cleanup(ctx, h, maxAgeHours)
}

// Deletes cached videos older than the age in hours and returns the count
func CleanupVideoCache(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	return videoCache.cleanup(ctx, h, maxAgeHours)
}

// Deletes cached documents older than the age in hours and returns the count
func CleanupDocumentCache(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	return documentCache.cleanup(ctx, h, maxAgeHours)
}

// Deletes cached screenshots older than the age in hours and returns the count
func CleanupScreenshotCache(ctx context.Context, h string, maxAgeHours float64) (int, error) {
	return screenshotCache.cleanup(ctx, h, maxAgeHours)
}

// Whether the bytes start with a PNG, JPEG, GIF, BMP or WEBP signature
func looksLikeImage(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	s := string(data)
	return strings.HasPrefix(s, "\x89PNG\r\n\x1a\n") || strings.HasPrefix(s, "\xff\xd8\xff") ||
		strings.HasPrefix(s, "GIF87a") || strings.HasPrefix(s, "GIF89a") || strings.HasPrefix(s, "BM") ||
		(strings.HasPrefix(s, "RIFF") && len(data) >= 12 && s[8:12] == "WEBP")
}

// Creates a media cache directory 0700 under the secure dir policy, plain mkdir for managed installs
func secureMediaCacheDir(ctx context.Context, h, dir string) error {
	if home.ManagedSystem(h) != "" {
		return os.MkdirAll(dir, 0o777)
	}
	if _, err := home.MakeUserDir(ctx, dir, 0o700, true, true); err != nil {
		slog.Debug(fmt.Sprintf("media cache dir creation failed for %s: %v", dir, err))
		return os.MkdirAll(dir, 0o777)
	}
	config.SecureDir(h, dir)
	return nil
}

// Twelve random hex digits like uuid4().hex[:12]
func uuidHex12() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Writes the bytes owner only to prefix_uuid12 plus ext inside the directory and returns the path
func writeCacheFile(dir, prefix, ext string, data []byte) (string, error) {
	path := filepath.Join(dir, prefix+"_"+uuidHex12()+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		if werr := os.WriteFile(path, data, 0o666); werr != nil {
			return "", werr
		}
		return path, nil
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	return path, nil
}

// Caches image bytes and returns the path, refusing oversized payloads and bytes that are not an image
func CacheImageFromBytes(ctx context.Context, h string, data []byte, ext string) (string, error) {
	if err := ValidateInboundMediaSize(ctx, h, int64(len(data)), "image", nil); err != nil {
		return "", err
	}
	if !looksLikeImage(data) {
		head := data
		if len(head) > 80 {
			head = head[:80]
		}
		snippet := pystr.DecodeUTF8Replace(head)
		return "", &ValueError{Msg: fmt.Sprintf("Refusing to cache non-image data as %s (starts with: %s)", ext, pyrepr.Repr(snippet))}
	}
	dir, err := ImageCacheDir(ctx, h)
	if err != nil {
		return "", err
	}
	return writeCacheFile(dir, "img", ext, data)
}

// Caches audio bytes under the container sniffed extension and returns the path
func CacheAudioFromBytes(ctx context.Context, h string, data []byte, ext string) (string, error) {
	if err := ValidateInboundMediaSize(ctx, h, int64(len(data)), "audio", nil); err != nil {
		return "", err
	}
	dir, err := AudioCacheDir(ctx, h)
	if err != nil {
		return "", err
	}
	return writeCacheFile(dir, "audio", SniffAudioExt(data, ext), data)
}

// Caches video bytes and returns the path, refusing oversized payloads
func CacheVideoFromBytes(ctx context.Context, h string, data []byte, ext string) (string, error) {
	if err := ValidateInboundMediaSize(ctx, h, int64(len(data)), "video", nil); err != nil {
		return "", err
	}
	dir, err := VideoCacheDir(ctx, h)
	if err != nil {
		return "", err
	}
	return writeCacheFile(dir, "video", ext, data)
}

// Caches document bytes as doc_uuid12_name with the name sanitized, refusing a path that leaves the cache
func CacheDocumentFromBytes(ctx context.Context, h string, data []byte, filename string) (string, error) {
	dir, err := DocumentCacheDir(ctx, h)
	if err != nil {
		return "", err
	}
	safeName := "document"
	if filename != "" {
		safeName = pypath.PathName(filename)
	}
	safeName = pystr.Strip(strings.ReplaceAll(safeName, "\x00", ""))
	if safeName == "" || safeName == "." || safeName == ".." {
		safeName = "document"
	}
	path := filepath.Join(dir, "doc_"+uuidHex12()+"_"+safeName)
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return "", err
	}
	root, err := pypath.Resolve(dir)
	if err != nil {
		return "", err
	}
	if !isRelativeTo(resolved, root) {
		return "", &ValueError{Msg: "Path traversal rejected: " + pyrepr.Repr(filename)}
	}
	if err := os.WriteFile(path, data, 0o666); err != nil {
		return "", err
	}
	return path, nil
}

// pathlib is_relative_to over two resolved absolute paths
func isRelativeTo(p, root string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
