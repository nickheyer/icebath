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
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var (
	completeRegistry         = &handlerRegistry{}
	pasteMu                  sync.Mutex
	pasteCounter             int
	builtinAtPrefixes        = map[string]bool{"file": true, "folder": true, "url": true, "git": true, "diff": true, "staged": true}
	atDirectiveHints         = [][2]string{{"@diff", "git diff"}, {"@staged", "staged diff"}, {"@file:", "attach file"}, {"@folder:", "attach folder"}, {"@url:", "fetch url"}, {"@git:", "git log"}}
	terminalTool             = tools.TerminalTool
	detectCodeSkew           = gateway.DetectCodeSkew
	buildModelOptionsPayload = provider.BuildModelOptionsPayload
	backendListingCode       = `d=$1; case $d in "~") d=$HOME;; "~/"*) d=$HOME${d#?};; esac; [ -d "$d" ] || exit 0; ` +
		`for p in "$d"/* "$d"/.[!.]* "$d"/..?*; do [ -e "$p" ] || [ -L "$p" ] || continue; ` +
		`if [ -d "$p" ]; then printf "%s/\n" "${p##*/}"; else printf "%s\n" "${p##*/}"; fi; done`
)

func init() {
	completeRegistry.method("paste.collapse", pasteCollapse)
	completeRegistry.method("complete.path", catch(5021, completePath))
	completeRegistry.method("complete.slash", catch(5020, completeSlash))
	completeRegistry.scopedMethod("model.options", catch(5033, modelOptions))
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
	text, _ := params["text"].(string)
	if !truthy(params["text"]) {
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
	return ok(rid, map[string]any{"placeholder": placeholder, "path": pasteFile, "lines": lineCount})
}

// Profile mention completions for a bare word, the primary profile offered as icebath when no profile claims it
func profileMentionItems(ctx context.Context, prefix string) (out []map[string]any) {
	defer func() {
		if recover() != nil {
			out = []map[string]any{}
		}
	}()
	profiles, err := manage.ListProfiles(ctx, true)
	if err != nil {
		return []map[string]any{}
	}
	out = []map[string]any{}
	seen := map[string]bool{}
	for _, p := range profiles {
		name := pystr.Strip(p.Name)
		if name == "" {
			continue
		}
		seen[strings.ToLower(name)] = true
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			meta := pystr.Strip(p.Description)
			if meta == "" {
				meta = "agent profile"
			}
			out = append(out, completionItem("@"+name, meta))
		}
	}
	if strings.HasPrefix("icebath", strings.ToLower(prefix)) && !seen["icebath"] {
		out = append(out, completionItem("@icebath", "agent profile (primary)"))
	}
	return out
}

// Autocomplete of the plugin reference provider owning the prefix, nil when none owns it or it fails
func pluginReferenceItems(ctx context.Context, pfx, qval string) (out []map[string]any) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	prov, owned := agent.GetContextReferenceProviders()[pfx]
	if !owned || prov == nil {
		return nil
	}
	found, err := prov.Autocomplete(ctx, qval, 20)
	if err != nil {
		return nil
	}
	out = []map[string]any{}
	for _, it := range found {
		out = append(out, map[string]any{"text": "@" + pfx + ":" + it.Text, "display": it.Display, "meta": it.Meta})
	}
	return out
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
func atRootItems(ctx context.Context) []map[string]any {
	items := []map[string]any{}
	for _, h := range atDirectiveHints {
		items = append(items, completionItem(h[0], h[1]))
	}
	items = append(items, profileMentionItems(ctx, "")...)
	func() {
		defer func() { recover() }()
		providers := agent.GetContextReferenceProviders()
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
	}()
	return items
}

// One directory entry name with whether it is a directory
type dirEntry struct {
	name  string
	isDir bool
}

