//go:build !windows

package codeexec

import (
	"os"
	"os/exec"
)

// Hands the child the reply read end as descriptor 3 and the request write end as descriptor 4
func attachChildPipe(cmd *exec.Cmd, replies, requests *os.File) (ChildPipe, error) {
	cmd.ExtraFiles = []*os.File{replies, requests}
	return ChildPipe{Replies: 3, Requests: 4}, nil
}
