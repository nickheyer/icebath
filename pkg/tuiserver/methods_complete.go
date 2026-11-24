package tuiserver

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

var (
	completeRegistry   = &handlerRegistry{}
	pasteMu            sync.Mutex
	pasteCounter       int
	builtinAtPrefixes  = map[string]bool{"file": true, "folder": true, "url": true, "git": true, "diff": true, "staged": true}
	atDirectiveHints   = [][2]string{{"@diff", "git diff"}, {"@staged", "staged diff"}, {"@file:", "attach file"}, {"@folder:", "attach folder"}, {"@url:", "fetch url"}, {"@git:", "git log"}}
	terminalTool       = tools.TerminalTool
	detectCodeSkew     = gateway.DetectCodeSkew
	backendListingCode = `d=$1; case $d in "~") d=$HOME;; "~/"*) d=$HOME${d#?};; esac; [ -d "$d" ] || exit 0; ` +
		`for p in "$d"/* "$d"/.[!.]* "$d"/..?*; do [ -e "$p" ] || [ -L "$p" ] || continue; ` +
		`if [ -d "$p" ]; then printf "%s/\n" "${p##*/}"; else printf "%s\n" "${p##*/}"; fi; done`
)

func init() {
	completeRegistry.method("paste.collapse", pasteCollapse)
	completeRegistry.scopedMethod("model.save_key", catch(5034, modelSaveKey))
	completeRegistry.scopedMethod("model.disconnect", catch(5035, modelDisconnect))
	completeRegistry.install()
}

// Handler whose raised failure answers as the error text under the given code
func catch(failCode int, body func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)) Handler {
	return func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				resp = rpcErr(rid, failCode, panicMessage(r), nil)
			}
		}()
		out, err := body(ctx, rid, params)
		if err != nil {
			return rpcErr(rid, failCode, err.Error(), nil)
		}
		return out
	}
}

// Saves pasted text under the profile pastes dir and answers the line standing in for it
func pasteCollapse(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.PasteCollapse.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	text := request.Text
	if text == "" {
		return rpcErr(rid, 4004, "empty paste", nil)
	}
	pasteMu.Lock()
	pasteCounter++
	counter := pasteCounter
	pasteMu.Unlock()
	lineCount := strings.Count(text, "\n") + 1
	base := connectionProfileHome(ctx)
	if base == "" {
		base = importedHome()
	}
	pasteDir := filepath.Join(base, "pastes")
	if err := os.MkdirAll(pasteDir, 0o777); err != nil {
		panic(err)
	}
	pasteFile := filepath.Join(pasteDir, fmt.Sprintf("paste_%d_%s.txt", counter, time.Now().Format("150405")))
	if err := os.WriteFile(pasteFile, []byte(text), 0o666); err != nil {
		panic(err)
	}
	placeholder := fmt.Sprintf("[Pasted text #%d: %d lines → %s]", counter, lineCount, pasteFile)
	return ok(rid, tuigateway.Plain(tuigateway.PasteCollapseResult{Placeholder: placeholder, Path: pasteFile, Lines: int64(lineCount)}))
}

// Profile mention completions for a bare word, the primary profile offered as icebath when no profile claims it
func profileMentionItems(ctx context.Context, prefix string) ([]map[string]any, error) {
	profiles, err := manage.ListProfiles(ctx, true)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, p := range profiles {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		seen[strings.ToLower(name)] = true
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			meta := strings.TrimSpace(p.Description)
			if meta == "" {
				meta = "agent profile"
			}
			out = append(out, completionItem("@"+name, meta))
		}
	}
	if strings.HasPrefix("icebath", strings.ToLower(prefix)) && !seen["icebath"] {
		out = append(out, completionItem("@icebath", "agent profile (primary)"))
	}
	return out, nil
}

