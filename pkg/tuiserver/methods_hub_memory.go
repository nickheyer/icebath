package tuiserver

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubMemoryRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubMemoryRegistry, tuigateway.MemoryOverview.Name(), 5195, rpcShape{}, memoryOverview)
	scopedRPC(hubMemoryRegistry, tuigateway.MemoryChange.Name(), 5195, rpcShape{}, memoryChange)
	addLongHandlers(tuigateway.MemoryOverview.Name(), tuigateway.MemoryChange.Name())
	hubMemoryRegistry.install()
}

// Memory setup of the profile in the context, with nobody at a terminal to ask
func memorySetupOf(ctx context.Context) *memory.MemorySetup {
	return memory.NewMemorySetup(&tools.Env{Home: home.Dir(ctx)}, &cliui.Console{In: strings.NewReader(""), Out: io.Discard}, io.Discard)
}

// State of memory for the profile as the memory provider screen shows it
func memoryState(ctx context.Context) (tuigateway.MemoryOverviewResult, error) {
	view, err := memorySetupOf(ctx).Overview(ctx)
	if err != nil {
		return tuigateway.MemoryOverviewResult{}, err
	}
	out := tuigateway.MemoryOverviewResult{Provider: view.Provider, MemoryEnabled: view.MemoryEnabled, UserProfileEnabled: view.UserProfileEnabled,
		ToolEnabled: view.ToolEnabled, Installed: view.Installed, Ready: view.Ready, Problem: view.Problem,
		Providers: make([]tuigateway.MemoryProviderRow, 0, len(view.Providers))}
	for _, p := range view.Providers {
		row := tuigateway.MemoryProviderRow{Name: p.Name, Active: p.Active, OwnSetup: p.OwnSetup, Fields: make([]tuigateway.MemoryField, 0, len(p.Fields))}
		for _, f := range p.Fields {
			row.Fields = append(row.Fields, tuigateway.MemoryField{Key: f.Key, Label: f.Label, Value: f.Value, Secret: f.Secret, SecretSet: f.SecretSet,
				Choices: f.Choices, URL: f.URL, When: f.When})
		}
		out.Providers = append(out.Providers, row)
	}
	return out, nil
}

// State of memory for the memory provider screen
func memoryOverview(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	if _, err := tuigateway.MemoryOverview.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := memoryState(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One change to memory, answered with its state after it
func memoryChange(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.MemoryChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	var out tuigateway.MemoryChangeResult
	switch p.Verb {
	case "use":
		err = memorySetupOf(ctx).Configure(ctx, p.Provider, p.Values)
	case "off":
		err = memory.TurnOffProvider(ctx)
	case "erase":
		out.Erased, err = memory.EraseBuiltinMemory(ctx, p.Target)
	default:
		err = &memory.SetupRefusal{Message: "unknown memory change: " + p.Verb}
	}
	var refused *memory.SetupRefusal
	switch {
	case errors.As(err, &refused):
		return rpcErr(rid, 4195, refused.Message, nil), nil
	case err != nil:
		return nil, err
	}
	if out.State, err = memoryState(ctx); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
