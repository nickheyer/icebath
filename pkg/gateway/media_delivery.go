package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	MediaDeliveryAllowDirsEnv          = mediaAllowDirsEnv
	MediaDeliveryTrustRecentEnv        = "ICEBATH_MEDIA_TRUST_RECENT_FILES"
	MediaDeliveryTrustRecentSecondsEnv = mediaTrustRecentSecondsEnv
	MediaDeliveryStrictEnv             = "ICEBATH_MEDIA_DELIVERY_STRICT"

	mediaDeliveryTrustRecentDefaultSeconds = 600
)

var (
	mediaDeliveryCacheSubdirs = []string{"images", "audio", "videos", "documents", "screenshots", tools.GeneratedSubdir}

	mediaDeliveryDeniedPrefixes = []string{
		"/etc", "/proc", "/sys", "/dev", "/root", "/boot", "/var/log", "/var/lib", "/var/run"}
	mediaDeliveryDeniedHomeSubpaths = []string{
		".ssh", ".aws", ".gnupg", ".kube", ".docker", ".config", ".azure", ".gcloud", "Library/Keychains"}

	logUnsafeCharsRE = regexp.MustCompile("[\x00-\x1f\x7f\u0085  ]")
	dockerDriveRE    = regexp.MustCompile(`^[A-Za-z]:[/\\]`)
	kanbanBoardRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

	mediaNow = time.Now
)

// SQLite store plus its WAL, SHM and rollback journal sidecars
func sqliteFiles(name string) []string {
	return []string{name, name + "-wal", name + "-shm", name + "-journal"}
}

// Home relative credential stores every home root denies for delivery
func rootCredentialPaths() []string {
	out := []string{".env", "auth.json", "auth.lock", "credentials", "config.yaml", ".anthropic_oauth.json",
		"google_token.json", "google_oauth_pending.json", filepath.Join("auth", "google_oauth.json"),
		"webhook_subscriptions.json"}
	out = append(out, profiles.SecretCacheStores()...)
	out = append(out, "pairing", "mcp-tokens", "webapp", "sessions", "browser-profile")
	out = append(out, sqliteFiles("state.db")...)
	return append(out, sqliteFiles("kanban.db")...)
}

// Launch home cache directories media delivery always accepts, swapped by tests
var mediaDeliverySafeRoots = func() []string {
	launch := home.Process()
	roots := []string{
		home.HermesDir(launch, "cache/images", "image_cache"),
		home.HermesDir(launch, "cache/audio", "audio_cache"),
		home.HermesDir(launch, "cache/videos", "video_cache"),
		home.HermesDir(launch, "cache/documents", "document_cache"),
		home.HermesDir(launch, "cache/screenshots", "browser_screenshots"),
	}
	for _, d := range []string{"image_cache", "audio_cache", "video_cache", "document_cache", "browser_screenshots"} {
		roots = append(roots, filepath.Join(launch, d))
	}
	for _, d := range mediaDeliveryCacheSubdirs {
		roots = append(roots, filepath.Join(launch, "cache", d))
	}
	return roots
}

