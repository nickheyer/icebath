package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bounds of the per profile managed worker stderr sink
const (
	managedWorkerLogName = "managed-worker.log"
	maxWorkerLogBytes    = 1024 * 1024
	maxWorkerLineBytes   = 64 * 1024
	keptWorkerLineChars  = 4096
)

// Per file append locks of the worker log sinks
var (
	workerLogLocks   = map[string]*sync.Mutex{}
	workerLogLocksMu sync.Mutex
)

// Owning profile's sink, empty when the authority has no absolute home
func workerLogPath(profileID string) string {
	if !filepath.IsAbs(profileID) {
		return ""
	}
	return filepath.Join(profileID, "logs", managedWorkerLogName)
}

// Append lock of one sink path
func workerLogLock(path string) *sync.Mutex {
	workerLogLocksMu.Lock()
	defer workerLogLocksMu.Unlock()
	lock, ok := workerLogLocks[path]
	if !ok {
		lock = &sync.Mutex{}
		workerLogLocks[path] = lock
	}
	return lock
}

// Appends text under the file lock as a private file, rotating once past the size cap
func appendWorkerLog(ctx context.Context, path, text string) error {
	data := pystr.EncodeUTF8Replace(text)
	lock := workerLogLock(path)
	lock.Lock()
	defer lock.Unlock()
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	size := int64(0)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if size > 0 && size+int64(len(data)) > maxWorkerLogBytes {
		if err := os.Rename(path, filepath.Join(filepath.Dir(path), managedWorkerLogName+".1")); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if runtime.GOOS != "windows" {
		if err := file.Chmod(0o600); err != nil {
			return err
		}
	}
	if len(data) > maxWorkerLogBytes {
		data = data[:maxWorkerLogBytes]
	}
	_, err = file.Write(data)
	return err
}

// Line with this worker's exact secrets masked longest first, then the forced redaction patterns
func redactWorkerLine(line string, scrub func() []string) string {
	var secrets []string
	for _, value := range scrub() {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	sort.SliceStable(secrets, func(i, j int) bool { return pystr.Len(secrets[i]) > pystr.Len(secrets[j]) })
	for _, value := range secrets {
		line = strings.ReplaceAll(line, value, "***")
	}
	return tools.RedactSensitiveTextWith(nil, line, tools.RedactOptions{Force: true})
}

// Reads at most limit bytes up to and including the next newline, empty at end of stream
func readLineLimit(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for len(line) < limit {
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return line, nil
			}
			return line, err
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	return line, nil
}

// Drains the worker's stderr to end of stream into the redacted sink, never stopping on a write failure
func drainWorkerStderr(ctx context.Context, stream io.ReadCloser, path string, pid int, scrub func() []string) {
	defer stream.Close()
	reader := bufio.NewReaderSize(stream, maxWorkerLineBytes)
	skipping, warned := false, false
	for {
		chunk, err := readLineLimit(reader, maxWorkerLineBytes)
		if len(chunk) == 0 {
			return
		}
		whole := chunk[len(chunk)-1] == '\n'
		if skipping {
			skipping = !whole
			continue
		}
		skipping = !whole
		line := redactWorkerLine(pystr.DecodeUTF8Replace(chunk), scrub)
		entry := fmt.Sprintf("[pid %d] %s", pid, line)
		if !whole {
			entry = fmt.Sprintf("[pid %d] %s [truncated]\n", pid, pystr.Slice(line, 0, keptWorkerLineChars))
		}
		if werr := appendWorkerLog(ctx, path, entry); werr != nil && !warned {
			warned = true
			slog.Warn(fmt.Sprintf("managed worker log %s not written (%s); dropping its lines", path, pypath.OSErrorClass(werr)))
		}
		if err != nil {
			return
		}
	}
}
