package tools

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// DefaultContainerBase is where container backends mount the profile home
const DefaultContainerBase = "/root/.icebath"

// GeneratedSubdir holds provider generated media under the cache directory
const GeneratedSubdir = "generated"

// MediaCacheMaxAgeHours bounds how long generated media stays synced to remote backends
const MediaCacheMaxAgeHours = 24

const credentialFilesKey = "credential_files"

var generatedCache = "cache/" + GeneratedSubdir

var cacheDirs = [][2]string{
	{"cache/documents", "document_cache"},
	{"cache/images", "image_cache"},
	{"cache/audio", "audio_cache"},
	{"cache/videos", "video_cache"},
	{"cache/screenshots", "browser_screenshots"},
	{"cache/web", "web_cache"},
	{"cache/delegation", "delegation_cache"},
	{"cache/spillover", "cache/spillover"},
	{generatedCache, generatedCache},
	{"images", "images"},
	{"attachments", "attachments"},
	{"composer-pastes", "composer-pastes"},
}

var homeRelativeBackends = map[string]bool{"ssh": true, "daytona": true, "vercel_sandbox": true}

// Mount pairs a host path with the path the sandbox sees
type Mount struct {
	HostPath      string
	ContainerPath string
}

func mount(hostPath, containerPath string) Mount {
	return Mount{HostPath: hostPath, ContainerPath: containerPath}
}

var configCredentialFiles = struct {
	sync.Mutex
	byHome map[string][]Mount
}{byHome: map[string][]Mount{}}

var safeSkillsTemp struct {
	sync.Mutex
	dir string
}

// Reports whether a legacy path is a file or a populated directory
func legacyPathHasContent(p string) bool {
	st, err := os.Lstat(p)
	if err == nil && st.Mode()&os.ModeSymlink != 0 {
		st, err = os.Stat(p)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		return true
	}
	if !st.IsDir() {
		return true
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return true
	}
	return len(entries) > 0
}

// Resolves a home subdirectory honouring a populated legacy directory
func homeSubdir(h, newSubpath, oldName string) string {
	old := filepath.Join(h, oldName)
	if legacyPathHasContent(old) {
		return old
	}
	return filepath.Join(h, newSubpath)
}

func registeredCredentialFiles(env *Env) map[string]string {
	if env == nil || env.Values == nil {
		return map[string]string{}
	}
	v, _ := env.Values.LoadOrStore(credentialFilesKey, &credentialRegistry{files: map[string]string{}})
	reg := v.(*credentialRegistry)
	reg.Lock()
	defer reg.Unlock()
	out := make(map[string]string, len(reg.files))
	for k, v := range reg.files {
		out[k] = v
	}
	return out
}

type credentialRegistry struct {
	sync.Mutex
	files map[string]string
}

func credentialRegistryOf(env *Env) *credentialRegistry {
	if env == nil || env.Values == nil {
		return &credentialRegistry{files: map[string]string{}}
	}
	v, _ := env.Values.LoadOrStore(credentialFilesKey, &credentialRegistry{files: map[string]string{}})
	return v.(*credentialRegistry)
}

