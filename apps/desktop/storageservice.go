package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/storage"
	"github.com/nickheyer/icebath/pkg/desktop/winenv"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// StorageService binds the storage group of the preload API
type StorageService struct{ shell *Shell }

// UiPreferencesRead answers uiPreferences.read
func (b *StorageService) UiPreferencesRead() (storage.UIPreferencesSnapshot, error) {
	return b.shell.Storage.UiPreferencesRead()
}

// UiPreferencesWrite answers uiPreferences.write
func (b *StorageService) UiPreferencesWrite(payload json.RawMessage) (storage.UIPreferencesSnapshot, error) {
	return b.shell.Storage.UiPreferencesWrite(payload)
}

// PreparedSubmissionsRead answers preparedSubmissions.read
func (b *StorageService) PreparedSubmissionsRead() (string, error) {
	return b.shell.Storage.PreparedSubmissionsRead(rendererOrigin())
}

// PreparedSubmissionsUpdate answers preparedSubmissions.update
func (b *StorageService) PreparedSubmissionsUpdate(key, entry json.RawMessage) error {
	return b.shell.Storage.PreparedSubmissionsUpdate(rendererOrigin(), key, entry)
}

// PreparedSubmissionsCompareAndSet answers preparedSubmissions.compareAndSet
func (b *StorageService) PreparedSubmissionsCompareAndSet(key, expected, entry json.RawMessage) (*storage.Object, error) {
	return b.shell.Storage.PreparedSubmissionsCompareAndSet(rendererOrigin(), key, expected, entry)
}

// RevealLogs answers revealLogs
func (b *StorageService) RevealLogs() *storage.Object { return b.shell.Storage.RevealLogs() }

// GetRecentLogs answers getRecentLogs
func (b *StorageService) GetRecentLogs() *storage.Object { return b.shell.Storage.GetRecentLogs() }

// ReportRendererError handles reportRendererError
func (b *StorageService) ReportRendererError(report json.RawMessage) {
	b.shell.Storage.ReportRendererError(report)
}

// LogLine handles logLine
func (b *StorageService) LogLine(line json.RawMessage) { b.shell.Storage.LogLine(line) }

// SaveImageBuffer answers saveImageBuffer, data arriving as base64 from the bridge
func (b *StorageService) SaveImageBuffer(data *[]byte, ext, name json.RawMessage) (string, error) {
	var bytes []byte
	if data != nil {
		bytes = *data
		if bytes == nil {
			bytes = []byte{}
		}
	}
	return b.shell.Storage.SaveImageBuffer(bytes, ext, name)
}

// SavePastedText answers savePastedText
func (b *StorageService) SavePastedText(text json.RawMessage) (string, error) {
	return b.shell.Storage.SavePastedText(text)
}

// ReadPluginSource answers readPluginSource
func (b *StorageService) ReadPluginSource(filePath json.RawMessage) (*storage.Object, error) {
	return b.shell.Storage.ReadPluginSource(filePath)
}

// ReadFileText answers readFileText
func (b *StorageService) ReadFileText(filePath json.RawMessage) (any, error) {
	return b.shell.Storage.ReadFileText(filePath)
}

// ReadFileDataUrl answers readFileDataUrl
func (b *StorageService) ReadFileDataUrl(filePath json.RawMessage) (any, error) {
	return b.shell.Storage.ReadFileDataUrl(filePath)
}

// ReadFileDataUrlForAttach answers readFileDataUrlForAttach
func (b *StorageService) ReadFileDataUrlForAttach(filePath json.RawMessage) (any, error) {
	return b.shell.Storage.ReadFileDataUrlForAttach(filePath)
}

// SaveImageFromUrl answers saveImageFromUrl
func (b *StorageService) SaveImageFromUrl(rawURL json.RawMessage) (bool, error) {
	return b.shell.Storage.SaveImageFromUrl(rawURL)
}

// DataUrlReadMaxGet answers dataUrlReadMax.get
func (b *StorageService) DataUrlReadMaxGet() *storage.Object {
	return b.shell.Storage.DataUrlReadMaxGet()
}

// DataUrlReadMaxSet answers dataUrlReadMax.set, the bridge sending String(Number(maxMb))
func (b *StorageService) DataUrlReadMaxSet(numberText string) *storage.Object {
	return b.shell.Storage.DataUrlReadMaxSet(numberText)
}

// SettingsGetDefaultProjectDir answers settings.getDefaultProjectDir
func (b *StorageService) SettingsGetDefaultProjectDir() *storage.Object {
	return b.shell.Storage.SettingsGetDefaultProjectDir()
}

