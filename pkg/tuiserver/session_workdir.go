package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// A Windows drive path rewritten to its WSL mount on POSIX hosts, user syntax expanded
func normalizeCompletionPath(pathPart string) string {
	expanded := pypath.Expanduser(pathPart)
	if runtime.GOOS != "windows" {
		normalized := strings.ReplaceAll(expanded, "\\", "/")
		if len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/' && unicode.IsLetter(rune(normalized[0])) {
			return fmt.Sprintf("/mnt/%s/%s", strings.ToLower(normalized[:1]), normalized[3:])
		}
	}
	return expanded
}

// Named profile home resolver the cwd resolution consults
var resolveProfileHome = profileHome

// Working directory of a completion or session request, a ProfileUnavailableError for a deleted profile
func completionCwd(ctx context.Context, params map[string]any) (string, error) {
	if params == nil {
		params = map[string]any{}
	}
	clientCwd := strOr(params["cwd"])
	profile := strOr(params["profile"])
	sessionCwd := strOr(tuigateway.SessionRecord(strOr(params["session_id"])).Get("cwd"))
	profileHomePath := ""
	if profile != "" {
		resolved, err := resolveProfileHome(profile)
		if err != nil {
			var unavailable *ProfileUnavailableError
			if !errors.As(err, &unavailable) {
				return "", err
			}
			if (clientCwd != "" && !truthy(params["cwd_explicit"])) || !(clientCwd != "" || sessionCwd != "") {
				return "", err
			}
			resolved = ""
		}
		profileHomePath = resolved
	}
	if !truthy(params["cwd_explicit"]) && clientCwd != "" {
		if profileCwd := profileWorkspaceCwd(ctx, profileHomePath); profileCwd != "" {
			return profileCwd, nil
		}
	}
	namedSSH := profileHomePath != "" && cwdIsRemote(ctx, profileHomePath)
	raw := clientCwd
	for _, candidate := range []func() string{
		func() string { return sessionCwd },
		func() string { return profileDefaultCwd(ctx, profileHomePath) },
		func() string {
			if namedSSH {
				return "~"
			}
			return ""
		},
		func() string { return launchConfiguredCwd(ctx) },
		func() string { return os.Getenv("TERMINAL_CWD") },
		func() string { return sandboxWorkspaceCwd(ctx, "") },
		func() string { cwd, _ := os.Getwd(); return cwd },
	} {
		if raw != "" {
			break
		}
		raw = candidate()
	}
	if namedSSH {
		if isRemoteCwdShape(raw) {
			return raw, nil
		}
		if declared := declaredRemoteProfileCwd(ctx, profileHomePath); declared != "" {
			return declared, nil
		}
		return "~", nil
	}
	if profileHomePath == "" && strings.HasPrefix(raw, "~") && cwdIsRemote(ctx, "") {
		return raw, nil
	}
	if resolved, err := pypath.Abspath(pypath.Expanduser(raw)); err == nil && isDir(resolved) {
		return resolved, nil
	}
	if profileHomePath == "" && isRemoteCwdShape(raw) && cwdIsRemote(ctx, "") {
		return raw, nil
	}
	if isContainerPath(raw) && boundTerminalBackend(ctx, profileHomePath) != "local" {
		return raw, nil
	}
	cwd, err := os.Getwd()
	return cwd, err
}

