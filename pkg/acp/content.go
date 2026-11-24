package acp

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const maxACPResourceBytes = 512 * 1024

var textResourceMimeTypes = map[string]bool{
	"application/json": true, "application/javascript": true, "application/typescript": true, "application/xml": true,
	"application/x-yaml": true, "application/yaml": true, "application/toml": true, "application/sql": true,
}

var imageSuffixMime = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".svg": "image/svg+xml",
}

// Human readable attachment name from title, name, or the uri's last path component
func resourceDisplayName(uri, name, title string) (string, error) {
	rawName, rawTitle := strings.TrimSpace(name), strings.TrimSpace(title)
	if rawTitle != "" && rawName != "" && rawTitle != rawName {
		return rawTitle + " (" + rawName + ")", nil
	}
	if rawTitle != "" {
		return rawTitle, nil
	}
	if rawName != "" {
		return rawName, nil
	}
	parsed, err := urlutil.Split(uri)
	if err != nil {
		return "", err
	}
	candidate := uri
	if parsed.Scheme != "" {
		candidate = parsed.Path
	}
	if n := pathName(urlutil.Unescape(candidate)); n != "" {
		return n, nil
	}
	if uri != "" {
		return uri, nil
	}
	return "resource", nil
}

// Final pathlib component with the separators of the running platform
func pathName(p string) string {
	if runtime.GOOS == "windows" {
		return pathutil.WindowsBase(strings.TrimRight(p, `\/`))
	}
	return pathutil.Name(p)
}

func mimeMain(mimeType string) string {
	main, _, _ := strings.Cut(mimeType, ";")
	return strings.ToLower(strings.TrimSpace(main))
}

func isTextResource(mimeType string) bool {
	m := mimeMain(mimeType)
	return strings.HasPrefix(m, "text/") || textResourceMimeTypes[m]
}

func isImageResource(mimeType string) bool { return strings.HasPrefix(mimeMain(mimeType), "image/") }

func isAlphaByte(c byte) bool { return unicode.IsLetter(rune(c)) && c < utf8.RuneSelf }

