package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/stageduploads"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var mediaContentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

const mediaMaxBytes = 25 * 1024 * 1024

var streamableMediaExtensions = map[string]bool{
	".avi": true, ".flac": true, ".m4a": true, ".mkv": true, ".mov": true, ".mp3": true, ".mp4": true,
	".ogg": true, ".opus": true, ".wav": true, ".webm": true,
}

var fsReaddirHidden = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".cache": true, ".next": true, ".turbo": true, ".venv": true,
	"__pycache__": true, "build": true, "dist": true, "node_modules": true, "target": true, "venv": true,
}

const (
	fsTextSourceMaxBytes  = 64 * 1024 * 1024
	fsTextPreviewMaxBytes = 512 * 1024
	fsTextWriteMaxBytes   = 8 * 1024 * 1024
)

var fsPreviewLanguageByExt = map[string]string{
	".c": "c", ".conf": "ini", ".cpp": "cpp", ".css": "css", ".csv": "csv", ".go": "go",
	".graphql": "graphql", ".h": "c", ".hpp": "cpp", ".html": "html", ".java": "java",
	".js": "javascript", ".json": "json", ".jsx": "jsx", ".kt": "kotlin", ".lua": "lua",
	".md": "markdown", ".mjs": "javascript", ".py": "python", ".rb": "ruby", ".rs": "rust",
	".sh": "shell", ".sql": "sql", ".svg": "xml", ".toml": "toml", ".ts": "typescript",
	".tsx": "tsx", ".txt": "text", ".xml": "xml", ".yaml": "yaml", ".yml": "yaml", ".zsh": "shell",
}

var fsMimeTypes = map[string]string{
	".avi": "video/x-msvideo", ".bmp": "image/bmp", ".flac": "audio/flac", ".gif": "image/gif",
	".jpeg": "image/jpeg", ".jpg": "image/jpeg", ".m4a": "audio/mp4", ".mkv": "video/x-matroska",
	".mov": "video/quicktime", ".mp3": "audio/mpeg", ".mp4": "video/mp4", ".ogg": "audio/ogg",
	".opus": "audio/ogg; codecs=opus", ".png": "image/png", ".svg": "image/svg+xml",
	".wav": "audio/wav", ".webm": "video/webm", ".webp": "image/webp",
}

// Lowercased pathlib suffix of a path
func lowerSuffix(path string) string { return pystr.Lower(pypath.PathSuffix(path)) }

// Media type of a file by its suffix, the guessed type, octet stream otherwise
func fsMimeType(path string) string {
	if mime, ok := fsMimeTypes[lowerSuffix(path)]; ok {
		return mime
	}
	if guessed, _, _ := pymimetypes.GuessType(pypath.PathStr(path), true); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// Whether bytes hold a NUL or more than twelve percent control bytes outside tab and newlines
func fsLooksBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	suspicious := 0
	for _, b := range data {
		if b == 0 {
			return true
		}
		if b < 32 && b != 9 && b != 10 && b != 13 {
			suspicious++
		}
	}
	return float64(suspicious)/float64(len(data)) > 0.12
}

// Maps a permission error to 403 denied and any other OS error to 500 failed with its text
func ioErrors(denied, failed string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return NewHTTPException(403, denied)
	}
	if pypath.IsOSError(err) {
		return NewHTTPException(500, failed+": "+pypath.OSErrorText(err))
	}
	return err
}

// Runs fn while no SQLite connection to target is live, 409 while one is
func serveOffline(target string, fn func() error) error {
	var inner error
	err := state.OfflineFileAccess(target, "serve", func() { inner = fn() })
	var live *state.LiveConnectionError
	if errors.As(err, &live) {
		return NewHTTPException(409, live.Error())
	}
	if err != nil {
		return err
	}
	return inner
}

// Point in time 409 before streaming target while a SQLite connection to it is live
func refuseLiveDatabase(target string) error {
	return serveOffline(target, func() error { return nil })
}