// Stripped terminal key from config, empty when unset or unreadable
func workdirTerminalCfg(ctx context.Context, key string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	terminalCfg, ok := loadCfg(ctx)["terminal"].(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(strOr(terminalCfg[key]))
}

// Effective terminal policy of a named profile, empty for the launch profile or an unreadable one
func profileTerminalPolicy(ctx context.Context, profileHome string) tools.TerminalPolicy {
	if profileHome == "" {
		return tools.TerminalPolicy{}
	}
	policy, err := tools.BuildProfileTerminalScope(ctx, profileHome, nil)
	if err != nil {
		var unavailable *tools.TerminalPolicyUnavailable
		if errors.As(err, &unavailable) {
			return tools.TerminalPolicy{}
		}
		panic(err)
	}
	return policy
}

// Lowercased backend of a terminal policy, local when unset
func policyBackend(policy tools.TerminalPolicy) string {
	if backend := strings.ToLower(strings.TrimSpace(policy["TERMINAL_ENV"])); backend != "" {
		return backend
	}
	return "local"
}

// Terminal backend of the profile a session is bound to, the launch backend for the launch profile
func boundTerminalBackend(ctx context.Context, profileHome string) string {
	if profileHome != "" {
		return policyBackend(profileTerminalPolicy(ctx, profileHome))
	}
	return effectiveTerminalBackend(ctx)
}

// Whether the bound profile working directory lives on another host over ssh
func cwdIsRemote(ctx context.Context, profileHome string) bool {
	return boundTerminalBackend(ctx, profileHome) == "ssh"
}

// Terminal cwd a named ssh profile declares, unchecked against this host, empty otherwise
func declaredRemoteProfileCwd(ctx context.Context, profileHome string) string {
	policy := profileTerminalPolicy(ctx, profileHome)
	raw := strings.TrimSpace(policy["TERMINAL_CWD"])
	if policyBackend(policy) == "ssh" && isRemoteCwdShape(raw) {
		return raw
	}
	return ""
}

// An ssh working directory the remote shell resolves, tilde relative or absolute
func isRemoteCwdShape(raw string) bool {
	return config.IsSSHRemoteTildeCwd("ssh", raw) || filepath.IsAbs(raw)
}

// An absolute POSIX path, the only shape a container working directory takes
func isContainerPath(raw string) bool {
	return strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//")
}

// Configured container backend cwd missing on the host, empty for local or ssh backends or a host dir
func sandboxWorkspaceCwd(ctx context.Context, profileHome string) string {
	var backend, raw string
	if profileHome != "" {
		policy := profileTerminalPolicy(ctx, profileHome)
		backend, raw = policyBackend(policy), strings.TrimSpace(policy["TERMINAL_CWD"])
	} else {
		backend = effectiveTerminalBackend(ctx)
		raw = strings.TrimSpace(os.Getenv("TERMINAL_CWD"))
		if raw == "" {
			raw = workdirTerminalCfg(ctx, "cwd")
		}
	}
	if backend == "local" || backend == "ssh" || !isContainerPath(raw) || isDir(raw) {
		return ""
	}
	return raw
}

// Configured workspace of a named profile, its remote ssh dir, a host dir, else a container dir
func profileWorkspaceCwd(ctx context.Context, profileHome string) string {
	if cwd := declaredRemoteProfileCwd(ctx, profileHome); cwd != "" {
		return cwd
	}
	if cwd := profileConfiguredCwd(ctx, profileHome); cwd != "" {
		return cwd
	}
	if profileHome != "" {
		return sandboxWorkspaceCwd(ctx, profileHome)
	}
	return ""
}

// Landing cwd of a profile session, its workspace else a named local profile home, never the launch cwd
func profileDefaultCwd(ctx context.Context, profileHome string) string {
	if workspace := profileWorkspaceCwd(ctx, profileHome); workspace != "" {
		return workspace
	}
	if profileHome != "" && boundTerminalBackend(ctx, profileHome) == "local" {
		return profileHome
	}
	return ""
}

// Picked workspace of a profile session, raw for ssh else an existing host dir, an error for a missing one
func workspaceCwd(ctx context.Context, profileHome, raw string) (string, error) {
	if cwdIsRemote(ctx, profileHome) {
		if isRemoteCwdShape(raw) {
			return raw, nil
		}
		return "", &pypath.ValueError{Msg: fmt.Sprintf("remote working directory must be absolute or ~-relative: %s", raw)}
	}
	resolved, err := pypath.Abspath(pypath.Expanduser(raw))
	if err != nil {
		return "", err
	}
	if !isDir(resolved) {
		return "", &pypath.ValueError{Msg: fmt.Sprintf("working directory does not exist: %s", raw)}
	}
	return resolved, nil
}

// Session cwd, else the completion cwd
func sessionCwd(ctx context.Context, session *tuigateway.Session) string {
	if cwd := strOr(session.Get("cwd")); cwd != "" {
		return cwd
	}
	cwd, err := completionCwd(ctx, nil)
	if err != nil {
		panic(err)
	}
	return cwd
}

// A cwd inside a deleted directory walked up to its first existing ancestor and that ancestor's common git root
func healDeadCwd(cwd string) (healed string) {
	raw := strings.TrimSpace(cwd)
	if raw == "" || isDir(raw) {
		return raw
	}
	probe := raw
	for range 64 {
		parent := pypath.Dirname(probe)
		if parent == "" || parent == probe {
			break
		}
		probe = parent
		if isDir(probe) {
			break
		}
	}
	if !isDir(probe) {
		return raw
	}
	defer func() {
		if recover() != nil {
			healed = probe
		}
	}()
	if root := tuigateway.CommonRepoRoot(probe); root != "" {
		return root
	}
	if root := tuigateway.RepoRoot(probe); root != "" {
		return root
	}
	return probe
}

// Whether the session cwd can be stat'ed and git probed on this host
func sessionIsLocalBackend(ctx context.Context, session *tuigateway.Session) bool {
	if profileHome := strOr(session.Get("profile_home")); profileHome != "" && boundTerminalBackend(ctx, profileHome) != "local" {
		return false
	}
	return effectiveTerminalBackend(ctx) == "local"
}

// Active terminal backend name, TERMINAL_ENV when set else the terminal backend config key, local by default
func effectiveTerminalBackend(ctx context.Context) string {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("TERMINAL_ENV")))
	if backend == "" || backend == "local" {
		backend = strings.ToLower(workdirTerminalCfg(ctx, "backend"))
	}
	if backend == "" {
		return "local"
	}
	return backend
}

