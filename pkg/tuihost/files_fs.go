package tuihost

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Detail of a 404 for a path that likely names a file inside the agent sandbox
const SandboxNotFound = "File not found on the gateway host: the path may live inside the agent sandbox"

// Status and detail of the error a file path helper raises
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string { return e.Detail }

// Docker backend probes pkg/gateway owns, assigned by pkg/gateway at init
var GatewayDocker struct {
	EnvActive    func(ctx context.Context) (bool, error)
	VolumeMounts func(ctx context.Context) ([][2]string, error)
}

var (
	windowsHost       = runtime.GOOS == "windows"
	fileURLDriveRe    = regexp.MustCompile(`(?i)^/[a-z]:`)
	fileURLSlashRe    = regexp.MustCompile(`(?i)%2f`)
	fileURLBackslash  = regexp.MustCompile(`(?i)%5c`)
	pathSeparatorsRe  = regexp.MustCompile(`[\\/]`)
	sensitiveDirNames = map[string]bool{"mcp-tokens": true, "pairing": true}
)

// Absolute resolved candidate of a raw path, relative paths joined to cwd or the process cwd
func resolveFsCandidate(raw string, cwd *string) (string, error) {
	candidate, err := pathutil.ExpandUserPath(raw)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(candidate) {
		var base string
		if cwd != nil {
			if base, err = pathutil.ExpandUserPath(*cwd); err != nil {
				return "", err
			}
		} else if base, err = os.Getwd(); err != nil {
			return "", err
		}
		if !filepath.IsAbs(base) {
			return "", &HTTPError{400, "Session working directory is unavailable"}
		}
		candidate = pathutil.Join(base, candidate)
	}
	return pathutil.Resolve(candidate)
}

// Count of dot segments in a path split on the host separators
func dotSegments(p string) int {
	var parts []string
	if windowsHost {
		parts = pathSeparatorsRe.Split(p, -1)
	} else {
		parts = strings.Split(p, "/")
	}
	n := 0
	for _, segment := range parts {
		if segment == "." || segment == ".." {
			n++
		}
	}
	return n
}

// Raw percent decoded once and resolved, empty when decoding adds a dot segment
func decodedCandidate(raw string, cwd *string) (string, bool, error) {
	decoded := urlutil.Unescape(raw)
	if dotSegments(decoded) > dotSegments(raw) {
		return "", false, nil
	}
	resolved, err := resolveFsCandidate(decoded, cwd)
	return resolved, err == nil, err
}

// Native path a file URL names decoded once, refusing an encoded separator or a foreign host
func fileURLPath(url string) (string, error) {
	parsed, err := urlutil.Split(url)
	if err != nil {
		return "", err
	}
	uriPath := parsed.Path
	host := parsed.Authority
	if strings.ToLower(host) == "localhost" {
		host = ""
	}
	windows := windowsHost || host != "" || fileURLDriveRe.MatchString(urlutil.Unescape(uriPath))
	if fileURLSlashRe.MatchString(uriPath) || (windows && fileURLBackslash.MatchString(uriPath)) {
		return "", errors.New("file: URL encodes a path separator")
	}
	if host != "" {
		if !windowsHost {
			return "", errors.New("file: URL names another host")
		}
		uriPath = "//" + host + uriPath
	}
	return urlutil.FilePath("file:" + uriPath)
}

// Whether a candidate exists, the errors pathlib does not swallow returned
func candidateExists(p string) (bool, error) {
	fi, err := pathutil.StatIfExists(p)
	return fi != nil, err
}

// Resolved host path a raw path or file URL names, an HTTP 400 for an empty or invalid one
func FsPath(rawPath string, cwd *string, decodeFallback bool) (string, *HTTPError) {
	raw := strings.TrimSpace(rawPath)
	if raw == "" {
		return "", &HTTPError{400, "Path is required"}
	}
	if strings.Contains(raw, "\x00") {
		return "", &HTTPError{400, "Invalid path"}
	}
	invalid := &HTTPError{400, "Invalid path"}
	fromURL := strings.HasPrefix(strings.ToLower(raw), "file:")
	var err error
	if fromURL {
		if raw, err = fileURLPath(raw); err != nil {
			return "", invalid
		}
	} else if windowsHost {
		raw = tools.MsysToWindowsPath(raw)
	}
	candidate, err := resolveFsCandidate(raw, cwd)
	if err != nil {
		var herr *HTTPError
		if errors.As(err, &herr) {
			return "", herr
		}
		return "", invalid
	}
	if decodeFallback && !fromURL && strings.Contains(raw, "%") {
		exists, err := candidateExists(candidate)
		if err != nil {
			return "", invalid
		}
		if !exists {
			decoded, ok, err := decodedCandidate(raw, cwd)
			if err != nil {
				var herr *HTTPError
				if errors.As(err, &herr) {
					return "", herr
				}
				return "", invalid
			}
			if ok {
				found, err := candidateExists(decoded)
				if err != nil {
					return "", invalid
				}
				if found {
					candidate = decoded
				}
			}
		}
	}
	return candidate, nil
}

