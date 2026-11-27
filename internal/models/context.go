package models

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Window assumed when nothing knows the model's
	DefaultContext = 256_000
	// Least window a session, a model switch or a job accepts
	MinimumContext = 64_000
)

// Windows of model families, for when nothing that knows the provider knows the model; the longest name the model holds wins
var families = []family{
	{"claude-fable-5", 1000000}, {"claude-fable", 1000000}, {"claude-opus-5", 1000000}, {"claude-sonnet-5", 1000000},
	{"claude-opus-4-8", 1000000}, {"claude-opus-4.8", 1000000}, {"claude-opus-4-7", 1000000}, {"claude-opus-4.7", 1000000},
	{"claude-opus-4-6", 1000000}, {"claude-sonnet-4-6", 1000000}, {"claude-opus-4.6", 1000000}, {"claude-sonnet-4.6", 1000000},
	{"claude", 200000},
	{"gpt-6-astra", 1050000},
	{"gpt-6.1-sol", 1050000}, {"gpt-6.1-terra", 1050000},
	{"gpt-6-sol", 1050000}, {"gpt-6-luna", 1050000},
	{"gpt-5.6-luna", 1050000}, {"gpt-5.6-terra", 1050000}, {"gpt-5.6-sol", 1050000}, {"gpt-5.5", 1050000},
	{"gpt-5.4-nano", 400000}, {"gpt-5.4-mini", 400000}, {"gpt-5.4", 1050000},
	{"gpt-5.3-codex-spark", 128000}, {"gpt-5.1-chat", 128000}, {"gpt-5", 400000},
	{"gpt-4.1", 1047576}, {"gpt-4", 128000},
	{"gemini", 1048576},
	{"gemma-4", 256000}, {"gemma4", 256000}, {"gemma-4-31b", 256000}, {"gemma-3", 131072}, {"gemma", 8192},
	{"deepseek-v4-pro", 1_000_000}, {"deepseek-v4.1-flash", 1_000_000}, {"deepseek-v4-flash", 1_000_000}, {"deepseek-chat", 1_000_000},
	{"deepseek-reasoner", 1_000_000}, {"deepseek-flash", 1_000_000}, {"deepseek", 128000},
	{"llama", 131072}, {"muse-spark-1.3", 1_048_576}, {"muse-spark", 1_048_576}, {"inkling", 1_048_576},
	{"qwen3.8-max", 1_000_000}, {"qwen3.8-flash", 1_000_000}, {"qwen3.6-plus", 1048576}, {"qwen3.7-plus", 1048576},
	{"qwen3-coder-plus", 1000000}, {"qwen3-coder", 262144}, {"qwen3-max", 262144}, {"qwen", 131072},
	{"minimax-m3", 1000000}, {"minimax", 204800},
	{"glm-5.3", 1_310_720}, {"glm-5.3-flash", 1_310_720}, {"glm-5.3:batch", 1_048_576}, {"glm-5.3:us", 1_048_576},
	{"glm-5.3-flash:batch", 1_048_576}, {"glm-5.3-flash:us", 1_048_576}, {"glm-5.3-flashx", 1_048_576},
	{"glm-5.2", 1_048_576}, {"glm-5.2:free", 256_000},
	{"glm-5.1", 204_800}, {"glm-5-turbo", 202752}, {"glm-5v-turbo", 202752}, {"glm-5", 204_800},
	{"glm-4.7-flash", 202752}, {"glm-4.7", 204_800}, {"glm-4.6v", 131072}, {"glm-4.6", 204_800}, {"glm", 202752},
	{"grok-composer", 200000}, {"grok-build-latest", 500000}, {"grok-build", 256000}, {"grok-code-fast", 256000},
	{"grok-2-vision", 8192}, {"grok-4-fast", 2000000}, {"grok-4.20", 2000000},
	{"grok-4.6", 500000}, {"grok-4.5", 500000}, {"grok-4.3", 1000000}, {"grok-4", 256000},
	{"grok-3", 131072}, {"grok-2", 131072}, {"grok", 131072},
	{"kimi-k3", 1_048_576}, {"kimi", 262144},
	{"solar-open2", 262144}, {"solar-pro3", 131072}, {"solar-pro2", 65536}, {"solar-mini", 32768}, {"solar-", 524288},
	{"hy4-preview", 1_048_576}, {"hy3-preview", 262144}, {"hy3", 262144},
	{"x-preview-f", 1_048_576}, {"ox-alpha", 1_048_576}, {"union-alpha", 262144},
	{"nemotron-3.5-lightning", 1_000_000}, {"nemotron-3.5-super-vl", 1_000_000}, {"nemotron", 131072},
	{"laguna-s-2.1", 262144}, {"laguna-xs-2.1", 262144}, {"trinity", 262144}, {"elephant", 262144},
	{"Qwen/Qwen3.5-397B-A17B", 131072}, {"Qwen/Qwen3.5-35B-A3B", 131072}, {"deepseek-ai/DeepSeek-V3.2", 65536},
	{"moonshotai/Kimi-K2.5", 262144}, {"moonshotai/Kimi-K2.6", 262144}, {"moonshotai/Kimi-K2-Thinking", 262144},
	{"MiniMaxAI/MiniMax-M2.5", 204800}, {"XiaomiMiMo/MiMo-V2-Flash", 262144},
	{"mimo-v2.6-pro-ultraspeed", 1048576}, {"mimo-v2.6-pro", 1048576}, {"mimo-v2.6-flash", 1048576},
	{"mimo-v2-pro", 1048576}, {"mimo-v2.5-pro", 1048576}, {"mimo-v2.5", 1048576}, {"mimo-v2-omni", 262144}, {"mimo-v2-flash", 262144},
	{"zai-org/GLM-5", 202752},
}

