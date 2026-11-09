package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".bmp": true, ".tiff": true, ".tif": true, ".svg": true, ".ico": true,
}

// Reads the initial status bar visibility, YAML off and strings like hidden both meaning off
func statusBarVisibleFromDisplayConfig(displayConfig any) bool {
	display, _ := displayConfig.(map[string]any)
	statusbar, ok := display["statusbar"]
	if !ok {
		statusbar, ok = display["tui_statusbar"]
	}
	if !ok {
		statusbar = "top"
	}
	if s, isStr := statusbar.(string); isStr {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "0", "false", "hidden", "no", "off":
			return false
		}
		return true
	}
	return statusbar != false
}

// Resolves a user supplied attachment path with quotes, tilde, env vars and file URLs, empty unless an existing file
func resolveAttachmentPath(rawPath string) string {
	token := strings.TrimSpace(rawPath)
	if token == "" {
		return ""
	}
	if len(token) >= 2 && token[0] == token[len(token)-1] && (token[0] == '"' || token[0] == '\'') {
		token = strings.TrimSpace(token[1 : len(token)-1])
	}
	token = strings.ReplaceAll(token, "\\ ", " ")
	if token == "" {
		return ""
	}
	expanded := token
	if strings.HasPrefix(token, "file://") {
		if parsed, err := url.Parse(token); err == nil && parsed.Scheme == "file" {
			if p, uerr := url.PathUnescape(parsed.EscapedPath()); uerr == nil {
				expanded = p
			} else {
				expanded = parsed.Path
			}
			if parsed.Host != "" && runtime.GOOS == "windows" {
				expanded = "//" + parsed.Host + expanded
			} else if runtime.GOOS == "windows" && len(expanded) >= 3 && expanded[0] == '/' && unicode.IsLetter(rune(expanded[1])) && expanded[2] == ':' {
				expanded = expanded[1:]
			}
		}
	}
	expanded = os.ExpandEnv(pypath.Expanduser(expanded))
	if runtime.GOOS != "windows" {
		normalized := strings.ReplaceAll(expanded, "\\", "/")
		if len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/' && unicode.IsLetter(rune(normalized[0])) {
			expanded = fmt.Sprintf("/mnt/%s/%s", strings.ToLower(normalized[:1]), normalized[3:])
		}
	}
	path := expanded
	if !filepath.IsAbs(path) {
		base := os.Getenv("TERMINAL_CWD")
		if base == "" {
			base, _ = os.Getwd()
		}
		path = filepath.Join(base, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = filepath.Clean(path)
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return resolved
}

// fileDrop is a dragged or pasted file path found at the start of the input with the text after it
type fileDrop struct {
	Path      string
	IsImage   bool
	Remainder string
}

// Detects a dragged or pasted file path at the start of the input, nil when the text is not a path
func detectFileDrop(userInput string) *fileDrop {
	stripped := strings.TrimSpace(userInput)
	if stripped == "" {
		return nil
	}
	quoted := stripped[0] == '\'' || stripped[0] == '"'
	unquoted := stripped
	if quoted {
		unquoted = stripped[1:]
	}
	startsLikePath := strings.HasPrefix(unquoted, "/") || strings.HasPrefix(unquoted, "~") || strings.HasPrefix(unquoted, "./") || strings.HasPrefix(unquoted, "../") ||
		(!quoted && strings.HasPrefix(unquoted, "file://")) ||
		(len(unquoted) >= 3 && unquoted[1] == ':' && (unquoted[2] == '\\' || unquoted[2] == '/') && unicode.IsLetter(rune(unquoted[0])))
	if !startsLikePath {
		return nil
	}
	if direct := resolveAttachmentPath(stripped); direct != "" {
		return fileDropResult(direct, "")
	}
	firstToken, remainder := splitPathInput(stripped)
	dropPath := resolveAttachmentPath(firstToken)
	if dropPath == "" && strings.Contains(stripped, " ") && !quoted {
		for pos := len(stripped) - 1; pos >= 0; pos-- {
			if stripped[pos] != ' ' {
				continue
			}
			dropPath = resolveAttachmentPath(strings.TrimRight(stripped[:pos], " "))
			if dropPath != "" {
				remainder = strings.TrimSpace(stripped[pos+1:])
				break
			}
		}
	}
	if dropPath == "" {
		return nil
	}
	return fileDropResult(dropPath, remainder)
}

// Collects the local image attachments of a single query, the dropped path and the explicit image flag
func collectQueryImages(query, imageArg string) (string, []string, error) {
	message := query
	var images []string
	if dropped := detectFileDrop(message); dropped != nil && dropped.IsImage {
		images = append(images, dropped.Path)
		message = dropped.Remainder
		if message == "" {
			message = fmt.Sprintf("[User attached image: %s]", filepath.Base(dropped.Path))
		}
	}
	if imageArg != "" {
		explicit := resolveAttachmentPath(imageArg)
		if explicit == "" {
			return "", nil, fmt.Errorf("Image file not found: %s", imageArg)
		}
		if !imageExtensions[strings.ToLower(filepath.Ext(explicit))] {
			return "", nil, fmt.Errorf("Not a supported image file: %s", explicit)
		}
		images = append(images, explicit)
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range images {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	return message, unique, nil
}

// Builds the file drop record of a resolved path and the text after it
func fileDropResult(path, remainder string) *fileDrop {
	return &fileDrop{Path: path, IsImage: imageExtensions[strings.ToLower(filepath.Ext(path))], Remainder: remainder}
}

// Splits a leading quoted or backslash escaped path token from the trailing free form text
func splitPathInput(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if raw[0] == '"' || raw[0] == '\'' {
		quote := raw[0]
		for pos := 1; pos < len(raw); pos++ {
			ch := raw[pos]
			if ch == '\\' && pos+1 < len(raw) {
				pos++
				continue
			}
			if ch == quote {
				return raw[1:pos], strings.TrimSpace(raw[pos+1:])
			}
		}
		return raw[1:], ""
	}
	pos := 0
	for pos < len(raw) {
		ch := raw[pos]
		if ch == '\\' && pos+1 < len(raw) && raw[pos+1] == ' ' {
			pos += 2
		} else if ch == ' ' {
			break
		} else {
			pos++
		}
	}
	return strings.ReplaceAll(raw[:pos], "\\ ", " "), strings.TrimSpace(raw[pos:])
}
