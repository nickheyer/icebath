package tuigateway

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
)

// Name, mtime ns, size and optional inode of one file a roster memo keys on
type rosterFilePart struct {
	name    string
	mtimeNS int64
	size    int64
	ino     int64
	withIno bool
}

// Signature a memo holds plus the fields it derived
type rosterCacheEntry struct {
	signature []rosterFilePart
	fields    map[string]any
}

// Path and reader key of the reader dependent session memo
type rosterReaderKey struct {
	path   string
	reader any
}

const rosterMaxEntries = 512

var (
	rosterMu                 sync.Mutex
	rosterSessionCache       = map[string]rosterCacheEntry{}
	rosterUIMetaCache        = map[string]rosterCacheEntry{}
	rosterReaderSessionCache = map[rosterReaderKey]rosterCacheEntry{}
	rosterStoreFiles         = []string{"state.db", "state.db-wal"}
)

// Stat parts of a file, nil when it cannot be stat'ed
func rosterFileParts(path string, withInode bool) *rosterFilePart {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	sig := config.FileSignature(fi)
	part := &rosterFilePart{name: filepath.Base(path), mtimeNS: sig[0], size: sig[1]}
	if withInode {
		part.ino, part.withIno = sig[2], true
	}
	return part
}

// Fields of compute reused while the signature holds, a nil signature computing every time
func rosterCached[K comparable](cache map[K]rosterCacheEntry, key K, signature []rosterFilePart,
	compute func() (map[string]any, error), copier func(map[string]any) map[string]any) (map[string]any, error) {
	if signature == nil {
		return compute()
	}
	rosterMu.Lock()
	hit, ok := cache[key]
	rosterMu.Unlock()
	if ok && slices.Equal(hit.signature, signature) {
		return copier(hit.fields), nil
	}
	fields, err := compute()
	if err != nil {
		return nil, err
	}
	rosterMu.Lock()
	if len(cache) >= rosterMaxEntries {
		clear(cache)
	}
	cache[key] = rosterCacheEntry{signature: signature, fields: copier(fields)}
	rosterMu.Unlock()
	return fields, nil
}

// Shallow copy of a fields mapping
func rosterShallowCopy(fields map[string]any) map[string]any { return maps.Clone(fields) }

// Deep copy of a fields mapping
func rosterDeepCopy(fields map[string]any) map[string]any {
	copied, _ := pycopy.DeepCopy(fields).(map[string]any)
	return copied
}

// Stat parts of each session store file, nil without a store or while an mtime is still racy
func RosterStoreSignature(profilePath string) []rosterFilePart {
	var parts []rosterFilePart
	for _, name := range rosterStoreFiles {
		part := rosterFileParts(filepath.Join(profilePath, name), false)
		if part == nil || (strings.HasSuffix(part.name, "-wal") && part.size == 0) {
			continue
		}
		parts = append(parts, *part)
	}
	for _, part := range parts {
		if config.MtimeIsRacy(part.mtimeNS) {
			return nil
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// Name, mtime ns, size and inode of the profile's profile.yaml, nil when absent
func RosterProfileYAMLSignature(profileDir string) []rosterFilePart {
	part := rosterFileParts(filepath.Join(profileDir, "profile.yaml"), true)
	if part == nil {
		return nil
	}
	return []rosterFilePart{*part}
}

// Session fields of compute reused while the profile's session store has not moved
func CachedSessionFields(profilePath string, compute func() (map[string]any, error)) (map[string]any, error) {
	return rosterCached(rosterSessionCache, profilePath, RosterStoreSignature(profilePath), compute, rosterShallowCopy)
}

// Session fields that also depend on the comparable reader key, reused while the store has not moved
func CachedReaderSessionFields(profilePath string, reader any, compute func() (map[string]any, error)) (map[string]any, error) {
	return rosterCached(rosterReaderSessionCache, rosterReaderKey{profilePath, reader}, RosterStoreSignature(profilePath),
		compute, rosterShallowCopy)
}

// ui_meta fields of compute deep copied and reused while the profile.yaml has not changed
func CachedUIMetaFields(profileDir string, compute func() (map[string]any, error)) (map[string]any, error) {
	return rosterCached(rosterUIMetaCache, profileDir, RosterProfileYAMLSignature(profileDir), compute, rosterDeepCopy)
}

// Drops one profile's memos, or every memo when the path is nil
func InvalidateRosterCache(profilePath *string) {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	for _, cache := range []map[string]rosterCacheEntry{rosterSessionCache, rosterUIMetaCache} {
		if profilePath == nil {
			clear(cache)
		} else {
			delete(cache, *profilePath)
		}
	}
	for key := range rosterReaderSessionCache {
		if profilePath == nil || key.path == *profilePath {
			delete(rosterReaderSessionCache, key)
		}
	}
}
