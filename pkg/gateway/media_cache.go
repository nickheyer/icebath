package gateway

import (
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
