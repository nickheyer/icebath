package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

const mediaFetchMaxBytes = 50 * 1024 * 1024

var (
	mediaAudioExts             = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".m2a": true, ".wav": true, ".m4a": true, ".flac": true}
	telegramAudioAttachmentExt = map[string]bool{".mp3": true, ".m4a": true}
	remoteMediaBasenameRE      = regexp.MustCompile(`[^\p{L}\p{N}_.\-]`)
)

// Lowercase name of a platform value
func platformName(platform Platform) string { return pystr.Lower(string(platform)) }

// True when a media file goes through the platform audio sender, Telegram taking only voice or MP3 and M4A
func ShouldSendMediaAsAudio(platform Platform, ext string, isVoice bool) bool {
	normalized := pystr.Lower(ext)
	if !mediaAudioExts[normalized] {
		return false
	}
	if platformName(platform) != "telegram" {
		return true
	}
	return isVoice || telegramAudioAttachmentExt[normalized]
}

// Safe MEDIA paths of a response with their voice flags, unsafe ones logged and dropped
func FilterMediaDeliveryPaths(ctx context.Context, media []MediaRef) ([]MediaRef, error) {
	return FilterMediaDeliveryPathsFor(ctx, media, "", nil)
}

// Safe MEDIA paths for a session, each rejected path appended to dropped with its reason when dropped is set
func FilterMediaDeliveryPathsFor(ctx context.Context, media []MediaRef, sessionKey string, dropped *[]map[string]any) ([]MediaRef, error) {
	out := []MediaRef{}
	for _, m := range media {
		safe, err := validatedDeliveryPath(ctx, m.Path, sessionKey, "MEDIA directive path", dropped)
		if err != nil {
			return nil, err
		}
		if safe != "" {
			out = append(out, MediaRef{Path: safe, IsVoice: m.IsVoice})
		}
	}
	return out, nil
}

// Safe bare local file paths for a session, unsafe ones logged and dropped
func FilterLocalDeliveryPaths(ctx context.Context, paths []string, sessionKey string) ([]string, error) {
	out := []string{}
	for _, p := range paths {
		safe, err := validatedDeliveryPath(ctx, p, sessionKey, "local file path", nil)
		if err != nil {
			return nil, err
		}
		if safe != "" {
			out = append(out, safe)
		}
	}
	return out, nil
}

// Validated host path, retried through the active remote sandbox, a rejection warned with its reason
func validatedDeliveryPath(ctx context.Context, raw, sessionKey, label string, dropped *[]map[string]any) (string, error) {
	safe, err := ValidateMediaDeliveryPath(ctx, raw, sessionKey)
	if err != nil {
		return "", err
	}
	if safe == "" {
		if safe, err = FetchRemoteMedia(ctx, raw); err != nil {
			return "", err
		}
	}
	if safe == "" {
		reason := "denied by the delivery policy"
		if !existingRegularFile(raw) {
			reason = "not found on this host"
		}
		slog.Warn(fmt.Sprintf("Skipping %s (%s): %s", label, reason, logSafePath(raw)))
		if dropped != nil {
			*dropped = append(*dropped, map[string]any{"path": raw, "reason": reason})
		}
	}
	return safe, nil
}

// True when the user expanded path names a regular file
func existingRegularFile(raw string) bool {
	if expanduserRaises(raw) != nil {
		return false
	}
	info, err := os.Stat(pypath.Expanduser(raw))
	return err == nil && info.Mode().IsRegular()
}

// Home relative denylist entries applied to sandbox paths, the credential stores under the profile dir included
func remoteDeniedHomeRelative() []string {
	out := append([]string(nil), mediaDeliveryDeniedHomeSubpaths...)
	for _, rel := range rootCredentialPaths() {
		out = append(out, path.Join(".icebath", strings.ReplaceAll(rel, string(os.PathSeparator), "/")))
	}
	return out
}

