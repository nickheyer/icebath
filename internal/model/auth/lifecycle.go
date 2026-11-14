package auth

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
)

func (s *Store) providersForEnvVar(ctx context.Context, name string) []string {
	hits := []string{}
	for _, spec := range s.dir.Specs(ctx) {
		if slices.Contains(spec.EnvVars, name) {
			hits = append(hits, spec.Provider)
		}
	}
	return hits
}

func (s *Store) pruneEnvPoolEntries(ctx context.Context, name string) ([]string, error) {
	source := "env:" + name
	pruned := []string{}
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return pruned, err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return pruned, err
	}
	pool := store.obj("credential_pool")
	if pool == nil {
		return pruned, nil
	}
	for _, provider := range slices.Clone(pool.keys) {
		entries, ok := pool.value(provider).([]any)
		if !ok {
			continue
		}
		kept := []any{}
		for _, v := range entries {
			if row := asRow(v); row != nil && row.str("source") == source {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == len(entries) {
			continue
		}
		pruned = append(pruned, provider)
		if len(kept) > 0 {
			pool.set(provider, kept)
		} else {
			pool.del(provider)
		}
	}
	if len(pruned) > 0 {
		if err := s.save(ctx, store, s.authPath); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

func (s *Store) CredentialRemoved(ctx context.Context, name string) (pruned, providers []string, err error) {
	ctx = s.bind(ctx)
	if pruned, err = s.pruneEnvPoolEntries(ctx, name); err != nil {
		return nil, nil, err
	}
	providers = slices.Clone(pruned)
	for _, p := range s.providersForEnvVar(ctx, name) {
		if !slices.Contains(providers, p) {
			providers = append(providers, p)
		}
	}
	sort.Strings(providers)
	for _, provider := range providers {
		if err := s.Suppress(ctx, provider, "env:"+name); err != nil {
			slog.Debug(fmt.Sprintf("auth: could not suppress env:%s for %s", name, provider), "error", err)
			break
		}
	}
	for _, provider := range providers {
		s.dir.ClearModelsCache(ctx, provider)
	}
	return pruned, providers, nil
}

func (s *Store) CredentialSaved(ctx context.Context, name string) {
	ctx = s.bind(ctx)
	providers := s.providersForEnvVar(ctx, name)
	for _, provider := range providers {
		if _, err := s.Unsuppress(ctx, provider, "env:"+name); err != nil {
			slog.Debug(fmt.Sprintf("auth: could not lift the suppression of env:%s for %s", name, provider), "error", err)
			break
		}
	}
	for _, provider := range providers {
		if _, err := s.Pool(ctx, provider); err != nil {
			slog.Debug(fmt.Sprintf("auth: could not seed the %s pool after saving %s", provider, name), "error", err)
			break
		}
	}
}
