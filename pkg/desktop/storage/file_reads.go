package storage

import (
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// Bytes of a text preview the renderer receives, the rest marked truncated
const textPreviewMaxBytes = 512 * 1024

var mediaMimeTypes = map[string]string{
	".avi": "video/x-msvideo", ".bmp": "image/bmp", ".flac": "audio/flac", ".gif": "image/gif",
	".jpeg": "image/jpeg", ".jpg": "image/jpeg", ".m4a": "audio/mp4", ".mkv": "video/x-matroska",
	".mov": "video/quicktime", ".mp3": "audio/mpeg", ".mp4": "video/mp4", ".ogg": "audio/ogg",
	".opus": "audio/ogg; codecs=opus", ".pdf": "application/pdf", ".png": "image/png", ".svg": "image/svg+xml",
	".wav": "audio/wav", ".webm": "video/webm", ".webp": "image/webp",
}

var previewLanguageByExt = map[string]string{
	".c": "c", ".conf": "ini", ".cpp": "cpp", ".css": "css", ".csv": "csv", ".go": "go", ".graphql": "graphql",
	".h": "c", ".hpp": "cpp", ".html": "html", ".java": "java", ".js": "javascript", ".json": "json", ".jsx": "jsx",
	".kt": "kotlin", ".lua": "lua", ".md": "markdown", ".mjs": "javascript", ".py": "python", ".rb": "ruby",
	".rs": "rust", ".sh": "shell", ".sql": "sql", ".svg": "xml", ".toml": "toml", ".ts": "typescript", ".tsx": "tsx",
	".txt": "text", ".xml": "xml", ".yaml": "yaml", ".yml": "yaml", ".zsh": "shell",
}

// Lower case of s as String.prototype.toLowerCase gives it, dotted capital I keeping its combining dot
func jsLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// Extension of p as Node path.extname finds it with the separators of this platform
func nodeExtname(p string) string {
	isSep := func(c byte) bool { return c == '/' }
	start := 0
	if runtime.GOOS == "windows" {
		isSep = func(c byte) bool { return c == '/' || c == '\\' }
		if len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' {
			start = 2
		}
	}
	startDot, startPart, end, preDotState := -1, start, -1, 0
	matchedSlash := true
	for i := len(p) - 1; i >= start; i-- {
		c := p[i]
		if isSep(c) {
			if !matchedSlash {
				startPart = i + 1
				break
			}
			continue
		}
		if end == -1 {
			matchedSlash = false
			end = i + 1
		}
		if c == '.' {
			if startDot == -1 {
				startDot = i
			} else if preDotState != 1 {
				preDotState = 1
			}
		} else if startDot != -1 {
			preDotState = -1
		}
	}
	if startDot == -1 || end == -1 || preDotState == 0 || (preDotState == 1 && startDot == end-1 && startDot == startPart+1) {
		return ""
	}
	return p[startDot:end]
}

// Media type of a path by its lower case extension, octet stream when unknown
func mimeTypeForPath(filePath string) string {
	if m, ok := mediaMimeTypes[jsLower(nodeExtname(filePath))]; ok {
		return m
	}
	return "application/octet-stream"
}

// True for a NUL byte or more than 12 percent control bytes other than tab, LF and CR
func looksBinary(buf []byte) bool {
	if len(buf) == 0 {
		return false
	}
	suspicious := 0
	for _, b := range buf {
		if b == 0 {
			return true
		}
		if b < 32 && b != 9 && b != 10 && b != 13 {
			suspicious++
		}
	}
	return float64(suspicious)/float64(len(buf)) > 0.12
}

// Renderer path argument as JavaScript String with null read as empty, and its WSL bridged read path
func readArg(filePath json.RawMessage) (string, string) {
	v, _ := jsParse(string(filePath))
	requested := jsString(v)
	return requested, files.ResolveIpcFileReadPath(requested)
}

// Answers readFileText with the first 512 KiB of the file as UTF-8 text, a missing file as data
func (s *Service) ReadFileText(filePath json.RawMessage) (any, error) {
	requested, bridged := readArg(filePath)
	out, err := readTextPreview(bridged)
	if err != nil {
		if hardening.IsMissingFileError(err) {
			return hardening.MissingFileResult(requested, err), nil
		}
		return nil, err
	}
	return out, nil
}

// Text preview of a checked file with its language, media type and binary guess
func readTextPreview(bridged string) (*Object, error) {
	rf, err := hardening.ResolveReadableFileForIpc(bridged, hardening.ReadableFileOptions{MaxBytes: hardening.TextPreviewSourceMaxBytes, Purpose: "Text preview"})
	if err != nil {
		return nil, err
	}
	ext := jsLower(nodeExtname(rf.ResolvedPath))
	f, err := os.Open(rf.ResolvedPath)
	if err != nil {
		return nil, hardening.NodeErr(err)
	}
	defer f.Close()
	size := rf.Stat.Size()
	buf := make([]byte, min(size, textPreviewMaxBytes))
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, hardening.NodeErr(err)
	}
	language, ok := previewLanguageByExt[ext]
	if !ok {
		language = "text"
	}
	text, _ := pycodecs.DecodeReplace(buf[:n], "utf-8")
	out := NewObject()
	out.Set("binary", looksBinary(buf[:min(n, 4096)]))
	out.Set("byteSize", float64(size))
	out.Set("language", language)
	out.Set("mimeType", mimeTypeForPath(rf.ResolvedPath))
	out.Set("path", rf.ResolvedPath)
	out.Set("text", text)
	out.Set("truncated", size > textPreviewMaxBytes)
	return out, nil
}

// Answers readFileDataUrl with the file as a data URL under the configured preview cap
func (s *Service) ReadFileDataUrl(filePath json.RawMessage) (any, error) {
	s.dataURLMax.mu.Lock()
	maxBytes := DataURLReadMaxBytesFromMB(float64(s.dataURLMax.mb))
	s.dataURLMax.mu.Unlock()
	return readDataURL(filePath, maxBytes, "File preview")
}

// Answers readFileDataUrlForAttach with the file as a data URL under the attachment upload cap
func (s *Service) ReadFileDataUrlForAttach(filePath json.RawMessage) (any, error) {
	return readDataURL(filePath, hardening.AttachmentUploadDefaultMaxBytes, "Attachment upload")
}

// Data URL of a checked file under maxBytes, a missing file answered as data
func readDataURL(filePath json.RawMessage, maxBytes int64, purpose string) (any, error) {
	requested, bridged := readArg(filePath)
	url, err := func() (string, error) {
		resolved, err := hardening.ResolveRequestedPathForIpc(bridged, purpose, "")
		if err != nil {
			return "", err
		}
		return hardening.ReadFileDataURLForIpc(bridged, hardening.ReadableFileOptions{MaxBytes: maxBytes, Purpose: purpose}, mimeTypeForPath(resolved))
	}()
	if err != nil {
		if hardening.IsMissingFileError(err) {
			return hardening.MissingFileResult(requested, err), nil
		}
		return nil, err
	}
	return url, nil
}
