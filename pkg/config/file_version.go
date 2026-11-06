package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Age under which a stat signature cannot rule out a same tick write after the stat
const RacyMtimeWindowNS int64 = 2_000_000_000

// Change detection key of a stat result as mtime ns, size, inode and ctime ns
func FileSignature(fi os.FileInfo) [4]int64 {
	ino, ctime := statInoCtime(fi)
	return [4]int64{fi.ModTime().UnixNano(), fi.Size(), ino, ctime}
}

// Signature seam the same tick tests freeze to one coarse clock tick
var fileSignature = FileSignature

// Racy check seam the settle tests replace
var mtimeIsRacy = MtimeIsRacy

// Reader of the cache key content digest, the seam the settle tests count opens through
var readDigestBytes = os.ReadFile

// Reports whether a timestamp is too recent to rule out a same tick write after the stat
func MtimeIsRacy(mtimeNS int64) bool {
	return time.Now().UnixNano()-mtimeNS < RacyMtimeWindowNS
}

// Short hex digest of data for cache keys
func ContentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// Reports whether path is a config.yaml read inside a worker whose config is a frozen snapshot
func frozenConfigWorker(path string) bool {
	return filepath.Base(path) == ConfigFilename && currentSafeWorkerPolicy() != nil
}

// Digest of the file bytes, or the bracketed error name when the file stats but cannot be read
func fileDigest(path string) string {
	data, err := readDigestBytes(path)
	if err != nil {
		return "<" + pyOSErrorName(err) + ">"
	}
	return ContentDigest(data)
}

// Python OSError subclass name of a file error
func pyOSErrorName(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case errors.As(err, &errno) && errno == syscall.EISDIR:
		return "IsADirectoryError"
	case errors.As(err, &errno) && errno == syscall.ENOTDIR:
		return "NotADirectoryError"
	}
	return "OSError"
}

// Empty once the signature has settled, a digest of the file bytes while the later of mtime and ctime is racy
func RacyContentDigest(path string, sig [4]int64) string {
	if !mtimeIsRacy(max(sig[0], sig[3])) || frozenConfigWorker(path) {
		return ""
	}
	return fileDigest(path)
}

// Signature plus content digest of a file, digesting on every call when alwaysDigest is set
func FileCacheVersion(path string, alwaysDigest bool) ([]any, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return fileCacheVersionOf(path, fi, alwaysDigest), nil
}

// Cache version of a file from a stat result already taken
func fileCacheVersionOf(path string, fi os.FileInfo, alwaysDigest bool) []any {
	sig := fileSignature(fi)
	digest := ""
	if alwaysDigest && !frozenConfigWorker(path) {
		digest = fileDigest(path)
	} else {
		digest = RacyContentDigest(path, sig)
	}
	return []any{sig[0], sig[1], sig[2], sig[3], digest}
}

// Parsed YAML root of bytes, nil for an empty document, mappings string keyed
func FastSafeLoad(data []byte) (any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &node); err != nil {
		return nil, err
	}
	var doc any
	if len(node.Content) > 0 {
		resolveYAML11(&node, false)
		if err := yaml11TimestampError(&node); err != nil {
			return nil, err
		}
		if err := node.Decode(&doc); err != nil {
			return nil, err
		}
	}
	doc = normalizeMaps(doc)
	recordNodeOrder(&node, doc)
	return doc, nil
}

type yamlFileCacheEntry struct {
	version []any
	data    any
}

var (
	yamlFileCacheMu sync.Mutex
	yamlFileCache   = map[string]yamlFileCacheEntry{}
)

// Parse of a YAML file shared until its cache version changes, the result must never be mutated
func LoadYAMLFileReadonly(path string) (any, error) {
	version, err := FileCacheVersion(path, false)
	if err != nil {
		return nil, err
	}
	yamlFileCacheMu.Lock()
	cached, ok := yamlFileCache[path]
	yamlFileCacheMu.Unlock()
	if ok && reflect.DeepEqual(cached.version, version) {
		return cached.data, nil
	}
	raw, err := readYAMLFile(path)
	if err != nil {
		return nil, err
	}
	data, err := FastSafeLoad(raw)
	if err != nil {
		return nil, err
	}
	yamlFileCacheMu.Lock()
	yamlFileCache[path] = yamlFileCacheEntry{version: version, data: data}
	yamlFileCacheMu.Unlock()
	return data, nil
}
