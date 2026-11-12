//go:build !windows

package plugins

import (
	"fmt"
	"os"
	"os/exec"
)

// Hands the child this process's standard streams as extra descriptors for plugin CLI commands, the null device for a closed one
func attachTerminal(cmd *exec.Cmd) (spec string, release func(), err error) {
	var opened []*os.File
	release = func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	files := make([]*os.File, 3)
	for i, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if f != nil {
			if _, statErr := f.Stat(); statErr == nil {
				files[i] = f
				continue
			}
		}
		flag := os.O_WRONLY
		if i == 0 {
			flag = os.O_RDONLY
		}
		null, err := os.OpenFile(os.DevNull, flag, 0)
		if err != nil {
			release()
			return "", nil, err
		}
		files[i], opened = null, append(opened, null)
	}
	base := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, files...)
	return fmt.Sprintf("%d,%d,%d", base, base+1, base+2), release, nil
}
