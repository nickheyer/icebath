package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// PluginFilesChanged is the event every app window hears when a plugin's grants change
const PluginFilesChanged = "pluginFiles:changed"

// DialogResult is the user's answer to an open dialog
type DialogResult struct {
	Canceled  bool
	FilePaths []string
}

// Host is what the shell provides, native dialogs attached to a window and the app window broadcast
type Host interface {
	MainWindow() WatchOwner
	ShowOpenDialog(parent WatchOwner, opts OpenDialogOptions) (DialogResult, error)
	ShowSaveDialog(parent WatchOwner, opts SaveDialogOptions) (SaveDialogResult, error)
	Broadcast(event string, payload any)
}

// Config holds the app paths the files group resolves against
type Config struct {
	HermesHome     string
	UserData       string
	DocumentsDir   string
	ProtectedHomes []string
	Workspace      Workspace
}

// Service is the files group, every method a preload function of the renderer
type Service struct {
	host    Host
	cfg     Config
	watches *PreviewWatches
	plugins *PluginFiles
	git     func() string
}

// Files group over the shell host, the trash and the app paths
func NewService(host Host, cfg Config) *Service {
	s := &Service{host: host, cfg: cfg, watches: NewPreviewWatches(electronPlatformName()), git: ResolveGitBinary}
	s.plugins = NewPluginFiles(PluginFilesDeps{
		UserDataDir:    func() string { return cfg.UserData },
		ProtectedRoots: func() []string { return cfg.ProtectedHomes },
		TrashItem:      TrashItem,
	})
	return s
}

// Stops every file watch and kills timed git commands, the quit teardown
func (s *Service) Shutdown() {
	s.watches.CloseAll()
	KillTimedGitChildren()
}

// Native picker for files or folders attached to the main window, the picked paths or none
func (s *Service) SelectPaths(options any) ([]string, error) {
	opts, _ := options.(map[string]any)
	properties := SelectPathsDialogProperties(jsTruthy(opts["directories"]), opts["multiple"])
	defaultPath := ""
	if jsTruthy(opts["defaultPath"]) {
		bridged := jsString(opts["defaultPath"])
		if hostOS == "windows" {
			var profile *string
			if p, ok := opts["profile"].(string); ok {
				profile = &p
			}
			bridged = ResolvePickerDefaultPath(bridged, "", profile)
		}
		if bridged != "" {
			defaultPath = resolvePath(bridged)
		}
	}
	title := "Add context"
	if jsTruthy(opts["title"]) {
		title = jsString(opts["title"])
	}
	result, err := s.host.ShowOpenDialog(s.host.MainWindow(), OpenDialogOptions{
		Title: title, DefaultPath: defaultPath, Properties: properties, Filters: rawFilters(opts["filters"]),
	})
	if err != nil {
		return nil, err
	}
	if result.Canceled || result.FilePaths == nil {
		return []string{}, nil
	}
	return result.FilePaths, nil
}

// Filters passed through as the renderer gave them when they are a list
func rawFilters(value any) []FileFilter {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := []FileFilter{}
	for _, entry := range list {
		m, _ := entry.(map[string]any)
		f := FileFilter{Name: jsStringOrEmpty(m["name"]), Extensions: []string{}}
		if exts, ok := m["extensions"].([]any); ok {
			for _, e := range exts {
				f.Extensions = append(f.Extensions, jsStringOrEmpty(e))
			}
		}
		out = append(out, f)
	}
	return out
}

func (s *Service) saveDeps() SaveDialogDeps {
	return SaveDialogDeps{
		DefaultDir: func() string { return s.cfg.DocumentsDir },
		ShowSaveDialog: func(o SaveDialogOptions) (SaveDialogResult, error) {
			return s.host.ShowSaveDialog(s.host.MainWindow(), o)
		},
	}
}

// Native save location picker, the path only
func (s *Service) SelectSavePath(options any) (*string, error) {
	return SelectSavePath(options, s.saveDeps())
}

// Save dialog then the main process write of the content
func (s *Service) SaveTextFileAs(request any) (SaveTextFileResult, error) {
	if request == nil {
		request = map[string]any{}
	}
	return SaveTextFileAs(request, s.saveDeps())
}

// Watches a previewed file for the calling window
func (s *Service) WatchPreviewFile(caller WatchOwner, url any) (any, error) {
	return s.watches.WatchPreviewFile(caller, jsStringOrFalsy(url))
}

// Watches a directory for entry churn for the calling window
func (s *Service) WatchDirectory(caller WatchOwner, dir any) (PreviewWatch, error) {
	return s.watches.WatchDirectory(caller, jsStringOrFalsy(dir))
}

// Stops a file or directory watch
func (s *Service) StopPreviewFileWatch(id any) bool { return s.watches.Stop(jsStringOrFalsy(id)) }