// Bytes of target or its first limit bytes, 403 or 400 on failure and 409 while a connection is live
func fsReadBytes(target string, limit *int64) ([]byte, error) {
	var data []byte
	err := serveOffline(target, func() error {
		f, err := os.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		if limit == nil {
			data, err = io.ReadAll(f)
		} else {
			data, err = io.ReadAll(io.LimitReader(f, *limit))
		}
		return err
	})
	if err == nil {
		return data, nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return nil, NewHTTPException(403, "File is not readable")
	}
	if pypath.IsOSError(err) {
		detail := pypath.OSErrorText(err)
		if detail == "" {
			detail = "File read failed"
		}
		return nil, NewHTTPException(400, detail)
	}
	return nil, err
}

// Nearest ancestor of start holding a git entry within fifty levels, false when none
func fsFindGitRoot(start string) (string, bool) {
	directory := start
	for range 50 {
		fi, err := pypath.PathlibStat(pypath.PathJoin(directory, ".git"))
		if err != nil {
			return "", false
		}
		if fi != nil {
			return pypath.PathStr(directory), true
		}
		parent := pypath.PathParent(directory)
		if parent == directory {
			return "", false
		}
		directory = parent
	}
	return "", false
}

// Host folder a new session of the profile starts in, its terminal cwd when that names a folder, else home
func fsDefaultCwd(ctx context.Context, profile string) (string, error) {
	sctx, scoped, release, err := profileScope(ctx, profile)
	if err != nil {
		return "", err
	}
	var candidates []any
	err = func() error {
		defer release()
		if scoped == "" {
			cfg, err := webLoadConfig(sctx)
			if err != nil {
				return err
			}
			terminal, _ := cfg["terminal"].(map[string]any)
			var envCwd any
			if v, ok := os.LookupEnv("TERMINAL_CWD"); ok {
				envCwd = v
			}
			candidates = []any{terminal["cwd"], envCwd}
			return nil
		}
		policy, err := tools.BuildProfileTerminalScope(sctx, scoped, nil)
		var unavailable *tools.TerminalPolicyUnavailable
		if errors.As(err, &unavailable) {
			return nil
		}
		if err != nil {
			return err
		}
		if v, ok := policy["TERMINAL_CWD"]; ok {
			candidates = []any{v}
		} else {
			candidates = []any{nil}
		}
		return nil
	}()
	if err != nil {
		return "", err
	}
	for _, rawValue := range candidates {
		raw := ""
		if pyrepr.Truthy(rawValue) {
			raw = pyrepr.Str(rawValue)
		}
		raw = pystr.Strip(raw)
		if raw == "" || raw == "." || raw == "auto" || raw == "cwd" {
			continue
		}
		expanded, err := pypath.PathExpanduser(raw)
		if err != nil {
			continue
		}
		candidate, err := pypath.Resolve(expanded)
		if err != nil {
			continue
		}
		if fi, _ := pypath.PathlibStat(candidate); fi != nil && fi.IsDir() {
			return candidate, nil
		}
	}
	return pypath.PathHome()
}

// Current git branch of a host checkout, empty when git fails or times out
func fsGitBranch(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "branch", "--show-current")
	cmd.SysProcAttr = tools.WindowsHideAttr()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	text, _ := pycodecs.DecodeReplace(out, "utf-8")
	return pystr.Strip(text)
}

