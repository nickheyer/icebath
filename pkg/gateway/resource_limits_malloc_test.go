//go:build linux && !faketime

package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

type mallocArenaSpec struct {
	Apply  bool              `json:"apply"`
	Cap    int               `json:"cap"`
	Setenv map[string]string `json:"setenv"`
}

type mallocArenaResult struct {
	Applied  bool `json:"applied"`
	Heaps    int  `json:"heaps"`
	Baseline int  `json:"baseline"`
}

func init() { mallocArenaChild = runMallocArenaChild }

func libcSym(name string) uintptr {
	sym, err := purego.Dlsym(purego.RTLD_DEFAULT, name)
	if err != nil {
		panic(err)
	}
	return sym
}

// Arena count glibc reports through malloc_info, one heap element per arena including the main one
func mallocInfoHeaps() int {
	buf, size := new(*byte), new(uintptr)
	var pinner runtime.Pinner
	pinner.Pin(buf)
	pinner.Pin(size)
	defer pinner.Unpin()
	stream, _, _ := purego.SyscallN(libcSym("open_memstream"), uintptr(unsafe.Pointer(buf)), uintptr(unsafe.Pointer(size)))
	purego.SyscallN(libcSym("malloc_info"), 0, stream)
	purego.SyscallN(libcSym("fclose"), stream)
	xml := string(unsafe.Slice(*buf, *size))
	purego.SyscallN(libcSym("free"), uintptr(unsafe.Pointer(*buf)))
	return strings.Count(xml, "<heap nr=")
}

// Arena count while 24 OS threads each hold a live malloc chunk, threads unlocked again so none exits
func arenasUnderLoad() int {
	mallocFn, freeFn := libcSym("malloc"), libcSym("free")
	var started sync.WaitGroup
	done := make(chan struct{})
	var finished sync.WaitGroup
	for range 24 {
		started.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ptr, _, _ := purego.SyscallN(mallocFn, 4096)
			started.Done()
			<-done
			purego.SyscallN(freeFn, ptr)
		}()
	}
	started.Wait()
	heaps := mallocInfoHeaps()
	close(done)
	finished.Wait()
	return heaps
}

func runMallocArenaChild(raw string) int {
	var spec mallocArenaSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		os.Stderr.WriteString(err.Error())
		return 2
	}
	os.Unsetenv(mallocArenaChildEnv)
	for k, v := range spec.Setenv {
		os.Setenv(k, v)
	}
	result := mallocArenaResult{Baseline: mallocInfoHeaps()}
	if spec.Apply {
		result.Applied = ApplyMallocArenaMax(map[string]any{"runtime": map[string]any{"malloc_arena_max": spec.Cap}})
	}
	result.Heaps = arenasUnderLoad()
	out, _ := json.Marshal(result)
	os.Stdout.Write(append(out, '\n'))
	return 0
}

// Reruns the test binary on one P so the cap lands before glibc latches its arena limit
func runMallocArena(t *testing.T, spec mallocArenaSpec, env ...string) mallocArenaResult {
	t.Helper()
	if GlibcMalloc() == nil {
		t.Skip("arena caps are glibc malloc behaviour (not musl)")
	}
	raw, _ := json.Marshal(spec)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); name != "MALLOC_ARENA_MAX" && name != "GLIBC_TUNABLES" && !strings.HasPrefix(name, "ICEBATH_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(append(cmd.Env, mallocArenaChildEnv+"="+string(raw), "ICEBATH_HOME="+t.TempDir(), "GOMAXPROCS=1"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child failed %v: %s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var result mallocArenaResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("child output %q: %v", out, err)
	}
	return result
}

func TestExportedMallocArenaMaxWinsOverConfig(t *testing.T) {
	result := runMallocArena(t, mallocArenaSpec{Apply: true, Cap: 2}, "MALLOC_ARENA_MAX=16")
	if result.Applied || result.Heaps <= 2 {
		t.Fatalf("glibc must honour the operator's 16, not the config's 2: %+v", result)
	}
}

func TestConfiguredCapBoundsArenasCreatedAfterIt(t *testing.T) {
	result := runMallocArena(t, mallocArenaSpec{Apply: true, Cap: 2})
	if !result.Applied || result.Baseline > 8 || result.Heaps > max(2, result.Baseline) {
		t.Fatalf("cap of 2 did not hold: %+v", result)
	}
}

func TestMallocArenaMaxLoadedAfterExecIsApplied(t *testing.T) {
	result := runMallocArena(t, mallocArenaSpec{Apply: true, Cap: 0, Setenv: map[string]string{"MALLOC_ARENA_MAX": "2"}})
	if !result.Applied || result.Baseline > 8 || result.Heaps > max(2, result.Baseline) {
		t.Fatalf("profile env cap of 2 did not hold: %+v", result)
	}
}

func TestUncappedProcessKeepsGlibcDefaultArenas(t *testing.T) {
	result := runMallocArena(t, mallocArenaSpec{})
	if result.Heaps <= 4 || result.Heaps <= result.Baseline {
		t.Fatalf("probe saw no per thread arenas: %+v", result)
	}
}
