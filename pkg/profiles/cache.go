package profiles

import (
	"os"
	"sync"
)

// Bound on cached derived values before the whole cache is dropped
const profileFileCacheMax = 512

// Stat signature of a profile file, mtime in nanoseconds, size and inode
type fileSignature struct {
	mtimeNs int64
	size    int64
	inode   uint64
}

type cacheKey struct{ path, kind string }

type cacheEntry struct {
	signature any
	value     any
}

var (
	profileFileCacheMu sync.Mutex
	profileFileCache   = map[cacheKey]cacheEntry{}
)

// Signature of the file at path, nil when the file is absent
func profileFileSignature(path string) any {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fileSignature{mtimeNs: st.ModTime().UnixNano(), size: st.Size(), inode: fileInode(path, st)}
}

// Value of compute reused while the signature of path is unchanged, never caching a missing file
func cachedProfileRead[T any](path, kind string, compute func() T, signatureOf func(string) any) T {
	if signatureOf == nil {
		signatureOf = profileFileSignature
	}
	signature := signatureOf(path)
	if signature == nil {
		return compute()
	}
	key := cacheKey{path, kind}
	profileFileCacheMu.Lock()
	cached, ok := profileFileCache[key]
	profileFileCacheMu.Unlock()
	if ok && signaturesEqual(cached.signature, signature) {
		if v, ok := cached.value.(T); ok {
			return v
		}
	}
	value := compute()
	profileFileCacheMu.Lock()
	if len(profileFileCache) >= profileFileCacheMax {
		clear(profileFileCache)
	}
	profileFileCache[key] = cacheEntry{signature: signature, value: value}
	profileFileCacheMu.Unlock()
	return value
}

// Reports whether two signatures compare equal like Python tuples and lists do
func signaturesEqual(a, b any) bool {
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok || bok {
		if !aok || !bok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !signaturesEqual(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

// Drops every cached derived value
func ResetFileCache() {
	profileFileCacheMu.Lock()
	clear(profileFileCache)
	profileFileCacheMu.Unlock()
}