// Autocomplete of the plugin reference provider owning the prefix, owned false when no provider claims it
func pluginReferenceItems(ctx context.Context, pfx, qval string) ([]map[string]any, bool, error) {
	prov, owned := agent.ContextReferenceProviders(ctx)[pfx]
	if !owned || prov == nil {
		return nil, false, nil
	}
	found, err := prov.Autocomplete(ctx, qval, 20)
	if err != nil {
		return nil, true, fmt.Errorf("the @%s: reference provider did not answer: %w", pfx, err)
	}
	out := []map[string]any{}
	for _, it := range found {
		out = append(out, map[string]any{"text": "@" + pfx + ":" + it.Text, "display": it.Display, "meta": it.Meta})
	}
	return out, true, nil
}

// One fuzzy basename hit with its rank
type rankedEntry struct {
	tier, score int
	rel, name   string
	isDir       bool
}

// Fuzzy basename search for a bare mention, folders first within a rank tier, at most thirty
func fuzzyBasenameItems(root, pathPart, prefixTag string) []map[string]any {
	var ranked []rankedEntry
	walkedDirs, seen := map[string]bool{}, map[string]bool{}
	wantHidden := strings.HasPrefix(pathPart, ".")
	consider := func(rel, name string, isDir bool) {
		if seen[rel] || (strings.HasPrefix(name, ".") && !wantHidden) {
			return
		}
		if tier, score, hit := fuzzyBasenameRank(name, pathPart); hit {
			seen[rel] = true
			ranked = append(ranked, rankedEntry{tier, score, rel, name, isDir})
		}
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if !fuzzyFallbackExcludes[entry.Name()] {
				consider(entry.Name(), entry.Name(), isDir(filepath.Join(root, entry.Name())))
			}
		}
	}
	for _, rel := range listRepoFiles(root) {
		consider(rel, pypath.Basename(rel), false)
		for parent := pypath.Dirname(rel); parent != "" && !walkedDirs[parent]; parent = pypath.Dirname(parent) {
			walkedDirs[parent] = true
			consider(parent, pypath.Basename(parent), true)
		}
	}
	slices.SortStableFunc(ranked, func(a, b rankedEntry) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(a.score, b.score), boolOrder(!a.isDir, !b.isDir),
			cmp.Compare(utf8.RuneCountInString(a.rel), utf8.RuneCountInString(b.rel)), strings.Compare(a.rel, b.rel))
	})
	tag := prefixTag
	if tag == "" {
		tag = "file"
	}
	out := []map[string]any{}
	for _, r := range ranked[:min(len(ranked), 30)] {
		if r.isDir {
			out = append(out, completionItem("@folder:"+r.rel+"/", "dir", r.name+"/"))
		} else {
			out = append(out, completionItem("@"+tag+":"+r.rel, pypath.Dirname(r.rel), r.name))
		}
	}
	return out
}

// Python ordering of two booleans, False first
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// Completions for a bare at sign, the directive hints, agent profiles and plugin reference prefixes
func atRootItems(ctx context.Context) ([]map[string]any, error) {
	items := []map[string]any{}
	for _, h := range atDirectiveHints {
		items = append(items, completionItem(h[0], h[1]))
	}
	profileItems, err := profileMentionItems(ctx, "")
	if err != nil {
		return nil, err
	}
	items = append(items, profileItems...)
	providers := agent.ContextReferenceProviders(ctx)
	keys := make([]string, 0, len(providers))
	for pfx := range providers {
		keys = append(keys, pfx)
	}
	slices.Sort(keys)
	for _, pfx := range keys {
		desc := providers[pfx].Description()
		if desc == "" {
			desc = "plugin: " + pfx
		}
		items = append(items, completionItem("@"+pfx+":", desc))
	}
	return items, nil
}

// One directory entry name with whether it is a directory
type dirEntry struct {
	name  string
	isDir bool
}

