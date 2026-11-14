package auth

import (
	"context"
	"slices"
)

var poolStatusFields = []string{
	"last_status", "last_status_at", "last_error_code", "last_error_reason", "last_error_message",
	"last_error_reset_at", "status_cleared_at",
}

var poolTokenGenerationFields = []string{
	"access_token", "refresh_token", "expires_at", "expires_at_ms", "expires_in", "obtained_at",
	"last_refresh", "agent_key", "agent_key_expires_at", "agent_key_expires_in", "agent_key_id",
	"agent_key_obtained_at", "agent_key_reused",
	"scope", "inference_base_url",
}

type tokenPair [2]any

func credentialTokenPair(row *doc) tokenPair {
	if row == nil {
		return tokenPair{}
	}
	return tokenPair{row.value("access_token"), row.value("refresh_token")}
}

func (p tokenPair) any() bool { return truthy(p[0]) || truthy(p[1]) }

func (p tokenPair) equal(o tokenPair) bool { return valueEqual(p[0], o[0]) && valueEqual(p[1], o[1]) }

func rowsOf(values []any) []*doc {
	rows := make([]*doc, 0, len(values))
	for _, v := range values {
		if row, ok := v.(*doc); ok && row != nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func rowID(row *doc) string {
	if id, ok := row.value("id").(string); ok {
		return id
	}
	return textOf(row.value("id"))
}

func entryIDs(rows []*doc) map[string]*doc {
	out := map[string]*doc{}
	for _, row := range rows {
		if id := rowID(row); id != "" {
			out[id] = row
		}
	}
	return out
}

func tokenPairsByID(rows []*doc) map[string]tokenPair {
	out := map[string]tokenPair{}
	for id, row := range entryIDs(rows) {
		out[id] = credentialTokenPair(row)
	}
	return out
}

func (s *Store) readPool(_ context.Context, provider string) ([]*doc, error) {
	store, err := s.load(s.authPath)
	if err != nil {
		return nil, err
	}
	if rows := store.obj("credential_pool").list(provider); len(rows) > 0 {
		return rowsOf(rows), nil
	}
	return rowsOf(s.loadRoot().obj("credential_pool").list(provider)), nil
}

func (s *Store) readPoolAll(_ context.Context) (*doc, error) {
	store, err := s.load(s.authPath)
	if err != nil {
		return nil, err
	}
	merged := newDoc()
	if pool := store.obj("credential_pool"); pool != nil {
		merged = pool.clone()
	}
	if global := s.loadRoot().obj("credential_pool"); global != nil {
		for _, key := range global.keys {
			entries := global.list(key)
			if len(entries) == 0 {
				continue
			}
			if existing := merged.list(key); len(existing) == 0 {
				merged.set(key, entries)
			}
		}
	}
	return merged, nil
}

func (s *Store) PoolProviders(ctx context.Context) ([]string, error) {
	all, err := s.readPoolAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, key := range all.keys {
		if len(all.list(key)) > 0 {
			out = append(out, key)
		}
	}
	return out, nil
}

func docExhaustedUntil(provider string, row *doc) (float64, bool) {
	return exhaustedUntil(entryFromDoc(provider, row), false)
}

func mergeDiskCooldownState(entry, disk *doc, provider string) *doc {
	if disk == nil {
		return entry
	}
	merged := entry
	if cooldowns := mergeModelCooldowns(disk.obj("model_cooldowns"), entry.obj("model_cooldowns")); cooldowns.size() > 0 {
		merged = entry.clone()
		merged.set("model_cooldowns", cooldowns)
	}
	withDiskStatus := func() *doc {
		out := merged.clone()
		for _, f := range poolStatusFields {
			out.set(f, disk.value(f))
		}
		return out
	}
	memTS, _ := parseAbsolute(entry.value("last_status_at"))
	clearedTS, _ := parseAbsolute(disk.value("status_cleared_at"))
	memStatus := entry.str("last_status")
	if (memStatus == StatusDead || memStatus == StatusExhausted) && clearedTS > memTS {
		return withDiskStatus()
	}
	diskStatus := disk.str("last_status")
	if diskStatus != StatusDead && diskStatus != StatusExhausted {
		return merged
	}
	memAccess, diskAccess := textOf(entry.value("access_token")), textOf(disk.value("access_token"))
	if memAccess != "" && diskAccess != "" && memAccess != diskAccess {
		return entry
	}
	diskTS, _ := parseAbsolute(disk.value("last_status_at"))
	if diskTS <= memTS {
		return merged
	}
	if diskStatus == StatusExhausted {
		until, ok := docExhaustedUntil(provider, disk)
		if !ok || until <= nowEpoch() {
			return merged
		}
	}
	return withDiskStatus()
}

func mergePoolRowGeneration(entry, disk *doc, provider string, base *tokenPair, statusCleared bool) *doc {
	mergeDisk := disk
	if statusCleared {
		mergeDisk = nil
	}
	diskPair := credentialTokenPair(disk)
	if base == nil || !diskPair.any() || diskPair.equal(*base) {
		return mergeDiskCooldownState(entry, mergeDisk, provider)
	}
	merged := entry.clone()
	take := func(fields []string) {
		for _, f := range fields {
			if disk.has(f) {
				merged.set(f, disk.value(f))
			} else {
				merged.del(f)
			}
		}
	}
	take(poolTokenGenerationFields)
	if !statusCleared && entry.str("last_status") == StatusDead {
		take(append(slices.Clone(poolStatusFields), "failure_reason"))
	}
	return mergeDiskCooldownState(merged, mergeDisk, provider)
}

type poolWrite struct {
	removed       []string
	statusCleared []string
	tokenBases    map[string]tokenPair
}

func (s *Store) writePool(ctx context.Context, provider string, entries []*doc, w poolWrite) ([]*doc, error) {
	ctx = s.bind(ctx)
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return nil, err
	}
	pool := store.section("credential_pool")
	sanitized := make([]*doc, 0, len(entries))
	for _, e := range entries {
		sanitized = append(sanitized, sanitizeBorrowedPayload(e, provider))
	}
	existing := rowsOf(pool.list(provider))
	existingByID := entryIDs(existing)
	newIDs := entryIDs(sanitized)
	merged := make([]*doc, 0, len(sanitized)+len(existing))
	for _, e := range sanitized {
		id := rowID(e)
		var base *tokenPair
		if pair, ok := w.tokenBases[id]; ok {
			base = &pair
		}
		merged = append(merged, mergePoolRowGeneration(e, existingByID[id], provider, base, id != "" && slices.Contains(w.statusCleared, id)))
	}
	for _, disk := range existing {
		id := rowID(disk)
		if id == "" {
			continue
		}
		if _, ok := newIDs[id]; ok || slices.Contains(w.removed, id) {
			continue
		}
		merged = append(merged, sanitizeBorrowedPayload(disk, provider))
	}
	list := make([]any, 0, len(merged))
	for _, row := range merged {
		list = append(list, row)
	}
	pool.set(provider, list)
	if err := s.save(ctx, store, s.authPath); err != nil {
		return nil, err
	}
	return merged, nil
}