type family struct {
	key    string
	window int
}

// Families whose generation number is part of their name, matched only up to a separator after it
var generations = regexp.MustCompile(`(solar-[a-z]+)-(\d{1,2})([-:.@]|$)`)

// Whether the model is of the family: the family's name held in the model's, versions spelled with dots or dashes alike
func (f family) of(model string) bool {
	key := strings.ToLower(f.key)
	if strings.HasPrefix(key, "solar-") {
		model = generations.ReplaceAllString(model, "${1}${2}${3}")
		if strings.HasSuffix(key, "-") {
			tail := model[strings.LastIndex(model, "/")+1:]
			return regexp.MustCompile("^" + regexp.QuoteMeta(key) + "[a-z]").MatchString(tail)
		}
		return regexp.MustCompile(regexp.QuoteMeta(key) + `([-:.@]|$)`).MatchString(model)
	}
	return strings.Contains(model, key) || strings.Contains(strings.ReplaceAll(model, ".", "-"), strings.ReplaceAll(key, ".", "-"))
}

// Family the model belongs to, the longest name it holds winning and earlier ones among the same length
func familyOf(model string) (family, bool) {
	sorted := slices.Clone(families)
	slices.SortStableFunc(sorted, func(a, b family) int { return len(b.key) - len(a.key) })
	lower := strings.ToLower(model)
	for _, f := range sorted {
		if f.of(lower) {
			return f, true
		}
	}
	return family{}, false
}

// Families given their own window only after the model was known through a shorter one, so a window kept for it from then is stale
var lateFamilies = []string{"minimax-m3", "muse-spark-1.3", "muse-spark", "grok-4.3", "grok-4.6", "grok-4-fast", "grok-4.20", "qwen3.6-plus",
	"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro", "deepseek-chat", "deepseek-reasoner"}

// Whether a kept window is what the model got through a shorter family before its own family had a window
func staleWindow(model string, kept int) bool {
	lower := strings.ToLower(model)
	var matches []family
	for _, f := range families {
		if f.of(lower) {
			matches = append(matches, f)
		}
	}
	if len(matches) == 0 {
		return false
	}
	own := matches[0]
	for _, f := range matches[1:] {
		if len(f.key) > len(own.key) {
			own = f
		}
	}
	if !slices.Contains(lateFamilies, own.key) || kept >= own.window {
		return false
	}
	shorter, any := 0, false
	for _, f := range matches {
		if len(f.key) < len(own.key) && (!any || f.window > shorter) {
			shorter, any = f.window, true
		}
	}
	if !any {
		shorter = DefaultContext
	}
	return kept <= shorter
}

