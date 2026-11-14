package file

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
)

const (
	safeRootEnv     = "ICEBATH_WRITE_SAFE_ROOT"
	denialNone      = ""
	denialNamespace = "nt_namespace"
	denialSafeRoot  = "safe_root"
	denialProtected = "credential"
	depthSuffix     = " (Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"
)

var secretCacheStores = [...]string{
	"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json",
	"cache/bws_cache.json", "cache/bws_cache.enc.json", "cache/op_cache.json", "runtime/photon-sidecar.json",
}

var homeCredentialDirs = [...]string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud"}

var protectedSubpaths = [...]string{"state.db", "sessions", "mcp-tokens", "pairing", "vault", "browser-profile", "webapp"}

type readDeniedDir struct {
	subdir, dirMsg, fileMsg string
}

var readDeniedDirs = [...]readDeniedDir{
	{"mcp-tokens",
		"is the icebath MCP token directory and cannot be read directly.",
		"is an icebath MCP token file and cannot be read directly."},
	{"browser-profile",
		"is the icebath real-profile browser snapshot directory (copied cookies/logins) and cannot be read directly.",
		"is inside the icebath real-profile browser snapshot (copied cookies/logins) and cannot be read directly."},
	{"vault",
		"is the icebath credential vault directory and cannot be read directly (secrets are filled server-side by browser_vault).",
		"is inside the icebath credential vault (encrypted secrets + local key) and cannot be read directly (browser_vault resolves them server-side)."},
	{"webapp",
		"is the icebath Webapp sign-in directory and cannot be read directly.",
		"is an icebath Webapp sign-in file (the browser credential or its sign-in records) and cannot be read directly."},
}

type Safety struct {
	homes     *home.Resolver
	protector *approval.Protector
}

func NewSafety(homes *home.Resolver, protector *approval.Protector) (*Safety, error) {
	switch {
	case homes == nil:
		return nil, &tool.UnfilledError{Name: "file.Safety.Homes"}
	case protector == nil:
		return nil, &tool.UnfilledError{Name: "file.Safety.Protector"}
	}
	return &Safety{homes: homes, protector: protector}, nil
}

func HomeCredentialDirs() []string {
	return append([]string(nil), homeCredentialDirs[:]...)
}

