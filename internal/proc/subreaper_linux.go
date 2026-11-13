package proc

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Makes this process adopt orphaned descendants instead of init, so a Reaper here collects them
func BecomeSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("become child subreaper: %w", err)
	}
	return nil
}

// Inode number naming the pid namespace this process runs in, the stamp lock holders record beside a pid
func PIDNamespace() (string, error) {
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", err
	}
	start, end := strings.IndexByte(link, '['), strings.LastIndexByte(link, ']')
	if start < 0 || end <= start+1 || strings.Trim(link[start+1:end], "0123456789") != "" {
		return "", fmt.Errorf("unexpected pid namespace link %q", link)
	}
	return link[start+1 : end], nil
}
