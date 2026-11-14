//go:build !darwin && !windows

package auth

import (
	"os/exec"

	"github.com/nickheyer/icebath/internal/proc"
)

func platformBrowsers(func(string) string) []browserCandidate { return nil }

func openNative(string) bool { return false }

func browserSession(cmd *exec.Cmd) { proc.NewSession(cmd) }
