package gateway

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	autoBudgetFraction          = 0.65
	autoBudgetFloorMB           = 512
	defaultMaxEvictionsPerPass  = 16
	defaultProtectRecent        = 8
	acpBytesPerMB               = 1024 * 1024
	cgroupFiniteLimitUpperBound = int64(1) << 62
)

var memoryOffWords = map[string]bool{"": true, "off": true, "none": true, "false": true, "disabled": true}

// Operator agent cache bounds, unset size and ttl keeping the runner defaults and a nil budget turning pressure off
type AgentCacheBounds struct {
	MaxSize             *int
	IdleTTLSecs         *float64
	MemoryHighMB        *int
	MaxEvictionsPerPass int
	ProtectRecent       int
}

var (
	acpReadText         = cgroupFileText
	acpCgroupLimitBytes = cgroupLimitBytes
	acpTotalMemoryBytes = totalMemoryBytes
	acpMemorySnapshot   = func() memory.MemorySnapshot { return memory.CollectMemorySnapshot(nil) }
	acpProcessRSS       = func() (int64, error) { return proc.RSS(os.Getpid()) }
	// Anonymous memory in MB the pressure sweep compares against its budget, nil when unreadable
	ReadAnonRSSMB = readAnonRSSMB
)

// Text of a cgroup file with a leading byte order mark dropped
func cgroupFileText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), nil
}

// Python int that is not a bool
func isPlainInt(v any) bool {
	switch v.(type) {
	case int, int64, int32:
		return true
	}
	return false
}

// Positive integer value of v with bools and unparseable values rejected
func positiveInt(v any) *int {
	if v == nil {
		return nil
	}
	var n int
	switch x := v.(type) {
	case bool:
		return nil
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		n = int(x)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return nil
		}
		n = parsed
	default:
		return nil
	}
	if n <= 0 {
		return nil
	}
	return &n
}

