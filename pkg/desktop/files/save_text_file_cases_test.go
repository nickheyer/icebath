package files

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSaveTextFileAsElectronSideSaveWriteWritesTheContentWhereTheUserChoseDefaultingToABareNameInTheDefaultDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "picked.md")
	var seen SaveDialogOptions
	result, err := SaveTextFileAs(map[string]any{
		"content": "# hello", "defaultPath": "../../etc/hermes_session_x.md",
		"filters": []any{map[string]any{"name": "Markdown", "extensions": []any{"md"}}},
	}, SaveDialogDeps{
		DefaultDir: func() string { return dir },
		ShowSaveDialog: func(o SaveDialogOptions) (SaveDialogResult, error) {
			seen = o
			return SaveDialogResult{FilePath: target}, nil
		},
	})
	if err != nil || result != (SaveTextFileResult{Saved: true, Path: target}) || readText(t, target) != "# hello" {
		t.Fatal(result, err)
	}
	if seen.DefaultPath != filepath.Join(dir, "hermes_session_x.md") ||
		!reflect.DeepEqual(seen.Filters, []FileFilter{{Name: "Markdown", Extensions: []string{"md"}}}) {
		t.Fatal(seen)
	}
}

func TestSaveTextFileAsElectronSideSaveWriteAsksBeforeOverwritingAnExistingFileAndCanCreateADirectoryLinuxNeedsBothFlags(t *testing.T) {
	var seen SaveDialogOptions
	SaveTextFileAs(map[string]any{"content": "x", "defaultPath": "a.md"}, SaveDialogDeps{
		DefaultDir: func() string { return "/home/u" },
		ShowSaveDialog: func(o SaveDialogOptions) (SaveDialogResult, error) {
			seen = o
			return SaveDialogResult{Canceled: true}, nil
		},
	})
	if !reflect.DeepEqual(seen.Properties, []string{"createDirectory", "showOverwriteConfirmation"}) {
		t.Fatal(seen.Properties)
	}
}

func TestSaveTextFileAsElectronSideSaveWriteWritesNothingWhenTheDialogIsCancelled(t *testing.T) {
	var writes []string
	result, err := SaveTextFileAs(map[string]any{"content": "x", "defaultPath": "a.json"}, SaveDialogDeps{
		DefaultDir:     func() string { return "/nowhere" },
		ShowSaveDialog: func(SaveDialogOptions) (SaveDialogResult, error) { return SaveDialogResult{Canceled: true}, nil },
		WriteFile:      func(p, _ string) error { writes = append(writes, p); return nil },
	})
	if err != nil || result != (SaveTextFileResult{Saved: false, Canceled: true}) || len(writes) != 0 {
		t.Fatal(result, err, writes)
	}
}

func TestSaveTextFileAsElectronSideSaveWriteRefusesNonStringOrOversizedContentBeforeOpeningADialog(t *testing.T) {
	opened := false
	deps := SaveDialogDeps{
		DefaultDir: func() string { return "/nowhere" },
		ShowSaveDialog: func(SaveDialogOptions) (SaveDialogResult, error) {
			opened = true
			return SaveDialogResult{Canceled: true}, nil
		},
	}
	if _, err := SaveTextFileAs(map[string]any{"content": float64(42)}, deps); err == nil || err.Error() != "Invalid content" {
		t.Fatal(err)
	}
	if _, err := SaveTextFileAs(map[string]any{"content": strings.Repeat("x", SaveTextFileMaxBytes+1)}, deps); err == nil || err.Error() != "Content too large" {
		t.Fatal(err)
	}
	if opened {
		t.Fatal("dialog opened")
	}
}

func TestRegisterSaveDialogIpcRegistersThePathPickerAndTheMainProcessWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	host := &fakeHost{save: func(SaveDialogOptions) SaveDialogResult { return SaveDialogResult{FilePath: target} }}
	s := NewService(host, Config{DocumentsDir: dir, UserData: t.TempDir(), HermesHome: t.TempDir()})
	picked, err := s.SelectSavePath(map[string]any{"title": "Export"})
	if err != nil || picked == nil || *picked != target {
		t.Fatal(picked, err)
	}
	saved, err := s.SaveTextFileAs(map[string]any{"content": "{}"})
	if err != nil || saved != (SaveTextFileResult{Saved: true, Path: target}) || readText(t, target) != "{}" {
		t.Fatal(saved, err)
	}
}
