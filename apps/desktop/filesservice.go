package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// FilesService binds the files group preload functions to pkg/desktop/files
type FilesService struct {
	shell *Shell
	svc   *files.Service
}

// Files group over the shell's dialogs and windows, the app paths resolved as Electron resolves them
func newFilesService(s *Shell) *FilesService {
	home, _ := os.UserHomeDir()
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	hermesHome := backend.ResolveDesktopHermesHome(backend.HomeInputs{
		Home: home, Env: env, Platform: electronPlatform(), ReadWindowsHome: windowsUserHome,
		DirectoryExists: func(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() },
	})
	exe, _ := os.Executable()
	appRoot := s.rendererD
	if !s.packaged {
		appRoot, _ = os.Getwd()
	}
	docs := documentsDir(home)
	ws := files.Workspace{
		UserData: s.userData, Packaged: s.packaged, AppRoot: appRoot, ExecPath: exe, Platform: electronPlatform(),
		SourceRepoRoot: filepath.Clean(filepath.Join(appRoot, "..", "..")), Env: env, InitCwd: env["INIT_CWD"],
	}
	cfg := files.Config{
		HermesHome: hermesHome, UserData: s.userData, DocumentsDir: docs, Workspace: ws,
		ProtectedHomes: []string{hermesHome, backend.PlatformDefaultHermesHome(home, env, electronPlatform()), filepath.Join(home, ".icebath")},
	}
	return &FilesService{shell: s, svc: files.NewService(&filesHost{shell: s}, cfg)}
}

// Documents folder of the user, the XDG documents dir on Linux and Documents elsewhere
func documentsDir(home string) string {
	if runtime.GOOS == "linux" {
		if dir := xdgUserDir(home, "DOCUMENTS"); dir != "" {
			return dir
		}
	}
	return filepath.Join(home, "Documents")
}

