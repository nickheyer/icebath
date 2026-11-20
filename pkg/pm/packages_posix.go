//go:build !windows

package pm

import "os/exec"

// Console windows exist only on Windows, so a child spawns unchanged here
func hideWindow(cmd *exec.Cmd, hide bool) {}

// Copies bytes then metadata, the copyfile and copystat pair shutil copy2 runs off Windows
func platformCopy2(src, dst string) error {
	if err := copyFileData(src, dst); err != nil {
		return err
	}
	return copyStat(src, dst)
}