// Entries of one directory inside the non local terminal backend of the session, none when it is unreachable
func backendDirEntries(ctx context.Context, searchDir, sessionKey string) (entries []dirEntry) {
	result, failed := func() (result map[string]any, failed bool) {
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		timeout := 3
		command := "sh -c " + pyshlex.Quote(backendListingCode) + " sh " + pyshlex.Quote(searchDir)
		listing := observability.UnmeteredBackendCalls(ctx)
		raw := terminalTool(listing, tools.EnvForProfileHome(listing, tools.ToolEnvFrom(listing), home.Dir(listing)),
			tools.TerminalRequest{Command: command, TaskID: sessionKey, Timeout: &timeout, Force: true})
		decoded, err := pyjson.Loads(raw)
		if err != nil {
			return nil, true
		}
		parsed, isDict := pyjson.Plain(decoded).(map[string]any)
		if !isDict {
			return nil, true
		}
		return parsed, false
	}()
	if failed {
		return nil
	}
	exitCode, hasExit := result["exit_code"]
	if pyrepr.Truthy(result["error"]) || (hasExit && exitCode != nil && !pyrepr.Equal(exitCode, 0)) {
		return nil
	}
	output := ""
	if pyrepr.Truthy(result["output"]) {
		output = pyrepr.Str(result["output"])
	}
	for _, line := range pystr.Splitlines(output, false) {
		name := strings.TrimRight(line, "/")
		if name != "" && !strings.Contains(name, "/") {
			entries = append(entries, dirEntry{name, strings.HasSuffix(line, "/")})
		}
	}
	slices.SortFunc(entries, func(a, b dirEntry) int {
		return cmp.Or(strings.Compare(a.name, b.name), boolOrder(a.isDir, b.isDir))
	})
	return entries
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
		entries = backendDirEntries(ctx, searchDir, sessionKey)
	}
	for _, entry := range entries {
		if match != "" && !strings.HasPrefix(strings.ToLower(entry.name), strings.ToLower(match)) {
			continue
		}
		if isContext && (fuzzyFallbackExcludes[entry.name] || (prefixTag == "" && strings.HasPrefix(entry.name, "."))) {
			continue
		}
		if prefixTag != "" && (prefixTag == "folder") != entry.isDir {
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

// Path completions of the composer word resolved against the session folder or its backend cwd
func completePath(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	rawWord, present := params["word"]
	if !present || !pyrepr.Truthy(rawWord) {
		return ok(rid, map[string]any{"items": []any{}}), nil
	}
	word, isStr := rawWord.(string)
	if !isStr {
		return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(rawWord))
	}
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	local := effectiveTerminalBackend(ctx) == "local"
	clientCwd := params["cwd"]
	if session != nil && pyrepr.Equal(clientCwd, session.Get("cwd")) && isRemoteLaunchCwd(ctx, session) {
		clientCwd = nil
	}
	var root string
	if local {
		resolved, err := completionCwd(ctx, params)
		if err != nil {
			return nil, err
		}
		root = resolved
	} else if root = strOr(clientCwd); root == "" {
		if session != nil {
			root = terminalTaskCwd(ctx, session)
		}
		if root == "" {
			root = "."
		}
	}
	sessionKey := ""
	if session != nil {
		sessionKey = strOr(session.Get("session_key"))
	}
	items, err := pathCompletionItems(ctx, word, root, sessionKey, local)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"items": anyItems(items)}), nil
}

