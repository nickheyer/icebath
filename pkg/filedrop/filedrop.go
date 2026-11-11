package filedrop

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

// Suffixes the classic CLI treats as image attachments
var ImageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".bmp": true, ".tiff": true, ".tif": true, ".svg": true, ".ico": true,
}

func ResolveAttachmentPath(rawPath string) string {
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

// Dragged or pasted file path found at the start of the input with the text after it
type Drop struct {
	Path      string
	IsImage   bool
	Remainder string
}

// Detects a dragged or pasted file path at the start of the input, nil when the text is not a path
func DetectFileDrop(userInput string) *Drop {
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
	if direct := ResolveAttachmentPath(stripped); direct != "" {
		return dropResult(direct, "")
	}
	firstToken, remainder := SplitPathInput(stripped)
	dropPath := ResolveAttachmentPath(firstToken)
	if dropPath == "" && strings.Contains(stripped, " ") && !quoted {
		for pos := len(stripped) - 1; pos >= 0; pos-- {
			if stripped[pos] != ' ' {
				continue
			}
			dropPath = ResolveAttachmentPath(strings.TrimRight(stripped[:pos], " "))
			if dropPath != "" {
				remainder = strings.TrimSpace(stripped[pos+1:])
				break
			}
		}
	}
	if dropPath == "" {
		return nil
	}
	return dropResult(dropPath, remainder)
}

// File drop record of a resolved path and the text after it
func dropResult(path, remainder string) *Drop {
	return &Drop{Path: path, IsImage: ImageExtensions[strings.ToLower(filepath.Ext(path))], Remainder: remainder}
}

// Splits a leading quoted or backslash escaped path token from the trailing free form text
func SplitPathInput(raw string) (string, string) {
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
