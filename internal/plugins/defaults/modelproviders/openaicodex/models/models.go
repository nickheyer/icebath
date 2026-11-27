package models

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Catalog and windows of the models a ChatGPT account reaches through Codex, as its backend lists them for the account's token
type catalog struct {
	mu sync.Mutex
	// Windows and largest windows the backend listed for each token and address, and when; an empty listing is tried again sooner
	listed map[string]listing
}

type listing struct {
	windows map[string]int
	largest map[string]int
	// Speed tiers and cyber access programs listed for each slug, a slug listing none of either absent
	tiers, programs map[string]map[string]bool
	// Whether the backend listed any model
	any bool
	at  time.Time
}

const (
	listedTTL   = time.Hour
	unlistedTTL = 5 * time.Minute
	// Tokens whose listings are kept at once
	listedMax = 128
)

// The catalog every request of the kind reads, its listings shared with the speed tiers the provider negotiates
var shared = &catalog{listed: map[string]listing{}}

func Register(registry *models.Registry) {
	registry.Register(shared)
}

// Speed tiers the token's catalog lists for the model, legacy fast as priority; nil when the catalog says nothing of them
func Tiers(ctx context.Context, token, base, model string) map[string]bool {
	return shared.listing(ctx, token, base).tiers[strings.TrimSuffix(strings.TrimSpace(model), VariantSuffix)]
}

// Cyber access programs the token's catalog lists for the model; nil when the catalog says nothing of them
func Programs(ctx context.Context, token, base, model string) map[string]bool {
	return shared.listing(ctx, token, base).programs[strings.TrimSuffix(strings.TrimSpace(model), VariantSuffix)]
}

// Speed tiers an entry lists: its service_tiers' ids, else its additional_speed_tiers with fast as priority; none for an empty list, nil
// when it lists neither or lists them malformed
func tiersOf(entry map[string]any) map[string]bool {
	tiers, isList := entry["service_tiers"].([]any)
	if isList && len(tiers) > 0 {
		out := map[string]bool{}
		for _, item := range tiers {
			object, ok := item.(map[string]any)
			id, named := object["id"].(string)
			if !ok || !named {
				return nil
			}
			out[id] = true
		}
		return out
	}
	if speeds, ok := entry["additional_speed_tiers"].([]any); ok {
		out := map[string]bool{}
		for _, item := range speeds {
			speed, ok := item.(string)
			if !ok {
				return nil
			}
			if speed == "fast" {
				speed = "priority"
			}
			out[speed] = true
		}
		return out
	}
	if isList {
		return map[string]bool{}
	}
	return nil
}

