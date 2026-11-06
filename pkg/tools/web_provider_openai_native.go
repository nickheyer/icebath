package tools

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const openAINativeUnsupportedMsg = "openai-native declares OpenAI's server-side web_search tool; it cannot run as a " +
	"client-side search and requires the Codex Responses transport (provider " +
	"openai-codex). For client-side search use firecrawl (default) or another backend."

// Value under key of a decoded dict, nil for every other type
func dget(obj any, key string) any {
	if d, ok := pyDict(obj); ok {
		v, _ := d.Get(key)
		return v
	}
	return nil
}

// Stripped str of a value or empty for a falsy one, the str(x or "").strip() idiom
func pyStrOrEmptyStripped(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Test seam for the codex credential probe
var hasCodexCredentials = probeCodexCredentials

// Cheap openai-codex login probe over auth.json, the providers singleton then the credential pool
func probeCodexCredentials(env *Env) bool {
	h := home.Process()
	if env != nil && env.Home != "" {
		h = env.Home
	}
	raw, err := os.ReadFile(home.AuthPath(h))
	if err != nil {
		return false
	}
	if !utf8.Valid(raw) {
		return false
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	store, err := pyjson.Loads(text)
	if err != nil {
		return false
	}
	tokens := dget(dget(dget(store, "providers"), "openai-codex"), "tokens")
	if pyStrOrEmptyStripped(dget(tokens, "access_token")) != "" {
		return true
	}
	entries, ok := dget(dget(store, "credential_pool"), "openai-codex").([]any)
	if !ok {
		return false
	}
	for _, e := range entries {
		if d, ok := pyDict(e); ok {
			token, found := d.Get("access_token")
			if !found {
				token = ""
			}
			if pyStrOrEmptyStripped(token) != "" {
				return true
			}
		}
	}
	return false
}

type openAINativeProvider struct{ baseWebProvider }

// Available when an openai-codex login is likely usable
func (p *openAINativeProvider) Available(env *Env) bool { return hasCodexCredentials(env) }

// Reached only when the transport cannot host the server side tool, so it fails loudly
func (p *openAINativeProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return searchFail(openAINativeUnsupportedMsg), nil
}

// SetupSchema is the icebath tools picker entry
func (p *openAINativeProvider) SetupSchema() map[string]any {
	return setupSchema(p.display, "native",
		"Search runs on the provider side (needs the Codex Responses transport + an openai-codex login); search only, extraction still uses another backend",
		"", "", "", nil)
}

func init() {
	RegisterWebProvider(&openAINativeProvider{baseWebProvider{name: "openai-native", display: "OpenAI Native Web Search (Codex Responses)"}})
}
