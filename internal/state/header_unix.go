//go:build unix

package state

import (
	"sync"

	"golang.org/x/sys/unix"
)

// Descriptors the header probe reads live databases through, never closed so no POSIX lock of this process is cancelled
type headerProbes struct {
	mu  sync.Mutex
	fds map[string]probeFD
}

// One cached probe descriptor and the inode it was opened on
type probeFD struct {
	fd int
	id fileIdentity
}

// Empty probe cache
func newHeaderProbes() headerProbes { return headerProbes{fds: map[string]probeFD{}} }

// Up to n bytes of path at off through a cached descriptor, dropped unclosed when the path names another inode
func (e *Engine) preadDB(path string, off int64, n int) ([]byte, bool) {
	now, err := identityOf(path)
	if err != nil {
		return nil, false
	}
	p := &e.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	cached, ok := p.fds[path]
	if ok && cached.id != now {
		delete(p.fds, path)
		ok = false
	}
	if !ok {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, false
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			return nil, false
		}
		cached = probeFD{fd: fd, id: fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}}
		p.fds[path] = cached
	}
	buf := make([]byte, n)
	got, err := unix.Pread(cached.fd, buf, off)
	if err != nil {
		return nil, false
	}
	return buf[:got], true
}