// Positive float value of v with bools and unparseable values rejected
func positiveFloat(v any) *float64 {
	if v == nil {
		return nil
	}
	var f float64
	switch x := v.(type) {
	case bool:
		return nil
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case float64:
		f = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if !(f > 0) {
		return nil
	}
	return &f
}

// Memory limit file value when it is a real cap, nil for unreadable, empty, max or the near two to the sixty three sentinel
func finiteLimit(path string) *int64 {
	text, err := acpReadText(path)
	if err != nil {
		return nil
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || limit <= 0 || limit >= cgroupFiniteLimitUpperBound {
		return nil
	}
	return &limit
}

// Memory limit this process runs under, cgroup v2 high then max in its own cgroup then root, then v1
func cgroupLimitBytes() *int64 {
	if runtime.GOOS != "linux" {
		return nil
	}
	own, err := cgroupOwnPath()
	if err != nil {
		own = ""
	}
	var roots []string
	if own != "" && own != "/" {
		roots = append(roots, "/sys/fs/cgroup"+own)
	}
	roots = append(roots, "/sys/fs/cgroup")
	var candidates []string
	for _, r := range roots {
		for _, f := range []string{"high", "max"} {
			candidates = append(candidates, r+"/memory."+f)
		}
	}
	candidates = append(candidates, "/sys/fs/cgroup/memory/memory.limit_in_bytes")
	for _, candidate := range candidates {
		if limit := finiteLimit(candidate); limit != nil {
			return limit
		}
	}
	return nil
}

// Physical memory of the host, nil when the platform probe fails
func totalMemoryBytes() *int64 {
	total, ok := tools.PhysicalMemoryBytes()
	if !ok {
		return nil
	}
	return &total
}

// Absolute MB budget, auto from the cgroup limit or total memory, a positive number literal, off words disabling the pass
func ResolveMemoryHighMB(setting any) *int {
	switch s := setting.(type) {
	case string:
		normalized := strings.ToLower(strings.TrimSpace(s))
		if normalized != "auto" {
			if memoryOffWords[normalized] {
				return nil
			}
			return positiveInt(normalized)
		}
	case bool:
		if !s {
			return nil
		}
	default:
		return positiveInt(setting)
	}
	limit := acpCgroupLimitBytes()
	if limit == nil || *limit == 0 {
		limit = acpTotalMemoryBytes()
	}
	if limit == nil || *limit == 0 {
		return nil
	}
	budget := int(float64(*limit) * autoBudgetFraction / acpBytesPerMB)
	if budget < autoBudgetFloorMB {
		return nil
	}
	return &budget
}

// Bounds read from the raw agent agent_cache config section so an unset key stays distinct from an explicit one
func ResolveAgentCacheBounds(cfg map[string]any) AgentCacheBounds {
	var section map[string]any
	if cfg != nil {
		agentSection, _ := cfg["agent"].(map[string]any)
		if !pyrepr.Truthy(cfg["agent"]) {
			agentSection = map[string]any{}
		}
		if agentSection != nil {
			section, _ = agentSection["agent_cache"].(map[string]any)
		}
	}
	if section == nil {
		section = map[string]any{}
	}
	protectRecent := section["protect_recent"]
	protectParsed := positiveInt(protectRecent)
	if protectParsed == nil && isPlainInt(protectRecent) && pyrepr.Equal(protectRecent, 0) {
		zero := 0
		protectParsed = &zero
	}
	memorySetting, ok := section["memory_high_mb"]
	if !ok {
		memorySetting = "auto"
	}
	bounds := AgentCacheBounds{
		MaxSize:             positiveInt(section["max_size"]),
		IdleTTLSecs:         positiveFloat(section["idle_ttl_secs"]),
		MemoryHighMB:        ResolveMemoryHighMB(memorySetting),
		MaxEvictionsPerPass: defaultMaxEvictionsPerPass,
		ProtectRecent:       defaultProtectRecent,
	}
	if n := positiveInt(section["max_evictions_per_pass"]); n != nil {
		bounds.MaxEvictionsPerPass = *n
	}
	if protectParsed != nil {
		bounds.ProtectRecent = *protectParsed
	}
	return bounds
}

// Anonymous memory charged to this process's own capped cgroup v2, nil when uncapped or unreadable
func cgroupAnonBytes() *int64 {
	if runtime.GOOS != "linux" {
		return nil
	}
	own, err := cgroupOwnPath()
	if err != nil || own == "" || own == "/" {
		return nil
	}
	root := "/sys/fs/cgroup" + own
	capped := finiteLimit(root+"/memory.high") != nil || finiteLimit(root+"/memory.max") != nil
	text := ""
	if capped {
		text, err = acpReadText(root + "/memory.stat")
		if err != nil {
			return nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		if key == "anon" && value != "" && strings.Trim(value, "0123456789") == "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err == nil {
				return &n
			}
		}
	}
	return nil
}

// Anonymous memory in MB, own cgroup anon first, then the process status snapshot, then the process RSS
func readAnonRSSMB() *int {
	if charged := cgroupAnonBytes(); charged != nil && *charged != 0 {
		mb := int(*charged / acpBytesPerMB)
		return &mb
	}
	snapshot := acpMemorySnapshot()
	for _, kib := range []*int{snapshot.RSSAnonKiB, snapshot.RSSKiB} {
		if kib != nil && *kib > 0 {
			mb := *kib / 1024
			return &mb
		}
	}
	rss, err := acpProcessRSS()
	if err != nil {
		return nil
	}
	mb := int(rss / acpBytesPerMB)
	return &mb
}

// One cached session and its agent in least to most recently used order
type PressureEntry[A any] struct {
	Key   string
	Agent A
}

// Cached sessions to shed least recently used first, capped per pass with the hottest half protected at most
func PlanPressureEvictions[A any](ordered []PressureEntry[A], isEvictable func(key string, agent A) bool, maxEvictions, protectRecent int) []PressureEntry[A] {
	entries := ordered
	if maxEvictions <= 0 || len(entries) == 0 {
		return []PressureEntry[A]{}
	}
	protect := min(max(protectRecent, 0), len(entries)/2)
	if protect > 0 {
		entries = entries[:len(entries)-protect]
	}
	plan := []PressureEntry[A]{}
	for _, entry := range entries {
		if len(plan) >= maxEvictions {
			break
		}
		if isEvictable(entry.Key, entry.Agent) {
			plan = append(plan, entry)
		}
	}
	return plan
}