// Keeps a renderer cwd that is a real folder outside the app, else the resolved default
func (s *Service) SanitizeWorkspaceCwd(cwd any) WorkspaceCwd {
	return s.cfg.Workspace.SanitizeWorkspaceCwd(cwd)
}

// Project tree listing of a directory
func (s *Service) ReadDir(dirPath any) ReadDirResult { return ReadDirForIpc(dirPath, nil) }

// Git root of a path
func (s *Service) GitRoot(startPath any) *string { return GitRootForIpc(startPath) }

// File manager reveal, ShowItemInFolder in production
var showItemInFolder = ShowItemInFolder

// Reveals an existing path in the file manager, false for a path not on this machine
func (s *Service) RevealPath(targetPath any) bool {
	target := jsTrim(jsStringOrFalsy(targetPath))
	if target == "" {
		return false
	}
	local := ExpandUserPath(target)
	if _, err := os.Stat(local); err != nil {
		return false
	}
	showItemInFolder(local)
	return true
}

// OpenDirResult is the open directory answer
type OpenDirResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Opens a directory in the file manager, creating it first
func (s *Service) OpenDir(dirPath any) OpenDirResult {
	dir := jsTrim(jsStringOrFalsy(dirPath))
	if dir == "" {
		return OpenDirResult{OK: false, Error: "no path"}
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return OpenDirResult{OK: false, Error: nodeFsError(err, "mkdir", dir).Error()}
	}
	if msg := OpenPath(filepath.Clean(dir)); msg != "" {
		return OpenDirResult{OK: false, Error: msg}
	}
	return OpenDirResult{OK: true}
}

var desktopProfileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Active desktop profile recorded in active-profile.json, nil when unset or not a profile name
func (s *Service) readActiveDesktopProfile() string {
	raw, err := os.ReadFile(filepath.Join(s.cfg.UserData, "active-profile.json"))
	if err != nil {
		return ""
	}
	parsed, err := jsonParse(string(raw))
	if err != nil {
		return ""
	}
	obj, _ := parsed.(map[string]any)
	profile, _ := obj["profile"].(string)
	profile = jsTrim(profile)
	if desktopProfileNameRE.MatchString(profile) {
		return profile
	}
	return ""
}

// Profile aware local root under the icebath home, created on demand
func (s *Service) localPluginsRoot(dirName string, owner any) string {
	named := ""
	if o, ok := owner.(string); ok {
		named = jsTrim(o)
	}
	profile := s.readActiveDesktopProfile()
	if named != "" && (named == "default" || desktopProfileNameRE.MatchString(named)) {
		profile = named
	}
	base := s.cfg.HermesHome
	if profile != "" && profile != "default" {
		base = filepath.Join(s.cfg.HermesHome, "profiles", profile)
	}
	return EnsureDir(filepath.Join(base, dirName))
}

// App level desktop plugin root, profile scoped leftovers moved up and unified halves reconciled
func (s *Service) DesktopPluginsRoot() string {
	root := EnsureDir(filepath.Join(s.cfg.HermesHome, DesktopPluginsDir))
	MigrateProfileScopedDesktopPlugins(s.cfg.HermesHome, root)
	ReconcileUnifiedDesktopHalves(s.cfg.HermesHome, root)
	return root
}

// Reruns the unified half reconcile, naming the refused packages too
func (s *Service) ReconcileDesktopPlugins() DesktopReconcileReport {
	root := EnsureDir(filepath.Join(s.cfg.HermesHome, DesktopPluginsDir))
	return ReconcileDesktopHalvesReport(s.cfg.HermesHome, root)
}

// Logs root of the profile that owns the failing session
func (s *Service) LogsRoot(profile any) string { return s.localPluginsRoot("logs", profile) }

// PathResult is the path a write or rename landed at
type PathResult struct {
	Path string `json:"path"`
}

// Renames an item in place within its folder, refusing traversal and collisions
func (s *Service) RenamePath(targetPath, newName any) (PathResult, error) {
	src := jsTrim(jsStringOrFalsy(targetPath))
	name := jsTrim(jsStringOrFalsy(newName))
	if src == "" || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return PathResult{}, errors.New("Invalid rename")
	}
	dst := filepath.Join(filepath.Dir(src), name)
	if dst == src {
		return PathResult{Path: dst}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return PathResult{}, fmt.Errorf("\"%s\" already exists", name)
	}
	if err := os.Rename(src, dst); err != nil {
		return PathResult{}, nodeFsError(err, "rename", src, dst)
	}
	return PathResult{Path: dst}, nil
}