// Readable local path of a file uri or path, Windows drives mapped under /mnt, empty for other schemes
func pathFromFileURI(uri string) (string, bool, error) {
	raw := strings.TrimSpace(uri)
	if raw == "" {
		return "", false, nil
	}
	if len(raw) >= 3 && isAlphaByte(raw[0]) && raw[1] == ':' && (raw[2] == '/' || raw[2] == '\\') {
		raw = "file:///" + strings.ReplaceAll(raw, `\`, "/")
	}
	parsed, err := urlutil.Split(raw)
	if err != nil {
		return "", false, err
	}
	if parsed.Scheme != "" && parsed.Scheme != "file" {
		return "", false, nil
	}
	if parsed.Scheme == "file" && parsed.Authority != "" && parsed.Authority != "localhost" {
		return "", false, nil
	}
	pathText := urlutil.Unescape(raw)
	if parsed.Scheme == "file" {
		pathText = urlutil.Unescape(parsed.Path)
	}
	var drive, rest string
	switch {
	case len(pathText) >= 3 && pathText[0] == '/' && pathText[2] == ':' && isAlphaByte(pathText[1]):
		drive, rest = pathText[1:2], pathText[3:]
	case len(pathText) >= 2 && pathText[1] == ':' && isAlphaByte(pathText[0]):
		drive, rest = pathText[0:1], pathText[2:]
	default:
		return filepath.Clean(pathText), true, nil
	}
	rest = strings.ReplaceAll(strings.TrimLeft(rest, `/\`), `\`, "/")
	if runtime.GOOS == "windows" {
		return drive + ":/" + rest, true, nil
	}
	return filepath.Clean("/mnt/" + strings.ToLower(drive) + "/" + rest), true, nil
}

// Resource bytes decoded as text, false for probable binary
func decodeTextBytes(data []byte, mimeType string) (string, bool) {
	if strings.Contains(string(data), "\x00") && !isTextResource(mimeType) {
		return "", false
	}
	if utf8.Valid(data) {
		return strings.TrimPrefix(string(data), "\ufeff"), true
	}
	text, _ := charsetutil.Decode(data, "latin-1")
	return text, true
}

func formatResourceText(uri, body, name, title, note string) (string, error) {
	display, err := resourceDisplayName(uri, name, title)
	if err != nil {
		return "", err
	}
	header := "[Attached file: " + display + "]"
	if note != "" {
		header += " (" + note + ")"
	}
	return header + "\nURI: " + uri + "\n\n" + body, nil
}

func textParts(uri, body, name, title, note string) ([]map[string]any, error) {
	text, err := formatResourceText(uri, body, name, title, note)
	if err != nil {
		return nil, err
	}
	return []map[string]any{{"type": "text", "text": text}}, nil
}

// Text header plus an image_url data url so vision models see the attachment
func imageParts(uri, display string, data []byte, mime string) []map[string]any {
	header := "[Attached image: " + display + "]"
	if uri != "" {
		header += "\nURI: " + uri
	}
	return []map[string]any{
		{"type": "text", "text": header},
		{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}},
	}
}

func attr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// Content parts of a resource link, images inlined as image_url and other files as text
func resourceLinkToParts(block *acputil.ResourceContentBlock) ([]map[string]any, error) {
	uri := strings.TrimSpace(block.URI)
	if uri == "" {
		return nil, nil
	}
	name, title, mimeType := strings.TrimSpace(block.Name), attr(block.Title), attr(block.MimeType)
	path, ok, err := pathFromFileURI(uri)
	if err != nil {
		return nil, err
	}
	if !ok {
		return textParts(uri, "[Resource link only; Icebath cannot read non-file ACP resource URIs directly.]", name, title, "")
	}
	imageMime := imageSuffixMime[strings.ToLower(pathutil.Suffix(path))]
	if isImageResource(mimeType) {
		imageMime = mimeType
	}
	if imageMime != "" && isImageResource(imageMime) {
		st, err := os.Stat(path)
		if err == nil && st.Size() > maxACPResourceBytes {
			return textParts(uri, fmt.Sprintf("[Image too large to inline: %d bytes, cap=%d]", st.Size(), maxACPResourceBytes), name, title, "")
		}
		var data []byte
		if err == nil {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			pkgLog().Warning("ACP image resource read failed: " + uri)
			return textParts(uri, "[Could not read attached image: "+err.Error()+"]", name, title, "")
		}
		display, derr := resourceDisplayName(uri, name, title)
		if derr != nil {
			return nil, derr
		}
		return imageParts(uri, display, data, imageMime), nil
	}
	st, err := os.Stat(path)
	var data []byte
	if err == nil {
		var f *os.File
		if f, err = os.Open(path); err == nil {
			data, err = io.ReadAll(io.LimitReader(f, min(st.Size(), maxACPResourceBytes)))
			f.Close()
		}
	}
	if err != nil {
		pkgLog().Warning("ACP resource read failed: " + uri)
		return textParts(uri, "[Could not read attached file: "+err.Error()+"]", name, title, "")
	}
	size := st.Size()
	text, isText := decodeTextBytes(data, mimeType)
	if !isText {
		m := mimeType
		if m == "" {
			m = "unknown"
		}
		return textParts(uri, fmt.Sprintf("[Binary file omitted: %d bytes, mime=%s]", size, m), name, title, "")
	}
	note := ""
	if size > maxACPResourceBytes {
		note = fmt.Sprintf("truncated to %d of %d bytes", maxACPResourceBytes, size)
	}
	return textParts(uri, text, name, title, note)
}

// Content parts of an embedded resource, text inline, blobs as image or decoded text
func embeddedResourceToParts(block *acputil.EmbeddedResourceContentBlock) ([]map[string]any, error) {
	switch res := block.Resource.(type) {
	case *acputil.TextResourceContents:
		return textParts(strings.TrimSpace(res.URI), res.Text, "", "", "")
	case *acputil.BlobResourceContents:
		uri, mimeType := strings.TrimSpace(res.URI), attr(res.MimeType)
		data, err := base64.StdEncoding.DecodeString(res.Blob)
		if err != nil {
			data = []byte(res.Blob)
		}
		if isImageResource(mimeType) {
			if len(data) > maxACPResourceBytes {
				return textParts(uri, fmt.Sprintf("[Embedded image too large to inline: %d bytes, cap=%d]", len(data), maxACPResourceBytes), "", "", "")
			}
			display, derr := resourceDisplayName(uri, "", "")
			if derr != nil {
				return nil, derr
			}
			m := mimeType
			if m == "" {
				m = "image/png"
			}
			return imageParts(uri, display, data, m), nil
		}
		body, isText := decodeTextBytes(data[:min(len(data), maxACPResourceBytes)], mimeType)
		if !isText {
			m := mimeType
			if m == "" {
				m = "unknown"
			}
			body = fmt.Sprintf("[Binary embedded file omitted: %d bytes, mime=%s]", len(data), m)
		} else if len(data) > maxACPResourceBytes {
			body += fmt.Sprintf("\n\n[Truncated to %d of %d bytes]", maxACPResourceBytes, len(data))
		}
		return textParts(uri, body, "", "", "")
	}
	return nil, nil
}

// Plain text of the prompt blocks that carry text, newline joined
func ExtractText(prompt []acputil.ContentBlock) string {
	var parts []string
	for _, b := range prompt {
		if t, ok := b.(*acputil.TextContentBlock); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// OpenAI image_url part of an image block, nil without data or uri
func imageBlockToOpenAIPart(block *acputil.ImageContentBlock) map[string]any {
	data, uri := strings.TrimSpace(block.Data), attr(block.URI)
	mimeType := strings.TrimSpace(block.MimeType)
	if mimeType == "" {
		mimeType = "image/png"
	}
	var url string
	switch {
	case data != "" && strings.HasPrefix(data, "data:"):
		url = data
	case data != "":
		url = "data:" + mimeType + ";base64," + data
	case uri != "":
		url = uri
	default:
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func appendParts(parts []map[string]any, textParts []string, newParts []map[string]any) ([]map[string]any, []string) {
	for _, p := range newParts {
		parts = append(parts, p)
		if p["type"] == "text" {
			textParts = append(textParts, p["text"].(string))
		}
	}
	return parts, textParts
}

// User content of ACP prompt blocks, a string for pure text and parts when media is present
func ContentBlocksToOpenAIUserContent(prompt []acputil.ContentBlock) (any, error) {
	var parts []map[string]any
	var texts []string
	for _, block := range prompt {
		switch b := block.(type) {
		case *acputil.TextContentBlock:
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				texts = append(texts, b.Text)
			}
		case *acputil.ImageContentBlock:
			if p := imageBlockToOpenAIPart(b); p != nil {
				parts = append(parts, p)
			}
		case *acputil.ResourceContentBlock:
			np, err := resourceLinkToParts(b)
			if err != nil {
				return nil, err
			}
			parts, texts = appendParts(parts, texts, np)
		case *acputil.EmbeddedResourceContentBlock:
			np, err := embeddedResourceToParts(b)
			if err != nil {
				return nil, err
			}
			parts, texts = appendParts(parts, texts, np)
		}
	}
	if len(parts) == 0 {
		return ExtractText(prompt), nil
	}
	allText := true
	for _, p := range parts {
		if p["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		return strings.Join(texts, "\n"), nil
	}
	return parts, nil
}
