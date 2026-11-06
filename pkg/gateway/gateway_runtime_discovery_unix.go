//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Swappable account database lookups of the home privacy check
var (
	discoveryGetpwuid = pypwd.Getpwuid
	discoveryGetgrgid = pypwd.Getgrgid
	discoveryGetpwall = pypwd.Getpwall
)

// Whether a home mode lets another user write it, group write private only for the owner's own group
func HomeModeUnsafe(st *syscall.Stat_t, path string) bool {
	mode := uint32(st.Mode) & 0o7777
	if mode&0o002 != 0 {
		return true
	}
	if mode&0o020 == 0 {
		return false
	}
	if mayHaveAccessACL(path) {
		return true
	}
	user, err := discoveryGetpwuid(int(st.Uid))
	if err != nil {
		return true
	}
	group, err := discoveryGetgrgid(int(st.Gid))
	if err != nil {
		return true
	}
	all, err := discoveryGetpwall()
	if err != nil {
		return true
	}
	for _, p := range all {
		if p.GID == group.GID && p.UID != int(st.Uid) {
			return true
		}
	}
	for _, m := range group.Mem {
		if m != user.Name {
			return true
		}
	}
	return !(group.GID == user.GID && group.Name == user.Name)
}

// Lstat of a control node checked for kind, this uid as owner and a private mode
func privateNode(path, kind string, isHome bool) (*syscall.Stat_t, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st := info.Sys().(*syscall.Stat_t)
	raw := uint32(st.Mode)
	var match bool
	switch kind {
	case "socket":
		match = raw&0o170000 == 0o140000
	case "file":
		match = raw&0o170000 == 0o100000
	case "directory":
		match = raw&0o170000 == 0o040000
	}
	if !match {
		return nil, newDiscoveryError("unsafe_control_path", fmt.Sprintf("%s is %s, expected %s", path, pyfs.Filemode(raw), kind), "")
	}
	expected := discoveryGetuid()
	if int(st.Uid) != expected {
		return nil, newDiscoveryError("unsafe_control_path", fmt.Sprintf("%s is owned by uid %d, expected uid %d (gateway user); "+
			"chown it to the gateway user", path, st.Uid, expected), "")
	}
	mode := raw & 0o7777
	if isHome && HomeModeUnsafe(st, path) {
		return nil, newDiscoveryError("unsafe_control_permissions", fmt.Sprintf("%s has mode %04o, writable by another user", path, mode),
			"chmod go-w "+pyshlex.Quote(path))
	}
	if !isHome && mode&0o077 != 0 {
		want := uint32(0o600)
		if kind == "directory" {
			want = 0o700
		}
		return nil, openModeError(path, mode, want)
	}
	return st, nil
}

func openModeError(path string, mode, expected uint32) *DiscoveryError {
	return newDiscoveryError("unsafe_control_permissions", fmt.Sprintf("%s has mode %04o, expected %04o", path, mode, expected),
		fmt.Sprintf("chmod %o %s", expected, pyshlex.Quote(path)))
}

// Connected control socket of home whose listener runs as this user
func connectPrivate(home string, timeout time.Duration) (net.Conn, error) {
	path, err := controlSocketPath(home)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	uid, known, err := socketPeerUID(conn)
	if err == nil && known && uid != discoveryGetuid() {
		err = newDiscoveryError("unsafe_control_peer", "", "")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Validated direct socket or authenticated pointer target of home's control channel
func controlSocketPath(home string) (string, error) {
	if _, err := privateNode(home, "directory", true); err != nil {
		return "", err
	}
	direct := filepath.Join(home, "gateway.sock")
	if _, err := os.Lstat(direct); err == nil {
		if _, err := privateNode(direct, "socket", false); err != nil {
			return "", err
		}
		return direct, nil
	}
	pointer := filepath.Join(home, "gateway.sock.path")
	fd, err := syscall.Open(pointer, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: pointer, Err: err}
	}
	stream := os.NewFile(uintptr(fd), pointer)
	var meta syscall.Stat_t
	if err := syscall.Fstat(fd, &meta); err != nil {
		stream.Close()
		return "", err
	}
	if uint32(meta.Mode)&0o170000 != 0o100000 || int(meta.Uid) != discoveryGetuid() || uint32(meta.Mode)&0o077 != 0 {
		stream.Close()
		return "", newDiscoveryError("unsafe_control_pointer", "", "")
	}
	data, err := io.ReadAll(io.LimitReader(stream, 4097))
	stream.Close()
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", newDiscoveryError("invalid_control_pointer", "", "")
	}
	if !utf8.Valid(data) {
		return "", errors.New("'utf-8' codec can't decode the control pointer")
	}
	target := pypath.PosixPurePathStr(pystr.Strip(string(data)))
	if !filepath.IsAbs(target) {
		return "", newDiscoveryError("invalid_control_pointer", "", "")
	}
	hash, err := controlHomeHash(home)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(target)
	if filepath.Base(target) != "control.sock" || filepath.Base(parent) != "hermes-gw-"+hash {
		return "", newDiscoveryError("invalid_control_pointer", "", "")
	}
	if _, err := privateNode(parent, "directory", false); err != nil {
		return "", err
	}
	if _, err := privateNode(target, "socket", false); err != nil {
		return "", err
	}
	return target, nil
}

// identify result of home's control socket, keeping timeout, access and protocol failures as errors
func QueryIdentify(home string, timeout time.Duration) (*pyjson.Object, error) {
	deadline := time.Now().Add(timeout)
	conn, err := connectPrivate(home, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if time.Until(deadline) <= 0 {
		return nil, ErrControlTimeout
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.Write(identifyRequest); err != nil {
		return nil, err
	}
	var data []byte
	buf := make([]byte, 65536)
	for !containsNewline(data) {
		if time.Until(deadline) <= 0 {
			return nil, ErrControlTimeout
		}
		n, err := conn.Read(buf[:min(65536, 524289-len(data))])
		data = append(data, buf[:n]...)
		if n > 0 && len(data) > 524288 {
			return nil, newDiscoveryError("oversized_control_response", "", "")
		}
		if err == io.EOF && !containsNewline(data) {
			return nil, newDiscoveryError("incomplete_control_response", "", "")
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	return identifyResponse(data)
}

func containsNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' {
			return true
		}
	}
	return false
}

// Validates gateway.lock, narrowing an upgraded same owner lock to 0600 first
func privateLock(lock string) error {
	_, err := privateNode(lock, "file", false)
	var de *DiscoveryError
	if err == nil || !errors.As(err, &de) || de.Reason != "unsafe_control_permissions" {
		return err
	}
	fd, err := syscall.Open(lock, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: lock, Err: err}
	}
	f := os.NewFile(uintptr(fd), lock)
	err = tightenLockMode(f)
	f.Close()
	if err != nil {
		return err
	}
	_, err = privateNode(lock, "file", false)
	return err
}

// Owner a private overlay home's gateway.lock symlink names, home itself otherwise
func overlayOwnerHome(home string) string {
	lock := filepath.Join(home, "gateway.lock")
	if _, err := privateNode(home, "directory", true); err != nil {
		return home
	}
	target, err := filepath.EvalSymlinks(lock)
	if err != nil {
		return home
	}
	target, _ = filepath.Abs(target)
	info, err := os.Lstat(lock)
	if err != nil || info.Mode()&os.ModeSymlink == 0 || filepath.Base(target) != "gateway.lock" {
		return home
	}
	if _, err := privateNode(filepath.Dir(target), "directory", true); err != nil {
		return home
	}
	if err := privateLock(target); err != nil {
		return home
	}
	return filepath.Dir(target)
}