// Cyber access programs an entry lists, nil when it lists none or lists them malformed
func programsOf(entry map[string]any) map[string]bool {
	programs, _ := entry["available_access_programs"].(map[string]any)
	cyber, ok := programs["cyber"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, item := range cyber {
		program, ok := item.(string)
		if !ok {
			return nil
		}
		out[program] = true
	}
	return out
}

// Answered by the catalog below
func (c *catalog) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Models the backend lists for the account, else those the Codex CLI pins or last listed and the curated ones, a stand in
func (c *catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	route := env.Route
	if route.Spec != openaicodexconfig.Name {
		return models.Catalog{}, false
	}
	if route.APIKey != "" {
		if ids := fetchModels(ctx, route.APIKey, route.BaseURL); len(ids) > 0 {
			return models.Catalog{IDs: finalize(ids)}, true
		}
	}
	home := codexHome()
	var ids []string
	if pinned := pinnedModel(home); pinned != "" {
		ids = append(ids, pinned)
	}
	ids = append(ids, cliModels(home)...)
	ids = append(ids, defaults...)
	ids = slices.DeleteFunc(strs.Unique(ids), func(id string) bool { return strings.HasPrefix(strings.ToLower(id), "gpt-6-astra") })
	return models.Catalog{IDs: finalize(ids), Fallback: true}, true
}

// Curated Codex catalog with the newer slugs and 900k variants it stands for
func Curated() []string {
	return finalize(defaults)
}

// Curated Codex catalog, only slugs the ChatGPT backend accepts
var defaults = []string{"gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4-mini", "gpt-5.4", "gpt-5.3-codex-spark"}

// Newer slugs offered when an older one they stand in for is listed
var forward = []struct {
	slug      string
	templates []string
}{
	{"gpt-6-sol", []string{"gpt-5.6-sol", "gpt-5.5"}},
	{"gpt-6-luna", []string{"gpt-5.6-luna", "gpt-5.5"}},
	{"gpt-5.6-sol", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-terra", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-luna", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.5", []string{"gpt-5.4", "gpt-5.4-mini"}},
	{"gpt-5.3-codex-spark", []string{"gpt-5.4", "gpt-5.5"}},
}

// Slugs with the newer ones their listed templates stand for, each eligible one followed by its 900k variant
func finalize(ids []string) []string {
	ordered := strs.Unique(ids)
	for _, f := range forward {
		if !slices.Contains(ordered, f.slug) && slices.ContainsFunc(f.templates, func(t string) bool { return slices.Contains(ordered, t) }) {
			ordered = append(ordered, f.slug)
		}
	}
	var out []string
	for _, id := range ordered {
		out = append(out, id)
		if variant := id + VariantSuffix; Eligible(id) && !slices.Contains(ordered, variant) && !slices.Contains(out, variant) {
			out = append(out, variant)
		}
	}
	return out
}

// Suffix of the picker only variants that opt a model into the window its backend accepts above the one it advertises
const VariantSuffix = "-900k"

var (
	eligible  = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna", "gpt-5.4", "gpt-daybreak-blue-latest", "gpt-6-astra", "gpt-6.1-sol"}
	snapshots = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna"}
	dated     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Lower case slug without a vendor before it
func slug(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return model[strings.LastIndex(model, "/")+1:]
}

// Whether the model has a 900k variant: an eligible slug, or a dated snapshot of one
func Eligible(model string) bool {
	s := slug(model)
	if s == "" || strings.HasSuffix(s, VariantSuffix) {
		return false
	}
	if slices.Contains(eligible, s) {
		return true
	}
	for _, base := range snapshots {
		if rest, ok := strings.CutPrefix(s, base+"-"); ok && dated.MatchString(rest) {
			return true
		}
	}
	return false
}

// Eligible base of a 900k variant, empty for anything else
func variantBase(model string) string {
	s := slug(model)
	base, ok := strings.CutSuffix(s, VariantSuffix)
	if !ok || !Eligible(base) {
		return ""
	}
	return base
}

// Windows of Codex models when the backend says nothing, lower than the direct API's for the same slugs
var windows = []struct {
	key    string
	window int
}{
	{"gpt-6-astra", 272_000}, {"gpt-6.1-sol", 272_000}, {"gpt-6-sol", 272_000}, {"gpt-6-luna", 272_000},
	{"gpt-5.1-codex-max", 272_000}, {"gpt-5.1-codex-mini", 272_000}, {"gpt-5.3-codex", 272_000},
	{"gpt-5.3-codex-spark", 128_000}, {"gpt-5.2-codex", 272_000}, {"gpt-5.4-mini", 272_000},
	{"gpt-5.6-sol", 272_000}, {"gpt-5.6-terra", 272_000}, {"gpt-5.6-luna", 272_000}, {"gpt-daybreak-blue-latest", 272_000},
	{"gpt-5.5", 272_000}, {"gpt-5.4", 272_000}, {"gpt-5.2", 272_000}, {"gpt-5", 272_000},
}

// The window the backend advertises that a verified one may stand in for
const advertised = 272_000

// Window verified above the advertised one for the base of a 900k variant
func verified(model string) (int, bool) {
	base := variantBase(model)
	if base == "" {
		return 0, false
	}
	if slices.Contains([]string{"gpt-5.4", "gpt-daybreak-blue-latest", "gpt-6-astra", "gpt-6.1-sol"}, base) {
		return 900_000, true
	}
	for _, family := range []string{"gpt-5.6", "gpt-6-sol", "gpt-6-luna"} {
		if base == family || strings.HasPrefix(base, family+"-") || strings.HasPrefix(base, family+".") {
			return 900_000, true
		}
	}
	return 0, false
}

// Window of the route's Codex model: what the backend lists for the account, else the fallback table; a 900k variant at its verified
// window, capped by the largest the backend lists. A Codex proxy at an address no kind claims answers from the table, capped the same way
func (c *catalog) ContextLength(ctx context.Context, env *models.Env) (int, bool) {
	route := env.Route
	proxy := route.Mode == provider.ModeCodexResponses && env.Host.Kind(route.BaseURL) == ""
	if route.Spec != openaicodexconfig.Name && !proxy {
		return 0, false
	}
	model := strings.TrimSpace(route.Model)
	if model == "" {
		return 0, false
	}
	lookup := slug(model)
	if base := variantBase(model); base != "" {
		lookup = base
	}
	largest := 0
	if route.APIKey != "" && (!proxy || variantBase(model) != "") {
		listed := c.listing(ctx, route.APIKey, route.BaseURL)
		largest = find(listed.largest, lookup)
		if n := find(listed.windows, lookup); n > 0 && !proxy {
			return c.bump(model, n, largest), true
		}
	}
	for _, row := range slices.SortedStableFunc(slices.Values(windows), func(a, b struct {
		key    string
		window int
	}) int {
		return len(b.key) - len(a.key)
	}) {
		if strings.Contains(lookup, row.key) || strings.Contains(strings.ReplaceAll(lookup, ".", "-"), strings.ReplaceAll(row.key, ".", "-")) {
			n := c.bump(model, row.window, largest)
			if proxy {
				slog.Info(fmt.Sprintf("Using Codex OAuth context length %d for model %q (codex_responses route at %s)", n, model, route.BaseURL))
			}
			return n, true
		}
	}
	return 0, false
}

// Verified window of a 900k variant over the advertised one, capped by the largest the backend lists
func (c *catalog) bump(model string, n, largest int) int {
	raised, ok := verified(model)
	if !ok || n != advertised {
		return n
	}
	if largest > 0 && largest < raised {
		raised = largest
	}
	slog.Debug(fmt.Sprintf("Codex OAuth context for %s: advertised %d raised to live-verified %d", model, n, raised))
	return raised
}

// Window listed for the slug, exactly then ignoring case
func find(listed map[string]int, s string) int {
	if n, ok := listed[s]; ok {
		return n
	}
	for _, key := range slices.Sorted(maps.Keys(listed)) {
		if strings.EqualFold(key, s) {
			return listed[key]
		}
	}
	return 0
}

// Windows the backend lists for the token at the address, kept an hour, five minutes when it listed none
func (c *catalog) listing(ctx context.Context, token, base string) listing {
	if !Probeable(token, base) {
		return listing{}
	}
	sum := sha256.Sum256([]byte(token + "\n" + strings.TrimRight(strings.TrimSpace(base), "/")))
	key := hex.EncodeToString(sum[:])[:16]
	c.mu.Lock()
	held, ok := c.listed[key]
	c.mu.Unlock()
	if ok && (held.any && time.Since(held.at) < listedTTL || !held.any && time.Since(held.at) < unlistedTTL) {
		return held
	}
	found := listing{windows: map[string]int{}, largest: map[string]int{}, tiers: map[string]map[string]bool{}, programs: map[string]map[string]bool{},
		at: time.Now()}
	entries, err := Entries(ctx, token, base)
	if err != nil {
		slog.Debug("Codex /models probe failed", "error", err)
	}
	for _, raw := range entries {
		item, _ := raw.(map[string]any)
		name, _ := item["slug"].(string)
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		found.any = true
		if tiers := tiersOf(item); tiers != nil {
			found.tiers[name] = tiers
		}
		if programs := programsOf(item); programs != nil {
			found.programs[name] = programs
		}
		if window, ok := item["context_window"].(float64); ok && window > 0 {
			found.windows[strings.TrimSpace(name)] = int(window)
			if most, ok := item["max_context_window"].(float64); ok && most > 0 {
				found.largest[strings.TrimSpace(name)] = int(most)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.listed) >= listedMax {
		oldest := ""
		for other, entry := range c.listed {
			if oldest == "" || entry.at.Before(c.listed[oldest].at) {
				oldest = other
			}
		}
		delete(c.listed, oldest)
	}
	c.listed[key] = found
	return found
}

// Catalog the Codex backend lists, at the route's address or the default one
const catalogEndpoint = "https://chatgpt.com/backend-api/codex/models"

// Whether a probe may take the token to the address: chatgpt.com only takes the ChatGPT account's JWTs
func Probeable(token, base string) bool {
	if token == "" {
		return false
	}
	return !provider.BaseHostMatches(cmp.Or(strings.TrimSpace(base), catalogEndpoint), "chatgpt.com") || len(credentials.Claims(token)) > 0
}

// Models of the first catalog address answering with some, newest client version first
func Entries(ctx context.Context, token, base string) ([]any, error) {
	endpoint := catalogEndpoint
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base != "" {
		endpoint = base + "/models"
	}
	headers := AccountHeaders(token)
	headers["Authorization"] = "Bearer " + token
	for _, version := range []string{"99.0.0", "0.0.0"} {
		payload, err := models.Request(ctx, "GET", endpoint+"?client_version="+version, headers, nil, 10*time.Second)
		if models.Status(err) != 0 {
			continue
		}
		if err != nil {
			return nil, err
		}
		if entries, ok := payload["models"].([]any); ok && len(entries) > 0 {
			return entries, nil
		}
	}
	return nil, nil
}

// Visible slugs the backend lists for the account, by their priority then name; none when it refuses or can't be reached
func fetchModels(ctx context.Context, token, base string) []string {
	if !Probeable(token, cmp.Or(base, openaicodexconfig.BaseURL)) {
		return nil
	}
	entries, err := Entries(ctx, token, cmp.Or(base, openaicodexconfig.BaseURL))
	if err != nil {
		slog.Debug("Failed to fetch Codex models from API", "error", err)
		return nil
	}
	return ranked(entries)
}

// Slugs of listed models that are not hidden, by their priority then name
func ranked(entries []any) []string {
	type row struct {
		rank int
		slug string
	}
	var rows []row
	for _, raw := range entries {
		item, _ := raw.(map[string]any)
		name, _ := item["slug"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		if visibility, _ := item["visibility"].(string); strings.EqualFold(strings.TrimSpace(visibility), "hide") || strings.EqualFold(strings.TrimSpace(visibility), "hidden") {
			continue
		}
		rank := 10_000
		if priority, ok := item["priority"].(float64); ok {
			rank = int(priority)
		}
		rows = append(rows, row{rank, strings.TrimSpace(name)})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Or(a.rank-b.rank, strings.Compare(a.slug, b.slug)) })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.slug)
	}
	return strs.Unique(out)
}

// Folder of the Codex CLI: CODEX_HOME, else .codex in the home folder
func codexHome() string {
	if explicit := strings.TrimSpace(os.Getenv("CODEX_HOME")); explicit != "" {
		if rest, ok := strings.CutPrefix(explicit, "~"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, rest)
			}
		}
		return explicit
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	return filepath.Join(home, ".codex")
}

// Model the Codex CLI's config pins at its top level, empty when none
func pinnedModel(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return ""
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.HasPrefix(trimmed, "#") || strings.TrimSpace(name) != "model" {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, `"`):
			if unquoted, err := strconv.Unquote(value[:strings.LastIndex(value, `"`)+1]); err == nil && len(value) >= 2 {
				return strings.TrimSpace(unquoted)
			}
		case strings.HasPrefix(value, "'") && strings.Count(value, "'") >= 2:
			return strings.TrimSpace(value[1 : 1+strings.Index(value[1:], "'")])
		}
		return ""
	}
	return ""
}

