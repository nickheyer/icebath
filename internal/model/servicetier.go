package model

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

var (
	normalTierWords  = map[string]bool{"": true, "normal": true, "default": true, "standard": true, "off": true, "none": true}
	serviceTierWords = map[string]string{"fast": "priority", "priority": "priority", "on": "priority",
		"ultrafast": "ultrafast", "ultra": "ultrafast", "auto": "auto", "cold": "cold"}
	codexTierWords = map[string]string{"priority": "fast"}
	fastArgAliases = map[string]string{"ultra": "ultrafast"}
)

type UnknownServiceTierError struct{ Word string }

func (e *UnknownServiceTierError) Error() string { return "unknown service tier: " + e.Word }

func ParseServiceTier(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if normalTierWords[value] {
		return ""
	}
	return serviceTierWords[value]
}

func ParseExactServiceTier(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if _, known := serviceTierWords[value]; !normalTierWords[value] && !known {
		return "", &UnknownServiceTierError{Word: value}
	}
	return ParseServiceTier(value), nil
}

func ServiceTierWord(tier string) string {
	switch tier {
	case "priority":
		return "fast"
	case "":
		return "normal"
	}
	return tier
}

func CodexTierWord(tier string) (string, bool) {
	word, ok := codexTierWords[tier]
	return word, ok
}

func FastArgument(word string) string {
	if canonical, ok := fastArgAliases[word]; ok {
		return canonical
	}
	return word
}

func (r *Router) accountCatalogToken(ctx context.Context, accessToken auth.Secret) (auth.Secret, bool, error) {
	if _, ok := auth.JWTClaims(accessToken.Reveal()); ok && !accessToken.Empty() {
		return accessToken, true, nil
	}
	creds, err := r.c.o.Auth.ResolveCodex(ctx, auth.ResolveOptions{ReadOnly: true})
	if err != nil {
		var ae *auth.Error
		if errors.As(err, &ae) {
			return auth.Secret{}, false, nil
		}
		return auth.Secret{}, false, err
	}
	if creds.APIKey.Empty() {
		return auth.Secret{}, false, nil
	}
	return creds.APIKey, true, nil
}

func (r *Router) codexEntry(ctx context.Context, model, baseURL string, accessToken auth.Secret) (wire.Value, string, bool, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = openai.CodexBaseURL
	}
	if !openai.IsOfficialCodexURL(base) {
		return wire.Value{}, base, false, nil
	}
	token, ok, err := r.accountCatalogToken(ctx, accessToken)
	if err != nil || !ok {
		return wire.Value{}, base, false, err
	}
	entries, fresh := r.codexCatalog(ctx, token, base)
	if !fresh {
		return wire.Value{}, base, false, nil
	}
	entry, found := catalogEntry(entries, catalog.BareCodexSlug(model))
	return entry, base, found, nil
}

func (r *Router) CodexAccountOffersTier(ctx context.Context, model, tier, baseURL string, accessToken auth.Secret) (offered, known bool, err error) {
	ctx = r.bind(ctx)
	entry, _, found, err := r.codexEntry(ctx, model, baseURL, accessToken)
	if err != nil || !found {
		return false, false, err
	}
	tiers, ok := advertisedTiers(entry)
	if !ok {
		return false, false, nil
	}
	return tiers[tier], true, nil
}

func (r *Router) DaybreakOffered(ctx context.Context, model, provider, baseURL string, accessToken auth.Secret) (bool, error) {
	ctx = r.bind(ctx)
	if provider == "" || r.c.o.Catalog.NormalizeProvider(ctx, provider) != "openai-codex" {
		return false, nil
	}
	entry, _, found, err := r.codexEntry(ctx, model, baseURL, accessToken)
	if err != nil || !found {
		return false, err
	}
	programs, ok := cyberPrograms(entry)
	return ok && programs[daybreakProgram], nil
}

func (r *Router) UltrafastOffered(ctx context.Context, model, provider, baseURL string, accessToken auth.Secret) (bool, error) {
	ctx = r.bind(ctx)
	if _, ok := r.c.o.Catalog.ResolveFastModeOverrides(ctx, model, provider, baseURL, "ultrafast"); !ok {
		return false, nil
	}
	host := ""
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	onCodex := (provider != "" && r.c.o.Catalog.NormalizeProvider(ctx, provider) == "openai-codex") || host == "chatgpt.com"
	if !onCodex {
		return true, nil
	}
	offered, known, err := r.CodexAccountOffersTier(ctx, model, "ultrafast", baseURL, accessToken)
	if err != nil {
		return false, fmt.Errorf("model: reading the Codex catalog for ultrafast: %w", err)
	}
	return !known || offered, nil
}
