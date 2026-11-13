//go:build !windows

package qqbot

import "os/exec"

// Child starts as is since only Windows opens console windows for children
func hideConsoleWindow(*exec.Cmd) {}
