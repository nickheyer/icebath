//go:build !windows

package pm

import (
	"os"
	"path/filepath"
)

// Makes bin/python a symlink to the store interpreter the way uv venv lays it out, swapped in atomically
func healVenvLauncher(venv, storePython string) error {
	launcher := VenvPython(venv)
	if target, err := os.Readlink(launcher); err == nil && target == storePython {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launcher), 0o777); err != nil {
		return err
	}
	staged, err := os.CreateTemp(filepath.Dir(launcher), ".python-")
	if err != nil {
		return err
	}
	name := staged.Name()
	staged.Close()
	if err := os.Remove(name); err != nil {
		return err
	}
	if err := os.Symlink(storePython, name); err != nil {
		return err
	}
	if err := os.Rename(name, launcher); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
