package files

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// IpcPathError is a refused or failed path with the code the renderer reads
type IpcPathError struct {
	Code    string
	Message string
}

func (e *IpcPathError) Error() string { return e.Message }

func ipcPathError(code, message string) error { return &IpcPathError{Code: code, Message: message} }

var safeEnvSuffixes = map[string]bool{"dist": true, "example": true, "sample": true, "template": true}

var sensitiveExtensions = map[string]bool{".kdbx": true, ".p12": true, ".pem": true, ".pfx": true}

var sshPrivateKeyRE = regexp.MustCompile(`^id_(rsa|dsa|ecdsa|ed25519)(?:\..+)?$`)

// Reason a path is refused as secret material, empty when it is not
func SensitiveFileBlockReason(filePath string) string {
	normalized := strings.ToLower(strings.ReplaceAll(filePath, `\`, "/"))
	basename := normalized
	if i := strings.LastIndexByte(strings.TrimRight(normalized, "/"), '/'); i >= 0 {
		basename = strings.TrimRight(normalized, "/")[i+1:]
	} else {
		basename = strings.TrimRight(normalized, "/")
	}
	ext := ""
	if i := strings.LastIndexByte(basename, '.'); i > 0 {
		ext = basename[i:]
	}
	if basename == "" {
		return ""
	}
	if strings.Contains(normalized, "/.ssh/") {
		return "SSH key/config files are blocked."
	}
	if strings.Contains(normalized, "/.gnupg/") {
		return "GPG key material is blocked."
	}
	if strings.HasSuffix(normalized, "/.aws/credentials") {
		return "AWS credential files are blocked."
	}
	if basename == ".env" {
		return ".env files are blocked because they commonly contain secrets."
	}
	if strings.HasPrefix(basename, ".env.") && !safeEnvSuffixes[basename[len(".env."):]] {
		return basename + " is blocked because it appears to contain environment secrets."
	}
	if sshPrivateKeyRE.MatchString(basename) && !strings.HasSuffix(basename, ".pub") {
		return "SSH private key files are blocked."
	}
	if sensitiveExtensions[ext] {
		return ext + " key/certificate files are blocked."
	}
	if basename == ".npmrc" || basename == ".netrc" || basename == ".pypirc" {
		return basename + " is blocked because it may include auth credentials."
	}
	return ""
}

// Trimmed path text, refusing a non string, blank, NUL or Windows device spelling
func RejectUnsafePathSyntax(filePath any, purpose string) (string, error) {
	s, ok := filePath.(string)
	if !ok {
		return "", ipcPathError("invalid-path", purpose+" failed: file path is required.")
	}
	raw := jsTrim(s)
	if raw == "" {
		return "", ipcPathError("invalid-path", purpose+" failed: file path is required.")
	}
	if strings.ContainsRune(raw, 0) {
		return "", ipcPathError("invalid-path", purpose+" failed: file path is invalid.")
	}
	normalized := strings.ToLower(strings.ReplaceAll(raw, `\`, "/"))
	if strings.HasPrefix(normalized, "//?/") || strings.HasPrefix(normalized, "//./") ||
		strings.HasPrefix(normalized, "globalroot/device/") || strings.Contains(normalized, "/globalroot/device/") {
		return "", ipcPathError("device-path", purpose+" blocked: Windows device paths are not allowed.")
	}
	return raw, nil
}

// File stat the open guard reads, replaced only where a case stands in for the OS
var statPath = os.Stat

// Refuses a path that is not on disk before an open reaches the OS, other stat failures pass through
func AssertExistingPathForOpen(resolvedPath, purpose string) error {
	if purpose == "" {
		purpose = "Open file"
	}
	if _, err := statPath(resolvedPath); err != nil {
		if code := ErrorCode(err); code == "ENOENT" || code == "ENOTDIR" {
			return ipcPathError("missing-file", purpose+" failed: the file does not exist \u2014 it may have been deleted or moved, or it lives on another machine.")
		}
		return err
	}
	return nil
}

// Absolute path a renderer path names, tilde and file URLs expanded and device spellings refused
func ResolveRequestedPathForIpc(filePath any, purpose, baseDir string) (string, error) {
	if purpose == "" {
		purpose = "File read"
	}
	raw, err := RejectUnsafePathSyntax(filePath, purpose)
	if err != nil {
		return "", err
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		raw = filepath.Join(userHome(), raw[1:])
	}
	if len(raw) >= 5 && strings.EqualFold(raw[:5], "file:") {
		resolved, err := fileURLToPath(raw, runtime.GOOS)
		if err != nil {
			return "", ipcPathError("invalid-path", purpose+" failed: file URL is invalid.")
		}
		if _, err := RejectUnsafePathSyntax(resolved, purpose); err != nil {
			return "", err
		}
		return resolvePath(resolved), nil
	}
	base := baseDir
	if jsTrim(base) == "" {
		base, _ = os.Getwd()
	}
	safeBase, err := RejectUnsafePathSyntax(base, purpose)
	if err != nil {
		return "", err
	}
	resolvedBase := resolvePath(safeBase)
	if _, err := RejectUnsafePathSyntax(resolvedBase, purpose); err != nil {
		return "", err
	}
	resolved := resolveAgainst(resolvedBase, raw)
	if _, err := RejectUnsafePathSyntax(resolved, purpose); err != nil {
		return "", err
	}
	return resolved, nil
}

// Home directory of the user, the os.homedir answer
func userHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// StatFS is the stat a path resolver needs, with realpath when the implementation has one
type StatFS interface {
	Stat(name string) (fs.FileInfo, error)
}

type realpathFS interface {
	Realpath(name string) (string, error)
}

// osFS reaches the real file system
type osFS struct{}

func (osFS) Stat(name string) (fs.FileInfo, error)    { return os.Stat(name) }
func (osFS) Realpath(name string) (string, error)     { return realpath(name) }
func (osFS) ReadDir(name string) ([]Dirent, error)    { return osReadDir(name) }
func (osFS) StatEntry(name string) (EntryStat, error) { return os.Stat(name) }

func statForIpc(fsys StatFS, resolvedPath, purpose, typeLabel string) (fs.FileInfo, error) {
	st, err := fsys.Stat(resolvedPath)
	if err != nil {
		code := ErrorCode(err)
		if code == "ENOENT" || code == "ENOTDIR" {
			return nil, ipcPathError(code, fmt.Sprintf("%s failed: %s does not exist.", purpose, typeLabel))
		}
		if code == "" {
			code = "read-error"
		}
		return nil, ipcPathError(code, fmt.Sprintf("%s failed: %s", purpose, nodeFsError(err, "stat", resolvedPath).Error()))
	}
	return st, nil
}

func realpathForIpc(fsys StatFS, resolvedPath, purpose string) (string, error) {
	rp, ok := fsys.(realpathFS)
	if !ok {
		return resolvedPath, nil
	}
	real, err := rp.Realpath(resolvedPath)
	if err != nil {
		code := ErrorCode(err)
		if code == "" {
			code = "read-error"
		}
		return "", ipcPathError(code, fmt.Sprintf("%s failed: %s", purpose, nodeFsError(err, "realpath", resolvedPath).Error()))
	}
	if _, err := RejectUnsafePathSyntax(real, purpose); err != nil {
		code := ErrorCode(err)
		return "", ipcPathError(code, fmt.Sprintf("%s failed: %s", purpose, err.Error()))
	}
	return real, nil
}

// ResolvedPath is a hardened path, its realpath and its stat
type ResolvedPath struct {
	RealPath     string
	ResolvedPath string
	Stat         fs.FileInfo
}

// Directory a renderer path names, refused unless it exists and is a directory
func ResolveDirectoryForIpc(dirPath any, purpose, baseDir string, fsys StatFS) (ResolvedPath, error) {
	if purpose == "" {
		purpose = "Directory read"
	}
	if fsys == nil {
		fsys = osFS{}
	}
	resolved, err := ResolveRequestedPathForIpc(dirPath, purpose, baseDir)
	if err != nil {
		return ResolvedPath{}, err
	}
	st, err := statForIpc(fsys, resolved, purpose, "directory")
	if err != nil {
		return ResolvedPath{}, err
	}
	if !st.IsDir() {
		return ResolvedPath{}, ipcPathError("ENOTDIR", purpose+" failed: path is not a directory.")
	}
	real, err := realpathForIpc(fsys, resolved, purpose)
	if err != nil {
		return ResolvedPath{}, err
	}
	return ResolvedPath{RealPath: real, ResolvedPath: resolved, Stat: st}, nil
}

// ReadableFileOptions tunes the readable file resolver
type ReadableFileOptions struct {
	Purpose        string
	BaseDir        string
	AllowSensitive bool
	MaxBytes       int64
}

// Regular readable file a renderer path names, refusing secrets, directories and files over the cap
func ResolveReadableFileForIpc(filePath any, opts ReadableFileOptions) (ResolvedPath, error) {
	purpose := opts.Purpose
	if purpose == "" {
		purpose = "File read"
	}
	fsys := osFS{}
	resolved, err := ResolveRequestedPathForIpc(filePath, purpose, opts.BaseDir)
	if err != nil {
		return ResolvedPath{}, err
	}
	if !opts.AllowSensitive {
		if reason := SensitiveFileBlockReason(resolved); reason != "" {
			return ResolvedPath{}, ipcPathError("sensitive-file", purpose+" blocked for sensitive file: "+reason)
		}
	}
	st, err := statForIpc(fsys, resolved, purpose, "file")
	if err != nil {
		return ResolvedPath{}, err
	}
	if st.IsDir() {
		return ResolvedPath{}, ipcPathError("EISDIR", purpose+" failed: path points to a directory.")
	}
	if !st.Mode().IsRegular() {
		return ResolvedPath{}, ipcPathError("EINVAL", purpose+" failed: only regular files can be read.")
	}
	real, err := realpathForIpc(fsys, resolved, purpose)
	if err != nil {
		return ResolvedPath{}, err
	}
	if !opts.AllowSensitive {
		if reason := SensitiveFileBlockReason(real); reason != "" {
			return ResolvedPath{}, ipcPathError("sensitive-file", purpose+" blocked for sensitive file: "+reason)
		}
	}
	if opts.MaxBytes > 0 && st.Size() > opts.MaxBytes {
		return ResolvedPath{}, ipcPathError("EFBIG", fmt.Sprintf("%s failed: file is too large (%d bytes; limit %d bytes).", purpose, st.Size(), opts.MaxBytes))
	}
	if !readable(resolved) {
		return ResolvedPath{}, ipcPathError("EACCES", purpose+" failed: file is not readable.")
	}
	return ResolvedPath{RealPath: real, ResolvedPath: resolved, Stat: st}, nil
}

// MissingFileResult is the structured answer for a file that is not on disk
type MissingFileResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// True when a read failed because the file or its parent is not on disk
func IsMissingFileError(err error) bool { return isMissingErr(err) }

// Structured not on disk answer built from a caught read error
func missingFileResult(requested string, err error) MissingFileResult {
	code := ErrorCode(err)
	if code == "" {
		code = "ENOENT"
	}
	return MissingFileResult{OK: false, Error: code, Message: err.Error(), Path: requested}
}