// Context bound to the profile whose sandbox a container path names, its home and terminal policy
func agentPathScope(ctx context.Context, profileHome string) context.Context {
	bound := false
	if profileHome != "" && resolvedOrRaw(profileHome) != resolvedOrRaw(home.Dir(ctx)) {
		ctx = home.WithOverride(ctx, profileHome)
		bound = true
	}
	foreign := home.Override(ctx) != "" && resolvedOrRaw(home.Dir(ctx)) != resolvedOrRaw(home.Process())
	if bound || (foreign && tools.GetTerminalScope(ctx) == nil) {
		ctx = tools.WithProfileTerminalScope(ctx, home.Dir(ctx), nil)
	}
	return ctx
}

// Resolved path, the raw path when resolution fails
func resolvedOrRaw(p string) string {
	if resolved, err := pathutil.Resolve(p); err == nil {
		return resolved
	}
	return p
}

// Whether a path is a docker backend container path rather than a host path or a typo on the host
func IsUnmappedContainerPath(ctx context.Context, raw, profileHome string) (unmapped bool) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("container path classification failed", "path", raw, "error", r)
			unmapped = false
		}
	}()
	scoped := agentPathScope(ctx, profileHome)
	active, err := GatewayDocker.EnvActive(scoped)
	if err != nil {
		slog.Debug("container path classification failed", "path", raw, "error", err)
		return false
	}
	if !active || tools.MsysToWindowsPath(raw) != raw {
		return false
	}
	mounts, err := GatewayDocker.VolumeMounts(scoped)
	if err != nil {
		slog.Debug("container path classification failed", "path", raw, "error", err)
		return false
	}
	roots := []string{"/workspace", "/root"}
	for _, mount := range mounts {
		roots = append(roots, filepath.ToSlash(filepath.Clean(mount[1])))
	}
	for _, root := range roots {
		if raw == root || strings.HasPrefix(raw, strings.TrimRight(root, "/")+"/") {
			return true
		}
	}
	return false
}

// Whether a basename is an env file or a credential store, case insensitive
func IsSensitiveFilename(name string) bool {
	lowered := strings.ToLower(name)
	if lowered == ".env" || strings.HasPrefix(lowered, ".env.") || lowered == ".envrc" {
		return true
	}
	switch lowered {
	case "auth.json", "auth.lock", "credentials", "config.yaml", ".anthropic_oauth.json", "google_token.json",
		"google_oauth_pending.json", "google_oauth.json", "webhook_subscriptions.json", ".git-credentials":
		return true
	}
	for _, store := range profiles.SecretCacheStores() {
		if lowered == path.Base(store) {
			return true
		}
	}
	return false
}

// Whether the basename is sensitive or any component is a credential directory
func IsSensitivePath(p string) bool {
	if IsSensitiveFilename(pathutil.Name(p)) {
		return true
	}
	anchor, parts := pathutil.Parts(p)
	for _, part := range append([]string{anchor}, parts...) {
		if sensitiveDirNames[strings.ToLower(part)] {
			return true
		}
	}
	return false
}

// 404 detail telling an untranslated sandbox path apart from a plain missing file
func fsSandboxNotFoundDetail(ctx context.Context, p, profileHome string) string {
	if IsUnmappedContainerPath(ctx, filepath.ToSlash(filepath.Clean(p)), profileHome) {
		return SandboxNotFound
	}
	return "File not found"
}

// Resolved regular readable non sensitive file a path names with its stat, else the HTTP error
func FsRegularFile(ctx context.Context, p, profileHome string) (string, fs.FileInfo, *HTTPError) {
	target, herr := FsPath(filepath.Clean(p), nil, false)
	if herr != nil {
		return "", nil, herr
	}
	st, err := os.Stat(target)
	if err != nil {
		switch errorutil.Kind(err) {
		case "FileNotFoundError", "NotADirectoryError":
			return "", nil, &HTTPError{404, fsSandboxNotFoundDetail(ctx, target, profileHome)}
		case "PermissionError":
			return "", nil, &HTTPError{403, "File is not readable"}
		}
		detail := err.Error()
		if detail == "" {
			detail = "Invalid path"
		}
		return "", nil, &HTTPError{400, detail}
	}
	if st.IsDir() {
		return "", nil, &HTTPError{400, "Path points to a directory"}
	}
	if !st.Mode().IsRegular() {
		return "", nil, &HTTPError{400, "Only regular files can be read"}
	}
	if IsSensitivePath(target) {
		return "", nil, &HTTPError{403, "Access to sensitive files is not allowed"}
	}
	return target, st, nil
}
