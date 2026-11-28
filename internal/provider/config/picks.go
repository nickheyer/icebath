package config

import (
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

// Kinds of thing a setting's value names, by the name the providers screen registers a picker under: a setting declares one in Picks
const (
	// A model, kept with the provider that runs it
	PickModel = "model"
	// A provider of the profile, by its name there
	PickProvider = "provider"
	// An account of a provider, by its name under the provider's accounts
	PickAccount = "account"
)

// Kind of thing a setting's Picks names, what follows the kind after a colon left out: model for model:openrouter
func Kind(picks string) string {
	kind, _, _ := strings.Cut(picks, ":")
	return kind
}

// Picks of a model of the kinds of provider, or of the providers named, alone, as a setting declares it: model:openrouter for the models of
// OpenRouter, model:xai,xai-oauth for those of either kind of xAI provider
func ModelOf(kinds ...string) string {
	return PickModel + ":" + strings.Join(kinds, ",")
}

// Kinds of provider or names of providers an ask's Of names, as ModelOf writes them; none for every provider
func Of(of string) []string {
	var out []string
	for _, each := range strings.Split(of, ",") {
		if each = strings.TrimSpace(each); each != "" {
			out = append(out, each)
		}
	}
	return out
}

// Keys of the values a picker is handed in Ask.Values and hands to Done when a screen asks for itself, with no setting. A model picker
// handed an effort asks how hard the picked model thinks too, and hands it back under the same key, empty for the model's own
const (
	AskProvider = "provider"
	AskModel    = "model"
	AskAccount  = "account"
	AskEffort   = "effort"
)

// Provider a key of one provider's settings belongs to, such as anthropic for providers.anthropic.default_model; empty for any other key
func Owner(key string) string {
	parts := strings.Split(strings.ToLower(key), ".")
	if len(parts) < 3 || parts[0] != Key {
		return ""
	}
	return parts[1]
}

// Key of the provider kept with a model or an account at the key, as one choice: beside <prefix>.model, or <prefix>.<word>_model, the key
// <prefix>.provider, or <prefix>.<word>_provider; the same for an account. The prefix is kept as the key spells it, as it may hold a name
// a person gave, such as the entry of a table. Empty for a key of one provider's settings, whose provider is the one it belongs to, and
// for a key that names no model or account
func Beside(key string) string {
	if Owner(key) != "" {
		return ""
	}
	prefix, last := "", key
	if cut := strings.LastIndex(key, "."); cut >= 0 {
		prefix, last = key[:cut+1], key[cut+1:]
	}
	last = strings.ToLower(last)
	for _, kind := range []string{PickModel, PickAccount} {
		switch {
		case last == kind:
			return prefix + PickProvider
		case strings.HasSuffix(last, "_"+kind):
			return prefix + strings.TrimSuffix(last, kind) + PickProvider
		}
	}
	return ""
}

// Item of a list of models, such as fallback models: the provider's name and the model, as provider:model
func Pair(provider, model string) string {
	if provider == "" {
		return model
	}
	return provider + ":" + model
}

// Provider and model of an item of a list of models; no provider for an item that names a model alone
func Split(item string) (provider, model string) {
	item = strings.TrimSpace(item)
	if before, after, ok := strings.Cut(item, ":"); ok && config.Named(before) {
		return before, strings.TrimSpace(after)
	}
	return "", item
}

// Most characters of a key made from a name
const keyLength = 40

// Key a name a person gave is kept under, the one rule for providers and accounts: lower case letters and digits, every other run of
// characters one dash, at most 40 characters; numbered from 2 when taken, the fallback when the name holds no letter or digit
func KeyOf(name, fallback string, taken func(key string) bool) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case out.Len() > 0 && !strings.HasSuffix(out.String(), "-"):
			out.WriteByte('-')
		}
	}
	key := strings.Trim(out.String(), "-")
	if len(key) > keyLength {
		key = strings.TrimRight(key[:keyLength], "-")
	}
	if key == "" {
		key = fallback
	}
	for n, candidate := 2, key; ; n, candidate = n+1, key+"-"+strconv.Itoa(n) {
		if !taken(candidate) {
			return candidate
		}
	}
}
