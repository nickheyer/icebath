package tuiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var hubSetupRegistry = &handlerRegistry{}

func init() {
	for _, m := range []struct {
		name string
		body rpcBody
	}{
		{tuigateway.SetupKeys.Name(), setupKeys},
		{tuigateway.SetupSaveKey.Name(), setupSaveKey},
		{tuigateway.SetupFacts.Name(), setupFacts},
		{tuigateway.SetupToolsets.Name(), setupToolsets},
	} {
		scopedRPC(hubSetupRegistry, m.name, 5180, rpcShape{}, m.body)
	}
	hubSetupRegistry.install()
}

// Chat app a key of the catalog belongs to, the longest app name its name starts with, empty for none
func setupKeyApp(name string, apps []string) string {
	best := ""
	for _, app := range apps {
		if strings.HasPrefix(name, strings.ToUpper(app)+"_") && len(app) > len(best) {
			best = app
		}
	}
	return best
}

// Row of one key of the catalog, its value only read to say whether it is set
func setupKeyRow(ctx context.Context, h, name string, v config.EnvVar, required bool, apps []string) (tuigateway.SetupKey, error) {
	value, _, err := config.GetEnvValue(ctx, h, name)
	if err != nil {
		return tuigateway.SetupKey{}, err
	}
	row := tuigateway.SetupKey{Name: name, Label: firstText(v.Prompt, name), Help: firstText(v.Description, v.Help), URL: v.URL, Set: strings.TrimSpace(value) != "",
		Secret: v.Password, Category: v.Category, Tools: v.Tools, Required: required, Advanced: v.Advanced}
	if v.Category == "messaging" {
		row.App = setupKeyApp(name, apps)
	}
	return row, nil
}

// First of the texts that is not empty
func firstText(texts ...string) string {
	for _, t := range texts {
		if strings.TrimSpace(t) != "" {
			return t
		}
	}
	return ""
}

// Entry of the catalog of a key, whether it is required and whether the catalog knows it
func setupKeyEntry(name string) (config.EnvVar, bool, bool) {
	if v, ok := config.RequiredEnvVars[name]; ok {
		return v, true, true
	}
	v, ok := config.OptionalEnvVars.Get(name)
	return v, false, ok
}

// Keys of the catalog the params ask for, required ones first, names the catalog does not know left out
func setupKeys(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SetupKeys.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	names := p.Names
	if len(names) == 0 {
		names = append(config.SortedEnvNames(config.RequiredEnvVars), config.OptionalEnvVars.Names()...)
	}
	apps := gateway.MessagingPlatformLabels()
	out := tuigateway.SetupKeysResult{Keys: []tuigateway.SetupKey{}}
	for _, name := range names {
		v, required, known := setupKeyEntry(name)
		if !known {
			continue
		}
		if (p.Category != "" && v.Category != p.Category) || (v.Advanced && !p.Advanced && len(p.Names) == 0) {
			continue
		}
		row, err := setupKeyRow(ctx, h, name, v, required, apps)
		if err != nil {
			return nil, err
		}
		if p.Missing && row.Set {
			continue
		}
		out.Keys = append(out.Keys, row)
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Keeps a key of the catalog in the home's .env, or takes it out for an empty value
func setupSaveKey(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SetupSaveKey.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	v, required, known := setupKeyEntry(p.Name)
	if !known {
		return rpcErr(rid, 4001, fmt.Sprintf("unknown key %s", p.Name), nil), nil
	}
	h := home.Dir(ctx)
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install, the keys are read-only", nil), nil
	}
	if value := strings.TrimSpace(p.Value); value != "" {
		err = config.SaveEnvValueContext(ctx, h, p.Name, value)
	} else {
		_, err = config.RemoveEnvValue(home.EnvPath(h), p.Name)
	}
	if err != nil {
		return nil, err
	}
	row, err := setupKeyRow(ctx, h, p.Name, v, required, gateway.MessagingPlatformLabels())
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.SetupSaveKeyResult{Key: row})), nil
}

// Facts of this computer and home by name
func setupFacts(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SetupFacts.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	out := tuigateway.SetupFactsResult{Facts: map[string]bool{}}
	for _, name := range p.Names {
		holds, err := setupFact(ctx, h, cfg, name)
		if err != nil {
			return rpcErr(rid, 4001, err.Error(), nil), nil
		}
		out.Facts[name] = holds
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Tool groups of a platform with which are on and whether the keys of each are there
func setupToolsets(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SetupToolsets.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	platform := firstText(p.Platform, "cli")
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	enabled := tools.Default.PlatformTools(ctx, "", cfg, platform, tools.PlatformToolsOptions{})
	keyed := map[string][]string{}
	for _, item := range config.OptionalEnvVars.Items() {
		for _, tool := range item.Entry.Tools {
			keyed[tool] = append(keyed[tool], item.Name)
		}
	}
	out := tuigateway.SetupToolsetsResult{Toolsets: []tuigateway.SetupToolset{}}
	for _, ts := range tools.EffectiveConfigurableToolsets(ctx) {
		ready, err := tools.ToolsetHasKeys(ctx, ts.Key, cfg)
		if err != nil {
			return nil, err
		}
		// The catalog's labels open with an emoji; the screen shows the words
		label := strings.TrimSpace(strings.TrimLeftFunc(ts.Label, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
		row := tuigateway.SetupToolset{Name: ts.Key, Label: label, Detail: ts.Description, On: enabled[ts.Key], Ready: ready}
		for _, tool := range tools.Default.ResolveToolset(ctx, "", ts.Key) {
			for _, name := range keyed[tool] {
				if !slices.Contains(row.Keys, name) {
					row.Keys = append(row.Keys, name)
				}
			}
		}
		out.Toolsets = append(out.Toolsets, row)
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
