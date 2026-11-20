package tuiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const i18nErr = 5400

var i18nRegistry = &handlerRegistry{}

func init() {
	i18nRegistry.scopedMethod("i18n.languages", i18nLanguages)
	i18nRegistry.scopedMethod("i18n.catalog", i18nCatalog)
	i18nRegistry.install()
	addLongHandlers(i18nRegistry.names()...)
}

// Runs plugin discovery so plugin language packs are registered, logging a failed discovery
func loadLanguagePacks(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Warn("i18n: plugin discovery failed; serving languages without plugin packs", "error", r)
		}
	}()
	discoverPlugins(ctx, false)
}

// Message of a recovered panic the way str of an exception reads
func panicMessage(r any) string {
	if e, isErr := r.(error); isErr {
		return e.Error()
	}
	return pyrepr.Str(r)
}

// Every supported language of the profile with its endonym, direction, source and surfaces, beside the language in effect and the id a code, alias
// or regional tag given as resolve stands for
func i18nLanguages(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			resp = rpcErr(rid, i18nErr, panicMessage(r), nil)
		}
	}()
	loadLanguagePacks(ctx)
	i18n.ResetLanguageCache()
	languages := []any{}
	for _, opt := range i18n.LanguageOptions(ctx) {
		surfaces := make([]any, 0, len(opt.Surfaces))
		for _, s := range opt.Surfaces {
			surfaces = append(surfaces, s)
		}
		languages = append(languages, map[string]any{"id": opt.ID, "endonym": opt.Endonym, "rtl": opt.RTL, "source": opt.Source, "surfaces": surfaces})
	}
	result := map[string]any{"languages": languages, "current": i18n.GetLanguage(ctx)}
	if asked := pystr.Strip(strOr(params["resolve"])); asked != "" {
		result["resolved"] = i18n.ResolveLanguageID(ctx, asked, "")
	}
	return ok(rid, result)
}

// Pack and overlay messages of one surface for the resolved language, nothing bundled
func i18nCatalog(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
	surface := i18n.CoreSurface
	if raw := params["surface"]; pyrepr.Truthy(raw) {
		surface = pyrepr.Str(raw)
	}
	if !slices.Contains(i18n.Surfaces, surface) {
		return rpcErr(rid, 4002, fmt.Sprintf("surface must be one of %s", strings.Join(i18n.Surfaces, ", ")), nil)
	}
	defer func() {
		if r := recover(); r != nil {
			resp = rpcErr(rid, i18nErr, panicMessage(r), nil)
		}
	}()
	loadLanguagePacks(ctx)
	lang, hasLang := params["lang"]
	if !hasLang {
		lang = ""
	}
	resolved := i18n.ResolveLanguageID(ctx, lang, "")
	if resolved == "" {
		resolved = "en"
	}
	messages := i18n.SurfaceCatalog(ctx, resolved, surface)
	return ok(rid, tuigateway.Plain(tuigateway.I18nCatalogResult{Lang: resolved, Surface: tuigateway.LocaleSurface(surface), Messages: messages}))
}