// Slugs the Codex CLI last listed, by their priority
func cliModels(home string) []string {
	raw, err := os.ReadFile(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return nil
	}
	var cache map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &cache) != nil {
		return nil
	}
	entries, _ := cache["models"].([]any)
	return ranked(entries)
}

// ChatGPT account claims of a token
func account(token string) map[string]any {
	auth, _ := credentials.Claims(token)["https://api.openai.com/auth"].(map[string]any)
	return auth
}

// Headers the Codex backend takes the account's id and residency from
func AccountHeaders(token string) map[string]string {
	headers := map[string]string{}
	auth := account(token)
	if id, _ := auth["chatgpt_account_id"].(string); id != "" {
		headers["ChatGPT-Account-ID"] = id
	}
	residency, _ := auth["chatgpt_data_residency"].(string)
	if residency == "" {
		residency, _ = auth["chatgpt_compute_residency"].(string)
	}
	if residency = strings.TrimSpace(residency); residency != "" {
		headers["x-openai-internal-codex-residency"] = residency
	}
	return headers
}

// Identity a request to the Codex backend is made under, icebath's own on OpenAI's host and the Codex CLI's on a proxy of it, with the
// account the token signs in
func Headers(token, base string) map[string]string {
	h := map[string]string{"User-Agent": "codex_cli_rs/0.0.0 (Icebath Agent)", "originator": "codex_cli_rs"}
	if provider.OfficialCodex(base) {
		h = map[string]string{"User-Agent": provider.UserAgent("IcebathAgent"), "originator": "icebath-agent"}
	}
	maps.Copy(h, AccountHeaders(token))
	return h
}

// Non secret id of the ChatGPT account and user a token signs in, empty when it names neither
func Identity(token string) string {
	id, _ := account(token)["chatgpt_account_id"].(string)
	subject, _ := credentials.Claims(token)["sub"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(subject) == "" {
		return ""
	}
	return "codex:" + strings.TrimSpace(id) + ":" + strings.TrimSpace(subject)
}
