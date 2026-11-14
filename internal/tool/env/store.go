package env

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	sandboxDirMaxLen  = 128
	sandboxDirHashLen = 12
	jsonStoreLockWait = 10 * time.Second
)

var sandboxDirUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func SanitizeTaskID(taskID string) string {
	if taskID == "" {
		return "default"
	}
	cleaned := sandboxDirUnsafe.ReplaceAllString(taskID, "_")
	if cleaned == taskID && len([]rune(taskID)) <= sandboxDirMaxLen && taskID != "." && taskID != ".." &&
		!strings.HasSuffix(taskID, ".") && !strings.HasSuffix(taskID, " ") {
		return taskID
	}
	sum := sha256.Sum256([]byte(taskID))
	digest := hex.EncodeToString(sum[:])[:sandboxDirHashLen]
	stem := strings.Trim(firstRunes(cleaned, sandboxDirMaxLen-sandboxDirHashLen-1), "._")
	if stem == "" {
		stem = "task"
	}
	return stem + "-" + digest
}

func SandboxDir(ctx context.Context, homes *home.Resolver) (string, error) {
	dir := os.Getenv("TERMINAL_SANDBOX_DIR")
	if dir == "" {
		homeDir, err := homes.Home(ctx)
		if err != nil {
			return "", err
		}
		dir = home.Sandboxes.In(homeDir)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return dir, nil
}

func LoadJSONStore(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return map[string]any{}
	}
	return data
}

func SaveJSONStore(ctx context.Context, path string, data map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return fileutil.WriteJSON(ctx, path, data, fileutil.JSONStyle{Indent: 2})
}

func UpdateJSONStore(ctx context.Context, path string, update func(data map[string]any)) error {
	lockPath := filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock")
	lockCtx, cancel := context.WithTimeout(ctx, jsonStoreLockWait)
	defer cancel()
	lock, err := fileutil.OpenLock(lockCtx, lockPath,
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for the snapshot store lock (%s)", path)))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("Updating "+path+" without its lock", "err", err)
	} else {
		defer func() { _ = lock.Release() }()
	}
	data := LoadJSONStore(path)
	before := make(map[string]any, len(data))
	for k, v := range data {
		before[k] = v
	}
	update(data)
	if reflect.DeepEqual(data, before) {
		return nil
	}
	return SaveJSONStore(ctx, path, data)
}

type FileSignature struct {
	ModTime int64
	Size    int64
	Inode   uint64
	Ctime   int64
}

func FileSignatureOf(path string) (FileSignature, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return FileSignature{}, false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return FileSignature{}, false
	}
	id, err := fileutil.LstatID(real)
	if err != nil {
		return FileSignature{}, false
	}
	return FileSignature{ModTime: info.ModTime().UnixNano(), Size: info.Size(), Inode: id.Inode, Ctime: changeTime(info)}, true
}
