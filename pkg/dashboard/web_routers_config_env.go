package dashboard

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Reveal rate limiter window, at most five reveals in thirty seconds
var (
	revealMu         sync.Mutex
	revealTimestamps []float64
)

const (
	revealMaxPerWindow  = 5
	revealWindowSeconds = 30
)

// Display order of the config tabs, unlisted categories sorting after these
var configCategoryOrder = []string{
	"general", "agent", "terminal", "display", "delegation",
	"memory", "compression", "security", "browser", "voice",
	"tts", "stt", "logging", "discord", "auxiliary",
}

// Route error of an env write, a ValueError becoming 400 with its message and anything else a logged 500
func envWriteErrors(err error, logMsg string) error {
	if err == nil {
		return nil
	}
	if h, ok := asHTTPException(err); ok {
		return h
	}
	if pyrepr.ExceptionName(err) == "ValueError" {
		return NewHTTPException(400, err.Error())
	}
	routersLog().Error(logMsg, "exc_info", err)
	return NewHTTPException(500, "Internal server error")
}

// Body profile when set, else the query profile
func bodyOrQueryProfile(body *pyjson.Object, c *Call) string {
	if p, ok := body.GetOr("profile", nil).(string); ok && p != "" {
		return p
	}
	return c.Str("profile")
}

// Config map of the home in ctx with saved values only, defaults left out
func webReadRawConfig(ctx context.Context) (map[string]any, error) {
	return config.ReadRawConfig(ctx, home.Dir(ctx))
}

// Saves a config map into the home in ctx
func webSaveConfig(ctx context.Context, cfg map[string]any, opts config.SaveOptions) error {
	return config.SaveConfig(ctx, home.Dir(ctx), cfg, opts)
}

// Parsed .env of the home in ctx
func webLoadEnv(ctx context.Context) map[string]string { return config.LoadEnv(home.Dir(ctx)) }

// Router of the config read routes, mounted with its own dependencies
func configEnvConfigRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/config", &Endpoint{
		Name:   "get_config",
		Params: []Param{QueryParam("profile", optStr, nil), QueryParam("include_defaults", Bool, true)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			cfg, err := scopedToThread(ctx, c.Str("profile"), func(ctx context.Context) (map[string]any, error) {
				var loaded map[string]any
				var err error
				if c.Bool("include_defaults") {
					loaded, err = webLoadConfig(ctx)
				} else {
					loaded, err = webReadRawConfig(ctx)
				}
				if err != nil {
					return nil, err
				}
				return normalizeConfigForWeb(loaded), nil
			})
			if err != nil {
				return nil, err
			}
			out := pyjson.NewObject()
			for _, k := range config.MappingKeys(cfg) {
				if !strings.HasPrefix(k, "_") {
					out.Set(k, configJSON(cfg[k]))
				}
			}
			return out, nil
		},
	})
	r.Get("/api/config/defaults", &Endpoint{
		Name: "get_defaults",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return configJSON(map[string]any(config.Defaults())), nil
		},
	})
	r.Get("/api/config/schema", &Endpoint{
		Name:   "get_schema",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			fields, err := configScopedToThread(ctx, c.Str("profile"), func(ctx context.Context) (*pyjson.Object, error) {
				return schemaWithDynamicProviderOptions(ctx), nil
			})
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("fields", fields, "category_order", stringsAny(configCategoryOrder)), nil
		},
	})
	r.Get("/api/egress/status", &Endpoint{
		Name:   "get_egress_status",
		Doc:    "Dashboard/Desktop-readable egress proxy status and remediation text.",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			text, err := configScopedToThread(ctx, c.Str("profile"), func(ctx context.Context) (string, error) {
				return provider.FormatEgressStatusText(home.Dir(ctx), false)
			})
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("text", text), nil
		},
	})
	return r
}

