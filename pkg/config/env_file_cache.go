package config

import (
	"bytes"
	"container/list"
	"maps"
	"os"
	"sync"
)

const envFileCacheMax = 64

type envFileMemo struct {
	path   string
	raw    []byte
	values map[string]string
}

var (
	envFileCacheMu    sync.Mutex
	envFileCacheOrder = list.New()
	envFileCacheIndex = map[string]*list.Element{}
	parseEnvText      = ParseEnv
)

// SwapParseEnvText installs the dotenv parser the memo calls and returns the one it replaced
func SwapParseEnvText(fn func(string) map[string]string) func(string) map[string]string {
	envFileCacheMu.Lock()
	defer envFileCacheMu.Unlock()
	previous := parseEnvText
	parseEnvText = fn
	return previous
}

// InvalidateEnvFileCache drops one path from the dotenv memo, every path when the path is empty
func InvalidateEnvFileCache(path string) {
	envFileCacheMu.Lock()
	defer envFileCacheMu.Unlock()
	if path == "" {
		envFileCacheOrder.Init()
		envFileCacheIndex = map[string]*list.Element{}
		return
	}
	dropEnvFileMemo(path)
}

func dropEnvFileMemo(path string) {
	if el, ok := envFileCacheIndex[path]; ok {
		envFileCacheOrder.Remove(el)
		delete(envFileCacheIndex, path)
	}
}

// Assignments of a dotenv file memoised on its exact bytes, a fresh map per call, empty when unreadable
func LoadEnvFile(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		envFileCacheMu.Lock()
		dropEnvFileMemo(path)
		envFileCacheMu.Unlock()
		return map[string]string{}
	}
	envFileCacheMu.Lock()
	if el, ok := envFileCacheIndex[path]; ok {
		if memo := el.Value.(*envFileMemo); bytes.Equal(memo.raw, raw) {
			envFileCacheOrder.MoveToBack(el)
			envFileCacheMu.Unlock()
			return maps.Clone(memo.values)
		}
	}
	parse := parseEnvText
	envFileCacheMu.Unlock()
	values := parse(DecodeEnvBytes(raw))
	envFileCacheMu.Lock()
	defer envFileCacheMu.Unlock()
	memo := &envFileMemo{path: path, raw: raw, values: maps.Clone(values)}
	if el, ok := envFileCacheIndex[path]; ok {
		el.Value = memo
		envFileCacheOrder.MoveToBack(el)
	} else {
		envFileCacheIndex[path] = envFileCacheOrder.PushBack(memo)
	}
	for envFileCacheOrder.Len() > envFileCacheMax {
		oldest := envFileCacheOrder.Front()
		envFileCacheOrder.Remove(oldest)
		delete(envFileCacheIndex, oldest.Value.(*envFileMemo).path)
	}
	return values
}
