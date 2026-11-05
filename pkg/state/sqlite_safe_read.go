package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LiveConnectionError reports a raw file operation attempted on a database with live connections
type LiveConnectionError struct {
	Path string
	Main string
	What string
}

func (e *LiveConnectionError) Error() string {
	subject := "it"
	if e.Main != e.Path {
		subject = "its main database " + e.Main
	}
	return fmt.Sprintf("Refusing to %s %s: a connection to %s is still open in this process, and raw file access would cancel that connection's POSIX advisory locks. Close all database handles (stop the gateway/dashboard) and retry.", e.What, e.Path, subject)
}

var liveMu sync.Mutex
var liveConnections = map[string]int{}

// Canonical filesystem key of a database path
func liveKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return abs
		}
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func trackKey(key string, delta int) {
	remaining := liveConnections[key] + delta
	if remaining > 0 {
		liveConnections[key] = remaining
	} else {
		delete(liveConnections, key)
	}
}

// TrackConnection records that this process holds one more connection to the path
func TrackConnection(path string) {
	liveMu.Lock()
	defer liveMu.Unlock()
	trackKey(liveKey(path), 1)
}

// UntrackConnection records that one connection to the path was closed
func UntrackConnection(path string) {
	liveMu.Lock()
	defer liveMu.Unlock()
	trackKey(liveKey(path), -1)
}

// The tracked main database key that makes this key live, or empty
func liveMainKey(key string) string {
	if _, ok := liveConnections[key]; ok {
		return key
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if strings.HasSuffix(key, suffix) {
			main := strings.TrimSuffix(key, suffix)
			if _, ok := liveConnections[main]; ok {
				return main
			}
		}
	}
	return ""
}

// HasLiveConnection reports whether this process holds a connection to the path or its main database
func HasLiveConnection(path string) bool {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveMainKey(liveKey(path)) != ""
}

// OfflineFileAccess runs fn under the connection lifecycle lock, refusing while the database is live
func OfflineFileAccess(path, what string, fn func()) error {
	liveMu.Lock()
	defer liveMu.Unlock()
	key := liveKey(path)
	if main := liveMainKey(key); main != "" {
		if what == "" {
			what = "read"
		}
		return &LiveConnectionError{Path: path, Main: main, What: what}
	}
	fn()
	return nil
}

// ReadHeaderBytesPreopen reads the leading bytes of a database file only when no connection is live
func ReadHeaderBytesPreopen(path string, length int, force bool) ([]byte, bool) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if !force && liveMainKey(liveKey(path)) != "" {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	buf := make([]byte, length)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, false
	}
	return buf[:n], true
}