// Every profile directory under the root, read at check time
func mediaProfileDirs() []string {
	dir := filepath.Join(home.Root(), "profiles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name()); isDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// Per profile cache roots plus every home connector downloads directory
func profileCacheRoots(ctx context.Context) []string {
	dirs := mediaProfileDirs()
	var out []string
	for _, p := range dirs {
		for _, sub := range mediaDeliveryCacheSubdirs {
			out = append(out, filepath.Join(p, "cache", sub))
		}
	}
	for _, p := range append([]string{home.Dir(ctx)}, dirs...) {
		out = append(out, filepath.Join(p, home.ConnectorDownloadsSubdir))
	}
	return out
}

// Active home, shared root and every profile whose credential stores the denylist covers, first seen order
func credentialHomeRoots(ctx context.Context) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range append([]string{home.Dir(ctx), home.Root()}, mediaProfileDirs()...) {
		key := pypath.PathStr(p)
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// Root shared kanban home, ICEBATH_KANBAN_HOME over the default root
func kanbanRoot() (string, error) {
	root := pystr.Strip(os.Getenv("ICEBATH_KANBAN_HOME"))
	if root == "" {
		root = home.Root()
	}
	return pypath.PathExpanduser(root)
}

// Every directory under the kanban boards root
func kanbanBoardDirs() ([]string, error) {
	root, err := kanbanRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "kanban", "boards")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name()); isDir(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Durable kanban attachment roots, the override alone when set
func kanbanAttachmentRoots() ([]string, error) {
	if override := pystr.Strip(os.Getenv("ICEBATH_KANBAN_ATTACHMENTS_ROOT")); override != "" {
		expanded, err := pypath.PathExpanduser(override)
		if err != nil {
			return nil, err
		}
		return []string{expanded}, nil
	}
	root, err := kanbanRoot()
	if err != nil {
		return nil, err
	}
	boards, err := kanbanBoardDirs()
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Join(root, "kanban", "attachments")}
	for _, board := range boards {
		if st, err := os.Lstat(board); err == nil && st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if kanbanBoardRE.MatchString(filepath.Base(board)) && isRegularFile(filepath.Join(board, "kanban.db")) {
			roots = append(roots, filepath.Join(board, "attachments"))
		}
	}
	return roots, nil
}

// Roots from which model emitted local media may be delivered
func mediaDeliveryAllowedRoots(ctx context.Context) ([]string, error) {
	roots := append(mediaDeliverySafeRoots(), profileCacheRoots(ctx)...)
	kanban, err := kanbanAttachmentRoots()
	if err != nil {
		return nil, err
	}
	roots = append(roots, kanban...)
	for _, chunk := range strings.Split(MediaDeliveryAllowDirs(ctx), string(os.PathListSeparator)) {
		for _, raw := range strings.Split(chunk, ",") {
			raw = pystr.Strip(raw)
			if err := expanduserRaises(raw); err != nil {
				return nil, err
			}
			if root := pypath.Expanduser(raw); pypath.IsAbs(root) {
				roots = append(roots, root)
			}
		}
	}
	return roots, nil
}

// ValueError os.path.expanduser raises for a NUL inside the user name of a tilde path on POSIX
func expanduserRaises(p string) error {
	if runtime.GOOS == "windows" || !strings.HasPrefix(p, "~") {
		return nil
	}
	if user, _, _ := strings.Cut(p, "/"); strings.ContainsRune(user, 0) {
		return &pypath.ValueError{Msg: "embedded null byte"}
	}
	return nil
}

// Recency window in seconds for trusting fresh files, zero for pure allowlist mode
func mediaDeliveryRecencySeconds(ctx context.Context) float64 {
	if !MediaDeliveryTrustRecent(ctx) {
		return 0
	}
	custom := pystr.Strip(MediaDeliveryTrustRecentSeconds(ctx))
	if custom == "" {
		return mediaDeliveryTrustRecentDefaultSeconds
	}
	v, err := pyint.ParseFloat(custom)
	if err != nil {
		return mediaDeliveryTrustRecentDefaultSeconds
	}
	return math.Max(0, v)
}

// Named board kanban stores with sidecars beside the allowlisted attachments
func kanbanBoardDBPaths() ([]string, error) {
	boards, err := kanbanBoardDirs()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, board := range boards {
		for _, name := range sqliteFiles("kanban.db") {
			out = append(out, filepath.Join(board, name))
		}
	}
	return out, nil
}

// Absolute denylist paths under which delivery is never allowed
func mediaDeliveryDeniedPaths(ctx context.Context) ([]string, error) {
	userHome := pypath.Expanduser("~")
	out := append([]string(nil), mediaDeliveryDeniedPrefixes...)
	for _, sub := range mediaDeliveryDeniedHomeSubpaths {
		out = append(out, filepath.Join(userHome, sub))
	}
	for _, r := range credentialHomeRoots(ctx) {
		for _, rel := range rootCredentialPaths() {
			out = append(out, filepath.Join(r, rel))
		}
	}
	boardDBs, err := kanbanBoardDBPaths()
	if err != nil {
		return nil, err
	}
	return append(out, boardDBs...), nil
}

// Resolved path, optionally home expanded, or false when resolution fails
func resolveMediaPath(p string, strict, expand bool) (string, bool) {
	if expand {
		e, err := pypath.PathExpanduser(p)
		if err != nil {
			return "", false
		}
		p = e
	}
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	var out string
	var err error
	if strict {
		out, err = pypath.Realpath(pypath.PathStr(p), pypath.StrictAll)
		out = pypath.PathStr(out)
	} else {
		out, err = pypath.Resolve(p)
	}
	if err != nil {
		return "", false
	}
	return out, true
}

// Whether the resolved path lives under a denied path other than the running user home
func pathUnderDeniedPrefix(ctx context.Context, resolved string) (bool, error) {
	userHome, homeOK := resolveMediaPath(pypath.Expanduser("~"), false, false)
	denied, err := mediaDeliveryDeniedPaths(ctx)
	if err != nil {
		return false, err
	}
	for _, denied := range denied {
		resolvedDenied, ok := resolveMediaPath(denied, false, true)
		if !ok {
			continue
		}
		if pathIsWithin(resolved, resolvedDenied) && !(homeOK && samePath(resolvedDenied, userHome)) {
			return true, nil
		}
	}
	return false, nil
}

// Whether the file mtime falls inside the trust window
func fileIsRecentlyProduced(resolved string, window float64) bool {
	if window <= 0 {
		return false
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	age := float64(mediaNow().UnixNano()-st.ModTime().UnixNano()) / 1e9
	return age <= window
}

// Host and container path pairs of the TERMINAL_DOCKER_VOLUMES bind mounts
func parseDockerVolumeMounts(ctx context.Context) ([][2]string, error) {
	raw, err := tenv(ctx, "TERMINAL_DOCKER_VOLUMES", "")
	if err != nil {
		return nil, err
	}
	raw = pystr.Strip(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		return nil, nil
	}
	list, _ := parsed.([]any)
	var mounts [][2]string
	for _, entry := range list {
		spec := ""
		if s, ok := entry.(string); ok {
			spec = pystr.Strip(s)
		}
		start := 0
		if dockerDriveRE.MatchString(spec) {
			start = 2
		}
		sep := -1
		if start <= len(spec) {
			if i := strings.Index(spec[start:], ":/"); i >= 0 {
				sep = start + i
			}
		}
		if sep <= 0 {
			continue
		}
		containerRaw, _, _ := strings.Cut(spec[sep+1:], ":")
		hostExpanded := pypath.Expanduser(spec[:sep])
		hr := []rune(hostExpanded)
		if !(strings.HasPrefix(hostExpanded, "/") || (len(hr) > 1 && hr[1] == ':')) {
			continue
		}
		if hostPath, ok := resolveMediaPath(hostExpanded, false, false); ok {
			mounts = append(mounts, [2]string{hostPath, containerRaw})
		}
	}
	return mounts, nil
}

// Whether the active terminal backend is Docker
func dockerEnvActive(ctx context.Context) (bool, error) {
	v, err := tenv(ctx, "TERMINAL_ENV", "")
	return pystr.Lower(pystr.Strip(v)) == "docker", err
}

// Whether the Docker backend runs persistent containers
func dockerPersistentActive(ctx context.Context) (bool, error) {
	active, err := dockerEnvActive(ctx)
	if err != nil || !active {
		return false, err
	}
	v, err := tenv(ctx, "TERMINAL_CONTAINER_PERSISTENT", "true")
	return mediaTruthy[pystr.Lower(pystr.Strip(v))], err
}

// Host and container pairs of the auto mounted cache directories under Docker
func cacheDirContainerMounts(ctx context.Context) ([][2]string, error) {
	active, err := dockerEnvActive(ctx)
	if err != nil || !active {
		return nil, err
	}
	var out [][2]string
	for _, m := range tools.GetCacheDirectoryMounts(mediaToolsEnv(ctx), "") {
		out = append(out, [2]string{m.HostPath, m.ContainerPath})
	}
	return out, nil
}

// Logs why a container absolute media path failed translation, Docker only and silent when quiet
func warnUnresolvedDockerMedia(ctx context.Context, candidate, sessionKey, reason string, quiet bool) error {
	if quiet {
		return nil
	}
	active, err := dockerEnvActive(ctx)
	if err != nil || !active {
		return err
	}
	suffix := ""
	if sessionKey != "" {
		suffix = ", session_key=" + sessionKey
	}
	slog.Warn(fmt.Sprintf("Docker MEDIA path %s did not resolve to a host sandbox file (%s%s); the producing container's sandbox directory may not exist yet or was pruned",
		logSafePath(candidate), reason, suffix))
	return nil
}

// Host path of a container absolute path by longest prefix over the volume, cache, workspace and home mounts
func translateDockerContainerMediaPath(ctx context.Context, candidate, sessionKey, workspaceKey string, quiet bool, workspaceCwd string) (string, bool, error) {
	candidatePosix := pypath.PathAsPosix(candidate)
	if !strings.HasPrefix(candidatePosix, "/") {
		return "", false, nil
	}
	mounts, err := parseDockerVolumeMounts(ctx)
	if err != nil {
		return "", false, err
	}
	cacheMounts, err := cacheDirContainerMounts(ctx)
	if err != nil {
		return "", false, err
	}
	mounts = append(mounts, cacheMounts...)
	mounted := map[string]bool{}
	for _, m := range mounts {
		mounted[pypath.PathAsPosix(m[1])] = true
	}
	if !mounted["/workspace"] {
		roots, err := defaultDockerWorkspaceHostRoots(ctx, sessionKey, workspaceKey, workspaceCwd)
		if err != nil {
			return "", false, err
		}
		for _, r := range roots {
			mounts = append(mounts, [2]string{r, "/workspace"})
		}
	}
	if !mounted["/root"] && !strings.HasPrefix(candidatePosix, "/root/.icebath") {
		roots, err := dockerPersistentSandboxRoots(ctx, sessionKey, "home", workspaceKey)
		if err != nil {
			return "", false, err
		}
		for _, r := range roots {
			mounts = append(mounts, [2]string{r, "/root"})
		}
	}
	if len(mounts) == 0 {
		return "", false, warnUnresolvedDockerMedia(ctx, candidate, sessionKey, "no sandbox mounts resolved", quiet)
	}
	type hit struct {
		host, container string
		score           int
	}
	var matched []hit
	for _, m := range mounts {
		prefix := strings.TrimRight(pypath.PathAsPosix(m[1]), "/")
		if prefix == "" {
			prefix = "/"
		}
		if candidatePosix == prefix || strings.HasPrefix(candidatePosix, prefix+"/") {
			matched = append(matched, hit{m[0], m[1], len([]rune(prefix))})
		}
	}
	if len(matched) == 0 {
		return "", false, warnUnresolvedDockerMedia(ctx, candidate, sessionKey, "no mounted prefix matches", quiet)
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].score > matched[j].score })
	for _, m := range matched {
		rel, ok := pypath.PathRelativeTo(candidate, m.container)
		if !ok {
			continue
		}
		translated, ok := resolveMediaPath(filepath.Join(m.host, filepath.FromSlash(rel)), true, false)
		if ok && pathIsWithin(translated, m.host) {
			return translated, true, nil
		}
	}
	return "", false, warnUnresolvedDockerMedia(ctx, candidate, sessionKey, "host file missing from sandbox", quiet)
}

// Safe absolute file path for native media delivery, empty when the denylist or strict policy refuses it
func ValidateMediaDeliveryPath(ctx context.Context, path, sessionKey string) (string, error) {
	candidate := normalizeMediaTagPath(path)
	if candidate == "" || expanduserRaises(candidate) != nil {
		return "", nil
	}
	expanded := pypath.Expanduser(candidate)
	if !pypath.IsAbs(expanded) {
		return "", nil
	}
	resolved, ok, err := translateDockerContainerMediaPath(ctx, expanded, sessionKey, "", false, "")
	if err != nil {
		return "", err
	}
	if !ok {
		resolved, ok = resolveMediaPath(expanded, true, false)
	}
	if !ok || !isRegularFile(resolved) {
		return "", nil
	}
	allowed, err := mediaDeliveryAllowedRoots(ctx)
	if err != nil {
		return "", err
	}
	for _, root := range allowed {
		if resolvedRoot, ok := resolveMediaPath(root, false, true); ok && pathIsWithin(resolved, resolvedRoot) {
			return resolved, nil
		}
	}
	if !MediaDeliveryStrict(ctx) {
		denied, err := pathUnderDeniedPrefix(ctx, resolved)
		if err != nil || denied {
			return "", err
		}
		return resolved, nil
	}
	window := mediaDeliveryRecencySeconds(ctx)
	if window <= 0 {
		return "", nil
	}
	denied, err := pathUnderDeniedPrefix(ctx, resolved)
	if err != nil || denied {
		return "", err
	}
	if fileIsRecentlyProduced(resolved, window) {
		return resolved, nil
	}
	return "", nil
}

// Single line length bounded path for log output
func logSafePath(path string) string {
	r := []rune(logUnsafeCharsRE.ReplaceAllString(path, "?"))
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