// Session cwd for display and probes, healed past deleted worktrees with the healed value persisted, local only
func displaySessionCwd(ctx context.Context, session *tuigateway.Session) string {
	cwd := sessionCwd(ctx, session)
	if !sessionIsLocalBackend(ctx, session) {
		return cwd
	}
	healed := healDeadCwd(cwd)
	if healed != "" && healed != cwd && session != nil {
		session.Set("cwd", healed)
		persistSessionCwdAndScheduleGitMeta(ctx, session, healed, nil)
	}
	return healed
}

// Session store owning the row of a session, a profile db handle else the shared launch one, with its release
func workdirOwnerDB(ctx context.Context, session *tuigateway.Session, failLog string) (db *state.DB, failed bool, release func()) {
	release = func() {}
	profileHome := strOr(session.Get("profile_home"))
	if profileHome == "" {
		return getDB(ctx), false, release
	}
	opened, err := state.Acquire(ctx, filepath.Join(profileHome, "state.db"))
	if err != nil {
		logger().Debug(failLog, "error", err)
		return nil, true, release
	}
	return opened, false, func() {
		suppress(func() { state.ReleaseOrClose(opened) })
	}
}

// Session store owning the row of a session with its release, nil when unavailable
func sessionDB(ctx context.Context, session *tuigateway.Session) (*state.DB, func()) {
	db, failed, release := workdirOwnerDB(ctx, session, "failed to open profile db for session")
	if failed {
		return nil, release
	}
	return db, release
}

// A claimed db probe generation, a positive int
func workdirValidGeneration(generation *int64) bool {
	return generation != nil && *generation >= 1
}

// Resolves and persists the git branch and repo root of a session cwd in the background for one generation
func persistSessionGitMeta(ctx context.Context, session *tuigateway.Session, cwd string, generation *int64) {
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" || cwd == "" || !workdirValidGeneration(generation) {
		return
	}
	dbSession := tuigateway.NewSession(map[string]any{"session_key": sessionKey, "profile_home": session.Get("profile_home")})
	claimed := *generation
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer threadExceptHook("git-meta")
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("failed to persist session git metadata", "error", r)
			}
		}()
		branch, root := tuigateway.Branch(cwd), tuigateway.CommonRepoRoot(cwd)
		if branch == "" && root == "" {
			return
		}
		db, release := sessionDB(ctx, dbSession)
		defer release()
		if db != nil {
			if _, err := db.PublishSessionGitMetadata(ctx, sessionKey, cwd, claimed, branch, root); err != nil {
				logger().Debug("failed to persist session git metadata", "error", err)
			}
		}
	}()
}

// Claims a db backed probe generation for the new cwd then starts git enrichment, nil when nothing was claimed
func persistSessionCwdAndScheduleGitMeta(ctx context.Context, session *tuigateway.Session, cwd string, db *state.DB) *int64 {
	generation, ok := func() (generation *int64, ok bool) {
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("failed to persist session cwd", "error", r)
				generation, ok = nil, false
			}
		}()
		owner, release := db, func() {}
		if owner == nil {
			owner, release = sessionDB(ctx, session)
		}
		defer release()
		if owner == nil {
			return nil, false
		}
		claimed, err := owner.UpdateSessionCwdClaim(ctx, strOr(session.Get("session_key")), cwd, "", "", false)
		if err != nil {
			logger().Debug("failed to persist session cwd", "error", err)
			return nil, false
		}
		return claimed, true
	}()
	if !ok || !workdirValidGeneration(generation) {
		return nil
	}
	persistSessionGitMeta(ctx, session, cwd, generation)
	return generation
}