// SSH workspace adapter of the profile, nil for a host local filesystem, 503 when it cannot dial
func fsBackend(ctx context.Context, profile string) (*SshWorkspaceFs, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	cfg, err := func() (map[string]any, error) {
		defer release()
		return webLoadConfig(sctx)
	}()
	if err != nil {
		return nil, err
	}
	profileEnv := config.LoadEnv(home.Dir(sctx))
	terminal := map[string]any{}
	if section, ok := cfg["terminal"].(map[string]any); ok {
		for k, v := range section {
			terminal[k] = v
		}
	}
	for _, pair := range [][2]string{
		{"ssh_host", "TERMINAL_SSH_HOST"}, {"ssh_user", "TERMINAL_SSH_USER"},
		{"ssh_port", "TERMINAL_SSH_PORT"}, {"ssh_key", "TERMINAL_SSH_KEY"},
	} {
		if !pyrepr.Truthy(terminal[pair[0]]) && profileEnv[pair[1]] != "" {
			terminal[pair[0]] = profileEnv[pair[1]]
		}
	}
	profileKey := pystr.Lower(pystr.Strip(profile))
	if profile == "" {
		profileKey = "current"
	}
	if profileKey == "" {
		profileKey = "current"
	}
	backend, err := getSshWorkspaceFs(sctx, profileKey, terminal)
	var fsErr *SshWorkspaceFsError
	if errors.As(err, &fsErr) {
		cause := error(fsErr)
		if fsErr.Cause != nil {
			cause = fsErr.Cause
		}
		slog.Warn(fmt.Sprintf("SSH workspace for profile %s is unavailable: %v", profileKey, cause))
		return nil, fsBackendHTTPError(err)
	}
	return backend, err
}

// Profile image staging dir for browser pastes, the one attachment admission accepts
func chatImageDir(h string) string { return home.HermesDir(h, "cache/images", "image_cache") }

// Whether path is a host file of the profile, a paste in its image cache or a staged upload
func isHostFile(ctx context.Context, path, profile string) (bool, error) {
	if strings.HasPrefix(strings.ToLower(path), "file:") {
		decoded, err := tuigateway.FileURLPath(path)
		if err != nil {
			return false, nil
		}
		path = decoded
	}
	if !pypath.IsAbs(path) {
		return false, nil
	}
	target, err := pypath.Realpath(path, pypath.StrictAll)
	if err != nil || strings.Contains(path, "\x00") {
		return false, nil
	}
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return false, err
	}
	h := home.Dir(sctx)
	cache := chatImageDir(h)
	release()
	if resolvedCache, err := pypath.Realpath(cache, pypath.StrictAll); err == nil &&
		pypath.Normcase(pypath.PathParent(target)) == pypath.Normcase(resolvedCache) {
		return true, nil
	}
	return stageduploads.IsStagedUpload(h, target), nil
}

// SSH workspace reader for path, nil when this host owns it
func remoteFsBackend(ctx context.Context, path, profile string) (*SshWorkspaceFs, error) {
	host, err := isHostFile(ctx, path, profile)
	if err != nil || host {
		return nil, err
	}
	return fsBackend(ctx, profile)
}

var fsBackendStatus = map[string]int{
	"EACCES": 403, "ECONN": 503, "EFBIG": 413, "EINVAL": 400, "EIO": 502, "ENOENT": 404, "ENOTREG": 400,
}

var fsBackendDetail = map[string]string{
	"EACCES": "File is not readable", "ECONN": "SSH workspace is unavailable", "EFBIG": "File too large",
	"EINVAL": "Invalid path", "EIO": "SSH workspace request failed", "ENOENT": "File not found",
	"ENOTREG": "Only regular files can be read",
}

// HTTP answer of an SSH workspace error, any other error unchanged
func fsBackendHTTPError(err error) error {
	var fsErr *SshWorkspaceFsError
	if !errors.As(err, &fsErr) {
		return err
	}
	status, ok := fsBackendStatus[fsErr.Code]
	if !ok {
		status = 400
	}
	detail, ok := fsBackendDetail[fsErr.Code]
	if !ok {
		if detail = fsErr.Error(); detail == "" {
			detail = "Filesystem request failed"
		}
	}
	return NewHTTPException(status, detail)
}

// Resolved directories GET media may read, where the agent and attach pipeline write media
func mediaServeRoots(ctx context.Context) []string {
	h := home.Dir(ctx)
	var out []string
	for _, name := range []string{"images", "screenshots", "cache"} {
		if root, err := pypath.Resolve(pypath.PathJoin(h, name)); err == nil {
			out = append(out, root)
		}
	}
	return out
}

// Base64 of a bounded file read while no SQLite connection to it is live
func readBase64File(path string) (string, error) {
	var data []byte
	err := serveOffline(path, func() error {
		var err error
		data, err = os.ReadFile(path)
		return err
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