// Resolves a relative path under the home refusing absolute paths and escapes
func containedHostPath(env *Env, rel, absMsg, traversalMsg string) string {
	if filepath.IsAbs(rel) {
		envLog(env).Warn(absMsg, "path", rel)
		return ""
	}
	hostPath := filepath.Join(env.Home, rel)
	if containment := ValidateWithinDir(hostPath, env.Home); containment != "" {
		envLog(env).Warn(traversalMsg, "path", rel, "error", containment)
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(hostPath); err == nil {
		return resolved
	}
	return hostPath
}

// RegisterCredentialFile registers a home relative credential file for mounting
func RegisterCredentialFile(env *Env, relativePath, containerBase string) bool {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	resolved := containedHostPath(env, relativePath, "credential_files: rejected absolute path (must be relative to ICEBATH_HOME)", "credential_files: rejected path traversal")
	if resolved == "" {
		return false
	}
	if !isFile(resolved) {
		envLog(env).Debug("credential_files: skipping (not found)", "path", resolved)
		return false
	}
	if denied := readBlockError(env, resolved); denied != "" {
		envLog(env).Warn("credential_files: refused a credential store the agent is denied from reading; a skill may mount its own service token, not the master key files", "path", relativePath)
		return false
	}
	containerPath := strings.TrimRight(containerBase, "/") + "/" + relativePath
	reg := credentialRegistryOf(env)
	reg.Lock()
	reg.files[containerPath] = resolved
	reg.Unlock()
	envLog(env).Debug("credential_files: registered", "host", resolved, "container", containerPath)
	return true
}

// RegisterCredentialFiles registers skill frontmatter entries and returns the missing ones
func RegisterCredentialFiles(env *Env, entries []any, containerBase string) []string {
	var missing []string
	for _, entry := range entries {
		rel := ""
		switch v := entry.(type) {
		case map[string]any:
			rel = asStr(v["path"])
			if rel == "" {
				rel = asStr(v["name"])
			}
		case string:
			rel = v
		default:
			continue
		}
		rel = strings.TrimSpace(rel)
		if rel != "" && !RegisterCredentialFile(env, rel, containerBase) {
			missing = append(missing, rel)
		}
	}
	return missing
}

// Loads terminal.credential_files from the raw config, cached per profile home
func loadConfigCredentialFiles(env *Env) []Mount {
	key := home.Key(env.Home)
	configCredentialFiles.Lock()
	if cached, ok := configCredentialFiles.byHome[key]; ok {
		configCredentialFiles.Unlock()
		return cached
	}
	configCredentialFiles.Unlock()
	var result []Mount
	raw, err := readRawConfig(env)
	if err != nil {
		envLog(env).Warn("Could not read terminal.credential_files from config", "error", err)
	} else {
		for _, item := range asList(raw.Get("terminal.credential_files")) {
			rel := strings.TrimSpace(asStr(item))
			if rel == "" {
				continue
			}
			resolved := containedHostPath(env, rel, "credential_files: rejected absolute config path", "credential_files: rejected config path traversal")
			if resolved != "" && isFile(resolved) {
				result = append(result, mount(resolved, DefaultContainerBase+"/"+rel))
			}
		}
	}
	configCredentialFiles.Lock()
	configCredentialFiles.byHome[key] = result
	configCredentialFiles.Unlock()
	return result
}

// GetCredentialFileMounts lists skill registered and config credential files that still exist
func GetCredentialFileMounts(env *Env) []Mount {
	mounts := map[string]string{}
	var order []string
	for cp, hp := range registeredCredentialFiles(env) {
		if isFile(hp) {
			mounts[cp] = hp
			order = append(order, cp)
		}
	}
	sortStrings(order)
	for _, entry := range loadConfigCredentialFiles(env) {
		if _, ok := mounts[entry.ContainerPath]; !ok && isFile(entry.HostPath) {
			mounts[entry.ContainerPath] = entry.HostPath
			order = append(order, entry.ContainerPath)
		}
	}
	out := make([]Mount, 0, len(order))
	for _, cp := range order {
		out = append(out, mount(mounts[cp], cp))
	}
	return out
}

// Every existing skills directory with the container root it maps to
func skillDirRoots(env *Env, containerBase string) []Mount {
	base := strings.TrimRight(containerBase, "/")
	var out []Mount
	if skillsDir := home.SkillsDir(env.Home); isDir(skillsDir) {
		out = append(out, mount(skillsDir, base+"/skills"))
	}
	locks := skillLocks()
	for _, group := range []struct {
		label string
		dirs  []string
	}{{"external_skills", locks.ExternalSkillsDirs(env)}, {"project_skills", locks.ProjectSkillsDirs(env)}} {
		for idx, d := range group.dirs {
			if isDir(d) {
				out = append(out, mount(d, base+"/"+group.label+"/"+itoa(idx)))
			}
		}
	}
	return out
}

type skillTreeDir struct {
	dir   string
	files []string
}

// Walks a skills tree yielding regular non symlink files, pruning excluded directories
func walkSkillTree(root string) []skillTreeDir {
	locks := skillLocks()
	var out []skillTreeDir
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var files, subdirs []string
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if !locks.IsExcludedSkillDir(e.Name()) {
					subdirs = append(subdirs, full)
				}
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if isFile(full) {
				files = append(files, full)
			}
		}
		out = append(out, skillTreeDir{dir: dir, files: files})
		sortStrings(subdirs)
		for _, sd := range subdirs {
			walk(sd)
		}
	}
	walk(root)
	return out
}

