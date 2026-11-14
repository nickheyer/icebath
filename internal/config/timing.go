package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	EpochMin = 0.0
	EpochMax = 4_200_000_000.0
)

const MaxSafeTimeout = 365 * 24 * time.Hour

func decimalFloat(raw string) (float64, bool) {
	text, ok := plainNumberText(trimEnvSpace(raw))
	if !ok || text == "" || strings.ContainsAny(text, "xXpP") {
		return 0, false
	}
	if strings.EqualFold(strings.TrimLeft(text, "+-"), "nan") && len(text) <= 4 {
		return math.NaN(), true
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return v, true
}

func epochValue(value any) (float64, bool) {
	switch v := value.(type) {
	case time.Time:
		return float64(v.UnixNano()) / 1e9, true
	case string:
		return decimalFloat(v)
	case []byte:
		return decimalFloat(string(v))
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

func CoerceEpoch(value any, sessionID, field string) (float64, bool) {
	if value == nil {
		return 0, false
	}
	if text, ok := value.(string); ok && text == "" {
		return 0, false
	}
	if field == "" {
		field = "timestamp"
	}
	ts, ok := epochValue(value)
	if !ok || math.IsNaN(ts) || ts < EpochMin || ts > EpochMax {
		where := ""
		if sessionID != "" {
			where = " on session " + sessionID
		}
		shown := fmt.Sprintf("%v", value)
		if text, isText := value.(string); isText {
			shown = quoted(text)
		}
		slog.Warn(fmt.Sprintf("Ignoring corrupt %s %s%s", field, shown, where))
		return 0, false
	}
	return ts, true
}

func epochFalsy(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []byte:
		return len(v) == 0
	case bool:
		return !v
	case time.Time:
		return false
	}
	f, ok := epochValue(value)
	return ok && f == 0
}

func RelativeTime(ts any, sessionID string) string {
	if epochFalsy(ts) {
		return "?"
	}
	v, ok := CoerceEpoch(ts, sessionID, "last_active")
	if !ok {
		return "?"
	}
	delta := float64(time.Now().UnixNano())/1e9 - v
	switch {
	case delta < 60:
		return "just now"
	case delta < 3600:
		return fmt.Sprintf("%dm ago", int(delta/60))
	case delta < 86400:
		return fmt.Sprintf("%dh ago", int(delta/3600))
	case delta < 172800:
		return "yesterday"
	case delta < 604800:
		return fmt.Sprintf("%dd ago", int(delta/86400))
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)).Format("2006-01-02")
}

func ClampTimeout(seconds float64) (time.Duration, bool) {
	if math.IsNaN(seconds) {
		slog.Warn("clamp_timeout: NaN timeout; treating as unbounded")
		return 0, false
	}
	if seconds <= 0 {
		return 0, false
	}
	if seconds >= MaxSafeTimeout.Seconds() {
		return MaxSafeTimeout, true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

func timeoutNode(t TimeoutTree, key string) *yaml.Node {
	parts := strings.Split(key, ".")
	raw, ok := t.Get(parts[0])
	if !ok || raw.node == nil {
		return nil
	}
	cur := resolveAlias(raw.node)
	for _, part := range parts[1:] {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for _, p := range mappingPairs(cur) {
			if p.key.Value == part {
				next = resolveAlias(p.value)
			}
		}
		cur = next
	}
	return cur
}

func timeoutSeconds(n *yaml.Node) (float64, bool) {
	if n.Kind != yaml.ScalarNode {
		return 0, false
	}
	tag, text := scalarKind(n)
	var v float64
	switch tag {
	case tagInt:
		i, ok := constructInt(text)
		if !ok {
			return 0, false
		}
		v, _ = new(big.Float).SetInt(i).Float64()
	case tagFloat:
		f, ok := constructFloat(text)
		if !ok {
			return 0, false
		}
		v = f
	case tagStr:
		f, ok := decimalFloat(text)
		if !ok {
			return 0, false
		}
		v = f
	default:
		return 0, false
	}
	return v, !math.IsNaN(v)
}

func (s *Store) ResolveTimeout(ctx context.Context, key string, def float64, envVar string) (time.Duration, bool) {
	if cfg, err := s.LoadReadonly(ctx); err != nil {
		slog.Debug(fmt.Sprintf("timeouts: config read failed; using defaults: %v", err))
	} else if raw := timeoutNode(cfg.Extra.Timeouts, key); raw != nil && !isNull(raw) {
		if v, ok := timeoutSeconds(raw); ok {
			return ClampTimeout(v)
		}
		slog.Warn(fmt.Sprintf("timeouts.%s: invalid value %s in config.yaml; ignoring", key, literalText(raw)))
	}
	if envVar != "" {
		value, _ := s.env(ctx, envVar)
		if text := strings.TrimSpace(value); text != "" {
			if v, ok := decimalFloat(text); ok {
				return ClampTimeout(v)
			}
			slog.Warn(fmt.Sprintf("invalid %s=%s; ignoring", envVar, quoted(text)))
		}
	}
	return ClampTimeout(def)
}

func (s *Store) ProviderRequestTimeout(ctx context.Context, provider, model string) (float64, bool) {
	return s.providerTimeout(ctx, provider, model,
		func(m ProviderModel) *float64 { return m.TimeoutSeconds },
		func(p ProviderEntry) *float64 { return p.RequestTimeoutSeconds })
}

func (s *Store) ProviderStaleTimeout(ctx context.Context, provider, model string) (float64, bool) {
	return s.providerTimeout(ctx, provider, model,
		func(m ProviderModel) *float64 { return m.StaleTimeoutSeconds },
		func(p ProviderEntry) *float64 { return p.StaleTimeoutSeconds })
}

func (s *Store) providerTimeout(ctx context.Context, provider, model string, perModel func(ProviderModel) *float64, perProvider func(ProviderEntry) *float64) (float64, bool) {
	if provider == "" {
		return 0, false
	}
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("provider timeouts: config read failed: %v", err))
		return 0, false
	}
	entry, ok := cfg.Providers.Get(provider)
	if !ok {
		return 0, false
	}
	if model != "" {
		if m, ok := entry.Models.Get(model); ok {
			if v := perModel(m); v != nil && *v > 0 {
				return *v, true
			}
		}
	}
	if v := perProvider(entry); v != nil && *v > 0 {
		return *v, true
	}
	return 0, false
}
