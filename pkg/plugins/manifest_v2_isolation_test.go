package plugins

import (
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func isoHooks(m *Manager, name string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks[name])
}

type isoInline struct{ ids *[]uint64 }

func (p isoInline) Manifest() Manifest { return Manifest{Name: "inline"} }
func (p isoInline) Register(*Context) error {
	*p.ids = append(*p.ids, isoGoroutineID())
	return nil
}

func isoGoroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	id, _ := strconv.ParseUint(strings.Fields(strings.TrimPrefix(string(buf), "goroutine "))[0], 10, 64)
	return id
}

func TestLoadTimeoutZeroRunsRegisterInline(t *testing.T) {
	bundledMu.Lock()
	savedByKey, savedOrder := bundledByKey, bundledOrder
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
	bundledMu.Unlock()
	t.Cleanup(func() {
		bundledMu.Lock()
		bundledByKey, bundledOrder = savedByKey, savedOrder
		bundledMu.Unlock()
	})
	var ids []uint64
	RegisterBundled(isoInline{ids: &ids})
	for _, c := range []struct {
		timeout string
		inline  bool
	}{{"0", true}, {"5", false}} {
		h := t.TempDir()
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [inline]\n  load_timeout_seconds: "+c.timeout+"\n")
		t.Setenv("ICEBATH_HOME", h)
		ids = nil
		m := NewManager(h)
		m.DiscoverAndLoad(homeCtx(h), false)
		if !m.plugins["inline"].Enabled || len(ids) != 1 || (ids[0] == isoGoroutineID()) != c.inline {
			t.Fatalf("timeout %s plugin %+v ids %v caller %d", c.timeout, m.plugins["inline"], ids, isoGoroutineID())
		}
	}
}
