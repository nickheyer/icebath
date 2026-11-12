package files

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

var errInvalidFileURL = errors.New("invalid file URL")

var winDriveRE = regexp.MustCompile(`^[A-Za-z]:`)

// Path of a file URL as Node fileURLToPath answers it, an error for a URL Node refuses
func fileURLToPath(raw string, goos string) (string, error) {
	if len(raw) < 5 || !strings.EqualFold(raw[:5], "file:") {
		return "", errInvalidFileURL
	}
	rest := strings.ReplaceAll(raw[5:], `\`, "/")
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	host := ""
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := strings.IndexByte(rest, '/')
		if end < 0 {
			host, rest = rest, ""
		} else {
			host, rest = rest[:end], rest[end:]
		}
		if winDriveRE.MatchString(host) && len(host) == 2 {
			rest, host = "/"+host+rest, ""
		}
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		host = ""
	}
	pathname := urlPathSegments(rest)
	lower := strings.ToLower(pathname)
	if goos == "windows" {
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return "", errInvalidFileURL
		}
		decoded, err := decodeURIComponent(pathname)
		if err != nil {
			return "", err
		}
		decoded = strings.ReplaceAll(decoded, "/", `\`)
		if host != "" {
			return `\\` + host + decoded, nil
		}
		if len(decoded) < 3 || !winDriveRE.MatchString(decoded[1:]) {
			return "", errInvalidFileURL
		}
		return decoded[1:], nil
	}
	if host != "" {
		return "", errInvalidFileURL
	}
	if strings.Contains(lower, "%2f") {
		return "", errInvalidFileURL
	}
	return decodeURIComponent(pathname)
}

// Percent decoding that refuses what JS decodeURIComponent refuses, a bad escape or invalid UTF-8
func decodeURIComponent(s string) (string, error) {
	decoded, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(decoded) {
		return "", errInvalidFileURL
	}
	return decoded, nil
}

// File URL of a path as Node pathToFileURL builds it
func pathToFileURL(p string) string {
	return pathToFileURLFor(p, runtime.GOOS)
}

func pathToFileURLFor(p string, goos string) string {
	resolved := p
	if goos == runtime.GOOS {
		resolved = resolvePath(p)
		if strings.HasSuffix(p, string(filepath.Separator)) && !strings.HasSuffix(resolved, string(filepath.Separator)) {
			resolved += string(filepath.Separator)
		}
	}
	host := ""
	if goos == "windows" {
		if strings.HasPrefix(resolved, `\\`) {
			rest := resolved[2:]
			end := strings.IndexByte(rest, '\\')
			if end < 0 {
				host, resolved = rest, ""
			} else {
				host, resolved = rest[:end], rest[end:]
			}
		}
		resolved = strings.ReplaceAll(resolved, `\`, "/")
		if !strings.HasPrefix(resolved, "/") {
			resolved = "/" + resolved
		}
	}
	var b strings.Builder
	b.WriteString("file://")
	b.WriteString(strings.ToLower(host))
	for i := 0; i < len(resolved); {
		r, size := utf8.DecodeRuneInString(resolved[i:])
		chunk := resolved[i : i+size]
		i += size
		switch {
		case r == '%':
			b.WriteString("%25")
		case r == '\\' && goos != "windows":
			b.WriteString("%5C")
		case r == '\n':
			b.WriteString("%0A")
		case r == '\r':
			b.WriteString("%0D")
		case r == '\t':
			b.WriteString("%09")
		case r < 0x20 || r > 0x7e || strings.ContainsRune(" \"#<>?`{}", r):
			for j := 0; j < len(chunk); j++ {
				b.WriteString("%" + strings.ToUpper(hexByte(chunk[j])))
			}
		default:
			b.WriteString(chunk)
		}
	}
	return b.String()
}

// URL path after the WHATWG dot segment rules of a file URL, drive letters never popped
func urlPathSegments(rest string) string {
	var out []string
	parts := strings.Split(rest[1:], "/")
	for i, seg := range parts {
		last := i == len(parts)-1
		switch strings.ToLower(seg) {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(out) > 0 && !(len(out) == 1 && len(out[0]) == 2 && winDriveRE.MatchString(out[0])) {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case ".", "%2e":
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/")
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}

// Absolute clean form of p against the working directory as Node path.resolve answers
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// p resolved against base as Node path.resolve(base, p) answers
func resolveAgainst(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if runtime.GOOS == "windows" && (strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/") || winDriveRE.MatchString(p)) {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return resolvePath(filepath.Join(base, p))
}