// Entries of one directory inside the non local terminal backend of the session, the backend's failure when it cannot list
func backendDirEntries(ctx context.Context, searchDir, sessionKey string) (entries []dirEntry, err error) {
	timeout := 3
	command := "sh -c " + pyshlex.Quote(backendListingCode) + " sh " + pyshlex.Quote(searchDir)
	raw := terminalTool(ctx, tools.EnvForProfileHome(ctx, tools.ToolEnvFrom(ctx), home.Dir(ctx)),
		tools.TerminalRequest{Command: command, TaskID: sessionKey, Timeout: &timeout, Force: true})
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		return nil, fmt.Errorf("the terminal backend answered a listing that is not JSON: %w", err)
	}
	result, isDict := pyjson.Plain(decoded).(map[string]any)
	if !isDict {
		return nil, fmt.Errorf("the terminal backend answered a listing that is %s, not an object", errchain.Kind(pyjson.Plain(decoded)))
	}
	if pyrepr.Truthy(result["error"]) {
		return nil, fmt.Errorf("the terminal backend could not list %s: %s", searchDir, pyrepr.Str(result["error"]))
	}
	if exitCode, hasExit := result["exit_code"]; hasExit && exitCode != nil && !pyrepr.Equal(exitCode, 0) {
		return nil, fmt.Errorf("listing %s in the terminal backend exited %s: %s", searchDir, pyrepr.Str(exitCode), pyrepr.Str(result["output"]))
	}
	output := ""
	if pyrepr.Truthy(result["output"]) {
		output = pyrepr.Str(result["output"])
	}
	for _, line := range textutil.Lines(output) {
		name := strings.TrimRight(line, "/")
		if name != "" && !strings.Contains(name, "/") {
			entries = append(entries, dirEntry{name, strings.HasSuffix(line, "/")})
		}
	}
	slices.SortFunc(entries, func(a, b dirEntry) int {
		return cmp.Or(strings.Compare(a.name, b.name), boolOrder(a.isDir, b.isDir))
	})
	return entries, nil
}

// Path functions of the host or of POSIX, the dialect a listing speaks
type completionPathOps struct {
	dirname, basename, normpath func(string) string
	isabs                       func(string) bool
	join                        func(string, ...string) string
	relpath                     func(p, start string) (string, error)
}

var (
	hostPathOps  = completionPathOps{pypath.Dirname, pypath.Basename, pypath.Normpath, pypath.IsAbs, pypath.Join, pypath.Relpath}
	posixPathOps = completionPathOps{pypath.PosixDirname, pypath.PosixBasename, pypath.PosixNormpath, pypath.PosixIsAbs, pypath.PosixJoin, pypath.PosixRelpath}
)

