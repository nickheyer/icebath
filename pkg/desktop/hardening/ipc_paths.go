package hardening

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	safeEnvSuffixes    = map[string]bool{"dist": true, "example": true, "sample": true, "template": true}
	sensitiveExtension = map[string]bool{".kdbx": true, ".p12": true, ".pem": true, ".pfx": true}
	sshPrivateKey      = regexp.MustCompile(`^id_(rsa|dsa|ecdsa|ed25519)(?:\..+)?$`)
	fileURLPrefix      = regexp.MustCompile(`(?i)^file:`)
)

// Error of an IPC path check carrying the code the renderer branches on
type IPCPathError struct {
	Code    string
	Message string
}

func (e *IPCPathError) Error() string { return e.Message }

// Reason a path names a likely secret file, false when it may be read
func SensitiveFileBlockReason(filePath string) (string, bool) {
	normalized := strings.ToLower(strings.ReplaceAll(filePath, `\`, "/"))
	trimmed := strings.TrimRight(normalized, "/")
	base := trimmed[strings.LastIndex(trimmed, "/")+1:]
	ext := path.Ext(base)
	if strings.LastIndex(base, ".") <= 0 {
		ext = ""
	}
	switch {
	case base == "":
		return "", false
	case strings.Contains(normalized, "/.ssh/"):
		return "SSH key/config files are blocked.", true
	case strings.Contains(normalized, "/.gnupg/"):
		return "GPG key material is blocked.", true
	case strings.HasSuffix(normalized, "/.aws/credentials"):
		return "AWS credential files are blocked.", true
	case base == ".env":
		return ".env files are blocked because they commonly contain secrets.", true
	}
	if strings.HasPrefix(base, ".env.") && !safeEnvSuffixes[strings.TrimPrefix(base, ".env.")] {
		return base + " is blocked because it appears to contain environment secrets.", true
	}
	if sshPrivateKey.MatchString(base) && !strings.HasSuffix(base, ".pub") {
		return "SSH private key files are blocked.", true
	}
	if sensitiveExtension[ext] {
		return ext + " key/certificate files are blocked.", true
	}
	if base == ".npmrc" || base == ".netrc" || base == ".pypirc" {
		return base + " is blocked because it may include auth credentials.", true
	}
	return "", false
}

// Trimmed path text, refusing a non string, blank, NUL or Windows device path
func RejectUnsafePathSyntax(filePath any, purpose string) (string, error) {
	s, ok := filePath.(string)
	if !ok {
		return "", &IPCPathError{"invalid-path", purpose + " failed: file path is required."}
	}
	raw := jsTrim(s)
	if raw == "" {
		return "", &IPCPathError{"invalid-path", purpose + " failed: file path is required."}
	}
	if strings.Contains(raw, "\x00") {
		return "", &IPCPathError{"invalid-path", purpose + " failed: file path is invalid."}
	}
	normalized := strings.ToLower(strings.ReplaceAll(raw, `\`, "/"))
	if strings.HasPrefix(normalized, "//?/") || strings.HasPrefix(normalized, "//./") ||
		strings.HasPrefix(normalized, "globalroot/device/") || strings.Contains(normalized, "/globalroot/device/") {
		return "", &IPCPathError{"device-path", purpose + " blocked: Windows device paths are not allowed."}
	}
	return raw, nil
}

// Filesystem path of a file URL as Node fileURLToPath answers on this platform
func fileURLToPath(u *url.URL) (string, error) {
	if strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") || (runtime.GOOS == "windows" && strings.Contains(strings.ToLower(u.EscapedPath()), "%5c")) {
		return "", errors.New("must not include encoded / characters")
	}
	p, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if u.Host != "" {
			return "", errors.New("file URL host must be localhost or empty")
		}
		return p, nil
	}
	if u.Host != "" {
		return `\\` + u.Host + strings.ReplaceAll(p, "/", `\`), nil
	}
	if len(p) < 3 || p[2] != ':' {
		return "", errors.New("file URL path must be absolute")
	}
	return strings.ReplaceAll(p[1:], "/", `\`), nil
}

// Absolute path the renderer asked for, expanding a leading tilde and file URLs and resolving against baseDir
func ResolveRequestedPathForIpc(filePath any, purpose, baseDir string) (string, error) {
	if purpose == "" {
		purpose = "File read"
	}
	raw, err := RejectUnsafePathSyntax(filePath, purpose)
	if err != nil {
		return "", err
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		home, _ := os.UserHomeDir()
		raw = filepath.Join(home, raw[1:])
	}
	if fileURLPrefix.MatchString(raw) {
		u, perr := url.Parse(raw)
		var resolved string
		if perr == nil && strings.EqualFold(u.Scheme, "file") {
			resolved, perr = fileURLToPath(u)
		} else if perr == nil {
			perr = errors.New("not a file URL")
		}
		if perr != nil {
			return "", &IPCPathError{"invalid-path", purpose + " failed: file URL is invalid."}
		}
		if _, err := RejectUnsafePathSyntax(resolved, purpose); err != nil {
			return "", err
		}
		return filepath.Abs(resolved)
	}
	baseInput := baseDir
	if jsTrim(baseDir) == "" {
		baseInput, _ = os.Getwd()
	}
	safeBase, err := RejectUnsafePathSyntax(baseInput, purpose)
	if err != nil {
		return "", err
	}
	resolvedBase, _ := filepath.Abs(safeBase)
	if _, err := RejectUnsafePathSyntax(resolvedBase, purpose); err != nil {
		return "", err
	}
	resolved := raw
	if !filepath.IsAbs(raw) {
		resolved = filepath.Join(resolvedBase, raw)
	}
	resolved, _ = filepath.Abs(resolved)
	if _, err := RejectUnsafePathSyntax(resolved, purpose); err != nil {
		return "", err
	}
	return resolved, nil
}

func rejectSensitiveFilePath(filePath, purpose string) error {
	if reason, blocked := SensitiveFileBlockReason(filePath); blocked {
		return &IPCPathError{"sensitive-file", purpose + " blocked for sensitive file: " + reason}
	}
	return nil
}

func statForIpc(resolved, purpose, typeLabel string) (fs.FileInfo, error) {
	st, err := os.Stat(resolved)
	if err == nil {
		return st, nil
	}
	code := NodeCode(err)
	if code == "ENOENT" || code == "ENOTDIR" {
		return nil, &IPCPathError{code, fmt.Sprintf("%s failed: %s does not exist.", purpose, typeLabel)}
	}
	if code == "UNKNOWN" {
		code = "read-error"
	}
	return nil, &IPCPathError{code, purpose + " failed: " + NodeErr(err).Error()}
}

func realpathForIpc(resolved, purpose string) (string, error) {
	real, err := filepath.EvalSymlinks(resolved)
	if err == nil {
		real, err = filepath.Abs(real)
	}
	if err != nil {
		code := NodeCode(err)
		if code == "UNKNOWN" {
			code = "read-error"
		}
		return "", &IPCPathError{code, purpose + " failed: " + NodeErr(err).Error()}
	}
	if _, err := RejectUnsafePathSyntax(real, purpose); err != nil {
		return "", &IPCPathError{"read-error", purpose + " failed: " + err.Error()}
	}
	return real, nil
}

// Checked readable regular file with its resolved and real paths
type ReadableFile struct {
	RealPath     string
	ResolvedPath string
	Stat         fs.FileInfo
}

// Options of a readable file check, sensitive files blocked unless AllowSensitive
type ReadableFileOptions struct {
	Purpose        string
	BaseDir        string
	AllowSensitive bool
	MaxBytes       int64
}

// Validates a renderer requested file for reading by existence, type, size, sensitivity and permission
func ResolveReadableFileForIpc(filePath any, o ReadableFileOptions) (ReadableFile, error) {
	purpose := o.Purpose
	if purpose == "" {
		purpose = "File read"
	}
	resolved, err := ResolveRequestedPathForIpc(filePath, purpose, o.BaseDir)
	if err != nil {
		return ReadableFile{}, err
	}
	if !o.AllowSensitive {
		if err := rejectSensitiveFilePath(resolved, purpose); err != nil {
			return ReadableFile{}, err
		}
	}
	st, err := statForIpc(resolved, purpose, "file")
	if err != nil {
		return ReadableFile{}, err
	}
	if st.IsDir() {
		return ReadableFile{}, &IPCPathError{"EISDIR", purpose + " failed: path points to a directory."}
	}
	if !st.Mode().IsRegular() {
		return ReadableFile{}, &IPCPathError{"EINVAL", purpose + " failed: only regular files can be read."}
	}
	real, err := realpathForIpc(resolved, purpose)
	if err != nil {
		return ReadableFile{}, err
	}
	if !o.AllowSensitive {
		if err := rejectSensitiveFilePath(real, purpose); err != nil {
			return ReadableFile{}, err
		}
	}
	if o.MaxBytes > 0 && st.Size() > o.MaxBytes {
		return ReadableFile{}, &IPCPathError{"EFBIG", fmt.Sprintf("%s failed: file is too large (%d bytes; limit %d bytes).", purpose, st.Size(), o.MaxBytes)}
	}
	if !readable(resolved) {
		return ReadableFile{}, &IPCPathError{"EACCES", purpose + " failed: file is not readable."}
	}
	return ReadableFile{RealPath: real, ResolvedPath: resolved, Stat: st}, nil
}

// Trims the whitespace JavaScript String.prototype.trim removes
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}
