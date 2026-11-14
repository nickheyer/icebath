package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Agreed mime to extension union of the adapters, the extension common in the wild winning over the registered one
var DefaultMimeToExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"audio/ogg": ".ogg", "audio/x-opus+ogg": ".ogg", "audio/opus": ".ogg",
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/wav": ".wav",
	"audio/mp4": ".m4a", "audio/x-m4a": ".m4a", "audio/aac": ".aac",
	"video/mp4": ".mp4", "application/pdf": ".pdf", "application/zip": ".zip",
}

// Lowercased mime type with any charset style parameters dropped
func normalizeMime(mime string) string {
	return pystr.Lower(pystr.Strip(strings.SplitN(mime, ";", 2)[0]))
}

// Dotted extension of a mime from the overrides, the shared table and the mimetypes guess in turn, else the fallback
func ExtForMime(mime string, overrides map[string]string, useDefaults, useMimetypes bool, fallback string) string {
	primary := normalizeMime(mime)
	if primary == "" {
		return fallback
	}
	var stages [3]func(string) string
	if len(overrides) > 0 {
		stages[0] = func(key string) string { return overrides[key] }
	}
	if useDefaults {
		stages[1] = func(key string) string { return DefaultMimeToExt[key] }
	}
	if useMimetypes {
		stages[2] = func(key string) string { return pymimetypes.GuessExtension(key, true) }
	}
	for _, lookup := range stages {
		if lookup == nil {
			continue
		}
		if ext := lookup(primary); ext != "" {
			return ext
		}
	}
	return fallback
}

// Canonical mime per extension, the explicit inverse of the many to one forward table
var DefaultExtToMime = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".ogg": "audio/ogg", ".mp3": "audio/mpeg", ".wav": "audio/wav",
	".m4a": "audio/mp4", ".aac": "audio/aac", ".mp4": "video/mp4", ".pdf": "application/pdf",
	".zip": "application/zip",
}

// Mime of a dotted extension from the overrides then the shared inverse table, else the fallback
func MimeForExt(ext string, overrides map[string]string, fallback string) string {
	key := pystr.Lower(pystr.Strip(ext))
	if mime := overrides[key]; mime != "" {
		return mime
	}
	if mime, ok := DefaultExtToMime[key]; ok {
		return mime
	}
	return fallback
}

// Caches downloaded bytes through the image, audio or document primitive picked by kind hint or mime class
func CacheMediaBytesByMime(ctx context.Context, data []byte, mime, filenameHint string, kindHint *string, extOverrides map[string]string) (string, error) {
	primary := normalizeMime(mime)
	kind := "document"
	switch {
	case kindHint != nil:
		kind = *kindHint
	case strings.HasPrefix(primary, "image/"):
		kind = "image"
	case strings.HasPrefix(primary, "audio/"):
		kind = "audio"
	}
	if kind == "image" {
		ext := ExtForMime(primary, extOverrides, true, true, ".jpg")
		if ext == "" {
			ext = ".jpg"
		}
		return cacheImageFromBytes(ctx, data, ext)
	}
	if kind == "audio" {
		ext := ExtForMime(primary, extOverrides, true, true, ".ogg")
		if ext == "" {
			ext = ".ogg"
		}
		return cacheAudioFromBytes(ctx, data, ext)
	}
	filename := filenameHint
	if filename == "" {
		filename = "file_" + uuidHex()[:8] + ExtForMime(primary, extOverrides, true, true, ".bin")
	}
	return cacheDocumentFromBytes(ctx, data, filename)
}
