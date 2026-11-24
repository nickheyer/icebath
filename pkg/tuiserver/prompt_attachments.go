package tuiserver

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	attachBytesMaxBytes = 25 * 1024 * 1024
)

var imageMagic = []struct {
	sig []byte
	ext string
}{{[]byte("\x89PNG\r\n\x1a\n"), ".png"}, {[]byte("\xff\xd8\xff"), ".jpg"}, {[]byte("GIF87a"), ".gif"}, {[]byte("GIF89a"), ".gif"}, {[]byte("BM"), ".bmp"}}

// Refusal of a payload over its cap in whole megabytes
type payloadTooLarge struct{ msg string }

func (e *payloadTooLarge) Error() string      { return e.msg }
func (e *payloadTooLarge) PyTypeName() string { return "OverflowError" }

// Refusal text of a payload over its cap
func tooLarge(label string, size, maxBytes int) string {
	return fmt.Sprintf("%s too large (%d bytes; cap is %d MB)", label, size, maxBytes/(1024*1024))
}

// Text with every unicode whitespace character removed
func stripAllSpace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Strictly decoded bytes of an optionally data URL wrapped base64 payload, the cap refusal before decoding when the text is already too long
func b64Payload(raw string, dataURL *regexp.Regexp, maxBytes int, label string) ([]byte, error) {
	cleaned := strings.TrimSpace(raw)
	if m := dataURL.FindStringSubmatch(cleaned); m != nil {
		cleaned = m[1]
	}
	cleaned = stripAllSpace(cleaned)
	if len(cleaned) > 4*((maxBytes+2)/3) {
		padding := len(cleaned) - len(strings.TrimRight(cleaned, "="))
		return nil, &payloadTooLarge{tooLarge(label, len(cleaned)*3/4-padding, maxBytes)}
	}
	payload, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxBytes {
		return nil, &payloadTooLarge{tooLarge(label, len(payload), maxBytes)}
	}
	return payload, nil
}

// Decoded payload of a data URL of the mime prefix or bare base64, nil when invalid and the cap refusal when too large
func decodeAttachBase64(raw, mimePrefix string, maxBytes int, label string) ([]byte, error) {
	pattern := regexp.MustCompile(`(?s)^data:` + regexp.QuoteMeta(mimePrefix) + `[a-zA-Z0-9.+-]*;base64,(.*)$`)
	payload, err := b64Payload(raw, pattern, maxBytes, label)
	var over *payloadTooLarge
	if errors.As(err, &over) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	return payload, nil
}

// Image extension from the filename hint, else the magic bytes, else png
func sniffImageExt(imgBytes []byte, filename string) string {
	if filename != "" {
		if suffix := strings.ToLower(pypath.Suffix(filename)); suffix != "" {
			return suffix
		}
	}
	head := imgBytes[:min(16, len(imgBytes))]
	if bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && string(head[8:12]) == "WEBP" {
		return ".webp"
	}
	for _, m := range imageMagic {
		if bytes.HasPrefix(head, m.sig) {
			return m.ext
		}
	}
	return ".png"
}

// Writes image bytes into a private collision free file of the directory and returns its path
func stageImageBytes(directory string, imgBytes []byte, ext, prefix string) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(directory, prefix+"_*"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err = f.Write(imgBytes); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
