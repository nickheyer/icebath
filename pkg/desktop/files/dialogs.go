package files

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FileFilter is one named extension filter of a native dialog
type FileFilter struct {
	Name       string   `json:"name"`
	Extensions []string `json:"extensions"`
}

// OpenDialogOptions is a native open dialog request in Electron's terms
type OpenDialogOptions struct {
	Title       string
	DefaultPath string
	ButtonLabel string
	Properties  []string
	Filters     []FileFilter
}

// SaveDialogOptions is a native save dialog request in Electron's terms
type SaveDialogOptions struct {
	Title       string
	DefaultPath string
	Filters     []FileFilter
	Properties  []string
}

// SaveDialogResult is the user's answer to a save dialog
type SaveDialogResult struct {
	Canceled bool
	FilePath string
}

// Dialog properties of a selectPaths request, folder pickers able to create one
func SelectPathsDialogProperties(directories bool, multiple any) []string {
	properties := []string{"openFile"}
	if directories {
		properties = []string{"openDirectory", "createDirectory"}
	}
	if multiple != false {
		properties = append(properties, "multiSelections")
	}
	return properties
}

// SaveTextFileMaxBytes caps a /save write well above any realistic transcript
const SaveTextFileMaxBytes = 64 * 1024 * 1024

// SaveDialogDeps are the default folder, the native dialog and the write a save uses
type SaveDialogDeps struct {
	DefaultDir     func() string
	ShowSaveDialog func(SaveDialogOptions) (SaveDialogResult, error)
	WriteFile      func(filePath, content string) error
}

// Filters with a string name and an extension list, non string extensions dropped, nil when none
func safeFilters(value any) []FileFilter {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []FileFilter
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, ok := m["name"].(string)
		exts, extsOK := m["extensions"].([]any)
		if !ok || !extsOK {
			continue
		}
		f := FileFilter{Name: name, Extensions: []string{}}
		for _, e := range exts {
			if s, ok := e.(string); ok {
				f.Extensions = append(f.Extensions, s)
			}
		}
		out = append(out, f)
	}
	return out
}

// Bare file name of a renderer suggestion, never its directory
func suggestedName(value any) string {
	s, ok := value.(string)
	if !ok || jsTrim(s) == "" {
		return ""
	}
	base := path.Base(strings.ReplaceAll(jsTrim(s), `\`, "/"))
	if base == "." || base == ".." || base == "/" {
		return ""
	}
	return base
}

// SaveTextFileResult is the /save answer
type SaveTextFileResult struct {
	Saved    bool   `json:"saved"`
	Canceled bool   `json:"canceled,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Asks where the rendered content goes and writes it there from the main process
func SaveTextFileAs(request any, deps SaveDialogDeps) (SaveTextFileResult, error) {
	req, _ := request.(map[string]any)
	content, ok := req["content"].(string)
	if !ok {
		return SaveTextFileResult{}, errors.New("Invalid content")
	}
	if len(content) > SaveTextFileMaxBytes {
		return SaveTextFileResult{}, errors.New("Content too large")
	}
	title := "Save"
	if t, ok := req["title"].(string); ok && t != "" {
		title = t
	}
	defaultPath := ""
	if name := suggestedName(req["defaultPath"]); name != "" {
		defaultPath = filepath.Join(deps.DefaultDir(), name)
	}
	result, err := deps.ShowSaveDialog(SaveDialogOptions{
		Title: title, DefaultPath: defaultPath, Filters: safeFilters(req["filters"]),
		Properties: []string{"createDirectory", "showOverwriteConfirmation"},
	})
	if err != nil {
		return SaveTextFileResult{}, err
	}
	if result.Canceled || result.FilePath == "" {
		return SaveTextFileResult{Saved: false, Canceled: true}, nil
	}
	write := deps.WriteFile
	if write == nil {
		write = func(p, text string) error {
			if err := os.WriteFile(p, []byte(text), 0o666); err != nil {
				return nodeFsError(err, "open", p)
			}
			return nil
		}
	}
	if err := write(result.FilePath, content); err != nil {
		return SaveTextFileResult{}, err
	}
	return SaveTextFileResult{Saved: true, Path: result.FilePath}, nil
}

// Native save location picker, the picked path or nil when cancelled
func SelectSavePath(options any, deps SaveDialogDeps) (*string, error) {
	opts, _ := options.(map[string]any)
	title := "Save"
	if jsTruthy(opts["title"]) {
		title = jsString(opts["title"])
	}
	defaultPath := ""
	if jsTruthy(opts["defaultPath"]) {
		defaultPath = jsString(opts["defaultPath"])
	}
	var filters []FileFilter
	if list, ok := opts["filters"].([]any); ok {
		filters = []FileFilter{}
		for _, entry := range list {
			if m, ok := entry.(map[string]any); ok {
				f := FileFilter{Name: jsStringOrEmpty(m["name"]), Extensions: []string{}}
				if exts, ok := m["extensions"].([]any); ok {
					for _, e := range exts {
						f.Extensions = append(f.Extensions, jsStringOrEmpty(e))
					}
				}
				filters = append(filters, f)
			}
		}
	}
	result, err := deps.ShowSaveDialog(SaveDialogOptions{Title: title, DefaultPath: defaultPath, Filters: filters})
	if err != nil {
		return nil, err
	}
	if result.Canceled || result.FilePath == "" {
		return nil, nil
	}
	return &result.FilePath, nil
}
