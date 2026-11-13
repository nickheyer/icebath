package files

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// True when dir lives inside a packaged app bundle or install tree
func IsPackagedInstallPath(dir string, installRoots []string, isPackaged bool) bool {
	if !isPackaged || dir == "" {
		return false
	}
	resolved := resolvePath(dir)
	seen := map[string]bool{}
	for _, candidate := range installRoots {
		if candidate == "" {
			continue
		}
		root := resolvePath(candidate)
		if seen[root] {
			continue
		}
		seen[root] = true
		if resolved == root {
			return true
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func posixInside(dir, file string) bool {
	rel := posixRelative(dir, file)
	return rel != "" && !strings.HasPrefix(rel, "..") && !path.IsAbs(rel)
}

// Node path.posix.relative of two absolute POSIX paths
func posixRelative(from, to string) string {
	from, to = path.Clean(from), path.Clean(to)
	if from == to {
		return ""
	}
	fp := strings.Split(strings.TrimPrefix(from, "/"), "/")
	tp := strings.Split(strings.TrimPrefix(to, "/"), "/")
	if from == "/" {
		fp = nil
	}
	if to == "/" {
		tp = nil
	}
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	var parts []string
	for range fp[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, tp[i:]...)
	return strings.Join(parts, "/")
}

func posixResolve(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	wd, _ := os.Getwd()
	return path.Clean(path.Join(filepath.ToSlash(wd), p))
}

// AppImage file this executable runs from, empty when it runs from none or inherited another app's values
func OwnAppImage(execPath string, env map[string]string) string {
	appDir, appImage := env["APPDIR"], env["APPIMAGE"]
	if appImage == "" || execPath == "" {
		return ""
	}
	exe := posixResolve(execPath)
	if appDir == "" {
		for _, segment := range strings.Split(path.Dir(exe), "/") {
			if strings.HasPrefix(segment, ".mount_") {
				return appImage
			}
		}
		return ""
	}
	if posixInside(posixResolve(appDir), exe) {
		return appImage
	}
	realAppDir, err1 := filepath.EvalSymlinks(appDir)
	realExe, err2 := filepath.EvalSymlinks(exe)
	if err1 == nil && err2 == nil && posixInside(realAppDir, realExe) {
		return appImage
	}
	return ""
}

var (
	winInstallDirRE = regexp.MustCompile(`(?i)[\\/]Hermes$`)
	winDesktopDirRE = regexp.MustCompile(`(?i)[\\/]icebath-desktop$`)
)

// Removable app bundle of the running executable, empty when none can be named with confidence
func ResolveRemovableAppPath(execPath, platform string, env map[string]string) string {
	if execPath == "" {
		return ""
	}
	if platform == "darwin" {
		appBundle := path.Dir(path.Dir(path.Dir(execPath)))
		if strings.HasSuffix(appBundle, ".app") {
			return appBundle
		}
		return ""
	}
	if platform == "win32" {
		dir := win32Dirname(execPath)
		if winInstallDirRE.MatchString(dir) || winDesktopDirRE.MatchString(dir) {
			return dir
		}
		return ""
	}
	if image := OwnAppImage(execPath, env); image != "" {
		return image
	}
	if dir := path.Dir(execPath); strings.HasSuffix(dir, "-unpacked") {
		return dir
	}
	return ""
}

// Node path.win32.dirname of a Windows path
func win32Dirname(p string) string {
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	rootEnd := 0
	if len(p) >= 2 && p[1] == ':' {
		rootEnd = 2
		if len(p) > 2 && isSep(p[2]) {
			rootEnd = 3
		}
	} else if len(p) > 0 && isSep(p[0]) {
		rootEnd = 1
	}
	end := len(p)
	for end > rootEnd && isSep(p[end-1]) {
		end--
	}
	i := strings.LastIndexAny(p[rootEnd:end], `\/`)
	if i < 0 {
		if rootEnd == 0 {
			return "."
		}
		return p[:rootEnd]
	}
	j := rootEnd + i
	for j > rootEnd && isSep(p[j-1]) {
		j--
	}
	if j <= rootEnd {
		return p[:rootEnd]
	}
	return p[:j]
}

// Workspace resolves the working directory sessions start in
type Workspace struct {
	UserData       string
	Packaged       bool
	AppRoot        string
	ExecPath       string
	Platform       string
	SourceRepoRoot string
	Env            map[string]string
	InitCwd        string
}

// Folders that make up the installed app
func (w Workspace) installRoots() []string {
	return []string{w.AppRoot, filepath.Dir(w.ExecPath), ResolveRemovableAppPath(w.ExecPath, w.Platform, w.Env)}
}

// True when dir is inside the packaged app
func (w Workspace) IsPackagedInstallPath(dir string) bool {
	return IsPackagedInstallPath(dir, w.installRoots(), w.Packaged)
}

// Saved default project directory when it still exists
func (w Workspace) ReadDefaultProjectDir() string {
	raw, err := os.ReadFile(filepath.Join(w.UserData, "project-dir.json"))
	if err != nil {
		return ""
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil || parsed == nil {
		return ""
	}
	dir, ok := parsed["dir"].(string)
	if !ok || jsTrim(dir) == "" {
		return ""
	}
	resolved := resolvePath(dir)
	if directoryExists(resolved) {
		return resolved
	}
	return ""
}

func directoryExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Session working directory, the saved default, env hints, dev cwd, source checkout, then home
func (w Workspace) ResolveHermesCwd() string {
	cwd, _ := os.Getwd()
	candidates := []string{w.ReadDefaultProjectDir(), w.Env["ICEBATH_DESKTOP_CWD"]}
	if !w.Packaged {
		candidates = append(candidates, w.InitCwd, cwd, w.SourceRepoRoot)
	}
	candidates = append(candidates, userHome())
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		resolved := resolvePath(candidate)
		if w.IsPackagedInstallPath(resolved) {
			continue
		}
		if directoryExists(resolved) {
			return resolved
		}
	}
	return userHome()
}

// WorkspaceCwd is the sanitized workspace answer
type WorkspaceCwd struct {
	Cwd       string `json:"cwd"`
	Sanitized bool   `json:"sanitized"`
}

// A renderer cwd kept when it is a real folder outside the app, else the resolved default
func (w Workspace) SanitizeWorkspaceCwd(cwd any) WorkspaceCwd {
	trimmed := ""
	if s, ok := cwd.(string); ok {
		trimmed = jsTrim(s)
	}
	if trimmed == "" || w.IsPackagedInstallPath(trimmed) {
		return WorkspaceCwd{Cwd: w.ResolveHermesCwd(), Sanitized: trimmed != ""}
	}
	if resolved := resolvePath(trimmed); directoryExists(resolved) {
		return WorkspaceCwd{Cwd: resolved, Sanitized: false}
	}
	return WorkspaceCwd{Cwd: w.ResolveHermesCwd(), Sanitized: true}
}