func realPath(path string) string {
	if runtime.GOOS != "windows" {
		return resolveLinks(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	var tail []string
	cur := abs
	for hops := 0; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 && hops < 40 {
			if target, err := os.Readlink(cur); err == nil {
				hops++
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(cur), target)
				}
				cur = filepath.Clean(target)
				continue
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isUnder(resolved, base string) bool {
	if samePath(resolved, base) {
		return true
	}
	prefix := base
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return len(resolved) >= len(prefix) && samePath(resolved[:len(prefix)], prefix)
}

func joinHome(base, rel string) string {
	return filepath.Join(base, filepath.FromSlash(rel))
}

func (s *Safety) homePaths(ctx context.Context) (string, string, error) {
	active, err := s.homes.Home(ctx)
	if err != nil {
		return "", "", err
	}
	root, err := home.DefaultRoot()
	if err != nil {
		return "", "", err
	}
	return active, root, nil
}

func (s *Safety) productDirs(ctx context.Context) ([]string, error) {
	active, root, err := s.homePaths(ctx)
	if err != nil {
		return nil, err
	}
	dirs := []string{realPath(active)}
	if resolvedRoot := realPath(root); resolvedRoot != dirs[0] {
		dirs = append(dirs, resolvedRoot)
	}
	return dirs, nil
}

func (s *Safety) guardHomes(ctx context.Context, path string) []string {
	candidates := []string{home.ExpandUser("~")}
	for _, c := range [...]string{home.RealHome(ctx, nil), s.homes.SubprocessHome(ctx, nil), home.ProfileHomeDir(ctx, nil)} {
		if c != "" {
			candidates = append(candidates, c)
		}
	}
	if len(path) > 1 && path[0] == '~' && path[1] != '/' && path[1] != '\\' {
		name := path[1:]
		if i := strings.IndexAny(name, `/\`); i >= 0 {
			name = name[:i]
		}
		if expanded := home.ExpandUser("~" + name); !strings.HasPrefix(expanded, "~") {
			candidates = append(candidates, expanded)
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	var out []string
	for _, c := range candidates {
		resolved := realPath(c)
		if _, dup := seen[resolved]; !dup {
			seen[resolved] = struct{}{}
			out = append(out, resolved)
		}
	}
	return out
}

func IsNTNamespacePath(path string) bool {
	s := strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(s, `\??\`) || strings.HasPrefix(s, `\\.\`) {
		return true
	}
	if strings.HasPrefix(s, `\\?\`) {
		upper := strings.ToUpper(s[4:])
		return strings.HasPrefix(upper, `UNC\`) || strings.HasPrefix(upper, `GLOBALROOT\`)
	}
	return false
}

func NTNamespaceError(path, verb string) string {
	if !IsNTNamespacePath(path) {
		return ""
	}
	return verb + " denied: '" + path + "' uses a Windows NT/device namespace prefix " +
		"(\\??\\, \\\\.\\, \\\\?\\UNC\\, or GLOBALROOT). These paths bypass " +
		"normal path normalization and can trigger outbound SMB " +
		"authentication (NTLM credential leak) merely by being resolved. " +
		"Use a normal absolute path instead."
}

func (s *Safety) writeDeniedPaths(userHome, active, root string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, rel := range [...]string{
		".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519",
		".netrc", ".pgpass", ".npmrc", ".pypirc", ".git-credentials",
	} {
		out[realPath(joinHome(userHome, rel))] = struct{}{}
	}
	productFiles := append([]string{".env", ".anthropic_oauth.json", "auth/google_oauth.json"}, secretCacheStores[:]...)
	for _, rel := range productFiles {
		for _, base := range [...]string{active, root} {
			out[realPath(joinHome(base, rel))] = struct{}{}
		}
	}
	for _, p := range [...]string{"/etc/sudoers", "/etc/passwd", "/etc/shadow"} {
		out[realPath(p)] = struct{}{}
	}
	return out
}

func writeDeniedPrefixes(userHome string) []string {
	var out []string
	for _, rel := range homeCredentialDirs {
		out = append(out, realPath(joinHome(userHome, rel))+string(filepath.Separator))
	}
	for _, p := range [...]string{"/etc/sudoers.d", "/etc/systemd"} {
		out = append(out, realPath(p)+string(filepath.Separator))
	}
	return out
}

func SafeWriteRoots() []string {
	seen := make(map[string]struct{})
	var roots []string
	for _, p := range strings.Split(os.Getenv(safeRootEnv), string(filepath.ListSeparator)) {
		if p == "" {
			continue
		}
		resolved := realPath(home.ExpandUser(p))
		if _, dup := seen[resolved]; !dup {
			seen[resolved] = struct{}{}
			roots = append(roots, resolved)
		}
	}
	sort.Strings(roots)
	return roots
}

func writeApprovalPath(userHome string) string {
	return realPath(joinHome(userHome, ".ssh/config"))
}

func (s *Safety) classifyResolved(ctx context.Context, homes []string, resolved string) (string, error) {
	for _, h := range homes {
		if resolved == writeApprovalPath(h) {
			return denialNone, nil
		}
	}
	active, root, err := s.homePaths(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range homes {
		if _, denied := s.writeDeniedPaths(h, active, root)[resolved]; denied {
			return denialProtected, nil
		}
		for _, prefix := range writeDeniedPrefixes(h) {
			if strings.HasPrefix(resolved, prefix) {
				return denialProtected, nil
			}
		}
	}
	dirs, err := s.productDirs(ctx)
	if err != nil {
		return "", err
	}
	for _, base := range dirs {
		for _, sub := range protectedSubpaths {
			if isUnder(resolved, realPath(joinHome(base, sub))) {
				return denialProtected, nil
			}
		}
	}
	if roots := SafeWriteRoots(); len(roots) > 0 {
		inside := false
		for _, r := range roots {
			if isUnder(resolved, r) {
				inside = true
				break
			}
		}
		if !inside {
			return denialSafeRoot, nil
		}
	}
	return denialNone, nil
}

func (s *Safety) classifyWriteDenial(ctx context.Context, path string, entry bool) (string, error) {
	if IsNTNamespacePath(path) {
		return denialNamespace, nil
	}
	homes := s.guardHomes(ctx, path)
	resolved := realPath(home.ExpandUser(path))
	if _, protected := s.protector.Path(path, true); protected {
		return denialProtected, nil
	}
	if entry {
		if _, protected := s.protector.Path(path, false); protected {
			return denialProtected, nil
		}
	}
	denial, err := s.classifyResolved(ctx, homes, resolved)
	if err != nil || denial != denialNone || !entry {
		return denial, err
	}
	parent, leaf := approval.SplitEntry(home.ExpandUser(path))
	if parent == "" {
		parent = "."
	}
	return s.classifyResolved(ctx, homes, filepath.Join(realPath(parent), leaf))
}

func (s *Safety) IsWriteDenied(ctx context.Context, path string) (bool, error) {
	denial, err := s.classifyWriteDenial(ctx, path, false)
	return denial != denialNone, err
}

func (s *Safety) WriteDenied(ctx context.Context, path, verb string, entry bool) (string, error) {
	denial, err := s.classifyWriteDenial(ctx, path, entry)
	if err != nil {
		return "", err
	}
	switch denial {
	case denialNone:
		return "", nil
	case denialSafeRoot:
		return verb + " denied: '" + path + "' is outside " + safeRootEnv + " " +
			"(" + strings.Join(SafeWriteRoots(), string(filepath.ListSeparator)) + "). Unset the variable or add this path's directory prefix.", nil
	case denialNamespace:
		return NTNamespaceError(path, verb), nil
	}
	return verb + " denied: '" + path + "' is a protected system/credential file.", nil
}

func (s *Safety) WriteApprovalRequired(ctx context.Context, path string) bool {
	resolved := realPath(home.ExpandUser(path))
	for _, h := range s.guardHomes(ctx, path) {
		if resolved == writeApprovalPath(h) {
			return true
		}
	}
	return false
}

func isBlockedEnvBasename(name string) bool {
	switch strings.ToLower(name) {
	case ".env", ".env.local", ".env.development", ".env.production", ".env.test", ".env.staging", ".envrc":
		return true
	}
	return false
}

func (s *Safety) ReadBlockError(ctx context.Context, path string) (string, error) {
	if msg := NTNamespaceError(path, "Read"); msg != "" {
		return msg, nil
	}
	resolved := realPath(home.ExpandUser(path))
	dirs, err := s.productDirs(ctx)
	if err != nil {
		return "", err
	}
	reason := ""
	for _, hd := range dirs {
		if isUnder(resolved, joinHome(hd, "skills/.hub")) {
			reason = "is an internal icebath cache file and cannot be read directly to prevent " +
				"prompt injection. Use the skills_list or skill_view tools instead."
			break
		}
	}
	if reason == "" {
		credentialFiles := append([]string{"auth.json", "auth.lock", ".anthropic_oauth.json", ".env",
			"webhook_subscriptions.json", "auth/google_oauth.json"}, secretCacheStores[:]...)
	credentials:
		for _, name := range credentialFiles {
			for _, hd := range dirs {
				if samePath(resolved, realPath(joinHome(hd, name))) {
					reason = "is an icebath credential store and cannot be read directly. Provider tools " +
						"consume these credentials through internal channels." + depthSuffix
					break credentials
				}
			}
		}
	}
	if reason == "" {
	denied:
		for _, d := range readDeniedDirs {
			for _, hd := range dirs {
				blocked := realPath(joinHome(hd, d.subdir))
				if isUnder(resolved, blocked) {
					reason = d.fileMsg + depthSuffix
					if samePath(resolved, blocked) {
						reason = d.dirMsg + depthSuffix
					}
					break denied
				}
			}
		}
	}
	if reason == "" && isBlockedEnvBasename(filepath.Base(resolved)) {
		reason = "is a secret-bearing environment file and cannot be read to prevent credential " +
			"leakage. If you need to check the file structure, read .env.example instead." + depthSuffix
	}
	if reason == "" {
		return "", nil
	}
	return "Access denied: " + path + " " + reason, nil
}

func (s *Safety) ActiveProfileName(ctx context.Context) string {
	active, root, err := s.homePaths(ctx)
	if err != nil {
		return "default"
	}
	profiles := filepath.Join(realPath(root), "profiles")
	resolved := realPath(active)
	if !isUnder(resolved, profiles) || samePath(resolved, profiles) {
		return "default"
	}
	rest := strings.TrimPrefix(resolved[len(profiles):], string(filepath.Separator))
	if i := strings.IndexRune(rest, filepath.Separator); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "default"
	}
	return rest
}

type MirrorTarget struct {
	TargetPath string
	MirrorRoot string
	InnerPath  string
}

func ClassifySandboxMirror(path string) (MirrorTarget, bool) {
	target := realPath(home.ExpandUser(path))
	sep := string(filepath.Separator)
	parts := strings.Split(target, sep)
	for i, part := range parts {
		if part == "sandboxes" && i+5 < len(parts) && parts[i+3] == "home" && parts[i+4] == ".icebath" {
			inner := i + 4
			return MirrorTarget{
				TargetPath: target,
				MirrorRoot: strings.Join(parts[:inner+1], sep),
				InnerPath:  strings.Join(parts[inner+1:], sep),
			}, true
		}
	}
	return MirrorTarget{}, false
}

func ClassifyContainerMirror(path, mirrorPrefix string) (MirrorTarget, bool) {
	if mirrorPrefix == "" {
		return MirrorTarget{}, false
	}
	target := realPath(home.ExpandUser(path))
	mirror := realPath(home.ExpandUser(mirrorPrefix))
	if !isUnder(target, mirror) {
		return MirrorTarget{}, false
	}
	inner := strings.TrimPrefix(target[len(mirror):], string(filepath.Separator))
	if inner == "" {
		inner = "."
	}
	return MirrorTarget{TargetPath: target, MirrorRoot: mirror, InnerPath: filepath.ToSlash(inner)}, true
}

func mirrorWarning(m MirrorTarget, body, bypass string) string {
	return "Sandbox-mirror write blocked by soft guard: " + m.TargetPath + " " +
		"sits under " + quote(m.MirrorRoot) + ", which is " + body + " " +
		"Use the host-side tool for authoritative state (e.g. ``memory`` for memories), " +
		"or address the host path directly. To bypass " + bypass + " with ``cross_profile=True``. " +
		"(Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"
}

func SandboxMirrorWarning(path string) string {
	m, ok := ClassifySandboxMirror(path)
	if !ok {
		return ""
	}
	return mirrorWarning(m,
		"a per-task mirror created by a non-local terminal backend (docker/daytona/etc.). "+
			"Writes here land on a copy that the host icebath process never reads — the "+
			"authoritative file is likely "+quote(m.InnerPath)+" under the real ICEBATH_HOME.",
		"this guard after explicit user direction, retry the call")
}

func ContainerMirrorWarning(path, mirrorPrefix string) string {
	m, ok := ClassifyContainerMirror(path, mirrorPrefix)
	if !ok {
		return ""
	}
	return mirrorWarning(m,
		"the container's bind-mounted home — a per-task mirror that the host icebath "+
			"process never reads. The authoritative file is "+quote(m.InnerPath)+" under "+
			"the real ICEBATH_HOME.",
		"after explicit user direction, retry")
}
