package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var (
	DefaultNofileSoftLimit = runtimeDefault("nofile_soft_limit")
	DefaultMallocArenaMax  = runtimeDefault("malloc_arena_max")
)

// Integer default of a runtime config key
func runtimeDefault(key string) int64 {
	v, _ := anyutil.Int(config.Defaults().Map("runtime")[key])
	return v
}

// Config loader of the runtime knobs when the caller passes none
var resourceLimitsLoadConfig = func() (map[string]any, error) {
	ctx := context.Background()
	return config.LoadReadonly(ctx, home.Dir(ctx), nil)
}

// Positive integer runtime key, the default when missing, false when disabled, malformed or unloadable
func runtimeInt(cfg map[string]any, key string, def int64) (int64, bool) {
	if cfg == nil {
		loaded, err := resourceLimitsLoadConfig()
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Could not load config for runtime.%s: %v", key, err))
			return 0, false
		}
		cfg = loaded
	}
	runtimeRaw, ok := cfg["runtime"]
	if !ok {
		return def, true
	}
	runtime, isMap := runtimeRaw.(map[string]any)
	if t, isTree := runtimeRaw.(config.Tree); isTree {
		runtime, isMap = t, true
	}
	if !isMap {
		return 0, false
	}
	raw, ok := runtime[key]
	if !ok {
		return def, true
	}
	n, isInt := anyutil.Int(raw)
	if !isInt || n <= 0 {
		return 0, false
	}
	return n, true
}

// Configured runtime.nofile_soft_limit shared by the in process floor and service generators, false when disabled
func ConfiguredNofileSoftLimit(cfg map[string]any) (int64, bool) {
	return runtimeInt(cfg, "nofile_soft_limit", DefaultNofileSoftLimit)
}

// Raises the open file soft limit toward the configured target within the hard limit, true when changed
func ApplyNofileSoftLimit(cfg map[string]any) bool {
	if !nofileSupported {
		return false
	}
	target, ok := ConfiguredNofileSoftLimit(cfg)
	if !ok {
		return false
	}
	soft, hard, err := getNofile()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not raise RLIMIT_NOFILE soft limit: %v", err))
		return false
	}
	if target <= 0 || soft == rlimInfinity || soft >= uint64(target) {
		return false
	}
	newSoft := uint64(target)
	if hard != rlimInfinity && hard < newSoft {
		newSoft = hard
	}
	if newSoft <= soft {
		return false
	}
	if err := setNofile(newSoft, hard); err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not raise RLIMIT_NOFILE soft limit: %v", err))
		return false
	}
	return true
}

const (
	mArenaMax = -8
	cIntMax   = 1<<31 - 1
)

// Arena cap an environment asks glibc for, the exact arena_max tunable first, then MALLOC_ARENA_MAX
func arenaMaxSetting(lookup func(string) (string, bool)) (string, bool) {
	tunables, _ := lookup("GLIBC_TUNABLES")
	for _, item := range strings.Split(tunables, ":") {
		name, value, sep := strings.Cut(item, "=")
		if sep && name == "glibc.malloc.arena_max" {
			return value, true
		}
	}
	return lookup("MALLOC_ARENA_MAX")
}

// Exec time environment block of this process
var procSelfEnviron = func() ([]byte, error) { return os.ReadFile("/proc/self/environ") }

// Environment this process was exec'd with, nil when the exec time block is unreadable
func launchEnviron() map[string]string {
	raw, err := procSelfEnviron()
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		if name, value, sep := strings.Cut(string(entry), "="); sep {
			env[name] = value
		}
	}
	return env
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// Positive C int of a decimal string, false when unparseable or outside 1 to INT_MAX
func positiveCInt(value string) (int32, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n <= 0 || n > cIntMax {
		return 0, false
	}
	return int32(n), true
}

// Caps glibc malloc arenas for this process through mallopt, true only when the cap was set
func ApplyMallocArenaMax(cfg map[string]any) bool {
	libc := GlibcMalloc()
	if libc == nil {
		return false
	}
	current, hasCurrent := arenaMaxSetting(os.LookupEnv)
	launched, hasLaunched := current, hasCurrent
	if launchEnv := launchEnviron(); launchEnv != nil {
		launched, hasLaunched = arenaMaxSetting(lookupIn(launchEnv))
	}
	if hasLaunched && (!hasCurrent || current == launched) {
		return false
	}
	var target int32
	if hasCurrent {
		n, ok := positiveCInt(current)
		if !ok {
			return false
		}
		target = n
	} else {
		n, ok := runtimeInt(cfg, "malloc_arena_max", DefaultMallocArenaMax)
		if !ok || n > cIntMax {
			return false
		}
		target = int32(n)
	}
	return libc.Mallopt(mArenaMax, target) != 0
}

// Reports whether argv launches a foreground gateway, the one long lived server
func ServerProcessArgv(argv []string) (bool, error) {
	args, err := pm.CommandArgv(argv)
	if err != nil || len(args) == 0 {
		return false, err
	}
	return args[0] == "gateway" && (len(args) == 1 || args[1] == "run" || strings.HasPrefix(args[1], "-")), nil
}
