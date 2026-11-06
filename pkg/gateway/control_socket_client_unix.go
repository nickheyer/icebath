//go:build !windows

package gateway

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// XDG_RUNTIME_DIR when it is an absolute directory owned by this user with no group or other access
func privateRuntimeDir() string {
	raw := os.Getenv("XDG_RUNTIME_DIR")
	if raw == "" || !filepath.IsAbs(raw) {
		return ""
	}
	info, err := os.Lstat(raw)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return ""
	}
	return filepath.Clean(raw)
}

// One request and response line over the unix control socket, every socket failure answering nil
func controlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	path, err := ResolveClientSocketPath(home)
	if err != nil || path == "" {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, nil
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return nil, nil
	}
	if _, err := conn.Write(request); err != nil {
		return nil, nil
	}
	buf := make([]byte, 65536)
	line, err := readResponseLine(func() ([]byte, error) {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		n, err := conn.Read(buf)
		if err == io.EOF {
			return buf[:n:n], nil
		}
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), buf[:n]...), nil
	}, time.Now().Add(timeout))
	if err != nil {
		return nil, nil
	}
	return line, nil
}
