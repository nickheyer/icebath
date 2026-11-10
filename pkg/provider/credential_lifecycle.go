package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// EnvCredentialPurge names the providers whose env seeded pool rows were pruned and every provider the purge touched
type EnvCredentialPurge struct {
	PoolPruned []string
	Providers  []string
}

// ProviderEnvCredentialSave is the outcome of saving a provider key, naming the config mirrors rotated with it
type ProviderEnvCredentialSave struct {
	OK            bool
	Key           string
	ConfigUpdates []string
}

// ProviderEnvCredentialRemoval is the outcome of removing a provider key from every store that held it
type ProviderEnvCredentialRemoval struct {
	OK             bool
	Key            string
	Removed        bool
	PoolPruned     []string
	Providers      []string
	ConfigScrubbed []string
	Found          bool
}

// Pool env of an explicit home with the context scoped to it
func credentialLifecycleEnv(ctx context.Context, h string) PoolEnv {
	if ctx == nil {
		ctx = context.Background()
	}
	return modelsPoolEnv(home.WithOverride(ctx, h))
}

// Provider ids whose registered API key env vars include the variable
func providersForEnvVar(ctx context.Context, envVar string) []string {
	var hits []string
	for _, row := range canonicalRegistryRows(ctx) {
		if row != nil && containsString(row.KeyEnv, envVar) {
			hits = append(hits, row.Name)
		}
	}
	return hits
}

// Runs fn for every provider in order, the first failure ending the run without surfacing
func forEachProvider(providers []string, fn func(provider string) error) {
	for _, p := range providers {
		if fn(p) != nil {
			return
		}
	}
}