func dirHasSymlinks(root string) []string {
	var links []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			links = append(links, p)
		}
		return nil
	})
	return links
}

// Returns the skills dir itself when symlink free else a sanitized temp copy
func safeSkillsPath(env *Env, skillsDir string) string {
	links := dirHasSymlinks(skillsDir)
	if len(links) == 0 {
		return skillsDir
	}
	for _, link := range links {
		target, _ := os.Readlink(link)
		envLog(env).Warn("credential_files: skipping symlink in skills dir", "link", link, "target", target)
	}
	safeSkillsTemp.Lock()
	defer safeSkillsTemp.Unlock()
	if safeSkillsTemp.dir != "" && isDir(safeSkillsTemp.dir) {
		_ = os.RemoveAll(safeSkillsTemp.dir)
	}
	safeDir, err := os.MkdirTemp("", "icebath-skills-safe-")
	if err != nil {
		return skillsDir
	}
	safeSkillsTemp.dir = safeDir
	for _, td := range walkSkillTree(skillsDir) {
		rel, err := filepath.Rel(skillsDir, td.dir)
		if err != nil {
			continue
		}
		_ = os.MkdirAll(filepath.Join(safeDir, rel), 0o755)
		for _, item := range td.files {
			relItem, err := filepath.Rel(skillsDir, item)
			if err != nil {
				continue
			}
			copyFilePreserving(item, filepath.Join(safeDir, relItem))
		}
	}
	envLog(env).Info("credential_files: created symlink-safe skills copy", "path", safeDir)
	return safeDir
}

func copyFilePreserving(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	st, err := os.Stat(src)
	if err != nil {
		return
	}
	if err := os.WriteFile(dst, data, st.Mode().Perm()); err != nil {
		return
	}
	_ = os.Chtimes(dst, time.Now(), st.ModTime())
}

// CleanupSafeSkillsCopy removes the sanitized skills temp copy at exit
func CleanupSafeSkillsCopy() {
	safeSkillsTemp.Lock()
	defer safeSkillsTemp.Unlock()
	if safeSkillsTemp.dir != "" && isDir(safeSkillsTemp.dir) {
		_ = os.RemoveAll(safeSkillsTemp.dir)
	}
	safeSkillsTemp.dir = ""
}

// GetSkillsDirectoryMount lists directory mounts for every skills dir, sanitized when symlinked
func GetSkillsDirectoryMount(env *Env, containerBase string) []Mount {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	var out []Mount
	for _, m := range skillDirRoots(env, containerBase) {
		out = append(out, mount(safeSkillsPath(env, m.HostPath), m.ContainerPath))
	}
	return out
}

// IterSkillsFiles lists per file entries for every skills file
func IterSkillsFiles(env *Env, containerBase string) []Mount {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	var out []Mount
	for _, root := range skillDirRoots(env, containerBase) {
		for _, td := range walkSkillTree(root.HostPath) {
			for _, item := range td.files {
				rel, err := filepath.Rel(root.HostPath, item)
				if err != nil {
					continue
				}
				out = append(out, mount(item, root.ContainerPath+"/"+filepath.ToSlash(rel)))
			}
		}
	}
	return out
}

// Host and container roots for every cache dir, always mapping to the new container layout
func cacheDirRoots(env *Env, containerBase string, createMissing bool) []Mount {
	base := strings.TrimRight(containerBase, "/")
	var out []Mount
	for _, pair := range cacheDirs {
		hostDir := homeSubdir(env.Home, pair[0], pair[1])
		if !isDir(hostDir) {
			if !createMissing {
				continue
			}
			if err := os.MkdirAll(hostDir, 0o755); err != nil {
				continue
			}
		}
		out = append(out, mount(hostDir, base+"/"+pair[0]))
	}
	return out
}

