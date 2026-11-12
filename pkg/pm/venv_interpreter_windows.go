//go:build windows

package pm

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Launcher file uv venv copies for an interpreter home plus the side files an embedded copy needs
func windowsVenvLauncher(home string) (string, []string, error) {
	launcher := "venvlauncher.exe"
	if free, _ := filepath.Glob(filepath.Join(home, "python3*t.dll")); len(free) > 0 {
		launcher = "venvlaunchert.exe"
	}
	shims := filepath.Join(home, "Lib", "venv", "scripts", "nt")
	for _, candidate := range []string{filepath.Join(shims, "python.exe"), filepath.Join(shims, launcher), filepath.Join(home, launcher)} {
		if isRegularFile(candidate) {
			return candidate, nil, nil
		}
	}
	python := filepath.Join(home, "python.exe")
	if !isRegularFile(python) {
		return "", nil, &fs.PathError{Op: "venv launcher", Path: python, Err: fs.ErrNotExist}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", nil, err
	}
	var extras []string
	for _, entry := range entries {
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".dll", ".pyd", ".zip":
			if entry.Type().IsRegular() {
				extras = append(extras, filepath.Join(home, entry.Name()))
			}
		}
	}
	return python, extras, nil
}

// Reports whether destination holds the same bytes as source
func sameFileBytes(source, destination string) (bool, error) {
	want, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	have, err := os.ReadFile(destination)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(want, have), nil
}

// Copies source over destination through a staged file so a reader never sees a partial launcher
func copyLauncherFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return DurableWriteBytes(destination, data)
}

// Copies the store interpreter's venv launcher into Scripts as uv venv picks it, the embedded fallback with its side files
func healVenvLauncher(venv, storePython string) error {
	source, extras, err := windowsVenvLauncher(filepath.Dir(storePython))
	if err != nil {
		return err
	}
	scripts := filepath.Dir(VenvPython(venv))
	pairs := [][2]string{{source, VenvPython(venv)}}
	for _, extra := range extras {
		pairs = append(pairs, [2]string{extra, filepath.Join(scripts, filepath.Base(extra))})
	}
	for _, pair := range pairs {
		same, err := sameFileBytes(pair[0], pair[1])
		if err != nil {
			return err
		}
		if same {
			continue
		}
		if err := copyLauncherFile(pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}
