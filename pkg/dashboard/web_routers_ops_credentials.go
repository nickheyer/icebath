package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Redacted row of one pooled credential, its index one based like the pool removal index
func poolEntrySummary(entry provider.PooledCredential, index int) *pyjson.Object {
	preview := ""
	if entry.AccessToken != "" {
		preview = config.RedactKey(entry.AccessToken)
	}
	var lastStatus any
	if entry.LastStatus != "" {
		lastStatus = entry.LastStatus
	}
	return pyjson.ObjectOf(
		"index", int64(index),
		"id", entry.ID,
		"label", entry.Label,
		"auth_type", entry.AuthType,
		"source", entry.Source,
		"priority", int64(entry.Priority),
		"last_status", lastStatus,
		"request_count", int64(entry.RequestCount),
		"token_preview", preview,
		"has_refresh", entry.RefreshToken != "",
	)
}

// Every provider of the profile auth store that holds pooled credentials, tokens redacted
func listCredentialPool(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		env := bootstrapPoolEnv(sctx)
		pools, err := env.Store.ReadCredentialPools()
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(pools))
		for id := range pools {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		providers := []any{}
		for _, id := range ids {
			entries := provider.LoadPool(env, id).Entries()
			if len(entries) == 0 {
				continue
			}
			rows := make([]any, 0, len(entries))
			for i, e := range entries {
				rows = append(rows, poolEntrySummary(e, i+1))
			}
			providers = append(providers, pyjson.ObjectOf("provider", id, "entries", rows))
		}
		return pyjson.ObjectOf("providers", providers), nil
	})
}

// Six hex characters of fresh random bytes, the id of a manually added pool entry
func poolEntryID() string {
	raw := make([]byte, 3)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// Lifts every suppressed source of a provider so a source deleted earlier seeds again
func unsuppressProviderSources(env provider.PoolEnv, providerID string) error {
	data, err := env.Store.Load()
	if err != nil {
		return err
	}
	suppressed, ok := data["suppressed_sources"]
	if !ok {
		return nil
	}
	byProvider, ok := suppressed.(map[string]any)
	if !ok {
		return fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(suppressed))
	}
	sources, err := opsListOf(byProvider[providerID])
	if err != nil {
		return err
	}
	for _, src := range sources {
		text, ok := src.(string)
		if !ok {
			return fmt.Errorf("TypeError: suppressed source must be str, not %s", pyrepr.TypeName(src))
		}
		if _, err := env.Store.UnsuppressCredentialSource(providerID, text); err != nil {
			return err
		}
	}
	return nil
}

// Adds a manual API key entry to the pool of a provider and lifts its source suppressions
func addCredentialPoolEntry(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	providerID := pystr.Lower(pystr.Strip(bodyStr(body, "provider")))
	apiKey := pystr.Strip(bodyStr(body, "api_key"))
	if providerID == "" || apiKey == "" {
		return nil, NewHTTPException(400, "provider and api_key are required")
	}
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		env := bootstrapPoolEnv(sctx)
		pool := provider.LoadPool(env, providerID)
		label := pystr.Strip(bodyStr(body, "label"))
		if label == "" {
			label = fmt.Sprintf("key #%d", len(pool.Entries())+1)
		}
		_, err := pool.AddEntry(provider.PooledCredential{Provider: providerID, ID: poolEntryID(), Label: label,
			AuthType: provider.AuthTypeAPIKey, Priority: 0, Source: provider.SourceManual, AccessToken: apiKey})
		if err != nil {
			routersLog().Error("POST /api/credentials/pool failed", "exc_info", err)
			return nil, NewHTTPException(400, err.Error())
		}
		if !strings.HasPrefix(providerID, provider.CustomPoolPrefix) {
			if err := unsuppressProviderSources(env, providerID); err != nil {
				routersLog().Error("unsuppress after pool add failed (non-fatal)", "exc_info", err)
			}
		}
		return pyjson.ObjectOf("ok", true, "provider", providerID, "count", int64(len(pool.Entries()))), nil
	})
}

