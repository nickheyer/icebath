package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/nodehttp"
)

var dataURLPattern = regexp.MustCompile(`(?s)^data:([^;,]+)?(;base64)?,(.*)$`)

// Filters of the Save Image dialog
var imageSaveFilters = []files.FileFilter{
	{Name: "Images", Extensions: []string{"png", "jpg", "jpeg", "gif", "webp", "bmp", "svg"}},
	{Name: "All Files", Extensions: []string{"*"}},
}

// Answers saveImageFromUrl, writing the image where the save dialog points, false when the dialog is canceled
func (s *Service) SaveImageFromUrl(rawURL json.RawMessage) (bool, error) {
	v, _ := jsParse(string(rawURL))
	raw := ""
	if jsTruthy(v) {
		raw = jsString(v)
	}
	buf, mimeType, err := resourceBufferFromURL(raw)
	if err != nil {
		return false, err
	}
	ext := extensionForMimeType(mimeType)
	if ext == "" {
		ext = ".png"
	}
	base := filenameFromURL(raw, "image"+ext)
	fallback := base
	if nodeExtname(base) == "" {
		fallback = base + ext
	}
	defaultPath := fallback
	if dir := s.host.DownloadsDir(); dir != "" {
		defaultPath = filepath.Join(dir, fallback)
	}
	res, err := s.host.ShowSaveDialog(files.SaveDialogOptions{Title: "Save Image", DefaultPath: defaultPath, Filters: imageSaveFilters})
	if err != nil {
		return false, err
	}
	if res.Canceled || res.FilePath == "" {
		return false, nil
	}
	if err := os.WriteFile(res.FilePath, buf, 0o666); err != nil {
		return false, hardening.NodeErr(err)
	}
	return true, nil
}

// Bytes and media type of a data, file or http URL
func resourceBufferFromURL(raw string) ([]byte, string, error) {
	if raw == "" {
		return nil, "", errors.New("Missing URL")
	}
	if strings.HasPrefix(raw, "data:") {
		m := dataURLPattern.FindStringSubmatch(raw)
		if m == nil {
			return nil, "", errors.New("Invalid data URL")
		}
		mimeType := m[1]
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if m[2] != "" {
			return nodehttp.DecodeBase64(m[3]), mimeType, nil
		}
		text, err := nodehttp.DecodeURIComponent(m[3])
		if err != nil {
			return nil, "", err
		}
		return []byte(text), mimeType, nil
	}
	if len(raw) >= 5 && strings.EqualFold(raw[:5], "file:") {
		rf, err := hardening.ResolveReadableFileForIpc(raw, hardening.ReadableFileOptions{Purpose: "Image file"})
		if err != nil {
			return nil, "", err
		}
		data, err := os.ReadFile(rf.ResolvedPath)
		if err != nil {
			return nil, "", hardening.NodeErr(err)
		}
		return data, mimeTypeForPath(rf.ResolvedPath), nil
	}
	u, err := nodehttp.ParseURL(raw)
	if err != nil {
		return nil, "", err
	}
	module := "http:"
	if u.Protocol() == "https:" {
		module = "https:"
	}
	res, err := nodehttp.Get(context.Background(), u, module, nil)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		io.Copy(io.Discard, res.Body)
		return nil, "", fmt.Errorf("Failed to fetch %s: %d", raw, res.StatusCode)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}
	mimeType := res.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return data, mimeType, nil
}

// File extension of an image media type, empty for any other type
func extensionForMimeType(mimeType string) string {
	t, _, _ := strings.Cut(mimeType, ";")
	switch jsLower(jsTrim(t)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	}
	return ""
}

// File name the URL path ends in when it holds a dot, fallback otherwise
func filenameFromURL(raw, fallback string) string {
	u, err := nodehttp.ParseURL(raw)
	if err != nil {
		return fallback
	}
	decoded, err := nodehttp.DecodeURIComponent(u.Pathname())
	if err != nil {
		return fallback
	}
	if base := nodeBasename(decoded); base != "" && strings.Contains(base, ".") {
		return base
	}
	return fallback
}

// Last path segment as Node path.basename gives it with the separators of this platform
func nodeBasename(p string) string {
	isSep := func(c byte) bool { return c == '/' }
	start := 0
	if runtime.GOOS == "windows" {
		isSep = func(c byte) bool { return c == '/' || c == '\\' }
		if len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' {
			start = 2
		}
	}
	end := len(p)
	for end > start && isSep(p[end-1]) {
		end--
	}
	i := end
	for i > start && !isSep(p[i-1]) {
		i--
	}
	return p[i:end]
}