// Whether the model's family is known to be reported with 32K windows it doesn't have
func underreported(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "kimi") || strings.Contains(lower, "moonshot") || strings.HasPrefix(lower, "minimax") || strings.Contains(lower, "minimaxai/")
}

// How long a window found for a route is used before it is found again
const windowTTL = 5 * time.Minute

type windowKey struct{ provider, kind, model, base, key string }

type windowHit struct {
	window int
	at     time.Time
}

// Tokens the route's model takes in at once: what the profile says of it, what the route's kind or endpoint states, what was found before,
// what the models.dev registry and public directories list, what a server of this machine reports, the family's window, else the default.
// What was found is used for five minutes, or until a window is kept or forgotten
func (m *Models) ContextLength(ctx context.Context, cfg *config.Config, route provider.Route) int {
	s := m.of(cfg)
	key := windowKey{route.Provider, route.Spec, route.Model, strings.TrimRight(route.BaseURL, "/"), fingerprint(route.APIKey)}
	s.mu.Lock()
	hit, ok := s.windows[key]
	s.mu.Unlock()
	if ok && time.Since(hit.at) < windowTTL {
		return hit.window
	}
	n := m.contextLength(ctx, cfg, route)
	s.mu.Lock()
	s.windows[key] = windowHit{n, time.Now()}
	s.mu.Unlock()
	return n
}

func (m *Models) contextLength(ctx context.Context, cfg *config.Config, route provider.Route) int {
	model, base, key := route.Model, strings.TrimRight(strings.TrimSpace(route.BaseURL), "/"), route.APIKey
	spec := m.Spec(route)
	if instance, ok := m.providers.Instance(cfg, route.Provider); ok {
		if n := instance.Settings.GetInt(providerconfig.ContextLength); n > 0 {
			return n
		}
	}
	if n, ok := m.overrideInt(cfg, m.explicit(cfg, route), "context_window"); ok {
		return n
	}
	if parsed, err := url.Parse(base); base != "" && (err != nil || parsed.Port() != "" && !digits(parsed.Port())) {
		base = ""
	}
	if strings.TrimSpace(model) == "" {
		slog.Info(fmt.Sprintf("No model id provided for context length resolution — defaulting to %s tokens.", strs.Thousands(DefaultContext)))
		return DefaultContext
	}
	model = bareModel(m, model)
	if variant := variantBase(model); variant != "" && routed(route, spec) {
		slog.Debug("resolving context length for an OpenRouter routing variant via base id", "model", model, "base", variant)
		model = variant
	}
	route.Model, route.BaseURL = model, base
	if spec.ContextLength != nil {
		if n := spec.ContextLength(model, base); n > 0 {
			return n
		}
	}
	proxy := route.Mode == provider.ModeCodexResponses && m.Kind(base) == ""
	if base != "" && !spec.ProbeAlways && !proxy {
		if kept, ok := m.CachedContext(cfg, model, base); ok {
			if n := m.validated(ctx, cfg, model, base, kept, key); n > 0 {
				return n
			}
		}
	}
	if n, ok := m.Registry.sized(ctx, m.env(cfg, route, false)); ok {
		return n
	}
	if spec.ListsContext {
		if n, ok := m.endpointContext(ctx, cfg, model, cmp.Or(base, spec.BaseURL), key); ok {
			if base != "" {
				m.SaveContext(cfg, model, base, n)
			}
			return n
		}
	}
	if base != "" && !provider.BaseHostMatches(base, "openrouter.ai") && m.Kind(base) == "" && !spec.Included {
		return m.endpointWindow(ctx, cfg, route, spec)
	}
	effective := route.Spec
	if base != "" && (effective == "" || spec.Aggregator || spec.BaseURL == "") {
		effective = cmp.Or(m.Kind(base), effective)
	}
	if base != "" {
		if inferred := m.Kind(base); inferred == "" || strings.Contains(inferred, "ollama") {
			if n := m.ollamaShow(ctx, cfg, model, base, key); n > 0 {
				if !spec.ProbeAlways {
					m.SaveContext(cfg, model, base, n)
				}
				return n
			}
		}
	}
	if effective != "" {
		lookup := route
		lookup.Spec = effective
		if n, ok := m.devContext(cfg, lookup); ok && n > 0 {
			if f, ok := familyOf(model); ok && f.key == "minimax-m3" && n < f.window {
				slog.Info(fmt.Sprintf("Rejecting models.dev context=%d for %q (MiniMax-M3 underreport); using hardcoded default %s", n, model, strs.Thousands(f.window)))
				n = f.window
			}
			return n
		}
	}
	if effective == "" {
		if row, ok := m.Registry.lookup(ctx, m.env(cfg, route, false), model); ok && row.Context > 0 {
			if row.Context == 32768 && underreported(model) {
				slog.Info(fmt.Sprintf("Rejecting OpenRouter metadata context=%d for %q (known 32K underreport); falling through to hardcoded defaults", row.Context, model))
			} else {
				return row.Context
			}
		}
	}
	if base != "" && Local(base) {
		if n := m.localProbe(ctx, cfg, model, base, key, spec); n > 0 {
			return n
		}
	}
	if f, ok := familyOf(model); ok {
		return f.window
	}
	m.defaulted(cfg, model, base)
	return DefaultContext
}