// Prefix matched entries of the directory the path part points at, at most thirty
func dirListingItems(ctx context.Context, root, word, pathPart, prefixTag string, isContext bool, sessionKey string, local bool) ([]map[string]any, error) {
	pth := posixPathOps
	if local {
		pth = hostPathOps
	}
	expanded := "."
	if pathPart != "" {
		expanded = pathPart
		if local {
			expanded = normalizeCompletionPath(pathPart)
		}
	}
	searchDir, match := expanded, ""
	if expanded == "." || expanded == "" || strings.HasSuffix(expanded, "/") {
		if searchDir == "" {
			searchDir = "."
		}
	} else {
		searchDir, match = pth.dirname(expanded), pth.basename(expanded)
		if searchDir == "" {
			searchDir = "."
		}
	}
	if !(pth.isabs(searchDir) || strings.HasPrefix(searchDir, "~")) {
		searchDir = pth.join(root, searchDir)
	}
	searchDir = pth.normpath(searchDir)
	items := []map[string]any{}
	var entries []dirEntry
	if local {
		if !isDir(searchDir) {
			return items, nil
		}
		listed, err := os.ReadDir(searchDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range listed {
			entries = append(entries, dirEntry{entry.Name(), isDir(filepath.Join(searchDir, entry.Name()))})
		}
	} else {
		listed, err := backendDirEntries(ctx, searchDir, sessionKey)
		if err != nil {
			return nil, err
		}
		entries = listed
	}
	for _, entry := range entries {
		if match != "" && !strings.HasPrefix(strings.ToLower(entry.name), strings.ToLower(match)) {
			continue
		}
		if isContext && (fuzzyFallbackExcludes[entry.name] || (prefixTag == "" && strings.HasPrefix(entry.name, "."))) {
			continue
		}
		// A file reference lists the folders too, each one ending in a slash so taking it descends into it
		if prefixTag == "folder" && !entry.isDir {
			continue
		}
		full := pth.join(searchDir, entry.name)
		rel, err := pth.relpath(full, root)
		if err != nil {
			return nil, err
		}
		rel = strings.ReplaceAll(rel, string(os.PathSeparator), "/")
		suffix := ""
		if entry.isDir {
			suffix = "/"
		}
		var text string
		switch {
		case isContext:
			kind := prefixTag
			if kind == "" {
				kind = "file"
				if entry.isDir {
					kind = "folder"
				}
			}
			text = "@" + kind + ":" + rel + suffix
		case strings.HasPrefix(word, "~"):
			userHome := "~"
			if local {
				userHome = pypath.Expanduser("~")
			}
			fromHome, err := pth.relpath(full, userHome)
			if err != nil {
				return nil, err
			}
			text = "~/" + fromHome + suffix
		default:
			text = rel + suffix
			if strings.HasPrefix(word, "./") {
				text = "./" + text
			}
		}
		meta := ""
		if entry.isDir {
			meta = "dir"
		}
		items = append(items, completionItem(text, meta, entry.name+suffix))
		if len(items) >= 30 {
			break
		}
	}
	return items, nil
}

// Typed rows of the item maps a completion builder produced, a misfit reported against the method
func completionRows(method string, items []map[string]any) []tuigateway.CompletionItem {
	out := make([]tuigateway.CompletionItem, 0, len(items))
	for _, item := range items {
		out = append(out, typedRow[tuigateway.CompletionItem](method, item))
	}
	return out
}

// Items of complete.path for the word resolved against root
func pathCompletionItems(ctx context.Context, word, root, sessionKey string, local bool) ([]map[string]any, error) {
	isContext := strings.HasPrefix(word, "@")
	query := word
	if isContext {
		query = word[1:]
	}
	if isContext && query == "" {
		return atRootItems(ctx)
	}
	if isContext && strings.Contains(query, ":") {
		pfx, qval, _ := strings.Cut(query, ":")
		if !builtinAtPrefixes[pfx] {
			pluginItems, owned, err := pluginReferenceItems(ctx, pfx, qval)
			if owned || err != nil {
				return pluginItems, err
			}
		}
	}
	prefixTag, pathPart := "", query
	if isContext && (query == "file" || query == "folder" || strings.HasPrefix(query, "file:") || strings.HasPrefix(query, "folder:")) {
		prefixTag, pathPart, _ = strings.Cut(query, ":")
	}
	if isContext && strings.HasPrefix(pathPart, "/") && !strings.HasPrefix(pathPart, "//") && local && !absCompletionPrefixExists(pathPart) {
		pathPart = strings.TrimLeft(pathPart, "/")
	}
	bareWord := isContext && pathPart != "" && !strings.Contains(pathPart, "/")
	var items []map[string]any
	if local && bareWord && utf8.RuneCountInString(strings.TrimSpace(pathPart)) >= 2 && prefixTag != "folder" {
		items = fuzzyBasenameItems(root, pathPart, prefixTag)
	} else {
		listed, err := dirListingItems(ctx, root, word, pathPart, prefixTag, isContext, sessionKey, local)
		if err != nil {
			return nil, err
		}
		items = listed
	}
	if bareWord && prefixTag == "" {
		profileItems, err := profileMentionItems(ctx, pathPart)
		if err != nil {
			return nil, err
		}
		items = append(profileItems, items...)
	}
	return items, nil
}

// Refusal of model serving calls while this process runs code older than the install on disk
func modelSkewErr(ctx context.Context, rid any) map[string]any {
	bootRev, diskRev, skewed := detectCodeSkew()
	if !skewed {
		return nil
	}
	inner := i18n.T(ctx, "gateway.model.err_skew", "", map[string]any{"boot_rev": bootRev, "disk_rev": diskRev})
	return rpcErr(rid, 5098, i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{"error": inner}), nil)
}

