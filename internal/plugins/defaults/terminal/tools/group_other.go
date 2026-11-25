//go:build !unix

package tools

import "os/exec"

func group(*exec.Cmd) {}