// Router of the config write, env var and custom endpoint routes
func configEnvRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Put("/api/config", &Endpoint{
		Name:   "update_config",
		Params: []Param{QueryParam("profile", optStr, nil), QueryParam("preserve_language", Bool, false)},
		Body:   []BodyParam{Body("body", configUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			profile := bodyOrQueryProfile(body, c)
			detail := "Internal server error"
			result, err := updateConfigSync(ctx, profile, configMapOf(body.GetOr("config", nil)).(map[string]any), c.Bool("preserve_language"))
			if err != nil {
				return nil, httpFailure(err, "PUT /api/config failed", 500, "", &detail)
			}
			return result, nil
		},
	})
	r.Get("/api/env", &Endpoint{
		Name:   "get_env_vars",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getEnvVarsSync(ctx, c.Str("profile")) },
	})
	r.Put("/api/env", &Endpoint{
		Name:   "set_env_var",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Body:   []BodyParam{Body("body", envVarUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			if isRedactedCredentialPreview(body.GetOr("value", nil)) {
				return nil, NewHTTPException(400, redactedCredentialWriteDetail)
			}
			result, err := scopedToThread(ctx, bodyOrQueryProfile(body, c), func(ctx context.Context) (*pyjson.Object, error) {
				return saveEnvCredential(ctx, body.GetOr("key", "").(string), body.GetOr("value", "").(string),
					body.GetOr("provider_setup", false) == true)
			})
			if err != nil {
				return nil, envWriteErrors(err, "PUT /api/env failed")
			}
			return result, nil
		},
	})
	addCustomEndpointRoutes(r)
	r.Post("/api/providers/validate", &Endpoint{
		Name: "validate_provider_credential",
		Doc:  "Live-probe a provider credential before it's saved.",
		Body: []BodyParam{Body("body", envVarUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			return validateProviderCredential(ctx, c.Object("body")), nil
		},
	})
	r.Delete("/api/env", &Endpoint{
		Name:   "remove_env_var",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Body:   []BodyParam{Body("body", envVarDeleteModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			key := body.GetOr("key", "").(string)
			removal, err := scopedToThread(ctx, bodyOrQueryProfile(body, c), func(ctx context.Context) (provider.ProviderEnvCredentialRemoval, error) {
				return provider.RemoveProviderEnvCredential(ctx, home.Dir(ctx), key)
			})
			if err != nil {
				return nil, envWriteErrors(err, "DELETE /api/env failed")
			}
			if !removal.Found {
				return nil, NewHTTPException(404, key+" not found in .env")
			}
			return credentialRemovalObject(removal), nil
		},
	})
	r.Post("/api/env/reveal", &Endpoint{
		Name:   "reveal_env_var",
		Doc:    "Return the real (unredacted) value of a single env var.",
		Params: []Param{QueryParam("profile", optStr, nil)},
		Body:   []BodyParam{Body("body", envVarRevealModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			if !admitReveal(float64(time.Now().UnixNano()) / 1e9) {
				return nil, NewHTTPException(429, "Too many reveal requests. Try again shortly.")
			}
			body := c.Object("body")
			key := body.GetOr("key", "").(string)
			env, err := scopedToThread(ctx, bodyOrQueryProfile(body, c), func(ctx context.Context) (map[string]string, error) {
				return webLoadEnv(ctx), nil
			})
			if err != nil {
				return nil, err
			}
			value, ok := env[key]
			if !ok {
				return nil, NewHTTPException(404, key+" not found in .env")
			}
			routersLog().Info("env/reveal: " + key)
			return pyjson.ObjectOf("key", key, "value", value), nil
		},
	})
	return r
}

// Records a reveal at now unless the window already holds the maximum
func admitReveal(now float64) bool {
	revealMu.Lock()
	defer revealMu.Unlock()
	cutoff := now - revealWindowSeconds
	kept := revealTimestamps[:0]
	for _, t := range revealTimestamps {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	revealTimestamps = kept
	if len(revealTimestamps) >= revealMaxPerWindow {
		return false
	}
	revealTimestamps = append(revealTimestamps, now)
	return true
}

// Deep merges a web config over the scoped config under the mutation lock and broadcasts an approvals change
func updateConfigSync(ctx context.Context, profile string, incoming map[string]any, preserveLanguage bool) (*pyjson.Object, error) {
	changed, err := func() (bool, error) {
		sctx, _, release, err := profileScope(ctx, profile)
		if err != nil {
			return false, err
		}
		defer release()
		lctx, unlock := provider.LockConfigMutation(sctx)
		defer unlock()
		existing, err := config.RequireReadableConfigBeforeWrite(lctx, "")
		if err != nil {
			return false, err
		}
		denormalized, err := denormalizeConfigFromWeb(lctx, incoming)
		if err != nil {
			return false, err
		}
		merged := config.DeepMerge(existing, denormalized)
		changed := approvalModeOf(merged) != approvalModeOf(existing)
		opts := config.SaveOptions{}
		if preserveLanguage {
			opts.PreserveKeys = [][]string{{"display", "language"}}
		}
		return changed, webSaveConfig(lctx, merged, opts)
	}()
	if err != nil {
		return nil, err
	}
	if changed {
		other, err := isOtherProfile(profile)
		if err != nil {
			return nil, err
		}
		if !other {
			broadcastGatewaySessionInfo(ctx)
		}
	}
	return pyjson.ObjectOf("ok", true), nil
}

// Desktop card metadata of a provider env var
func providerCard(d provider.ProviderDescriptor, description string, url any, isPassword bool, advanced any) *pyjson.Object {
	return pyjson.ObjectOf("provider", d.Slug, "provider_label", d.Label, "description", description, "url", url,
		"is_password", isPassword, "advanced", advanced, "category", "provider")
}

// Env vars of the auth types that carry no API key, with the card description each gets
var authTypeEnvVars = map[string][][2]string{
	"aws_sdk": {{"AWS_REGION", "%s (%s)"}, {"AWS_PROFILE", "%s (%s)"}},
	"vertex":  {{"VERTEX_CREDENTIALS_PATH", "%s — service account JSON path (or use ADC)"}},
}

// Card description an auth type env var gets for a provider
func authTypeEnvDescription(format, label, envVar string) string {
	if strings.Count(format, "%s") == 2 {
		return strings.Replace(strings.Replace(format, "%s", label, 1), "%s", envVar, 1)
	}
	return strings.Replace(format, "%s", label, 1)
}

// Provider env var to desktop card metadata derived from the provider catalog
func catalogProviderEnvMetadata() *pyjson.Object {
	nonProviderKeys := map[string]bool{}
	for _, item := range config.OptionalEnvVars.Items() {
		if item.Entry.Category != "" && item.Entry.Category != "provider" {
			nonProviderKeys[item.Name] = true
		}
	}
	meta := pyjson.NewObject()
	profileOf := func(entry *pyjson.Object) *pyjson.Object {
		return pyjson.ObjectOf("provider", entry.GetOr("provider", nil), "provider_label", entry.GetOr("provider_label", nil),
			"description", entry.GetOr("description", nil), "url", entry.GetOr("url", nil),
			"primary", pyrepr.Truthy(entry.GetOr("provider_primary", nil)))
	}
	addProviderEnv := func(envVar string, entry *pyjson.Object) {
		raw, held := meta.Get(envVar)
		if !held {
			meta.Set(envVar, entry)
			return
		}
		existing := raw.(*pyjson.Object)
		if pyjson.Equal(existing.GetOr("provider", nil), entry.GetOr("provider", nil)) {
			return
		}
		if _, ok := existing.Get("provider_profiles"); !ok {
			existing.Set("provider_profiles", []any{profileOf(existing)})
		}
		profiles := existing.GetOr("provider_profiles", nil).([]any)
		for _, p := range profiles {
			if pyjson.Equal(p.(*pyjson.Object).GetOr("provider", nil), entry.GetOr("provider", nil)) {
				return
			}
		}
		existing.Set("provider_profiles", append(profiles, profileOf(entry)))
	}
	for _, d := range provider.ProviderCatalog() {
		if d.Tab != "keys" {
			continue
		}
		for index, envVar := range d.APIKeyEnvVars {
			if nonProviderKeys[envVar] {
				continue
			}
			var signup any
			if d.SignupURL != "" {
				signup = d.SignupURL
			}
			entry := providerCard(d, d.Description, signup, true, false)
			entry.Set("provider_primary", index == 0)
			addProviderEnv(envVar, entry)
		}
		if d.BaseURLEnvVar != "" {
			if _, held := meta.Get(d.BaseURLEnvVar); !held {
				meta.Set(d.BaseURLEnvVar, providerCard(d, d.Label+" base URL override", nil, false, true))
			}
		}
		for _, pair := range authTypeEnvVars[d.AuthType] {
			envVar := pair[0]
			existing := pyjson.NewObject()
			if raw, ok := meta.Get(envVar); ok {
				existing = raw.(*pyjson.Object)
			}
			description := ""
			if s, ok := existing.GetOr("description", nil).(string); ok {
				description = s
			}
			if !pyrepr.Truthy(existing.GetOr("description", nil)) {
				description = authTypeEnvDescription(pair[1], d.Label, envVar)
			}
			meta.Set(envVar, providerCard(d, description, existing.GetOr("url", nil), false, existing.GetOr("advanced", true)))
		}
	}
	return meta
}

// Env var rows of the Keys page, catalog vars and the custom keys of the scoped .env included
func getEnvVarsSync(ctx context.Context, profile string) (*pyjson.Object, error) {
	var envOnDisk map[string]string
	var envOrder []string
	if _, err := scopedToThread(ctx, profile, func(ctx context.Context) (struct{}, error) {
		envOnDisk = webLoadEnv(ctx)
		envOrder = config.EnvKeyOrder(home.Dir(ctx))
		return struct{}{}, nil
	}); err != nil {
		return nil, err
	}
	channelKeys := channelManagedEnvKeys(ctx)
	catalogMeta := catalogProviderEnvMetadata()
	row := func(varName string, info *config.EnvVar, custom bool) *pyjson.Object {
		var value any
		if v, ok := envOnDisk[varName]; ok {
			value = v
		}
		catMeta := pyjson.NewObject()
		if raw, ok := catalogMeta.Get(varName); ok {
			catMeta = raw.(*pyjson.Object)
		}
		description := catMeta.GetOr("description", "")
		url := catMeta.GetOr("url", nil)
		category := catMeta.GetOr("category", "")
		isPassword := catMeta.GetOr("is_password", false)
		tools := []any{}
		advanced := catMeta.GetOr("advanced", false)
		if info != nil {
			if info.Description != "" {
				description = info.Description
			}
			if info.URL != "" {
				url = info.URL
			}
			if info.Category != "" {
				category = info.Category
			}
			if !info.PasswordOmitted {
				isPassword = info.Password
			}
			tools = stringsAny(info.Tools)
			if info.Advanced {
				advanced = true
			}
		}
		return pyjson.ObjectOf(
			"is_set", pyrepr.Truthy(value),
			"redacted_value", redactedCredentialPreview(value),
			"description", description,
			"url", url,
			"category", category,
			"is_password", isPassword,
			"tools", tools,
			"advanced", advanced,
			"channel_managed", channelKeys[varName],
			"provider", catMeta.GetOr("provider", ""),
			"provider_label", catMeta.GetOr("provider_label", ""),
			"provider_profiles", catMeta.GetOr("provider_profiles", []any{}),
			"provider_primary", pyrepr.Truthy(catMeta.GetOr("provider_primary", false)),
			"custom", custom,
		)
	}
	result := pyjson.NewObject()
	for _, item := range config.OptionalEnvVars.Items() {
		info := item.Entry
		result.Set(item.Name, row(item.Name, &info, false))
	}
	for _, varName := range catalogMeta.Keys {
		if _, held := result.Get(varName); !held {
			result.Set(varName, row(varName, nil, false))
		}
	}
	for _, varName := range envKeysInOrder(envOnDisk, envOrder) {
		if _, held := result.Get(varName); held || channelKeys[varName] {
			continue
		}
		r := row(varName, nil, true)
		r.Set("category", "custom")
		r.Set("is_password", true)
		result.Set(varName, r)
	}
	return result, nil
}

// Keys of a loaded env mapping in file order, keys the order misses following sorted
func envKeysInOrder(env map[string]string, order []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, k := range order {
		if _, ok := env[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	var rest []string
	for k := range env {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Saves a provider env credential in the scoped home and counts a new API key as a provider setup
func saveEnvCredential(ctx context.Context, key, value string, providerSetup bool) (*pyjson.Object, error) {
	var previous any
	if v, ok := webLoadEnv(ctx)[key]; ok {
		previous = v
	}
	result, err := provider.SaveProviderEnvCredential(ctx, home.Dir(ctx), key, value)
	if err != nil {
		return nil, err
	}
	observability.RecordAPIKeySaved(ctx, key, value, previous, observability.WebSetupSurface(), providerSetup)
	return pyjson.ObjectOf("ok", result.OK, "key", result.Key, "config_updates", stringsAny(result.ConfigUpdates)), nil
}

// Response body of a provider credential removal
func credentialRemovalObject(r provider.ProviderEnvCredentialRemoval) *pyjson.Object {
	return pyjson.ObjectOf("ok", r.OK, "key", r.Key, "removed", r.Removed, "pool_pruned", stringsAny(r.PoolPruned),
		"providers", stringsAny(r.Providers), "config_scrubbed", stringsAny(r.ConfigScrubbed), "found", r.Found)
}

// Request value of a body field that pydantic validated as a string, empty for None
func bodyStr(body *pyjson.Object, key string) string {
	s, _ := body.GetOr(key, nil).(string)
	return s
}

// Stripped request value of a string field
func bodyStrip(body *pyjson.Object, key string) string { return pystr.Strip(bodyStr(body, key)) }
