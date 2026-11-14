package model

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

func (x *resolution) raiseIfDeclaredKeyMissing(requested string, rt *Route, ek explicitCreds) error {
	if rt.Provider != "custom" || keyText(rt.Credential) != noKeyPlaceholder {
		return nil
	}
	if keyPresent(ek.key) || rt.Mode == wire.ModeCodexAppServer || rt.Mode == wire.ModeBedrock {
		return nil
	}
	if !x.o().Secrets.Routed(x.ctx) {
		return nil
	}
	label, keyEnv := x.declaredCustomKey(strings.ToLower(strings.TrimSpace(requested)), rt)
	if keyEnv == "" {
		return nil
	}
	if v, err := x.secretTrim(keyEnv); err != nil || v != "" {
		return err
	}
	where, err := x.whereToSetKey(keyEnv)
	if err != nil {
		return err
	}
	return authError(fmt.Sprintf("No API key for %s: it reads %s, which is not set for this profile, so the "+
		"request was not sent. %s", label, keyEnv, where), firstOf(strings.TrimSpace(requested), "custom"), "missing_api_key")
}

func (x *resolution) declaredCustomKey(requested string, rt *Route) (string, string) {
	name := ""
	if requested != "" && requested != "auto" && requested != "custom" {
		name = requested
	}
	var entryFound bool
	var label, keyEnv string
	if name != "" && strings.HasPrefix(rt.Source, "custom_provider:") {
		if e, ok := x.namedCustomEntry(name); ok {
			entryFound, label, keyEnv = true, firstOf(e.DisplayName, e.Name), firstOf(e.KeyEnv, e.APIKeyEnv)
		}
	} else if e, ok := x.owningEntry(rt.BaseURL, name); ok {
		entryFound, label, keyEnv = true, firstOf(e.DisplayName, e.Name), firstOf(e.KeyEnv, e.APIKeyEnv)
	}
	if keyEnv = strings.TrimSpace(keyEnv); entryFound && keyEnv != "" {
		return "custom provider '" + label + "'", keyEnv
	}
	mc := x.model()
	cfgURL := trimURL(mc.BaseURL)
	if cfgURL != "" && cfgURL == trimURL(rt.BaseURL) {
		if declared := strings.TrimSpace(firstOf(mc.KeyEnv, mc.APIKeyEnv)); declared != "" {
			return "the custom endpoint in config.yaml (model.key_env)", declared
		}
	}
	return "", ""
}

func (x *resolution) profileName() string {
	if name, ok := home.ProfileName(x.r.c.o.Home); ok && name != "" {
		return name
	}
	return "default"
}

func (x *resolution) whereToSetKey(envVar string) (string, error) {
	profile := x.profileName()
	shown, err := home.Display(x.r.c.o.Home)
	if err != nil {
		return "", err
	}
	hint := fmt.Sprintf("Add it to profile '%s': run `icebath %sconfig set %s <your key>` (stored in %s/.env).",
		profile, home.ProfileSelector(x.r.c.o.Home), envVar, shown)
	if x.o().Secrets.Routed(x.ctx) && os.Getenv(envVar) != "" {
		hint = fmt.Sprintf("%s is set in this server's environment, but profile '%s' reads keys only from its own .env. %s", envVar, profile, hint)
	}
	return hint, nil
}

func (x *resolution) namedProfileKeyHint(envVars []string) (string, error) {
	var names []string
	for _, v := range envVars {
		if strings.TrimSpace(v) != "" {
			names = append(names, v)
		}
	}
	name, ok := home.ProfileName(x.r.c.o.Home)
	if len(names) == 0 || !ok || name == "default" {
		return "", nil
	}
	where, err := x.whereToSetKey(names[0])
	if err != nil {
		return "", err
	}
	return " " + where, nil
}

func (x *resolution) missingCredentialsSuffix(provider string) string {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return ""
	}
	p, ok := x.o().Providers.Get(x.ctx, name)
	if !ok {
		return ""
	}
	h, ok := p.(providers.CredentialsHinter)
	if !ok {
		return ""
	}
	if hint, ok := h.MissingCredentialsHint(); ok && strings.TrimSpace(hint) != "" {
		return " " + strings.TrimSpace(hint)
	}
	return ""
}

func (r *Router) MissingCredentialsSuffix(ctx context.Context, provider string) string {
	x := &resolution{r: r, ctx: r.bind(ctx)}
	return x.missingCredentialsSuffix(provider)
}