// Window of a model on an endpoint no kind claims: its listing, a probe of a server of this machine, Ollama's show, else the family's
func (m *Models) endpointWindow(ctx context.Context, cfg *config.Config, route provider.Route, spec provider.Spec) int {
	model, base, key := route.Model, route.BaseURL, route.APIKey
	if n, ok := m.endpointContext(ctx, cfg, model, base, key); ok {
		return n
	}
	if Local(base) {
		if n := m.localProbe(ctx, cfg, model, base, key, spec); n > 0 {
			return n
		}
	}
	if n := m.ollamaShow(ctx, cfg, model, base, key); n > 0 {
		if !spec.ProbeAlways {
			m.SaveContext(cfg, model, base, n)
		}
		return n
	}
	slog.Info(fmt.Sprintf("Could not detect context length for model %q at %s — defaulting to %s tokens (probe-down). Set the provider's context_length to override.",
		model, base, strs.Thousands(DefaultContext)))
	if f, ok := familyOf(model); ok {
		slog.Info(fmt.Sprintf("Using hardcoded context length %s for model %q (custom endpoint, catalog match on %q)", strs.Thousands(f.window), model, f.key))
		return f.window
	}
	m.defaulted(cfg, model, base)
	return DefaultContext
}

// Window a server of this machine reports, kept on disk when it is at least the least a session accepts
func (m *Models) localProbe(ctx context.Context, cfg *config.Config, model, base, key string, spec provider.Spec) int {
	n := m.localWindow(ctx, cfg, model, base, key)
	if n > 0 && !spec.ProbeAlways && n >= MinimumContext {
		m.SaveContext(cfg, model, base, n)
	}
	return n
}

// Warns once for each model and endpoint that the default window stands in for one nothing knows
func (m *Models) defaulted(cfg *config.Config, model, base string) {
	s := m.of(cfg)
	s.mu.Lock()
	seen := s.defaulted[model+"\x00"+base]
	s.defaulted[model+"\x00"+base] = true
	s.mu.Unlock()
	if !seen {
		slog.Warn(fmt.Sprintf("Could not determine context length for model %q (base_url=%s) — falling back to %s tokens. Set the provider's context_length to override.",
			model, cmp.Or(base, "default"), strs.Thousands(DefaultContext)))
	}
}

// Window kept for the model, kept as it is, dropped when it is known to be wrong, or replaced by what a server of this machine reports now;
// 0 to find it again
func (m *Models) validated(ctx context.Context, cfg *config.Config, model, base string, kept int, key string) int {
	switch {
	case kept <= 0:
		slog.Warn(fmt.Sprintf("Dropping non-positive cache entry %s@%s -> %d; re-resolving", model, base, kept))
	case kept <= 32768 && underreported(model):
		slog.Info(fmt.Sprintf("Dropping stale cached context entry %s@%s -> %s (known 32K underreport); re-resolving via hardcoded defaults", model, base, strs.Thousands(kept)))
	case staleWindow(model, kept):
		slog.Info(fmt.Sprintf("Dropping stale pre-catalog cache entry %s@%s -> %s; re-resolving via hardcoded defaults", model, base, strs.Thousands(kept)))
	case Local(base):
		live := m.localWindow(ctx, cfg, model, base, key)
		if live <= 0 || live == kept {
			return kept
		}
		if live < MinimumContext {
			slog.Info(fmt.Sprintf("Live local probe for %s@%s reports %s (< minimum %s); invalidating stale cache — agent init should reject", model, base,
				strs.Thousands(live), strs.Thousands(MinimumContext)))
		} else {
			slog.Info(fmt.Sprintf("Reconciling stale local cache entry %s@%s: %s -> %s (live probe)", model, base, strs.Thousands(kept), strs.Thousands(live)))
		}
		m.ForgetContext(cfg, model, base)
		if live >= MinimumContext {
			m.SaveContext(cfg, model, base, live)
		}
		return live
	default:
		return kept
	}
	m.ForgetContext(cfg, model, base)
	return 0
}

