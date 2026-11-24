package tuiserver

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var hubFallbackRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubFallbackRegistry, tuigateway.FallbackList.Name(), 5191, rpcShape{}, fallbackList)
	scopedRPC(hubFallbackRegistry, tuigateway.FallbackChange.Name(), 5191, rpcShape{}, fallbackChange)
	scopedRPC(hubFallbackRegistry, tuigateway.FallbackCheck.Name(), 5191, rpcShape{}, fallbackCheck)
	addLongHandlers(tuigateway.FallbackCheck.Name())
	hubFallbackRegistry.install()
}

// Refusal of a fallback request that cannot be carried out as asked
func fallbackRefused(rid any, message string) map[string]any {
	return rpcErr(rid, 4191, message, nil)
}

// Text of one field of a chain entry, empty when it holds none
func fallbackField(entry map[string]any, key string) string {
	text, _ := entry[key].(string)
	return strings.TrimSpace(text)
}

// The stored chain and the model it backs up, as the fallback models screen shows them
func fallbackListing(ctx context.Context) (tuigateway.FallbackListResult, error) {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return tuigateway.FallbackListResult{}, err
	}
	primary, err := provider.DescribePrimary(cfg)
	if err != nil {
		return tuigateway.FallbackListResult{}, err
	}
	chain := provider.ReadFallbackChain(cfg)
	out := tuigateway.FallbackListResult{Primary: primary, Entries: make([]tuigateway.FallbackRow, 0, len(chain))}
	for _, entry := range chain {
		out.Entries = append(out.Entries, tuigateway.FallbackRow{Provider: fallbackField(entry, "provider"), Model: fallbackField(entry, "model"), BaseURL: fallbackField(entry, "base_url")})
	}
	return out, nil
}

// Fallback chain of the profile for the fallback models screen
func fallbackList(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	if _, err := tuigateway.FallbackList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := fallbackListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One change to the fallback chain, answered with the chain after it
func fallbackChange(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.FallbackChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	switch p.Verb {
	case "add":
		name, model := strings.TrimSpace(p.Provider), strings.TrimSpace(p.Model)
		if name == "" || model == "" {
			return fallbackRefused(rid, "a fallback needs a provider and a model"), nil
		}
		_, err = provider.AppendFallback(ctx, h, provider.NewFallbackEntry(name, model, strings.TrimSpace(p.BaseURL)))
	case "remove":
		_, _, err = provider.RemoveFallbackAt(ctx, h, int(p.Index))
	case "move":
		_, err = provider.MoveFallback(ctx, h, int(p.Index), int(p.To))
	case "clear":
		err = provider.ClearFallbacks(ctx, h)
	default:
		return fallbackRefused(rid, "unknown fallback change: "+p.Verb), nil
	}
	switch {
	case errors.Is(err, provider.ErrFallbackIsPrimary), errors.Is(err, provider.ErrFallbackInChain), errors.Is(err, provider.ErrFallbackNoEntry):
		return fallbackRefused(rid, err.Error()), nil
	case err != nil:
		return nil, err
	}
	out, err := fallbackListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Whether each entry of the fallback chain can be used, with one small request sent to each when asked
func fallbackCheck(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.FallbackCheck.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	results, err := selfupdate.RunFallbackChecks(ctx, provider.GetFallbackChain(cfg), p.Live)
	if err != nil {
		return nil, err
	}
	out := tuigateway.FallbackCheckResult{Results: make([]tuigateway.FallbackCheckRow, 0, len(results))}
	for _, r := range results {
		row := tuigateway.FallbackCheckRow{Provider: r.Provider, Model: r.Model, Ready: r.Ready(), Status: r.Status(), Error: r.Error}
		if r.LatencyMs != nil {
			row.LatencyMs = int64(*r.LatencyMs)
		}
		out.Results = append(out.Results, row)
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
