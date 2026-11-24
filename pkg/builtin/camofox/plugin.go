// Package camofox drives a self hosted camofox-browser server over its REST API as the camofox toolset
package camofox

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset every Camofox tool registers under
const Toolset = "camofox"

func init() { plugins.RegisterPort(Plugin{}) }

// Camofox catalog plugin port, active once icebath plugins install camofox lands it in a home
type Plugin struct{}

// Manifest of the camofox catalog plugin at its compiled-in pin
func (Plugin) Manifest() plugins.Manifest { return plugins.CatalogPortManifest("camofox") }

// Schema, handler, availability check and emoji of one tool
type toolEntry struct {
	schema  *pyjson.Object
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
		name, _ := e.schema.Vals["name"].(string)
		if _, err := pctx.RegisterTool(context.Background(), plugins.ToolSpec{Name: name, Toolset: Toolset, Schema: e.schema,
			Handler: e.handler, Check: e.check, Emoji: e.emoji}); err != nil {
			return err
		}
	}
	if _, err := pctx.RegisterBrowserPageBackend(newPageBackend(pctx.HomeContext())); err != nil {
		pkgLog().Info(fmt.Sprintf("camofox: browser_vault fill unavailable here (%v)", err))
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
