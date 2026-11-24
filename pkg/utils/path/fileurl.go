package pathutil

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// File URL of an absolute path with every byte but unreserved characters and separators percent-encoded, file:///C:/x for a Windows drive and file://host/share/x for a share, the \\?\ long-path forms of both included
func FileURL(p string) (string, error) {
	if runtime.GOOS != "windows" {
		if !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("relative path %q has no file URL", p)
		}
		return "file://" + escapeURLPath(filepath.Clean(p)), nil
	}
	drive, root, _ := WindowsSplitRoot(p)
	if drive == "" || root == "" {
		return "", fmt.Errorf("relative path %q has no file URL", p)
	}
	slashed := strings.ReplaceAll(WindowsClean(p), `\`, "/")
	switch {
	case len(slashed) >= 8 && strings.EqualFold(slashed[:8], "//?/UNC/"):
		slashed = "/" + slashed[7:]
	case len(slashed) >= 7 && slashed[:4] == "//?/" && isASCIILetter(slashed[4]) && slashed[5] == ':' && slashed[6] == '/':
		slashed = slashed[4:]
	}
	if len(slashed) >= 2 && slashed[1] == ':' {
		return "file:///" + slashed[:2] + escapeURLPath(slashed[2:]), nil
	}
	return "file:" + escapeURLPath(slashed), nil
}

// Path text with every byte outside the unreserved set and the slash written as %XX
func escapeURLPath(p string) string {
	const hex = "0123456789ABCDEF"
	n := 0
	for i := range len(p) {
		if !urlPathSafe(p[i]) {
			n++
		}
	}
	if n == 0 {
		return p
	}
	buf := make([]byte, 0, len(p)+2*n)
	for i := range len(p) {
		c := p[i]
		if urlPathSafe(c) {
			buf = append(buf, c)
			continue
		}
		buf = append(buf, '%', hex[c>>4], hex[c&15])
	}
	return string(buf)
}

func urlPathSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/", c) >= 0
}

// Native path a file URL names, failing for another scheme, an encoded separator, or another host except on Windows where a host names a share
func FromFileURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("%q is not a file URL", rawURL)
	}
	escaped, p := u.EscapedPath(), u.Path
	if u.Opaque != "" {
		if p, err = url.PathUnescape(u.Opaque); err != nil {
			return "", err
		}
		escaped = u.Opaque
	}
	host := u.Host
	if strings.EqualFold(host, "localhost") {
		host = ""
	}
	if p == "" && host == "" {
		return "", fmt.Errorf("file URL %q names no path", rawURL)
	}
	onWindows := runtime.GOOS == "windows"
	drive := len(p) >= 3 && p[0] == '/' && isASCIILetter(p[1]) && (p[2] == ':' || p[2] == '|') ||
		len(p) >= 2 && isASCIILetter(p[0]) && (p[1] == ':' || p[1] == '|')
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || (onWindows || host != "" || drive) && strings.Contains(lower, "%5c") {
		return "", fmt.Errorf("file URL %q encodes a path separator", rawURL)
	}
	if !onWindows {
		if host != "" {
			return "", fmt.Errorf("file URL %q names another host", rawURL)
		}
		return p, nil
	}
	if host != "" {
		return `\\` + host + strings.ReplaceAll(p, "/", `\`), nil
	}
	if rest := strings.TrimPrefix(p, "/"); len(rest) >= 2 && isASCIILetter(rest[0]) && (rest[1] == ':' || rest[1] == '|') {
		if p = rest[:1] + ":" + rest[2:]; len(p) == 2 {
			p += `\`
		}
	}
	return strings.ReplaceAll(p, "/", `\`), nil
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
