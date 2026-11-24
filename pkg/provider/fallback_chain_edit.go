package provider

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
)

// Why a change to the stored fallback chain was refused
var (
	// The entry is the route the chain backs up
	ErrFallbackIsPrimary = errors.New("a provider cannot be a fallback for itself")
	// The chain already holds the entry
	ErrFallbackInChain = errors.New("already in the fallback chain")
	// The chain has no entry at the position asked for
	ErrFallbackNoEntry = errors.New("no such fallback entry")
)

// NewFallbackEntry is a chain entry for a provider and model, with the base URL of an endpoint of the user's own
func NewFallbackEntry(provider, model, baseURL string) map[string]any {
	entry := map[string]any{"provider": provider, "model": model}
	keys := []string{"provider", "model"}
	if baseURL != "" {
		entry["base_url"] = baseURL
		keys = append(keys, "base_url")
	}
	config.SetMappingKeys(entry, keys)
	return entry
}

// AppendFallback adds the entry to the end of the stored chain and answers the chain after it, refusing the primary route and a route the chain already holds
func AppendFallback(ctx context.Context, h string, entry map[string]any) ([]map[string]any, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	identity := FallbackEntryIdentity(entry)
	primary, _, err := ExtractFallbackFromModelCfg(cfg["model"])
	if err != nil {
		return nil, err
	}
	if primary != nil && SameDeploymentIn(ctx, FallbackEntryIdentity(primary), identity) {
		return nil, ErrFallbackIsPrimary
	}
	chain := ReadFallbackChain(cfg)
	for _, existing := range chain {
		if SameDeploymentIn(ctx, FallbackEntryIdentity(existing), identity) {
			return nil, ErrFallbackInChain
		}
	}
	chain = append(chain, entry)
	WriteFallbackChain(cfg, chain)
	if err := config.Save(h, cfg); err != nil {
		return nil, err
	}
	return chain, nil
}

// RemoveFallbackAt takes the entry at the position out of the stored chain and answers it with the chain after it
func RemoveFallbackAt(ctx context.Context, h string, index int) (map[string]any, []map[string]any, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, nil, err
	}
	chain := ReadFallbackChain(cfg)
	if index < 0 || index >= len(chain) {
		return nil, nil, ErrFallbackNoEntry
	}
	removed := chain[index]
	chain = append(chain[:index:index], chain[index+1:]...)
	WriteFallbackChain(cfg, chain)
	if err := config.Save(h, cfg); err != nil {
		return nil, nil, err
	}
	return removed, chain, nil
}

// MoveFallback puts the entry at one position of the stored chain at another, so it is tried earlier or later, and answers the chain after it
func MoveFallback(ctx context.Context, h string, from, to int) ([]map[string]any, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	chain := ReadFallbackChain(cfg)
	if from < 0 || from >= len(chain) || to < 0 || to >= len(chain) {
		return nil, ErrFallbackNoEntry
	}
	entry := chain[from]
	chain = append(chain[:from:from], chain[from+1:]...)
	chain = append(chain[:to:to], append([]map[string]any{entry}, chain[to:]...)...)
	WriteFallbackChain(cfg, chain)
	if err := config.Save(h, cfg); err != nil {
		return nil, err
	}
	return chain, nil
}

// ClearFallbacks empties the stored chain
func ClearFallbacks(ctx context.Context, h string) error {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	WriteFallbackChain(cfg, nil)
	return config.Save(h, cfg)
}