// Items of complete.path for the word resolved against root
func pathCompletionItems(ctx context.Context, word, root, sessionKey string, local bool) ([]map[string]any, error) {
	isContext := strings.HasPrefix(word, "@")
	query := word
	if isContext {
		query = word[1:]
	}
	if isContext && query == "" {
		return atRootItems(ctx), nil
	}
	if isContext && strings.Contains(query, ":") {
		pfx, qval, _ := strings.Cut(query, ":")
		if !builtinAtPrefixes[pfx] {
			if pluginItems := pluginReferenceItems(ctx, pfx, qval); pluginItems != nil {
				return pluginItems, nil
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
	if local && bareWord && utf8.RuneCountInString(pystr.Strip(pathPart)) >= 2 && prefixTag != "folder" {
		items = fuzzyBasenameItems(root, pathPart, prefixTag)
	} else {
		listed, err := dirListingItems(ctx, root, word, pathPart, prefixTag, isContext, sessionKey, local)
		if err != nil {
			return nil, err
		}
		items = listed
	}
	if bareWord && prefixTag == "" {
		items = append(profileMentionItems(ctx, pathPart), items...)
	}
	return items, nil
}

// Slash popup completions bound to the calling session's profile and workspace, or a draft's named profile
func completeSlash(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	cwd, err := completionCwd(ctx, params)
	if err != nil {
		return nil, err
	}
	text := ""
	if raw, present := params["text"]; present {
		if s, isStr := raw.(string); isStr {
			text = s
		} else {
			return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(raw))
		}
	}
	result, err := slashCompletions(ctx, text, sessionHomeScope(session, cwd, strOr(params["profile"])))
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Live agent of the session the params name, nil when none
func paramsSessionAgent(params map[string]any) *agent.AIAgent {
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	if session == nil {
		return nil
	}
	return sessionAgent(session)
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

// Model picker inventory of the scoped profile, the live agent route taking precedence
func modelOptions(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	if skew := modelSkewErr(ctx, rid); skew != nil {
		return skew, nil
	}
	pc, err := modelPickerContext(ctx, paramsSessionAgent(params))
	if err != nil {
		return nil, err
	}
	payload, err := buildModelOptionsPayload(ctx, pc, truthy(params["explicit_only"]), truthy(params["include_unconfigured"]), truthy(params["refresh"]))
	if err != nil {
		return nil, err
	}
	return ok(rid, payload), nil
}

// Pool environment of the scoped profile home with its live secret scope
func scopedPoolEnv(ctx context.Context) provider.PoolEnv {
	h := home.Dir(ctx)
	env := provider.HomePoolEnv(h)
	env.Scope, env.Ctx = provider.SecretScopeFrom(ctx), ctx
	return env
}

// Saves an API key for the provider and answers its refreshed picker row marked authenticated
func modelSaveKey(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	if skew := modelSkewErr(ctx, rid); skew != nil {
		return skew, nil
	}
	slug, apiKey := pystr.Strip(strOr(params["slug"])), pystr.Strip(strOr(params["api_key"]))
	if slug == "" || apiKey == "" {
		return rpcErr(rid, 4001, "slug and api_key are required", nil), nil
	}
	h := home.Dir(ctx)
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — credentials are read-only", nil), nil
	}
	pconfig := provider.RegistryRow(ctx, slug)
	if pconfig == nil {
		return rpcErr(rid, 4002, "unknown provider: "+slug, nil), nil
	}
	if pconfig.AuthType != provider.AuthTypeAPIKey {
		return rpcErr(rid, 4003, fmt.Sprintf("%s uses %s auth — run `icebath model` to configure", pconfig.Label(), pconfig.AuthType), nil), nil
	}
	if len(pconfig.KeyEnv) == 0 {
		return rpcErr(rid, 4004, "no env var defined for "+pconfig.Label(), nil), nil
	}
	envVar := pconfig.KeyEnv[0]
	previous, hadPrevious := config.LoadEnv(h)[envVar]
	if _, err := provider.SaveProviderEnvCredential(ctx, h, envVar, apiKey); err != nil {
		return nil, err
	}
	if !hadPrevious || apiKey != previous {
		observability.RecordProviderSetupDone(ctx, resolveSessionPlatform(), slug, h, true)
	}
	provider.ReconcileSetupRecord(scopedPoolEnv(ctx))
	pc, err := modelPickerContext(ctx, paramsSessionAgent(params))
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

// Removes every env key and OAuth or pool credential of the provider, refusing before any removal when one is locked
func modelDisconnect(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	slug := pystr.Strip(strOr(params["slug"]))
	if slug == "" {
		return rpcErr(rid, 4001, "slug is required", nil), nil
	}
	h := home.Dir(ctx)
	pconfig := provider.RegistryRow(ctx, slug)
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
