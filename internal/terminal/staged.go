package terminal

import (
	"cmp"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Sandbox path a command's stdin is staged at in the temp folder, for a backend that can't hand a process stdin but can write a file
func StdinPath(temp string) string {
	return cmp.Or(strings.TrimRight(temp, "/"), "/") + "/.icebath-stdin-" + strs.Random(16)
}

// Script that reads its stdin from the staged file, removed before the script runs
func FromFile(script, path string) string {
	quoted := Quote(path)
	return "exec 0< " + quoted + " || exit $?\nrm -f -- " + quoted + " || exit $?\n" + script
}