// Writes a small UTF-8 file under an existing hardened parent
func (s *Service) WriteTextFile(filePath, content any) (PathResult, error) {
	raw := jsTrim(jsStringOrFalsy(filePath))
	if raw == "" {
		return PathResult{}, errors.New("Invalid path")
	}
	text := jsStringOrEmpty(content)
	if len(utf16.Encode([]rune(text))) > 1_000_000 {
		return PathResult{}, errors.New("Content too large")
	}
	resolved, err := ResolveRequestedPathForIpc(ExpandUserPath(raw), "Write text file", "")
	if err != nil {
		return PathResult{}, err
	}
	if !directoryExists(filepath.Dir(resolved)) {
		return PathResult{}, errors.New("Parent directory does not exist")
	}
	if err := os.WriteFile(resolved, []byte(text), 0o666); err != nil {
		return PathResult{}, nodeFsError(err, "open", resolved)
	}
	return PathResult{Path: resolved}, nil
}

// Moves an item to the OS trash
func (s *Service) TrashPath(targetPath any) (bool, error) {
	target := jsTrim(jsStringOrFalsy(targetPath))
	if target == "" {
		return false, errors.New("Invalid delete")
	}
	if err := TrashItem(target); err != nil {
		return false, err
	}
	return true, nil
}

const labelMax = 200

func pickerLabel(value any, fallback string) string {
	s, ok := value.(string)
	if !ok || jsTrim(s) == "" {
		return fallback
	}
	t := utf16.Encode([]rune(jsTrim(s)))
	if len(t) > labelMax {
		t = t[:labelMax]
	}
	return string(utf16.Decode(t))
}

// Native folder picker on the asking window, granting exactly the folder the user picked
func (s *Service) PluginFilesRequest(caller WatchOwner, pluginID, labels any) (map[string]any, error) {
	l, _ := labels.(map[string]any)
	result, err := s.host.ShowOpenDialog(caller, OpenDialogOptions{
		ButtonLabel: pickerLabel(l["buttonLabel"], "Allow access"),
		Properties:  []string{"openDirectory", "createDirectory"},
		Title:       pickerLabel(l["title"], "Choose a folder"),
	})
	if err != nil {
		return nil, err
	}
	if result.Canceled || len(result.FilePaths) == 0 || result.FilePaths[0] == "" {
		return map[string]any{"ok": true, "folder": nil}, nil
	}
	granted := s.plugins.AddGrant(pluginID, resolvePath(result.FilePaths[0]))
	if granted["ok"] == true {
		s.host.Broadcast(PluginFilesChanged, map[string]any{"pluginId": jsString(pluginID)})
	}
	return granted, nil
}

// Folder grants of a plugin
func (s *Service) PluginFilesGrants(pluginID any) ([]any, error) { return s.plugins.Grants(pluginID) }

// Revokes one grant or all of a plugin's grants, telling every window when any went
func (s *Service) PluginFilesRevoke(pluginID, folderID any, all bool) (map[string]any, error) {
	revoked, err := s.plugins.Revoke(pluginID, folderID, all)
	if err != nil {
		return nil, err
	}
	if n, _ := revoked["removed"].(int); n != 0 {
		s.host.Broadcast(PluginFilesChanged, map[string]any{"pluginId": jsString(pluginID)})
	}
	return revoked, nil
}

// One plugin file operation
func (s *Service) PluginFilesOp(pluginID, request any) map[string]any {
	return s.plugins.Run(pluginID, request)
}

func identifierOf(payload any) string {
	p, _ := payload.(map[string]any)
	v := p["identifier"]
	if !jsTruthy(v) {
		v = p["repo"]
	}
	return jsTrim(jsStringOrFalsy(v))
}

// Clones a plugin repository and reports what it carries
func (s *Service) ProbePluginRepo(payload any) *OrderedObject {
	identifier := identifierOf(payload)
	if identifier == "" {
		return NewOrderedObject("ok", false, "error", "identifier is required", "agent", false, "desktop", false, "warnings", []string{})
	}
	return ProbePluginRepo(s.git(), identifier)
}

// Installs a repository's desktop half at the reviewed commit
func (s *Service) InstallDesktopPlugin(payload any) *OrderedObject {
	identifier := identifierOf(payload)
	if identifier == "" {
		return NewOrderedObject("ok", false, "error", "identifier is required")
	}
	p, _ := payload.(map[string]any)
	var sha *string
	if v, ok := p["sha"].(string); ok {
		sha = &v
	}
	return InstallDesktopPluginFromGit(s.git(), identifier, s.DesktopPluginsRoot(), jsTruthy(p["force"]), sha)
}

// Uninstalls a standalone desktop plugin by folder name
func (s *Service) RemoveDesktopPlugin(payload any) *OrderedObject {
	p, _ := payload.(map[string]any)
	return RemoveDesktopPlugin(filepath.Join(s.cfg.HermesHome, DesktopPluginsDir), p["name"])
}
