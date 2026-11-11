package selfupdate

import (
	"math/big"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	minWebBuildHeapMB  = 1024
	maxWebBuildHeapMB  = 4096
	maxWebBuildThreads = 8
)

// Container memory limit in MB from cgroup v2 then v1, false when unconstrained
var webCgroupMemoryLimitMB = func() (int64, bool) {
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			continue
		}
		text = pystr.Strip(text)
		if text == "max" {
			return 0, false
		}
		value, ok := pyint.Parse(text)
		if !ok {
			continue
		}
		if value.Sign() <= 0 || value.Cmp(new(big.Int).Lsh(big.NewInt(1), 50)) >= 0 {
			return 0, false
		}
		return value.Int64() / (1024 * 1024), true
	}
	return 0, false
}

// Cores this process may run on, at least one
var webAvailableCores = func() int { return max(1, runtime.NumCPU()) }

// Half the cores clamped to one through the thread ceiling
func boundedWebThreads() int {
	half := webAvailableCores() / 2
	if half == 0 {
		half = 1
	}
	return max(1, min(maxWebBuildThreads, half))
}

// Heap ceiling from three quarters of the cgroup limit clamped to the floor and ceiling
func boundedWebHeapMB() int64 {
	limit, ok := webCgroupMemoryLimitMB()
	if !ok || limit == 0 {
		return maxWebBuildHeapMB
	}
	return max(minWebBuildHeapMB, min(maxWebBuildHeapMB, int64(float64(limit)*0.75)))
}

// Integer an explicit override names, false when it is not a Python int literal
func webBuildOverride(env map[string]string, name string) (*big.Int, bool, bool) {
	explicit := pystr.Strip(env[name])
	if explicit == "" {
		return nil, false, true
	}
	value, ok := pyint.Parse(explicit)
	return value, true, ok
}

// Copy of env bounding the web build heap and bundler threads, user settings always kept
func WebBuildLimits(env map[string]string) map[string]string {
	if env == nil {
		env = sourceEnviron()
	}
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	light := LightBuildRequested(out)
	tokens := strings.Fields(out["NODE_OPTIONS"])
	capped := false
	for _, t := range tokens {
		if strings.HasPrefix(t, "--max-old-space-size=") {
			capped = true
		}
	}
	if !capped {
		fallback := big.NewInt(boundedWebHeapMB())
		if light {
			fallback = big.NewInt(minWebBuildHeapMB)
		}
		heap := fallback
		if value, set, ok := webBuildOverride(out, "ICEBATH_WEB_BUILD_MAX_OLD_SPACE_SIZE"); set && ok {
			heap = value
		}
		if heap.Cmp(big.NewInt(minWebBuildHeapMB)) < 0 {
			heap = big.NewInt(minWebBuildHeapMB)
		}
		tokens = append(tokens, "--max-old-space-size="+heap.String())
		kept := tokens[:0]
		for _, t := range tokens {
			if t != "" {
				kept = append(kept, t)
			}
		}
		out["NODE_OPTIONS"] = pystr.Strip(strings.Join(kept, " "))
	}
	if out["RAYON_NUM_THREADS"] == "" {
		fallback := big.NewInt(int64(boundedWebThreads()))
		if light {
			fallback = big.NewInt(1)
		}
		threads := fallback
		if value, set, ok := webBuildOverride(out, "ICEBATH_WEB_BUILD_THREADS"); set && ok {
			threads = value
		}
		if threads.Sign() < 1 {
			threads = big.NewInt(1)
		}
		out["RAYON_NUM_THREADS"] = threads.String()
	}
	return out
}

// Applies the web build limits into env in place and returns it
func ApplyWebBuildLimits(env map[string]string) map[string]string {
	for k, v := range WebBuildLimits(env) {
		env[k] = v
	}
	return env
}

// True when ICEBATH_WEB_BUILD_LIGHT asks for the minimal footprint build
func LightBuildRequested(env map[string]string) bool {
	if env == nil {
		env = sourceEnviron()
	}
	switch strings.ToLower(pystr.Strip(env["ICEBATH_WEB_BUILD_LIGHT"])) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