// SettingsSetDefaultProjectDir answers settings.setDefaultProjectDir
func (b *StorageService) SettingsSetDefaultProjectDir(dir json.RawMessage) (*storage.Object, error) {
	return b.shell.Storage.SettingsSetDefaultProjectDir(dir)
}

// SettingsPickDefaultProjectDir answers settings.pickDefaultProjectDir
func (b *StorageService) SettingsPickDefaultProjectDir() (*storage.Object, error) {
	return b.shell.Storage.SettingsPickDefaultProjectDir()
}

// Origin of the pages the shell serves, the asset server URL every app window loads from
func rendererOrigin() string {
	if runtime.GOOS == "windows" {
		return storage.URLOrigin("http://wails.localhost/")
	}
	return storage.URLOrigin("wails://localhost/")
}

// Storage group host over the Wails windows and file manager
type storageHost struct{ shell *Shell }

// Sends event to every app window as webContents.send does
func (h *storageHost) Broadcast(event string, payload any) {
	for _, w := range h.shell.windowsList() {
		w.(*shellWindow).Send(event, payload)
	}
}

// Opens the file manager with path selected, logging a file manager failure to desktop.log
func (h *storageHost) ShowItemInFolder(path string) {
	if err := h.shell.app.Env.OpenFileManager(path, true); err != nil {
		h.shell.DesktopLog.Remember("[logs] show in folder failed: " + err.Error())
	}
}

// Shows the native save dialog over the main window, the directory and name split as Electron splits defaultPath
func (h *storageHost) ShowSaveDialog(o files.SaveDialogOptions) (files.SaveDialogResult, error) {
	opts := &application.SaveFileDialogOptions{Title: o.Title}
	if o.DefaultPath != "" {
		if st, err := os.Stat(o.DefaultPath); err == nil && st.IsDir() {
			opts.Directory = o.DefaultPath
		} else {
			if filepath.IsAbs(o.DefaultPath) {
				opts.Directory = filepath.Dir(o.DefaultPath)
			}
			opts.Filename = filepath.Base(o.DefaultPath)
		}
	}
	d := h.shell.app.Dialog.SaveFile()
	d.SetOptions(opts)
	for _, f := range o.Filters {
		d.AddFilter(f.Name, filterPattern(f.Extensions))
	}
	if w := h.shell.mainWindow(); w != nil && !w.IsDestroyed() {
		d.AttachToWindow(w.w)
	}
	picked, err := d.PromptForSingleSelection()
	if err != nil {
		return files.SaveDialogResult{}, err
	}
	return files.SaveDialogResult{Canceled: picked == "", FilePath: picked}, nil
}

// Downloads folder as app.getPath downloads resolves it, empty when the OS has none
func (h *storageHost) DownloadsDir() string { return downloadsDir() }

// Session workspace from the inputs the files group builds its own from, the app root and executable of this run
func storageWorkspace(s *Shell) files.Workspace {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	exe, _ := os.Executable()
	appRoot := s.rendererD
	if !s.packaged {
		appRoot, _ = os.Getwd()
	}
	return files.Workspace{
		UserData: s.userData, Packaged: s.packaged, AppRoot: appRoot, ExecPath: exe, Platform: electronPlatform(),
		SourceRepoRoot: filepath.Clean(filepath.Join(appRoot, "..", "..")), Env: env, InitCwd: env["INIT_CWD"],
	}
}

// Root ICEBATH_HOME the desktop resolves at startup, the live HKCU value on Windows
func machineHermesHome() string {
	env, order := winenv.ProcessEnv()
	home, _ := os.UserHomeDir()
	return backend.ResolveDesktopHermesHome(backend.HomeInputs{
		Home:     home,
		Env:      env,
		Platform: electronPlatform(),
		DirectoryExists: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && st.IsDir()
		},
		ReadWindowsHome: func() string {
			v, _ := winenv.ReadWindowsUserEnvVar("ICEBATH_HOME", winenv.Options{Platform: electronPlatform(), Env: env, EnvOrder: order})
			return v
		},
	})
}

// Shows the native folder picker that may create folders, false when canceled
func (h *storageHost) PickDirectory(title, defaultPath string) (string, bool, error) {
	dir, err := h.shell.app.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		CanCreateDirectories(true).
		SetTitle(title).
		SetDirectory(defaultPath).
		PromptForSingleSelection()
	if err != nil {
		return "", false, err
	}
	return dir, dir != "", nil
}