// GetCacheDirectoryMounts lists bind mount entries for each cache directory
func GetCacheDirectoryMounts(env *Env, containerBase string) []Mount {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	return cacheDirRoots(env, containerBase, true)
}

func relativeTo(p, root string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// MapCachePathToContainer returns the container path for a host path under a cache dir or empty
func MapCachePathToContainer(env *Env, hostPath, containerBase string) string {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	mounts := GetCacheDirectoryMounts(env, containerBase)
	for _, m := range mounts {
		if rel, ok := relativeTo(hostPath, m.HostPath); ok {
			return path.Join(m.ContainerPath, filepath.ToSlash(rel))
		}
	}
	real, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		return ""
	}
	for _, m := range mounts {
		root, err := filepath.EvalSymlinks(m.HostPath)
		if err != nil {
			continue
		}
		if rel, ok := relativeTo(real, root); ok {
			return path.Join(m.ContainerPath, filepath.ToSlash(rel))
		}
	}
	return ""
}

// Active TERMINAL_ENV through the per turn terminal scope
func terminalBackendName(env *Env) string {
	policy, err := terminalPolicy(env)
	if err != nil {
		return "local"
	}
	v := strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_ENV", "")))
	if v == "" {
		return "local"
	}
	return v
}

// FromAgentVisibleCachePath maps a docker cache path back to the host path
func FromAgentVisibleCachePath(env *Env, containerPath, containerBase string) string {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	if terminalBackendName(env) != "docker" {
		return containerPath
	}
	for _, m := range GetCacheDirectoryMounts(env, containerBase) {
		if strings.HasPrefix(containerPath, m.ContainerPath+"/") || containerPath == m.ContainerPath {
			rel := strings.TrimPrefix(strings.TrimPrefix(containerPath, m.ContainerPath), "/")
			return filepath.Join(m.HostPath, filepath.FromSlash(rel))
		}
	}
	return containerPath
}

// Translates a host cache path to where the active backend sees it
func toAgentVisibleCachePath(env *Env, hostPath string) string {
	containerBase := DefaultContainerBase
	backend := terminalBackendName(env)
	switch {
	case homeRelativeBackends[backend]:
		containerBase = "~/.icebath"
	case backend == "docker" || backend == "modal":
	default:
		pluginBase := ""
		if !builtinBackends[backend] {
			if p := GetTerminalBackend(backend); p != nil {
				pluginBase = p.CachePathBase()
			}
		}
		if pluginBase == "" {
			return hostPath
		}
		containerBase = pluginBase
	}
	if mapped := MapCachePathToContainer(env, hostPath, containerBase); mapped != "" {
		return mapped
	}
	return hostPath
}

// ToAgentVisibleCachePath exposes the host to sandbox cache path translation
func ToAgentVisibleCachePath(env *Env, hostPath string) string {
	return toAgentVisibleCachePath(env, hostPath)
}

// IterCacheFiles lists per file cache entries, bounding generated media by age
func IterCacheFiles(env *Env, containerBase string) []Mount {
	if containerBase == "" {
		containerBase = DefaultContainerBase
	}
	cutoff := time.Now().Add(-MediaCacheMaxAgeHours * time.Hour)
	genRoot := strings.TrimRight(containerBase, "/") + "/" + generatedCache
	var out []Mount
	for _, root := range cacheDirRoots(env, containerBase, false) {
		_ = filepath.WalkDir(root.HostPath, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if root.ContainerPath == genRoot && info.ModTime().Before(cutoff) {
				return nil
			}
			rel, err := filepath.Rel(root.HostPath, p)
			if err != nil {
				return nil
			}
			out = append(out, mount(p, root.ContainerPath+"/"+filepath.ToSlash(rel)))
			return nil
		})
	}
	return out
}

// ClearCredentialFiles resets the skill scoped registry of the session
func ClearCredentialFiles(env *Env) {
	reg := credentialRegistryOf(env)
	reg.Lock()
	reg.files = map[string]string{}
	reg.Unlock()
}