func digits(text string) bool {
	return text != "" && strings.Trim(text, "0123456789") == ""
}

const contextCache = "context_length_cache.yaml"

// Windows found for models on endpoints, kept across runs under model@base
type contextFile struct {
	Lengths map[string]int `yaml:"context_lengths"`
}

// Key of a model on an endpoint, and the spellings an older entry may have
func cacheKeys(model, base string) []string {
	key := model + "@" + strings.TrimRight(base, "/")
	return []string{key, model + "@" + base, key + "/"}
}

func (s *state) readContexts() map[string]int {
	raw, err := os.ReadFile(s.file(contextCache))
	if err != nil {
		return map[string]int{}
	}
	var file contextFile
	if err := yaml.Unmarshal(bytes.TrimPrefix(raw, []byte("\uFEFF")), &file); err != nil {
		slog.Debug(fmt.Sprintf("Failed to load context length cache: %s", err))
	}
	if file.Lengths == nil {
		file.Lengths = map[string]int{}
	}
	return file.Lengths
}

// Changes the kept windows under the lock other processes take too, skipped when it can't be had in five seconds
func (s *state) changeContexts(change func(lengths map[string]int) bool) {
	s.contexts.Lock()
	defer s.contexts.Unlock()
	path := s.file(contextCache)
	release, err := fsx.Lock(context.Background(), path+".lock", 5*time.Second)
	if err != nil {
		slog.Debug("Skipping context length cache update", "error", err)
		return
	}
	defer release()
	lengths := s.readContexts()
	if !change(lengths) {
		return
	}
	data, err := yaml.Marshal(contextFile{Lengths: lengths})
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Debug("failed to save context length cache", "error", err)
	}
}

// Forgets the windows found for routes, so they are found again
func (s *state) forgetWindows() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.windows)
}

// Window kept for the model on the endpoint
func (m *Models) CachedContext(cfg *config.Config, model, base string) (int, bool) {
	lengths := m.of(cfg).readContexts()
	for _, key := range cacheKeys(model, base) {
		if n, ok := lengths[key]; ok {
			return n, true
		}
	}
	return 0, false
}

// Keeps a window found for the model on the endpoint, such as one a provider's refusal stated
func (m *Models) SaveContext(cfg *config.Config, model, base string, n int) {
	keys := cacheKeys(model, base)
	if n <= 0 {
		slog.Warn(fmt.Sprintf("Refusing to cache non-positive context length %s -> %d tokens", keys[0], n))
		return
	}
	s := m.of(cfg)
	s.forgetWindows()
	s.changeContexts(func(lengths map[string]int) bool {
		if lengths[keys[0]] == n {
			return false
		}
		lengths[keys[0]] = n
		slog.Info(fmt.Sprintf("Cached context length %s -> %s tokens", keys[0], strs.Thousands(n)))
		return true
	})
}

// Forgets the window kept for the model on the endpoint, and what servers of this machine said of it lately
func (m *Models) ForgetContext(cfg *config.Config, model, base string) {
	s := m.of(cfg)
	s.forgetWindows()
	bare, stripped := bareModel(m, model), strings.TrimRight(base, "/")
	s.mu.Lock()
	delete(s.probes, probeKey{"", bare, stripped})
	delete(s.probes, probeKey{"ollama_show", bare, stripped})
	s.mu.Unlock()
	s.changeContexts(func(lengths map[string]int) bool {
		changed := false
		for _, key := range cacheKeys(model, base) {
			if _, ok := lengths[key]; ok {
				delete(lengths, key)
				changed = true
			}
		}
		return changed
	})
}
