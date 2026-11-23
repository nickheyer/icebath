package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Curated plugin catalog entries the words match, the in-tree catalog standing in when the live one cannot be read
func pluginsHubCatalog(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.PluginsCatalog.Params(params)
	if err != nil {
		return nil, err
	}
	out := tuigateway.PluginsCatalogResult{Entries: []tuigateway.PluginCatalogRow{}}
	for _, e := range plugins.FilterEntries(plugins.LoadCatalogLive(ctx), p.Term) {
		out.Entries = append(out.Entries, tuigateway.PluginCatalogRow{Name: e.Name, Title: e.Title, Description: e.Description, Tier: e.Tier,
			Category: e.Category, Version: e.Version, Repo: e.Repo, Maintainer: e.Maintainer, Tools: e.Capabilities.ProvidesTools,
			RequiresEnv: e.Capabilities.RequiresEnv, KnownIssues: e.KnownIssues, DocsURL: e.DocsURL})
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Verbs of plugins.hub.run that need a plugin name, path or source
var pluginsRunNeeds = map[string]string{"show": "name", "info": "name", "trust-update-url": "name", "adopt": "name", "pack-show": "source", "validate": "source"}

// Runs one plugins command the way icebath plugins runs it and answers what it printed, the commands that ask questions left to their own RPCs
func pluginsHubRun(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.PluginsHubRun.Params(params)
	if err != nil {
		return nil, err
	}
	name, source := strings.TrimSpace(p.Name), strings.TrimSpace(p.Source)
	switch pluginsRunNeeds[p.Verb] {
	case "name":
		if name == "" {
			return rpcErr(rid, 4019, p.Verb+" needs a plugin name", nil), nil
		}
	case "source":
		if source == "" {
			return rpcErr(rid, 4019, p.Verb+" needs a path or source", nil), nil
		}
	}
	captured := &capturedLines{}
	switch p.Verb {
	case "show":
		err = plugins.CmdShow(ctx, name, captured)
	case "info":
		err = plugins.CmdInfo(ctx, name, captured)
	case "capabilities":
		var only *string
		if name != "" {
			only = &name
		}
		err = plugins.CmdCapabilities(ctx, only, captured)
	case "check-updates":
		err = plugins.CmdCheckUpdates(ctx, false, captured)
	case "trust-update-url":
		err = plugins.CmdTrustUpdateURL(ctx, name, captured)
	case "adopt":
		err = plugins.CmdAdopt(ctx, name, captured)
	case "doctor":
		target := name
		if target == "" {
			target = source
		}
		if target == "" {
			target = "."
		}
		err = plugins.CmdPluginDoctor(ctx, target, false, captured)
	case "validate":
		err = plugins.CmdValidate(ctx, source, false, false, captured, captured)
	case "pack-show":
		err = plugins.CmdPackShow(ctx, source, captured)
	case "pack-export":
		packName := strings.TrimSpace(p.PackName)
		if packName == "" {
			packName = "my-icebath-pack"
		}
		err = plugins.CmdPackExport(ctx, p.EnabledOnly, packName, captured, captured)
	default:
		return rpcErr(rid, 4017, "unknown plugins action: "+p.Verb, nil), nil
	}
	return pluginsRunAnswer(rid, captured, err)
}

// Answer of a plugins command: what it printed, failed when it ended with an exit code or an error
func pluginsRunAnswer(rid any, captured *capturedLines, err error) (map[string]any, error) {
	lines := captured.text()
	var exit *plugins.CommandExit
	failed := err != nil
	if err != nil && !errors.As(err, &exit) {
		lines = append(lines, err.Error())
	}
	if errors.As(err, &exit) && exit.Code == 0 {
		failed = false
	}
	return ok(rid, tuigateway.Plain(tuigateway.PluginsHubRunResult{OK: !failed, Lines: lines})), nil
}

// Installs every plugin a pack pins once the user reviewed it in the screen, each install with its own capability consent
func pluginsPackInstall(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.PluginsPackInstall.Params(params)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Source) == "" {
		return rpcErr(rid, 4019, "a pack install needs the pack's path or URL", nil), nil
	}
	pack, err := plugins.LoadPack(ctx, strings.TrimSpace(p.Source))
	if err != nil {
		var refused *plugins.PackError
		if errors.As(err, &refused) {
			return rpcErr(rid, 4021, refused.Msg, nil), nil
		}
		return nil, err
	}
	captured := &capturedLines{}
	results, err := plugins.InstallPackPlugins(ctx, pack, plugins.ResolvePackPlugins(ctx, pack), plugins.NewConsole(captured), p.Force)
	if err != nil {
		return pluginsRunAnswer(rid, captured, err)
	}
	installed, failed := 0, 0
	for _, r := range results {
		if r.OK {
			installed++
			continue
		}
		failed++
		fmt.Fprintf(captured, "✗ %s: %s\n", r.Display, r.Error)
	}
	fmt.Fprintf(captured, "Pack '%s': %d installed, %d failed.\n", pack.Name, installed, failed)
	return ok(rid, tuigateway.Plain(tuigateway.PluginsHubRunResult{OK: failed == 0, Lines: captured.text()})), nil
}

var hubPluginsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubPluginsRegistry, tuigateway.PluginsCatalog.Name(), 5026, rpcShape{}, pluginsHubCatalog)
	scopedRPC(hubPluginsRegistry, tuigateway.PluginsHubRun.Name(), 5026, rpcShape{}, pluginsHubRun)
	scopedRPC(hubPluginsRegistry, tuigateway.PluginsPackInstall.Name(), 5026, rpcShape{}, pluginsPackInstall)
	hubPluginsRegistry.install()
	addLongHandlers(tuigateway.PluginsHubRun.Name(), tuigateway.PluginsPackInstall.Name(), tuigateway.PluginsCatalog.Name())
}