// Posix path components of a cleaned absolute or relative path
func posixParts(p string) []string {
	var parts []string
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

// Whether a sandbox path hits the host denylist, an unknown home matching home entries at any component
func RemotePathIsDenied(p string, remoteHome string) bool {
	if !strings.HasPrefix(p, "/") {
		return true
	}
	target := pypath.Normpath(p)
	under := func(root string) bool {
		return target == root || strings.HasPrefix(target, strings.TrimSuffix(root, "/")+"/")
	}
	homeDir := ""
	if remoteHome != "" {
		homeDir = pypath.Normpath(remoteHome)
	}
	for _, prefix := range mediaDeliveryDeniedPrefixes {
		if prefix != homeDir && under(prefix) {
			return true
		}
	}
	rels := remoteDeniedHomeRelative()
	if homeDir != "" {
		for _, rel := range rels {
			if under(path.Join(homeDir, rel)) {
				return true
			}
		}
		return false
	}
	parts := posixParts(target)
	for _, rel := range rels {
		relParts := posixParts(rel)
		for i := 0; i+len(relParts) <= len(parts); i++ {
			if strings.Join(parts[i:i+len(relParts)], "\x00") == strings.Join(relParts, "\x00") {
				return true
			}
		}
	}
	return false
}

// Live remote terminal environment of the current session, nil for the local backend or before one started
func activeRemoteEnv(ctx context.Context) (tools.Environment, error) {
	backend, err := tenv(ctx, "TERMINAL_ENV", "local")
	if err != nil {
		return nil, err
	}
	backend = pystr.Lower(pystr.Strip(backend))
	if !tools.IsRemoteTerminalBackend(backend) && !tools.PluginBackendIsRemote(backend) {
		return nil, nil
	}
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		env = mediaToolsEnv(ctx)
	}
	sessionID := tools.SessionEnv(env, "ICEBATH_SESSION_ID")
	if sessionID == "" {
		sessionID = tools.SessionEnv(env, "ICEBATH_SESSION_KEY")
	}
	if sessionID == "" {
		sessionID = "default"
	}
	return tools.GetActiveEnv(env, sessionID)
}

var activeRemoteEnvSeam = activeRemoteEnv

// Remote home an environment exposes, empty when it has none
func environmentRemoteHome(env tools.Environment) string {
	if rh, ok := env.(interface{ RemoteHome() (string, bool) }); ok {
		h, _ := rh.RemoteHome()
		return h
	}
	return ""
}

// Host path of a validated copy of a sandbox file, empty when no remote backend is active or the path is refused
func FetchRemoteMedia(ctx context.Context, raw string) (string, error) {
	if MediaDeliveryStrict(ctx) {
		return "", nil
	}
	env, err := activeRemoteEnvSeam(ctx)
	if err != nil || env == nil {
		return "", err
	}
	remoteHome := environmentRemoteHome(env)
	candidate := pypath.Normpath(normalizeMediaTagPath(raw))
	if candidate == "~" || strings.HasPrefix(candidate, "~/") {
		if remoteHome == "" {
			return "", nil
		}
		rest := ""
		if len(candidate) > 2 {
			rest = candidate[2:]
		}
		candidate = pypath.Normpath(path.Join(remoteHome, rest))
	}
	if !strings.HasPrefix(candidate, "/") || RemotePathIsDenied(candidate, remoteHome) {
		return "", nil
	}
	dest, err := fetchRemoteMediaCopy(ctx, env, candidate, remoteHome)
	if err != nil {
		var fetchErr *tools.FileFetchError
		if errors.As(err, &fetchErr) {
			slog.Warn(fmt.Sprintf("Remote media fetch of %s skipped: %s", logSafePath(candidate), fetchErr.Error()))
		} else {
			slog.Warn(fmt.Sprintf("Remote media fetch of %s failed", logSafePath(candidate)), "error", err)
		}
		return "", nil
	}
	if dest == "" {
		return "", nil
	}
	validated, err := ValidateMediaDeliveryPath(ctx, dest, "")
	if err != nil {
		return "", err
	}
	if validated == "" {
		_ = os.Remove(dest)
		return "", nil
	}
	slog.Info(fmt.Sprintf("Fetched remote media %s from the %s sandbox", logSafePath(candidate), env.EnvType()))
	return validated, nil
}

// Copies the resolved sandbox file into the document cache, empty when its link target is refused
func fetchRemoteMediaCopy(ctx context.Context, env tools.Environment, candidate, remoteHome string) (string, error) {
	resolved := env.FetchRealpath(ctx, candidate)
	if resolved == "" || RemotePathIsDenied(resolved, remoteHome) {
		return "", nil
	}
	basename := remoteMediaBasenameRE.ReplaceAllString(resolved[strings.LastIndex(resolved, "/")+1:], "_")
	if basename == "" {
		basename = "file"
	}
	cacheDir, err := mediacache.DocumentCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	token := make([]byte, 6)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return "", err
	}
	dest := path.Join(cacheDir, "remote_"+hex.EncodeToString(token)+"_"+basename)
	if err := env.FetchFile(ctx, resolved, dest, mediaFetchMaxBytes); err != nil {
		return "", err
	}
	return dest, nil
}
