package gateway

import (
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// True when text names icebath at a path segment or token boundary
func textNamesIcebath(text string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return pystr.IsSpace(r) || strings.ContainsRune("\\/=,;\"'", r)
	})
	for _, token := range tokens {
		if strings.HasPrefix(token, "icebath") || strings.HasPrefix(token, ".icebath") {
			return true
		}
	}
	return false
}

// True when the command line or executable of pid names icebath, false when either is unreadable
func processCommandIsIcebath(pid int) bool {
	argv, err := proc.Cmdline(pid)
	if err != nil {
		return false
	}
	exe, err := proc.Exe(pid)
	if err != nil {
		return false
	}
	return textNamesIcebath(strings.Join(argv, " ") + " " + exe)
}

// Whether a force stop may target pid, its start time unchanged since discovery and on Windows its command icebath
func pidIsIcebath(pid int, expectedStartTime any) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS != "windows" {
		return expectedStartTime == nil || GetProcessStartTime(pid) == expectedStartTime
	}
	current := GetProcessStartTime(pid)
	if current == nil {
		return false
	}
	if expectedStartTime != nil && current != expectedStartTime {
		return false
	}
	return processCommandIsIcebath(pid)
}