// Sources whose launch directory is an artifact of how the app started, not a picked workspace
var launchCwdNotAWorkspace = map[string]bool{"desktop": true}

// Cwd the terminal tool uses for a session, not host validated, empty when the backend default applies
func terminalTaskCwd(ctx context.Context, session *tuigateway.Session) string {
	cwd, _ := terminalTaskCwdWithSource(ctx, session)
	return cwd
}

// Terminal cwd and its source, session for this session's workspace or process for the global fallback
func terminalTaskCwdWithSource(ctx context.Context, session *tuigateway.Session) (string, string) {
	profileHome := strOr(session.Get("profile_home"))
	namedSSH := profileHome != "" && cwdIsRemote(ctx, profileHome)
	backend := "ssh"
	if !namedSSH {
		backend = effectiveTerminalBackend(ctx)
	}
	if backend != "local" {
		if session != nil && truthy(session.Get("explicit_cwd")) && truthy(session.Get("cwd")) {
			return strOr(session.Get("cwd")), "session"
		}
		if namedSSH {
			if remoteCwd := declaredRemoteProfileCwd(ctx, profileHome); remoteCwd != "" {
				return remoteCwd, "session"
			}
			return "~", "process"
		}
		raw := strings.TrimSpace(os.Getenv("TERMINAL_CWD"))
		if raw == "" {
			raw = workdirTerminalCfg(ctx, "cwd")
		}
		if raw != "" && !cwdPlaceholders[raw] {
			return raw, "process"
		}
		if backend == "ssh" {
			return "~", "process"
		}
	}
	if isRemoteLaunchCwd(ctx, session) {
		return "", "process"
	}
	if cwd := strOr(session.Get("cwd")); cwd != "" {
		return cwd, "session"
	}
	if !hostCwdIsTerminalCwd(ctx, profileHome) {
		return "", "process"
	}
	cwd, err := completionCwd(ctx, nil)
	if err != nil {
		panic(err)
	}
	return cwd, "process"
}

// Whether the session cwd came from the app launch rather than user intent
func contextCwdIsLaunchArtifact(session *tuigateway.Session) bool {
	return session != nil && !truthy(session.Get("explicit_cwd")) && launchCwdNotAWorkspace[sessionSource(session)]
}

// Explicit flag, session cwd and remote flag of a freshly created session
func resolveCreateCwd(ctx context.Context, params map[string]any, source, profileHome string) (bool, string, bool, error) {
	rawCwd := strings.TrimSpace(strOr(params["cwd"]))
	remoteCwd := rawCwd != "" && isRemoteCwdShape(rawCwd) && cwdIsRemote(ctx, profileHome)
	explicitCwd := false
	suppress(func() {
		if rawCwd == "" {
			return
		}
		if remoteCwd {
			explicitCwd = true
			return
		}
		resolved, err := pypath.Abspath(pypath.Expanduser(rawCwd))
		explicitCwd = err == nil && isDir(resolved)
	})
	sessionCwd, err := completionCwd(ctx, params)
	if err != nil {
		return false, "", false, err
	}
	if rawCwd != "" && !explicitCwd && source == "desktop" {
		if truthy(params["cwd_explicit"]) {
			explicitCwd, sessionCwd = true, rawCwd
		} else if resolved, err := pypath.Abspath(pypath.Expanduser(rawCwd)); sessionCwd != "" && err == nil && sessionCwd == resolved {
			explicitCwd = true
		}
	}
	return explicitCwd, sessionCwd, remoteCwd, nil
}

// Cwd to stamp on the session db row, empty to leave it unset under the launch dir rule
func persistedSessionCwd(ctx context.Context, session *tuigateway.Session) string {
	if truthy(session.Get("explicit_cwd")) {
		return sessionCwd(ctx, session)
	}
	if launchCwdNotAWorkspace[sessionSource(session)] || isRemoteLaunchCwd(ctx, session) {
		return ""
	}
	return strOr(session.Get("cwd"))
}

