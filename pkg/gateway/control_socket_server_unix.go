//go:build !windows

package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Seams over the listener and the stat calls, replaced by tests the way hermes tests monkeypatch asyncio and os
var (
	controlListenUnix = listenUnixControl
	controlLstat      = os.Lstat
	controlStat       = os.Stat
)

// Unix listener at path that leaves the socket file for the identity checked cleanup to remove
func listenUnixControl(path string) (net.Listener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	return listener, nil
}

// Starts the platform listener, the unix socket on every POSIX host
func (s *GatewayControlServer) startPlatform() error { return s.startPosix() }

// Binds the POSIX listener, moving to the private temp dir socket when the home refuses sockets
func (s *GatewayControlServer) startPosix() error {
	bindPath, pointerFile, err := ResolveServerSocketPath(s.home)
	if err != nil {
		return err
	}
	bound, err := s.bindPOSIX(bindPath, pointerFile)
	if err != nil {
		if pointerFile != "" || !errors.Is(err, syscall.EOPNOTSUPP) && !errors.Is(err, syscall.ENOTSUP) {
			return err
		}
		slog.Warn(fmt.Sprintf("Control socket cannot be created in %s (%s); listening in a private temp directory", s.home, controlStrerror(err)))
		pointerFile = filepath.Join(s.home, controlPointerFilename)
		fallback, ferr := fallbackSocketPath(s.home)
		if ferr != nil {
			return ferr
		}
		if bound, err = s.bindPOSIX(fallback, pointerFile); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindPath = bound
	if err := s.noteFileIdentity(bound); err != nil {
		return err
	}
	if pointerFile != "" {
		if err := publishPointer(pointerFile, bound); err != nil {
			return err
		}
		s.pointerFile = pointerFile
	}
	for _, path := range []string{s.bindPath, s.pointerFile} {
		if path == "" {
			continue
		}
		if err := s.noteFileIdentity(path); err != nil {
			return err
		}
	}
	slog.Info(fmt.Sprintf("Gateway control socket listening at %s", bound))
	return nil
}

// Strerror of an OSError, the whole error text when it carries no errno
func controlStrerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}

// Binds the listener at bindPath, a squat proof root when its shared temp directory is not private
func (s *GatewayControlServer) bindPOSIX(bindPath, pointerFile string) (string, error) {
	if pointerFile != "" {
		if err := os.Mkdir(filepath.Dir(bindPath), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		private, err := privateDirectory(filepath.Dir(bindPath))
		if err != nil {
			return "", err
		}
		if !private {
			slog.Warn(fmt.Sprintf("Fallback control directory %s is not private; using a fresh one", filepath.Dir(bindPath)))
			moved, err := squatProofFallback(bindPath)
			if err != nil {
				return "", err
			}
			bindPath = moved
			s.mu.Lock()
			s.fallbackRoot = filepath.Dir(filepath.Dir(bindPath))
			s.mu.Unlock()
		}
	}
	info, err := controlLstat(bindPath)
	if err != nil && !pathMissing(err) {
		return "", err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", &PyError{Kind: "PermissionError", Msg: "control socket cannot be a symlink"}
	}
	if _, err := controlStat(bindPath); err == nil {
		st, err := controlLstat(bindPath)
		if err != nil {
			return "", err
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
			return "", &PyError{Kind: "PermissionError", Msg: "control socket belongs to another user"}
		}
		if err := os.Remove(bindPath); err != nil {
			return "", err
		}
	} else if !pathMissing(err) {
		return "", err
	}
	oldUmask := syscall.Umask(0o177)
	listener, err := controlListenUnix(bindPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return "", err
	}
	_ = os.Chmod(bindPath, 0o600)
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	s.conns.Add(1)
	go s.acceptLoop(listener)
	return bindPath, nil
}

// Whether a stat failure is one pathlib reads as a missing path
func pathMissing(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP)
}

// Directory owned by this user with no group or other access, never following a symlink
func privateDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && ok && int(st.Uid) == os.Getuid() && uint32(st.Mode)&0o077 == 0, nil
}

// Same fallback leaf under a fresh unpredictable root, PermissionError when no such path fits sun_path
func squatProofFallback(bindPath string) (string, error) {
	root, err := os.MkdirTemp(filepath.Dir(filepath.Dir(bindPath)), "hgw-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, filepath.Base(filepath.Dir(bindPath)), filepath.Base(bindPath))
	fits, err := fitsSunPath(path)
	if err != nil {
		return "", err
	}
	if !fits {
		_ = os.Remove(root)
		return "", &PyError{Kind: "PermissionError", Msg: "fallback control directory is squatted and no private path fits sun_path"}
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// Atomically replaces the pointer file with a fresh 0600 one naming the bind path
func publishPointer(pointerFile, bindPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(pointerFile), "."+filepath.Base(pointerFile)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(bindPath); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), pointerFile); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Accepts connections until the listener closes, serving each on its own goroutine
func (s *GatewayControlServer) acceptLoop(listener net.Listener) {
	defer s.conns.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)
		go func() {
			defer s.conns.Done()
			s.handleConnection(conn)
		}()
	}
}

// Reads one request line and writes its one response line, then closes
func (s *GatewayControlServer) handleConnection(conn net.Conn) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(DefaultControlClientTimeout)); err != nil {
		return
	}
	raw, err := bufio.NewReaderSize(io.LimitReader(conn, maxControlRequestBytes+1), maxControlRequestBytes+1).ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(raw) > maxControlRequestBytes) {
		return
	}
	if len(raw) == 0 || len(raw) > maxControlRequestBytes {
		return
	}
	response := s.HandleRequestLine(bytes.TrimRight(raw, "\n"), s.posixPeerSubject(conn))
	if err := conn.SetWriteDeadline(time.Now().Add(DefaultControlClientTimeout)); err != nil {
		return
	}
	_, _ = conn.Write(response)
}

// uid subject of a same user peer on a private, unsymlinked home, empty otherwise
func (s *GatewayControlServer) posixPeerSubject(conn net.Conn) string {
	info, err := os.Lstat(s.home)
	if err != nil {
		return ""
	}
	resolved, err := pypath.Resolve(s.home)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(s.home)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if abs != resolved || !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || HomeModeUnsafe(st, s.home) {
		return ""
	}
	uid, known, err := socketPeerUID(conn)
	if err != nil || !known || uid != os.Getuid() {
		return ""
	}
	return fmt.Sprintf("uid:%d", uid)
}
