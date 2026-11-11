//go:build !windows

package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"golang.org/x/sys/unix"
)

var (
	sshTokenRuntimeDirRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sshTokenFilenameRE   = regexp.MustCompile(`^[0-9a-f]{16}\.token$`)
	sshTokenValueRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parts of a POSIX path the way pathlib splits it, empty and dot segments dropped
func posixPathParts(p string) []string {
	parts := []string{}
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

// Reads and unlinks a Desktop SSH token through its private runtime directory under the account home
func readSSHSessionTokenFile(path string) (token string, err error) {
	if !filepath.IsAbs(path) {
		return "", sshTokenExit("--ssh-session-token-file must be absolute")
	}
	userHome, _ := os.UserHomeDir()
	tokenRoot := filepath.Join(userHome, ".icebath", "desktop-ssh")
	rootParts, pathParts := posixPathParts(tokenRoot), posixPathParts(path)
	if len(pathParts) < len(rootParts) || strings.Join(pathParts[:len(rootParts)], "\x00") != strings.Join(rootParts, "\x00") {
		return "", sshTokenExit("--ssh-session-token-file must be under the desktop-ssh directory")
	}
	relative := pathParts[len(rootParts):]
	if len(relative) != 2 || !sshTokenRuntimeDirRE.MatchString(relative[0]) {
		return "", sshTokenExit("--ssh-session-token-file has an invalid runtime path")
	}
	if !sshTokenFilenameRE.MatchString(relative[1]) {
		return "", sshTokenExit("--ssh-session-token-file has an invalid filename")
	}
	directoryFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileFlags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	uid := uint32(os.Getuid())
	checkDir := func(fd int, what string) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return sshTokenExit("--ssh-session-token-file has an unsafe " + what)
		}
		if st.Uid != uid {
			return sshTokenExit("--ssh-session-token-file " + what + " has the wrong owner")
		}
		if what == "parent directory" && st.Mode&0o777 != 0o700 {
			return sshTokenExit("--ssh-session-token-file parent has unsafe permissions")
		}
		return nil
	}
	rootFD, directoryFD, fileFD := -1, -1, -1
	defer func() {
		if fileFD >= 0 {
			unix.Close(fileFD)
		}
		if directoryFD >= 0 {
			_ = unix.Unlinkat(directoryFD, relative[1], 0)
			unix.Close(directoryFD)
		}
		if rootFD >= 0 {
			unix.Close(rootFD)
		}
	}()
	open := func() error {
		var err error
		if rootFD, err = unix.Open(tokenRoot, directoryFlags, 0); err != nil {
			return err
		}
		if err := checkDir(rootFD, "runtime root"); err != nil {
			return err
		}
		if directoryFD, err = unix.Openat(rootFD, relative[0], directoryFlags, 0); err != nil {
			return err
		}
		if err := checkDir(directoryFD, "parent directory"); err != nil {
			return err
		}
		fileFD, err = unix.Openat(directoryFD, relative[1], fileFlags, 0)
		return err
	}
	if err := open(); err != nil {
		var exit *DashboardExit
		if errors.As(err, &exit) {
			return "", err
		}
		if errors.Is(err, unix.ELOOP) {
			return "", sshTokenExit("--ssh-session-token-file is a symlink")
		}
		return "", sshTokenExit("--ssh-session-token-file is not accessible")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fileFD, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", sshTokenExit("--ssh-session-token-file is not a regular file")
	}
	if st.Size != 64 {
		return "", sshTokenExit("--ssh-session-token-file contains an invalid token")
	}
	if st.Uid != uid {
		return "", sshTokenExit("--ssh-session-token-file has the wrong owner")
	}
	if st.Mode&0o777&^0o600 != 0 {
		return "", sshTokenExit("--ssh-session-token-file has unsafe permissions")
	}
	stream := os.NewFile(uintptr(fileFD), relative[1])
	fileFD = -1
	raw := make([]byte, 0, 80)
	buf := make([]byte, 80)
	for len(raw) < 80 {
		n, readErr := stream.Read(buf[:80-len(raw)])
		raw = append(raw, buf[:n]...)
		if readErr != nil {
			break
		}
	}
	stream.Close()
	text, decodeErr := pystr.DecodeUTF8Sig(raw)
	if decodeErr != nil {
		return "", decodeErr
	}
	if runes := []rune(text); len(runes) > 65 {
		text = string(runes[:65])
	}
	if !sshTokenValueRE.MatchString(text) {
		return "", sshTokenExit("--ssh-session-token-file contains an invalid token")
	}
	return text, nil
}
