// Package camofox drives a self hosted camofox-browser server over its REST API as the camofox toolset
package camofox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset every Camofox tool registers under
const Toolset = "camofox"

const pluginDescription = "Camofox for Icebath: drives a self-hosted camofox-browser server (Camoufox, a " +
	"Firefox fork with fingerprint spoofing) over its REST API. Provides the browser_navigate, " +
	"browser_snapshot, browser_click, browser_type, browser_scroll, browser_back, browser_press, " +
	"browser_get_images, browser_vision and browser_console tools (toolset camofox), cookie import " +
	"from a cookies.txt file (browser_import_cookies, with CAMOFOX_API_KEY), plus " +
	"`icebath camofox setup|status`, and lets browser_vault fill saved logins on the Camofox tab. " +
	"Icebath does not install or start the server; set CAMOFOX_URL. Built into Icebath.\n"

const camofoxURLHelp = "URL of your camofox-browser server (e.g. http://localhost:9377). The camofox tools appear once it is set."

var providedTools = []string{"browser_navigate", "browser_snapshot", "browser_click", "browser_type", "browser_scroll",
	"browser_back", "browser_press", "browser_get_images", "browser_vision", "browser_console", "browser_import_cookies"}

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled in Camofox browser backend plugin
type Plugin struct{}

// Manifest of the camofox plugin, a backend kind so it is active with nothing to install or enable
func (Plugin) Manifest() plugins.Manifest {
	toolsAny := make([]any, len(providedTools))
	for i, t := range providedTools {
		toolsAny[i] = t
	}
	return plugins.Manifest{
		Name:          "camofox",
		Version:       "1.1.0",
		Description:   pluginDescription,
		Author:        "NousResearch",
		Kind:          "backend",
		Homepage:      "https://github.com/NousResearch/hermes-official-plugins/tree/main/camofox",
		ProvidesTools: append([]string(nil), providedTools...),
		ProvidesHooks: []string{"on_session_end", "on_session_finalize"},
		RequiresEnv:   []plugins.EnvSpec{{Name: "CAMOFOX_URL", Description: camofoxURLHelp, URL: "https://github.com/jo-inc/camofox-browser"}},
		Raw: map[string]any{"name": "camofox", "label": "Camofox", "kind": "backend", "version": "1.1.0", "author": "NousResearch",
			"homepage": "https://github.com/NousResearch/hermes-official-plugins/tree/main/camofox", "description": pluginDescription,
			"provides_tools": toolsAny, "provides_hooks": []any{"on_session_end", "on_session_finalize"},
			"requires_env": []any{map[string]any{"name": "CAMOFOX_URL", "description": camofoxURLHelp, "url": "https://github.com/jo-inc/camofox-browser"}},
			"optional_env": []any{map[string]any{"name": "CAMOFOX_API_KEY", "description": "Bearer token for a Camofox server started with CAMOFOX_API_KEY",
				"prompt": "Camofox API key", "password": true}}},
	}
}

// Schema, handler, availability check and emoji of one tool
type toolEntry struct {
	schema  map[string]any
	handler tools.Handler
	check   func(env *tools.Env) bool
	emoji   string
}

// Registers the vision task, the eleven browser tools, the vault page backend, the session hooks and the CLI
func (Plugin) Register(pctx *plugins.Context) error {
	if _, err := pctx.RegisterAuxiliaryTask(visionTask, plugins.AuxTaskOptions{DisplayName: "Camofox browser vision",
		Description: "Screenshot analysis for browser_vision on the Camofox backend", InheritFrom: "vision"}); err != nil {
		return err
	}
	entries := []toolEntry{
		{browserNavigateSchema, handleNavigate, checkCamofoxConfigured, "\U0001F310"},
		{browserSnapshotSchema, handleSnapshot, checkCamofoxConfigured, "\U0001F4F8"},
		{browserClickSchema, handleClick, checkCamofoxConfigured, "\U0001F446"},
		{browserTypeSchema, handleType, checkCamofoxConfigured, "⌨️"},
		{browserScrollSchema, handleScroll, checkCamofoxConfigured, "\U0001F4DC"},
		{browserBackSchema, handleBack, checkCamofoxConfigured, "◀️"},
		{browserPressSchema, handlePress, checkCamofoxConfigured, "⌨️"},
		{browserGetImagesSchema, handleGetImages, checkCamofoxConfigured, "\U0001F5BC️"},
		{browserVisionSchema, visionHandler(pctx.LLM), checkCamofoxConfigured, "\U0001F441️"},
		{browserConsoleSchema, handleConsole, checkCamofoxConfigured, "\U0001F5A5️"},
		{browserImportCookiesSchema, handleImportCookies, checkCookieImportAvailable, "\U0001F36A"},
	}
	for _, e := range entries {
		name, _ := e.schema["name"].(string)
		if _, err := pctx.RegisterTool(context.Background(), plugins.ToolSpec{Name: name, Toolset: Toolset, Schema: e.schema,
			Handler: e.handler, Check: e.check, Emoji: e.emoji}); err != nil {
			return err
		}
	}
	if _, err := pctx.RegisterBrowserPageBackend(newPageBackend(pctx.HomeContext())); err != nil {
		slog.Info(fmt.Sprintf("camofox: browser_vault fill unavailable here (%v)", err))
	}
	pctx.RegisterHook("on_session_end", onSessionEnd, false)
	pctx.RegisterHook("on_session_finalize", onSessionFinalize, false)
	pctx.RegisterCLICommand("camofox", "Set up and check the Camofox browser server", setupCLI, handleCLI,
		"Camofox browser backend: point Icebath at a camofox-browser server and check it.")
	return nil
}

// Hook argument as text, empty when absent or falsy
func hookID(kwargs map[string]any, key string) string { return pyOrStr(kwargs[key]) }

// Cleans the task's ephemeral session after every turn unless browser.headed keeps the window for a watcher
func onSessionEnd(ctx context.Context, kwargs map[string]any) (any, error) {
	id := hookID(kwargs, "task_id")
	if id == "" {
		id = hookID(kwargs, "session_id")
	}
	if id == "" {
		return nil, nil
	}
	s := newScope(ctx, nil)
	if !s.headedMode() {
		s.cleanupTask(id)
	}
	return nil, nil
}

// Cleans the session when the conversation is finalized, the teardown a quit or new session gets
func onSessionFinalize(ctx context.Context, kwargs map[string]any) (any, error) {
	if id := hookID(kwargs, "session_id"); id != "" {
		newScope(ctx, nil).cleanupTask(id)
	}
	return nil, nil
}
