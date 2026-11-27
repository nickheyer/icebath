package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

const (
	// Least time between two refreshes of one account's reading, failed ones included
	usageRefreshGap = 2 * time.Minute
	// Age past which a reading no longer shows as live
	usageStale = 15 * time.Minute
	// Longest a provider's account usage hook takes
	usageDeadline = 10 * time.Second
)

// Slot of one account's reading: the provider of the profile, the account and the address
type usageKey struct{ provider, identity, base string }

type usageSlot struct {
	reading *provider.AccountUsage
	// When the fetch that landed the reading started
	started time.Time
	// When a refresh was last tried, and whether one runs
	tried    time.Time
	inflight bool
}

func (m *Models) usageSlot(provider, identity, base string) usageKey {
	return usageKey{strings.ToLower(strings.TrimSpace(provider)), strings.TrimSpace(identity), strings.TrimRight(strings.TrimSpace(base), "/")}
}

// Whether the route's kind can say what its accounts may still spend
func (m *Models) HasUsage(route provider.Route) bool {
	return m.Spec(route).AccountUsage != nil
}

// What the provider of the route says of the limits of its account, remembered for the route and its account; nil when the kind can't say,
// no account is signed in or the fetch failed
func (m *Models) Usage(ctx context.Context, cfg *config.Config, route provider.Route) *provider.AccountUsage {
	fetch := m.Spec(route).AccountUsage
	if fetch == nil {
		return nil
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, usageDeadline)
	defer cancel()
	reading, err := func() (reading *provider.AccountUsage, err error) {
		defer func() {
			if failure := recover(); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
		}()
		return fetch(ctx, route)
	}()
	if err != nil {
		slog.Debug("Account usage fetch failed", "provider", route.Provider, "error", err)
		return nil
	}
	m.remember(cfg, route, reading, started)
	return reading
}

// Lands a reading in the slots of its route and of its account, never over a fresher one nor with one that has no windows
func (m *Models) remember(cfg *config.Config, route provider.Route, reading *provider.AccountUsage, started time.Time) {
	if reading == nil || len(reading.Windows) == 0 {
		return
	}
	keys := []usageKey{m.usageSlot(route.Provider, "", route.BaseURL)}
	if reading.Identity != "" {
		keys = append(keys, m.usageSlot(route.Provider, reading.Identity, ""))
	}
	s := m.of(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		slot := s.usage[key]
		if slot == nil {
			slot = &usageSlot{}
			s.usage[key] = slot
		}
		if slot.reading != nil && slot.started.After(started) {
			continue
		}
		slot.reading, slot.started = reading, started
	}
}

// Reading remembered for the route, nil when none landed
func (m *Models) CachedUsage(cfg *config.Config, route provider.Route) *provider.AccountUsage {
	s := m.of(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.usage[m.usageSlot(route.Provider, "", route.BaseURL)]; slot != nil {
		return slot.reading
	}
	return nil
}

// Whether a reading is too old, or from the future, to show as live
func StaleUsage(reading *provider.AccountUsage) bool {
	if reading == nil || reading.Fetched.IsZero() {
		return true
	}
	age := time.Since(reading.Fetched)
	return age < 0 || age > usageStale
}

// Refreshes the reading of each route in the background, unless one runs for it or was tried in the last two minutes
func (m *Models) RefreshUsage(ctx context.Context, cfg *config.Config, routes []provider.Route) {
	s := m.of(cfg)
	detached := context.WithoutCancel(ctx)
	for _, route := range routes {
		if !m.HasUsage(route) {
			continue
		}
		key := m.usageSlot(route.Provider, "", route.BaseURL)
		s.mu.Lock()
		slot := s.usage[key]
		if slot == nil {
			slot = &usageSlot{}
			s.usage[key] = slot
		}
		due := !slot.inflight && time.Since(slot.tried) >= usageRefreshGap
		if due {
			slot.inflight, slot.tried = true, time.Now()
		}
		s.mu.Unlock()
		if !due {
			continue
		}
		go func() {
			defer func() {
				s.mu.Lock()
				slot.inflight = false
				s.mu.Unlock()
			}()
			m.Usage(detached, cfg, route)
		}()
	}
}

// Stable non secret id of a credential, for telling accounts apart
func Identity(key string) string {
	if key = strings.TrimSpace(key); key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "fp:" + hex.EncodeToString(sum[:])[:16]
}