// Drops credential pool rows seeded from the env var across all providers and returns the providers pruned
func pruneEnvPoolEntries(env PoolEnv, envVar string) ([]string, error) {
	source := "env:" + envVar
	pruned := []string{}
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := env.Store.Load()
	if err != nil {
		return nil, err
	}
	pool, ok := data["credential_pool"].(map[string]any)
	if !ok {
		return pruned, nil
	}
	providers := make([]string, 0, len(pool))
	for p := range pool {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		entries, ok := pool[p].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, e := range entries {
			if row, isMap := e.(map[string]any); isMap && row["source"] == source {
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == len(entries) {
			continue
		}
		pruned = append(pruned, p)
		if len(kept) > 0 {
			pool[p] = kept
		} else {
			delete(pool, p)
		}
	}
	if len(pruned) > 0 {
		if err := env.Store.Save(data); err != nil {
			return nil, err
		}
	}
	return pruned, nil
}

func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Rewrites or removes config.yaml api key mirrors holding the old value on the raw user config and returns the paths touched
func scrubConfigYAMLMirrors(ctx context.Context, h, oldValue string, newValue *string) ([]string, error) {
	touched := []string{}
	if oldValue == "" {
		return touched, nil
	}
	ctx, unlock := config.LockConfig(home.WithOverride(ctx, h))
	defer unlock()
	path := home.ConfigPath(h)
	exists, err := config.ConfigExists(ctx, path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return touched, nil
	}
	user, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil || len(user) == 0 {
		return touched, nil
	}
	fix := func(section any, keyPath string, fields ...string) {
		m, ok := section.(map[string]any)
		if !ok {
			return
		}
		for _, field := range fields {
			if current, ok := m[field].(string); ok && current == oldValue {
				if newValue != nil && *newValue != "" {
					m[field] = *newValue
				} else {
					delete(m, field)
				}
				touched = append(touched, keyPath+"."+field)
			}
		}
	}
	// The legacy api alias is a credential under model and auxiliary, but a base_url under keyed providers
	fix(user["model"], "model", "api_key", "api")
	if aux, ok := user["auxiliary"].(map[string]any); ok {
		for _, task := range sortedAnyKeys(aux) {
			fix(aux[task], "auxiliary."+task, "api_key", "api")
		}
	}
	switch custom := user["custom_providers"].(type) {
	case map[string]any:
		for _, name := range sortedAnyKeys(custom) {
			fix(custom[name], "custom_providers."+name, "api_key", "api")
		}
	case []any:
		for i, entry := range custom {
			fix(entry, "custom_providers."+strconv.Itoa(i), "api_key", "api")
		}
	}
	if providers, ok := user["providers"].(map[string]any); ok {
		for _, id := range sortedAnyKeys(providers) {
			fix(providers[id], "providers."+id, "api_key")
		}
	}
	if len(touched) > 0 {
		if err := config.AtomicConfigReplace(ctx, h, path, user, ""); err != nil {
			return nil, err
		}
	}
	return touched, nil
}

// PurgeEnvCredentialReferences prunes env seeded pool rows, suppresses the env source and clears affected model cache rows
func PurgeEnvCredentialReferences(ctx context.Context, h, envVar string, clearModelsCache bool) (EnvCredentialPurge, error) {
	env := credentialLifecycleEnv(ctx, h)
	pruned, err := pruneEnvPoolEntries(env, envVar)
	if err != nil {
		return EnvCredentialPurge{}, err
	}
	set := map[string]bool{}
	for _, p := range append(append([]string{}, pruned...), providersForEnvVar(env.ctx(), envVar)...) {
		set[p] = true
	}
	providers := sortedBoolKeys(set)
	// Sticky like auth remove so a lingering shell export cannot reseed the pool on the next load
	forEachProvider(providers, func(p string) error { return env.Store.SuppressCredentialSource(p, "env:"+envVar) })
	if clearModelsCache && len(providers) > 0 {
		forEachProvider(providers, func(p string) error {
			ClearProviderModelsCache(env.ctx(), p)
			return nil
		})
	}
	return EnvCredentialPurge{PoolPruned: pruned, Providers: providers}, nil
}

// SaveProviderEnvCredential writes the key to .env, rotates config mirrors of the old value, lifts suppression and seeds the pool
func SaveProviderEnvCredential(ctx context.Context, h, envVar, value string) (ProviderEnvCredentialSave, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A locked key fails here because the env refusal would otherwise return like a success
	if err := config.RequireEnvWritable(ctx, h, envVar, "set"); err != nil {
		return ProviderEnvCredentialSave{}, err
	}
	oldValue := config.LoadEnv(h)[envVar]
	if err := config.SaveEnvValueContext(ctx, h, envVar, value); err != nil {
		return ProviderEnvCredentialSave{}, err
	}
	updates := []string{}
	if value != "" && oldValue != "" && oldValue != value {
		var err error
		if updates, err = scrubConfigYAMLMirrors(ctx, h, oldValue, &value); err != nil {
			return ProviderEnvCredentialSave{}, err
		}
	}
	env := credentialLifecycleEnv(ctx, h)
	providers := providersForEnvVar(env.ctx(), envVar)
	forEachProvider(providers, func(p string) error {
		_, err := env.Store.UnsuppressCredentialSource(p, "env:"+envVar)
		return err
	})
	forEachProvider(providers, func(p string) error {
		if LoadPool(env, p) == nil {
			return fmt.Errorf("credential pool for %s did not load", p)
		}
		return nil
	})
	return ProviderEnvCredentialSave{OK: true, Key: envVar, ConfigUpdates: updates}, nil
}

// RemoveProviderEnvCredential removes the key from .env and the process env, the pool, the model cache and config mirrors
func RemoveProviderEnvCredential(ctx context.Context, h, envVar string) (ProviderEnvCredentialRemoval, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Before the pool prune and mirror scrub so a refused remove leaves the other stores alone
	if err := config.RequireEnvWritable(ctx, h, envVar, "remove"); err != nil {
		return ProviderEnvCredentialRemoval{}, err
	}
	oldValue := config.LoadEnv(h)[envVar]
	removed, err := config.RemoveHomeEnvValue(ctx, h, envVar)
	if err != nil {
		return ProviderEnvCredentialRemoval{}, err
	}
	refs, err := PurgeEnvCredentialReferences(ctx, h, envVar, true)
	if err != nil {
		return ProviderEnvCredentialRemoval{}, err
	}
	scrubbed := []string{}
	if oldValue != "" {
		if scrubbed, err = scrubConfigYAMLMirrors(ctx, h, oldValue, nil); err != nil {
			return ProviderEnvCredentialRemoval{}, err
		}
	}
	return ProviderEnvCredentialRemoval{OK: true, Key: envVar, Removed: removed, PoolPruned: refs.PoolPruned, Providers: refs.Providers,
		ConfigScrubbed: scrubbed, Found: removed || len(refs.PoolPruned) > 0 || len(scrubbed) > 0}, nil
}

// Config facade hook saving a provider key through the lifecycle
func saveProviderEnvCredential(ctx context.Context, h, key, value string) error {
	_, err := SaveProviderEnvCredential(ctx, h, key, value)
	return err
}

// Config facade hook removing a provider key from every store, reporting whether any store held it
func removeProviderEnvCredential(ctx context.Context, h, key string) (bool, error) {
	result, err := RemoveProviderEnvCredential(ctx, h, key)
	return result.Found, err
}