// Saves an API key as an owned login of the provider, its profile default, and answers its refreshed picker row marked authenticated
func modelSaveKey(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	if skew := modelSkewErr(ctx, rid); skew != nil {
		return skew, nil
	}
	slug, apiKey := strings.TrimSpace(strOr(params["slug"])), strings.TrimSpace(strOr(params["api_key"]))
	if slug == "" || apiKey == "" {
		return rpcErr(rid, tuihost.LoginCodeMissing, "slug and api_key are required", nil), nil
	}
	pconfig := provider.RegistryRow(ctx, slug)
	if pconfig == nil {
		return rpcErr(rid, tuihost.LoginCodeUnknownProvider, "unknown provider: "+slug, map[string]any{"reason": "unknown_provider"}), nil
	}
	if _, err := tuihost.AddKeyLogin(ctx, nil, tuigateway.LoginAddKeyParams{Provider: slug, APIKey: apiKey,
		Name: strings.TrimSpace(strOr(params["name"])), MakeDefault: true}); err != nil {
		return loginRefusalFrame(rid, err), nil
	}
	pc, err := modelPickerContext(ctx)
	if err != nil {
		return nil, err
	}
	maxModels := 50
	payload, err := provider.BuildModelsPayload(ctx, pc, provider.ModelsPayloadOptions{PickerHints: true, MaxModels: &maxModels, ProbeCustomProviders: true})
	if err != nil {
		return nil, err
	}
	var providerData map[string]any
	rows, _ := payload["providers"].([]any)
	for _, raw := range rows {
		if row, isDict := raw.(map[string]any); isDict && row["slug"] == slug {
			providerData = row
			break
		}
	}
	if providerData == nil {
		providerData = map[string]any{"slug": slug, "name": pconfig.Label(), "is_current": false, "models": []any{}, "total_models": 0}
	}
	providerData["authenticated"] = true
	return ok(rid, map[string]any{"provider": providerData}), nil
}

// Removes the provider's default or named login, or with all every env key and OAuth or pool credential, refusing before any removal when one is locked
func modelDisconnect(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	slug := strings.TrimSpace(strOr(params["slug"]))
	if slug == "" {
		return rpcErr(rid, 4001, "slug is required", nil), nil
	}
	h := home.Dir(ctx)
	pconfig := provider.RegistryRow(ctx, slug)
	if !truthy(params["all"]) {
		if resp, handled, err := disconnectOneLogin(ctx, rid, slug, strings.TrimSpace(strOr(params["login"])), pconfig); handled || err != nil {
			return resp, err
		}
	}
	var envVars []string
	if pconfig != nil {
		envVars = pconfig.KeyEnv
	}
	var removable []string
	for _, ev := range envVars {
		refusal := config.EnvWriteRefusal(ctx, h, ev, "remove")
		if refusal == "" {
			removable = append(removable, ev)
		} else if config.IsEnvManaged(ev) || os.Getenv(ev) != "" || config.LoadEnv(h)[ev] != "" {
			return rpcErr(rid, 5035, refusal, nil), nil
		}
	}
	clearedEnv := false
	for _, ev := range removable {
		removal, err := provider.RemoveProviderEnvCredential(ctx, h, ev)
		if err != nil {
			return nil, err
		}
		clearedEnv = clearedEnv || removal.Found
	}
	clearedAuth, err := provider.NewStore(h).ClearProviderAuth(slug)
	if err != nil {
		return nil, err
	}
	if !clearedEnv && !clearedAuth {
		return rpcErr(rid, 4005, "no credentials found for "+slug, nil), nil
	}
	name := slug
	if pconfig != nil {
		name = pconfig.Label()
	}
	return ok(rid, map[string]any{"slug": slug, "name": name, "disconnected": true}), nil
}
