package wire

import (
	"log/slog"
	"math"
	"strconv"
	"strings"
)

// Layout of the usage object a response carries
type UsageShape uint8

const (
	UsageChat UsageShape = iota + 1
	UsageResponses
	UsageAnthropic
)

// Token buckets of a provider usage object, nil when the provider sent none
func ParseUsage(raw []byte, shape UsageShape) *Usage {
	if !truthy(raw) {
		return nil
	}
	raw = trimJSON(raw)
	type path = []string
	first := func(paths ...path) int {
		for _, p := range paths {
			if v := usageCount(raw, p...); v != 0 {
				return v
			}
		}
		return 0
	}
	u := &Usage{Requests: 1}
	var total int
	switch shape {
	case UsageAnthropic:
		total = first(path{"input_tokens"})
		u.OutputTokens = first(path{"output_tokens"})
		u.CacheReadTokens = first(path{"cache_read_input_tokens"})
		u.CacheWriteTokens = first(path{"cache_creation_input_tokens"})
	case UsageResponses:
		total = first(path{"input_tokens"})
		u.OutputTokens = first(path{"output_tokens"})
		u.CacheReadTokens = first(path{"input_tokens_details", "cached_tokens"})
		u.CacheWriteTokens = first(path{"input_tokens_details", "cache_write_tokens"}, path{"input_tokens_details", "cache_creation_tokens"})
	default:
		total = first(path{"prompt_tokens"}, path{"input_tokens"})
		u.OutputTokens = first(path{"completion_tokens"}, path{"output_tokens"})
		u.CacheReadTokens = first(path{"prompt_tokens_details", "cached_tokens"}, path{"cache_read_input_tokens"},
			path{"prompt_cache_hit_tokens"}, path{"cached_tokens"})
		u.CacheWriteTokens = first(path{"prompt_tokens_details", "cache_write_tokens"},
			path{"prompt_tokens_details", "cache_creation_input_tokens"}, path{"cache_creation_input_tokens"}, path{"cache_write_tokens"})
	}
	u.InputTokens = total
	if shape != UsageAnthropic {
		u.InputTokens = max(0, total-u.CacheReadTokens-u.CacheWriteTokens)
	}
	u.ReasoningTokens = first(path{"output_tokens_details", "reasoning_tokens"}, path{"completion_tokens_details", "reasoning_tokens"})
	if tier, ok := objectMember(raw, "service_tier"); ok {
		u.ServedTier, _ = stringValue(tier)
	}
	if speed, ok := objectMember(raw, "speed"); ok {
		u.Speed, _ = stringValue(speed)
	}
	return u
}

// Counter at a member path of a usage object, zero for a missing, empty, negative or non-numeric value
func usageCount(obj []byte, path ...string) int {
	for _, hop := range path {
		if !truthy(obj) {
			return 0
		}
		next, ok := objectMember(obj, hop)
		if !ok {
			return 0
		}
		obj = next
	}
	obj = trimJSON(obj)
	if !truthy(obj) {
		return 0
	}
	var n int64
	switch obj[0] {
	case 't':
		n = 1
	case '"':
		parsed, ok := parseInteger(decodeString(obj))
		if !ok {
			return 0
		}
		n = parsed
	case '[', '{':
		return 0
	default:
		f, err := strconv.ParseFloat(string(obj), 64)
		if err != nil || math.IsNaN(f) || f < 0 {
			return 0
		}
		n = math.MaxInt64
		if exact, err := strconv.ParseInt(string(obj), 10, 64); err == nil {
			n = exact
		} else if f < math.MaxInt64 {
			n = int64(math.Trunc(f))
		}
	}
	return int(max(0, n))
}

// Logs the cache counters of a MiniMax reply on the Anthropic wire, where input token drops are the hit signal
func (u *Usage) ObserveCache(provider string, mode Mode) {
	name := strings.ToLower(trimSpace(provider))
	if (name != "minimax" && name != "minimax-cn") || mode != ModeAnthropic {
		return
	}
	slog.Debug("cache_observability", "provider", name, "mode", string(mode), "input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens, "cache_read_tokens", u.CacheReadTokens, "cache_write_tokens", u.CacheWriteTokens,
		"signal", "cache_read carries a constant floor of 128 on MiniMax-M3, track input_tokens drops across calls instead")
}