// Whether an unpicked session cwd is a host directory the session's remote or sandbox terminal never runs in
func isRemoteLaunchCwd(ctx context.Context, session *tuigateway.Session) bool {
	if session == nil || truthy(session.Get("explicit_cwd")) {
		return false
	}
	profileHome := strOr(session.Get("profile_home"))
	if cwdIsRemote(ctx, profileHome) {
		return true
	}
	cwd := strOr(session.Get("cwd"))
	return cwd != "" && !hostCwdIsTerminalCwd(ctx, profileHome) && isDir(cwd)
}

// Whether a host directory can be the terminal cwd of a profile session, local, docker or a host plugin backend
func hostCwdIsTerminalCwd(ctx context.Context, profileHome string) bool {
	backend := boundTerminalBackend(ctx, profileHome)
	if backend == "local" || backend == "docker" {
		return true
	}
	if slices.Contains(tools.BuiltinTerminalBackends(), backend) {
		return false
	}
	remote, _ := tools.TerminalProviderFlag(ctx, backend, "is_remote", true, profileHome).(bool)
	container, _ := tools.TerminalProviderFlag(ctx, backend, "is_container", true, profileHome).(bool)
	return !(remote || container)
}

// Whether an absolute cwd sits in the root home tree or the install tree, never a tilde remote path
func isHermesOwnedCwd(ctx context.Context, cwd, profileHome string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	path, err := pypath.Resolve(cwd)
	if err != nil {
		return false
	}
	h := profileHome
	if h == "" {
		h = home.Dir(ctx)
	}
	h = pypath.Expanduser(h)
	ownHome, err := pypath.Resolve(h)
	if err != nil {
		return false
	}
	root, err := pypath.Resolve(home.RootOf(h))
	if err != nil {
		return false
	}
	for _, r := range []string{ownHome, root} {
		if path == r || isUnder(path, r) {
			return true
		}
	}
	return tools.IsInstallTree(path)
}

// Whether a cwd is a launch directory, a home, root home, install tree, user home or filesystem root
func isLaunchDir(ctx context.Context, cwd, profileHome string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	path, err := pypath.Resolve(cwd)
	if err != nil {
		return false
	}
	h := profileHome
	if h == "" {
		h = home.Dir(ctx)
	}
	ownHome, err := pypath.Resolve(pypath.Expanduser(h))
	if err != nil {
		return false
	}
	root, err := pypath.Resolve(home.RootOf(ownHome))
	if err != nil {
		return false
	}
	realHome, err := pypath.Resolve(home.RealHome(ctx, nil))
	if err != nil {
		return false
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	userHome, err = pypath.Resolve(userHome)
	if err != nil {
		return false
	}
	anchor := filepath.VolumeName(path) + string(filepath.Separator)
	for _, candidate := range []string{ownHome, filepath.Join(ownHome, "home"), root, filepath.Join(root, "home"), anchor, realHome, userHome} {
		if path == candidate {
			return true
		}
	}
	return tools.IsInstallTree(path)
}

// Whether a named local profile's stored cwd belongs to another profile or to the root home itself
func isOtherProfilesCwd(ctx context.Context, cwd, profileHome string) bool {
	if profileHome == "" || cwdIsRemote(ctx, profileHome) || !filepath.IsAbs(cwd) {
		return false
	}
	path, err := pypath.Resolve(cwd)
	if err != nil {
		return false
	}
	own, err := pypath.Resolve(pypath.Expanduser(profileHome))
	if err != nil {
		return false
	}
	root, err := pypath.Resolve(home.RootOf(own))
	if err != nil {
		return false
	}
	if path == own || isUnder(path, own) {
		return false
	}
	return path == root || path == filepath.Join(root, "home") || isUnder(path, filepath.Join(root, "profiles")) || tools.IsInstallTree(path)
}

// Stored cwd a resume may adopt, empty for an ssh host launch dir, a sandbox dir or another profile dir
func resumableStoredCwd(ctx context.Context, cwd any, profileHome string) string {
	text := strOr(cwd)
	if text != "" && cwdIsRemote(ctx, profileHome) && isHermesOwnedCwd(ctx, text, profileHome) {
		return ""
	}
	if text != "" && !cwdIsRemote(ctx, profileHome) && !hostCwdIsTerminalCwd(ctx, profileHome) && isLaunchDir(ctx, text, profileHome) {
		return ""
	}
	if text != "" && isOtherProfilesCwd(ctx, text, profileHome) {
		return ""
	}
	return text
}

// Whether a resolved path sits strictly below a resolved directory
func isUnder(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