// XDG user directory from user-dirs.dirs, empty when unset
func xdgUserDir(home, name string) string {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	raw, err := os.ReadFile(filepath.Join(config, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "XDG_"+name+"_DIR" {
			continue
		}
		value = strings.Trim(value, `"`)
		return strings.Replace(value, "$HOME", home, 1)
	}
	return ""
}

// Stops the file watches and timed git commands when the app quits
func (b *FilesService) ServiceShutdown() error {
	b.svc.Shutdown()
	return nil
}

// filesOwner is a shell window as the owner of a file watch or dialog
type filesOwner struct{ sw *shellWindow }

func (o filesOwner) IsDestroyed() bool              { return o.sw.IsDestroyed() }
func (o filesOwner) Send(event string, payload any) { o.sw.Send(event, payload) }

func (o filesOwner) OnDestroyed(listener func()) func() {
	var off atomic.Bool
	o.sw.onDestroyed(func() {
		if !off.Load() {
			listener()
		}
	})
	return func() { off.Store(true) }
}

func (b *FilesService) caller(ctx context.Context) files.WatchOwner {
	if w := callerWindow(b.shell, ctx); w != nil {
		return filesOwner{w}
	}
	return nil
}

// filesHost gives the files group native dialogs and the app window broadcast
type filesHost struct{ shell *Shell }

func (h *filesHost) MainWindow() files.WatchOwner {
	if w := h.shell.mainWindow(); w != nil {
		return filesOwner{w}
	}
	return nil
}

func (h *filesHost) Broadcast(event string, payload any) {
	for _, w := range h.shell.windowsList() {
		w.(*shellWindow).Send(event, payload)
	}
}

func attachTo(parent files.WatchOwner) application.Window {
	if o, ok := parent.(filesOwner); ok && !o.sw.IsDestroyed() {
		return o.sw.w
	}
	return nil
}

// Wails filter pattern of an Electron extension list
func filterPattern(exts []string) string {
	parts := make([]string, 0, len(exts))
	for _, e := range exts {
		if e == "*" {
			parts = append(parts, "*")
		} else {
			parts = append(parts, "*."+e)
		}
	}
	return strings.Join(parts, ";")
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (h *filesHost) ShowOpenDialog(parent files.WatchOwner, o files.OpenDialogOptions) (files.DialogResult, error) {
	d := h.shell.app.Dialog.OpenFile()
	dirs := has(o.Properties, "openDirectory")
	d.CanChooseDirectories(dirs).CanChooseFiles(has(o.Properties, "openFile") || !dirs).
		CanCreateDirectories(has(o.Properties, "createDirectory"))
	if o.Title != "" {
		d.SetTitle(o.Title)
	}
	if o.ButtonLabel != "" {
		d.SetButtonText(o.ButtonLabel)
	}
	if o.DefaultPath != "" {
		if st, err := os.Stat(o.DefaultPath); err == nil && st.IsDir() {
			d.SetDirectory(o.DefaultPath)
		} else {
			d.SetDirectory(filepath.Dir(o.DefaultPath))
		}
	}
	for _, f := range o.Filters {
		d.AddFilter(f.Name, filterPattern(f.Extensions))
	}
	if w := attachTo(parent); w != nil {
		d.AttachToWindow(w)
	}
	if has(o.Properties, "multiSelections") {
		picked, err := d.PromptForMultipleSelection()
		if err != nil {
			return files.DialogResult{}, err
		}
		return files.DialogResult{Canceled: len(picked) == 0, FilePaths: picked}, nil
	}
	picked, err := d.PromptForSingleSelection()
	if err != nil {
		return files.DialogResult{}, err
	}
	if picked == "" {
		return files.DialogResult{Canceled: true, FilePaths: []string{}}, nil
	}
	return files.DialogResult{FilePaths: []string{picked}}, nil
}

func (h *filesHost) ShowSaveDialog(parent files.WatchOwner, o files.SaveDialogOptions) (files.SaveDialogResult, error) {
	dir, name := "", ""
	if o.DefaultPath != "" {
		if st, err := os.Stat(o.DefaultPath); err == nil && st.IsDir() {
			dir = o.DefaultPath
		} else {
			dir, name = filepath.Dir(o.DefaultPath), filepath.Base(o.DefaultPath)
		}
	}
	for {
		d := h.shell.app.Dialog.SaveFile()
		d.SetOptions(&application.SaveFileDialogOptions{Title: o.Title})
		d.CanCreateDirectories(has(o.Properties, "createDirectory"))
		if dir != "" {
			d.SetDirectory(dir)
		}
		if name != "" {
			d.SetFilename(name)
		}
		for _, f := range o.Filters {
			d.AddFilter(f.Name, filterPattern(f.Extensions))
		}
		if w := attachTo(parent); w != nil {
			d.AttachToWindow(w)
		}
		picked, err := d.PromptForSingleSelection()
		if err != nil {
			return files.SaveDialogResult{}, err
		}
		if picked == "" {
			return files.SaveDialogResult{Canceled: true}, nil
		}
		if !has(o.Properties, "showOverwriteConfirmation") || !nativeSaveNeedsOverwritePrompt() {
			return files.SaveDialogResult{FilePath: picked}, nil
		}
		if _, err := os.Stat(picked); err != nil || h.confirmReplace(parent, picked) {
			return files.SaveDialogResult{FilePath: picked}, nil
		}
		dir, name = filepath.Dir(picked), filepath.Base(picked)
	}
}

// Asks before a save replaces an existing file, the GTK chooser's own overwrite question
func (h *filesHost) confirmReplace(parent files.WatchOwner, picked string) bool {
	answer := make(chan bool, 1)
	q := h.shell.app.Dialog.Question().
		SetTitle("Confirm Save As").
		SetMessage("A file named “" + filepath.Base(picked) + "” already exists. Do you want to replace it?")
	replace := q.AddButton("Replace")
	replace.OnClick(func() { answer <- true })
	cancel := q.AddButton("Cancel")
	cancel.OnClick(func() { answer <- false })
	q.SetDefaultButton(cancel)
	q.SetCancelButton(cancel)
	if w := attachTo(parent); w != nil {
		q.AttachToWindow(w)
	}
	q.Show()
	return <-answer
}

// SelectPaths is selectPaths
func (b *FilesService) SelectPaths(options any) ([]string, error) { return b.svc.SelectPaths(options) }

// SelectSavePath is selectSavePath
func (b *FilesService) SelectSavePath(options any) (*string, error) {
	return b.svc.SelectSavePath(options)
}

// SaveTextFileAs is saveTextFileAs
func (b *FilesService) SaveTextFileAs(request any) (files.SaveTextFileResult, error) {
	return b.svc.SaveTextFileAs(request)
}

// WatchPreviewFile is watchPreviewFile for the calling window
func (b *FilesService) WatchPreviewFile(ctx context.Context, url any) (any, error) {
	return b.svc.WatchPreviewFile(b.caller(ctx), url)
}

// WatchDirectory is watchDirectory for the calling window
func (b *FilesService) WatchDirectory(ctx context.Context, dir any) (files.PreviewWatch, error) {
	return b.svc.WatchDirectory(b.caller(ctx), dir)
}

// StopPreviewFileWatch is stopPreviewFileWatch
func (b *FilesService) StopPreviewFileWatch(id any) bool { return b.svc.StopPreviewFileWatch(id) }

// SanitizeWorkspaceCwd is sanitizeWorkspaceCwd
func (b *FilesService) SanitizeWorkspaceCwd(cwd any) files.WorkspaceCwd {
	return b.svc.SanitizeWorkspaceCwd(cwd)
}

// ReadDir is readDir
func (b *FilesService) ReadDir(dirPath any) files.ReadDirResult { return b.svc.ReadDir(dirPath) }

// GitRoot is gitRoot
func (b *FilesService) GitRoot(startPath any) *string { return b.svc.GitRoot(startPath) }

// RevealPath is revealPath
func (b *FilesService) RevealPath(targetPath any) bool { return b.svc.RevealPath(targetPath) }

// OpenDir is openDir
func (b *FilesService) OpenDir(dirPath any) files.OpenDirResult { return b.svc.OpenDir(dirPath) }

// DesktopPluginsRoot is desktopPluginsRoot
func (b *FilesService) DesktopPluginsRoot() string { return b.svc.DesktopPluginsRoot() }

// ReconcileDesktopPlugins answers reconcileDesktopPluginsReport, and reconcileDesktopPlugins reads its published list
func (b *FilesService) ReconcileDesktopPlugins() files.DesktopReconcileReport {
	return b.svc.ReconcileDesktopPlugins()
}

// LogsRoot is logsRoot
func (b *FilesService) LogsRoot(profile any) string { return b.svc.LogsRoot(profile) }

// RenamePath is renamePath
func (b *FilesService) RenamePath(targetPath, newName any) (files.PathResult, error) {
	return b.svc.RenamePath(targetPath, newName)
}

// WriteTextFile is writeTextFile
func (b *FilesService) WriteTextFile(filePath, content any) (files.PathResult, error) {
	return b.svc.WriteTextFile(filePath, content)
}

// TrashPath is trashPath
func (b *FilesService) TrashPath(targetPath any) (bool, error) { return b.svc.TrashPath(targetPath) }

// PluginFilesRequest is pluginFiles.request on the calling window
func (b *FilesService) PluginFilesRequest(ctx context.Context, pluginID, labels any) (map[string]any, error) {
	return b.svc.PluginFilesRequest(b.caller(ctx), pluginID, labels)
}

// PluginFilesGrants is pluginFiles.grants
func (b *FilesService) PluginFilesGrants(pluginID any) ([]any, error) {
	return b.svc.PluginFilesGrants(pluginID)
}

// PluginFilesRevoke is pluginFiles.revoke, all set when the renderer named no folder
func (b *FilesService) PluginFilesRevoke(pluginID, folderID any, all bool) (map[string]any, error) {
	return b.svc.PluginFilesRevoke(pluginID, folderID, all)
}

// PluginFilesOp is pluginFiles.op
func (b *FilesService) PluginFilesOp(pluginID, request any) map[string]any {
	return b.svc.PluginFilesOp(pluginID, request)
}

// ProbePluginRepo is probePluginRepo
func (b *FilesService) ProbePluginRepo(payload any) json.RawMessage {
	return rawJSON(b.svc.ProbePluginRepo(payload))
}

// InstallDesktopPlugin is installDesktopPlugin
func (b *FilesService) InstallDesktopPlugin(payload any) json.RawMessage {
	return rawJSON(b.svc.InstallDesktopPlugin(payload))
}

// RemoveDesktopPlugin is removeDesktopPlugin
func (b *FilesService) RemoveDesktopPlugin(payload any) json.RawMessage {
	return rawJSON(b.svc.RemoveDesktopPlugin(payload))
}

func rawJSON(o *files.OrderedObject) json.RawMessage {
	b, _ := o.MarshalJSON()
	return b
}

// Wires native drop paths into a new window so getPathForFile answers synchronously
func (s *Shell) installFileDrop(sw *shellWindow) {
	sw.onRuntimeReady(func() { nativeInstallFileDrop(sw.w) })
}

// Dropped file entries the bridge matches File objects against, name, size and mtime of each path
func droppedFilesJSON(paths []string) string {
	type entry struct {
		Path         string  `json:"path"`
		Name         string  `json:"name"`
		Size         int64   `json:"size"`
		LastModified float64 `json:"lastModified"`
		Directory    bool    `json:"directory"`
	}
	out := []entry{}
	for _, p := range paths {
		e := entry{Path: p, Name: filepath.Base(p)}
		if st, err := os.Stat(p); err == nil {
			e.Size, e.LastModified, e.Directory = st.Size(), float64(st.ModTime().UnixMilli()), st.IsDir()
		}
		out = append(out, e)
	}
	b, _ := json.Marshal(out)
	return string(b)
}