// Removes a pool entry by its one based index and runs the removal step of its source
func removeCredentialPoolEntry(ctx context.Context, providerID string, index int64, profile string) (*pyjson.Object, error) {
	providerID = pystr.Lower(pystr.Strip(providerID))
	profile, err := destructiveProfile(ctx, profile, "DELETE /api/credentials/pool/{provider}/{index}")
	if err != nil {
		return nil, err
	}
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		env := bootstrapPoolEnv(sctx)
		pool := provider.LoadPool(env, providerID)
		removed, ok := pool.RemoveIndex(int(index))
		if !ok {
			return nil, NewHTTPException(404, "No pool entry at that index")
		}
		cleaned := []any{}
		hints := []any{}
		if step, found := provider.FindRemovalStep(providerID, removed.Source); found {
			result := step.Remove(env, providerID, removed)
			cleaned = stringsAny(result.Cleaned)
			hints = stringsAny(result.Hints)
			if result.Suppress {
				if err := env.Store.SuppressCredentialSource(providerID, removed.Source); err != nil {
					routersLog().Error(fmt.Sprintf("credential source cleanup failed for %s/%s; suppressing anyway", providerID, removed.Source), "exc_info", err)
					if err := env.Store.SuppressCredentialSource(providerID, removed.Source); err != nil {
						routersLog().Error("suppress_credential_source failed", "exc_info", err)
					}
				}
			}
		}
		return pyjson.ObjectOf("ok", true, "provider", providerID, "count", int64(len(pool.Entries())),
			"cleaned", cleaned, "hints", hints), nil
	})
}

var memoryFiles = [][2]string{{"MEMORY.md", "memory"}, {"USER.md", "user"}}

// Active memory provider, every discovered provider and the sizes of the built in memory files
func getMemoryStatus(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		active := ""
		if mem, ok := cfg["memory"].(map[string]any); ok {
			active = normalizeMemoryProviderName(mem["provider"])
		}
		memDir := pypath.PathJoin(home.Dir(sctx), "memories")
		files := pyjson.NewObject()
		for _, pair := range memoryFiles {
			size := int64(0)
			if st, err := os.Stat(pypath.PathJoin(memDir, pair[0])); err == nil {
				size = st.Size()
			}
			files.Set(pair[1], size)
		}
		statuses, err := discoverMemoryProviderStatuses(sctx)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(statuses))
		for _, s := range statuses {
			rows = append(rows, s)
		}
		return pyjson.ObjectOf("active", active, "providers", rows, "builtin_files", files), nil
	})
}

// Selects the memory provider of a profile after the provider proved ready inside that profile
func setMemoryProvider(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	name := normalizeMemoryProviderName(body.GetOr("provider", nil))
	wctx, release, err := configWriteScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := requireMemoryProviderReady(wctx, name); err != nil {
		return nil, err
	}
	cfg, err := webLoadConfig(wctx)
	if err != nil {
		return nil, err
	}
	mem, ok := cfg["memory"].(map[string]any)
	if !ok {
		mem = map[string]any{}
		cfg["memory"] = mem
	}
	mem["provider"] = name
	if err := webSaveConfig(wctx, cfg, config.SaveOptions{}); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "active", name), nil
}

// Deletes the built in memory files a target names and lists the ones removed
func resetMemory(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	target := bodyStr(body, "target")
	if target == "" {
		target = "all"
	}
	target = pystr.Lower(pystr.Strip(target))
	if target != "all" && target != "memory" && target != "user" {
		return nil, NewHTTPException(400, "target must be all, memory, or user")
	}
	profile, err := destructiveProfile(ctx, profile, "POST /api/memory/reset")
	if err != nil {
		return nil, err
	}
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		memDir := pypath.PathJoin(home.Dir(sctx), "memories")
		deleted := []any{}
		for _, pair := range memoryFiles {
			path := pypath.PathJoin(memDir, pair[0])
			if target != "all" && target != pair[1] {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue
			}
			if err := pypath.Unlink(path); err != nil {
				return nil, NewHTTPException(500, fmt.Sprintf("Could not delete %s: %s", pair[0], pypath.OSErrorText(err)))
			}
			deleted = append(deleted, pair[0])
		}
		return pyjson.ObjectOf("ok", true, "deleted", deleted), nil
	})
}
