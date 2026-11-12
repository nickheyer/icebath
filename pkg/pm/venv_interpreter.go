package pm

import (
	"os"
	"path/filepath"
	"strings"
)

// Leaves the venv starting on storePython, its launcher and pyvenv.cfg home rewritten only when they name another interpreter
func healVenvInterpreter(venv, storePython string) error {
	if err := healVenvHome(venv, filepath.Dir(storePython)); err != nil {
		return err
	}
	return healVenvLauncher(venv, storePython)
}

// Value of the first home line of pyvenv.cfg as getpath reads it, ok false when no line sets it
func pyvenvHome(text string) (string, int, bool) {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		key, value, hadEqu := strings.Cut(line, "=")
		if hadEqu && strings.ToLower(strings.TrimSpace(key)) == "home" {
			return strings.TrimSpace(value), i, true
		}
	}
	return "", -1, false
}

// Points the first home line of pyvenv.cfg at home, adding one ahead of the rest when the file has none
func healVenvHome(venv, home string) error {
	path := filepath.Join(venv, "pyvenv.cfg")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	current, index, ok := pyvenvHome(text)
	if ok && current == home {
		return nil
	}
	line := "home = " + home + "\n"
	lines := strings.SplitAfter(text, "\n")
	if ok {
		lines[index] = line
	} else {
		lines = append([]string{line}, lines...)
	}
	if err := DurableWriteBytes(path, []byte(strings.Join(lines, ""))); err != nil {
		return err
	}
	return os.Chmod(path, info.Mode().Perm())
}
