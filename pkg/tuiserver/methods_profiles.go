package tuiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Validated main model write of a profile, the dashboard profiles router's writer, assigned in cmd/icebath/wiring.go
var WriteProfileModel func(ctx context.Context, profileDir, providerName, model, baseURL string) error

// Asset extensions with their mime, in on disk lookup order
var assetExts = [][2]string{{"png", "image/png"}, {"jpg", "image/jpeg"}, {"webp", "image/webp"}}

// Magic byte ranges of each asset format, the format sniffed and never trusted from the mime
var assetMagic = map[string][]struct {
	start, end int
	magic      string
}{
	"png":  {{0, 8, "\x89PNG\r\n\x1a\n"}},
	"jpg":  {{0, 3, "\xff\xd8\xff"}},
	"webp": {{0, 4, "RIFF"}, {8, 12, "WEBP"}},
}

// Data URL of an image asset, its mime and base64 body
var assetDataURLRe = regexp.MustCompile(`(?s)^data:(image/(?:png|jpeg|webp));base64,(.*)$`)

// Serializes ui_meta compare and swap writes of profile.yaml
var profileUIMetaMu sync.Mutex

var profilesRegistry = &handlerRegistry{}

func init() {
	profileHandler("profiles.list", 5061, profilesList)
	profilesRegistry.method("profiles.create", profilesCreate)
	profileHandler("profiles.describe", 5063, profilesDescribe)
	profileHandler("profiles.configure", 5064, profilesConfigure)
	profileHandler("profiles.set_asset", 5065, profilesSetAsset)
	profileHandler("profiles.get_asset", 5066, profilesGetAsset)
	profilesRegistry.install()
}

// Registers a method whose raised failure answers its code with the error text
func profileHandler(name string, code int, fn Handler) {
	profilesRegistry.method(name, func(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				response = rpcErr(rid, code, failureText(failure), nil)
			}
		}()
		return fn(ctx, rid, params)
	})
}

// Pins a profile's main model through the validated writer
func pinProfileModel(ctx context.Context, profileDir, providerName, model, baseURL string) error {
	return WriteProfileModel(ctx, profileDir, providerName, model, baseURL)
}

// Model and provider params stripped
func modelProviderParams(params map[string]any) (string, string) {
	return strings.TrimSpace(relayString(params["model"])), strings.TrimSpace(relayString(params["provider"]))
}

// Runs fn, false when it raised or returned an error
func bestEffort(fn func() error) (succeeded bool) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			succeeded = false
		}
	}()
	return fn() == nil
}

// Result of fn, the default when it raised or returned an error
func tryOr[T any](fn func() (T, error), def T) (out T) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			out = def
		}
	}()
	value, err := fn()
	if err != nil {
		return def
	}
	return value
}

// Context bound to a profile's full runtime scope without secret hydration, the launch home keeping its launch scope
func hermesHomeScope(ctx context.Context, path string) context.Context {
	if resolveLax(path) == resolveLax(importedHome()) {
		return profileRuntimeScope(ctx, "", false)
	}
	return profileRuntimeScope(ctx, path, false)
}

// Name and directory of the requested profile, a 4063 or 4064 refusal when missing or unknown
func resolveProfileParam(rid any, params map[string]any) (string, string, map[string]any) {
	name := strings.TrimSpace(relayString(params["name"]))
	if name == "" {
		return name, "", rpcErr(rid, 4063, "name required", nil)
	}
	dir, err := profiles.GetProfileDir(name)
	if err != nil {
		var invalid *profiles.ValueError
		if errors.As(err, &invalid) {
			return name, "", rpcErr(rid, 4064, fmt.Sprintf("profile '%s' not found", name), nil)
		}
		panic(err)
	}
	if !isDir(dir) {
		return name, "", rpcErr(rid, 4064, fmt.Sprintf("profile '%s' not found", name), nil)
	}
	return name, dir, nil
}

// profile.yaml as a mapping, empty when missing, unreadable, unparseable or not a mapping
func readProfileYAML(profileDir string) map[string]any {
	loaded := tryOr(func() (map[string]any, error) {
		metaPath := filepath.Join(profileDir, "profile.yaml")
		info, err := os.Stat(metaPath)
		if err != nil || !info.Mode().IsRegular() {
			return map[string]any{}, nil
		}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			return nil, err
		}
		doc, err := config.FastSafeLoad(data)
		if err != nil {
			return nil, err
		}
		mapping, _ := doc.(map[string]any)
		if mapping == nil {
			return map[string]any{}, nil
		}
		return mapping, nil
	}, map[string]any{})
	if loaded == nil {
		return map[string]any{}
	}
	return loaded
}

// JSON form of a YAML value, its timestamps written in ISO 8601 and every other scalar unchanged
func yamlToJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = yamlToJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = yamlToJSON(item)
		}
		return out
	case pydatetime.Date:
		return v.PyStr()
	case pydatetime.DateTime:
		text := v.PyStr()
		return text[:10] + "T" + text[11:]
	}
	return value
}

// Revision map with str keys and non bool ints clamped at zero
func cleanRevisions(raw map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range raw {
		if n, isInt := value.(int); isInt {
			out[key] = max(0, n)
		}
	}
	return out
}

// Excerpt of at most 80 characters of the newest active user or assistant message, empty when none
func latestMessagePreview(ctx context.Context, db *state.DB, sessionID string) string {
	rows, err := db.ReadAll(ctx, "SELECT content FROM messages WHERE session_id = ? AND role IN ('user', 'assistant')"+
		" AND active = 1 AND content IS NOT NULL AND TRIM(content) != '' ORDER BY id DESC LIMIT 1", sessionID)
	if err != nil || len(rows) == 0 {
		return ""
	}
	content := rows[0].GetOr("content", nil)
	text := ""
	if truthy(content) {
		text = strings.Join(strings.Fields(pyrepr.Str(content)), " ")
	}
	runes := []rune(text)
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return text
}

// Unarchives an accidentally archived canonical row through a short lived writable handle, false otherwise
func resurrectRecoverableCanonical(ctx context.Context, db *state.DB, profilePath, sessionID string) (resurrected bool) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			resurrected = false
		}
	}()
	row, err := db.GetSession(ctx, sessionID)
	if err != nil || row == nil || !truthy(row.GetOr("archived", nil)) {
		return false
	}
	tipID := tryOr(func() (string, error) { return db.GetCompressionTip(ctx, sessionID) }, "")
	if tipID == "" {
		tipID = sessionID
	}
	tip := row
	if tipID != sessionID {
		if found := tryOr(func() (*pyjson.Object, error) { return db.GetSession(ctx, tipID) }, nil); found != nil {
			tip = found
		}
	}
	if !state.IsRecoverableEndReason(relayString(tip.GetOr("end_reason", nil))) {
		return false
	}
	writable, err := state.Acquire(ctx, filepath.Join(profilePath, "state.db"))
	if err != nil {
		return false
	}
	defer bestEffort(func() error { state.ReleaseOrClose(writable); return nil })
	done, err := writable.UnarchiveRecoverableSession(ctx, sessionID)
	return err == nil && done
}

// Summary of the profile's canonical Bot Chat row with its live tip, nil when absent, retired or unreadable
func canonicalSessionRow(ctx context.Context, db *state.DB, profilePath string) (out map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			out = nil
		}
	}()
	if db == nil {
		return nil
	}
	row, err := db.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil {
		return nil
	}
	sessionID := ""
	if row != nil {
		sessionID = strings.TrimSpace(relayString(row.GetOr("id", nil)))
	}
	if sessionID == "" || deniedSource(row) {
		return nil
	}
	if truthy(row.GetOr("archived", nil)) && !resurrectRecoverableCanonical(ctx, db, profilePath, sessionID) {
		return nil
	}
	tip := tryOr(func() (string, error) { return db.GetCompressionTip(ctx, sessionID) }, "")
	if tip == "" {
		tip = sessionID
	}
	tipRow, err := db.GetSession(ctx, tip)
	if err != nil {
		return nil
	}
	if tipRow == nil {
		tipRow = row
	}
	started := firstTruthy(row.GetOr("started_at", nil), 0)
	out = map[string]any{"id": sessionID, "resolved_id": tip, "root_title": firstTruthy(row.GetOr("title", nil), ""),
		"title": firstTruthy(tipRow.GetOr("title", nil), ""), "preview": latestMessagePreview(ctx, db, tip),
		"started_at":    firstTruthy(tipRow.GetOr("started_at", nil), started),
		"last_active":   firstTruthy(tipRow.GetOr("last_activity_at", nil), tipRow.GetOr("started_at", nil), started),
		"message_count": firstTruthy(tipRow.GetOr("message_count", nil), 0)}
	for key, value := range liveCountField(ctx, db, tip) {
		out[key] = value
	}
	return out
}

// Newest human facing session and newest worker session of a profile store, both nil on any failure
func latestProfileSessionRows(ctx context.Context, db *state.DB) (human, worker map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			human, worker = nil, nil
		}
	}()
	if db == nil {
		return nil, nil
	}
	limit := 20
	rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, OrderByLastActive: true, CompactRows: true})
	if err != nil {
		return nil, nil
	}
	for _, s := range rows {
		title := firstTruthy(s.GetOr("title", nil), "")
		lastActive := firstTruthy(s.GetOr("last_active", nil), s.GetOr("started_at", nil), 0)
		id := s.GetOr("id", nil)
		if deniedSource(s) {
			if worker == nil {
				worker = map[string]any{"id": id, "source": strings.ToLower(strings.TrimSpace(relayString(s.GetOr("source", nil)))),
					"title": title, "last_active": lastActive}
			}
		} else if human == nil {
			preview := latestMessagePreview(ctx, db, pyrepr.Str(id))
			human = map[string]any{"id": id, "title": title, "preview": firstTruthy(preview, s.GetOr("preview", nil), ""),
				"started_at": firstTruthy(s.GetOr("started_at", nil), 0), "last_active": lastActive,
				"message_count": firstTruthy(s.GetOr("message_count", nil), 0)}
			for key, value := range liveCountField(ctx, db, pyrepr.Str(id)) {
				human[key] = value
			}
		}
		if human != nil && worker != nil {
			break
		}
	}
	return human, worker
}

// Attaches last_session, worker_session and canonical_session from a read only attach of the profile store
func profileSessionFields(ctx context.Context, row map[string]any, profilePath string) {
	read := func() (map[string]any, error) {
		dbPath := filepath.Join(profilePath, "state.db")
		var db *state.DB
		if _, err := os.Stat(dbPath); err == nil {
			db = tryOr(func() (*state.DB, error) { return state.NewSessionDB(ctx, dbPath, true) }, nil)
		}
		defer func() {
			if db != nil {
				bestEffort(db.Close)
			}
		}()
		last, worker := latestProfileSessionRows(ctx, db)
		return map[string]any{"last_session": nilMap(last), "worker_session": nilMap(worker),
			"canonical_session": nilMap(canonicalSessionRow(ctx, db, profilePath))}, nil
	}
	fields, err := tuigateway.CachedSessionFields(profilePath, read)
	if err != nil {
		panic(err)
	}
	for key, value := range fields {
		row[key] = value
	}
}

// Attaches ui_meta, ui_meta_revisions and has_avatar from profile.yaml and the assets folder
func profileUIMetaFields(row map[string]any, profileDir string) {
	read := func() (map[string]any, error) {
		raw := readProfileYAML(profileDir)
		revisions := map[string]any{}
		if rawRevisions, isDict := raw["_ui_meta_revisions"].(map[string]any); isDict {
			revisions = tryOr(func() (map[string]any, error) { return cleanRevisions(rawRevisions), nil }, map[string]any{})
		}
		fields := map[string]any{"ui_meta_revisions": revisions}
		if uiMeta, isDict := raw["ui_meta"].(map[string]any); isDict && len(uiMeta) > 0 {
			fields["ui_meta"] = yamlToJSON(uiMeta)
		}
		return fields, nil
	}
	fields, err := tuigateway.CachedUIMetaFields(profileDir, read)
	if err != nil {
		panic(err)
	}
	for key, value := range fields {
		row[key] = value
	}
	row["has_avatar"] = tryOr(func() (bool, error) {
		for _, ext := range assetExts {
			if info, err := os.Stat(filepath.Join(profileDir, "assets", "avatar."+ext[0])); err == nil && info.Mode().IsRegular() {
				return true, nil
			}
		}
		return false, nil
	}, false)
}

// Every profile with its roster sessions, ui meta and avatar flag, the install id proving which machine answered
func profilesList(ctx context.Context, rid any, params map[string]any) map[string]any {
	includeSessions := true
	if raw, present := params["include_sessions"]; present {
		includeSessions = isTruthyValue(raw)
	}
	infos, err := manage.ListProfiles(ctx, true)
	if err != nil {
		panic(err)
	}
	out := []any{}
	for _, p := range infos {
		previous := []any{}
		for _, name := range p.PreviousNames {
			previous = append(previous, name)
		}
		row := map[string]any{"name": p.Name, "path": p.Path, "is_default": p.IsDefault, "model": p.Model, "provider": p.Provider,
			"description": p.Description, "display_name": p.DisplayName, "skill_count": p.SkillCount, "previous_names": previous}
		if includeSessions {
			profileSessionFields(ctx, row, p.Path)
		}
		profileUIMetaFields(row, p.Path)
		out = append(out, row)
	}
	installID := tryOr(func() (string, error) { return installidentity.GetInstallID(ctx, nil) }, "")
	return ok(rid, map[string]any{"profiles": out, "bot_mode_protocol": true, "install_id": installID})
}

// Creates a profile with bundled skills, its alias, soul, mirrored launch credentials and model
func profilesCreate(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := strings.TrimSpace(relayString(params["name"]))
	if name == "" {
		return rpcErr(rid, 4061, "name required", nil)
	}
	cloneFrom := strings.TrimSpace(relayString(params["clone_from"]))
	cloneAll := isTruthyValue(paramOr(params, "clone_all", false))
	opts := manage.CreateOptions{CloneAll: cloneAll, CloneConfig: cloneFrom != "" && !cloneAll,
		NoSkills: isTruthyValue(paramOr(params, "no_skills", false)), CloneChannels: isTruthyValue(paramOr(params, "clone_channels", false))}
	if cloneFrom != "" {
		opts.CloneFrom = &cloneFrom
	}
	if description := strings.TrimSpace(relayString(params["description"])); description != "" {
		opts.Description = &description
	}
	path, err := func() (path string, err error) {
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				err = errors.New(failureText(failure))
			}
		}()
		return manage.CreateProfile(ctx, name, opts)
	}()
	if err != nil {
		var valueErr *profiles.ValueError
		var existsErr *profiles.ExistsError
		var notFound *profiles.NotFoundError
		if errors.As(err, &valueErr) || errors.As(err, &existsErr) || errors.As(err, &notFound) || errors.Is(err, os.ErrExist) ||
			errors.Is(err, os.ErrNotExist) {
			return rpcErr(rid, 4062, err.Error(), nil)
		}
		return rpcErr(rid, 5062, err.Error(), nil)
	}
	if cloneFrom == "" {
		bestEffort(func() error { manage.SeedProfileSkills(ctx, path, true); return nil })
	}
	if !isTruthyValue(paramOr(params, "no_alias", false)) {
		bestEffort(func() error {
			collision, err := profiles.CheckAliasCollision(name)
			if err != nil || collision != "" {
				return err
			}
			_, err = profiles.CreateWrapperScript(name, "")
			return err
		})
	}
	soul, isText := params["soul"].(string)
	soulWritten := isText && strings.TrimSpace(soul) != "" && bestEffort(func() error {
		return os.WriteFile(filepath.Join(path, "SOUL.md"), []byte(soul), 0o644)
	})
	mirrored := mirrorLaunchCredentials(ctx, path, params)
	model, providerName := modelProviderParams(params)
	modelSet := false
	if model != "" && providerName != "" {
		modelSet = bestEffort(func() error { return pinProfileModel(ctx, path, providerName, model, "") })
	} else if isTruthyValue(paramOr(params, "mirror_credentials", true)) {
		mirrored["model_inherited"] = tryOr(func() (bool, error) { return inheritLaunchModel(ctx, path) }, false)
	}
	return ok(rid, map[string]any{"ok": true, "name": name, "path": path, "soul_written": soulWritten, "model_set": modelSet, "mirrored": mirrored})
}

// Editor snapshot of a profile, its skills, toolsets, soul, model and MCP servers
func profilesDescribe(ctx context.Context, rid any, params map[string]any) map[string]any {
	name, profileDir, failure := resolveProfileParam(rid, params)
	if failure != nil {
		return failure
	}
	scoped := hermesHomeScope(ctx, profileDir)
	cfg, err := config.LoadContext(scoped, profileDir, nil)
	if err != nil {
		panic(err)
	}
	disabled := map[string]bool{}
	for skill := range skills.GetDisabledSkills(cfg, "") {
		disabled[strings.ToLower(skill)] = true
	}
	installed := []any{}
	if skillsRoot := filepath.Join(profileDir, "skills"); isDir(skillsRoot) {
		for _, md := range skills.IterSkillIndexFiles(skillsRoot, "SKILL.md") {
			skillName := filepath.Base(filepath.Dir(md))
			installed = append(installed, map[string]any{"name": skillName, "enabled": !disabled[strings.ToLower(skillName)]})
		}
	}
	toolsetsOut, pinned := describeToolsets(scoped, cfg)
	soul := tryOr(func() (string, error) {
		soulPath := filepath.Join(profileDir, "SOUL.md")
		info, err := os.Stat(soulPath)
		if err != nil || !info.Mode().IsRegular() {
			return "", nil
		}
		data, err := os.ReadFile(soulPath)
		return strings.ToValidUTF8(string(data), "\ufffd"), err
	}, "")
	mcpOut := []any{}
	if mcpCfg, isDict := cfg["mcp_servers"].(map[string]any); isDict {
		mcpOut = tryOr(func() ([]any, error) {
			rows := []any{}
			for _, serverName := range sortedKeys(mcpCfg) {
				entry, isEntry := mcpCfg[serverName].(map[string]any)
				if !isEntry {
					continue
				}
				transport := "stdio"
				if truthy(entry["url"]) {
					transport = "http"
					if truthy(entry["transport"]) {
						transport = relayString(entry["transport"])
					}
				}
				rows = append(rows, map[string]any{"name": serverName, "enabled": mcpEntryEnabled(entry), "transport": transport})
			}
			return rows, nil
		}, []any{})
	}
	modelCfg, _ := cfg["model"].(map[string]any)
	meta := profiles.ReadProfileMeta(profileDir)
	return ok(rid, map[string]any{"name": name, "description": meta.Description, "soul": soul,
		"model":  map[string]any{"provider": relayString(modelCfg["provider"]), "default": relayString(modelCfg["default"])},
		"skills": installed, "toolsets": toolsetsOut, "toolsets_pinned": pinned != nil, "mcp_servers": mcpOut})
}

// Editor save of a profile's independent sections, each reported in applied
func profilesConfigure(ctx context.Context, rid any, params map[string]any) map[string]any {
	_, profileDir, failure := resolveProfileParam(rid, params)
	if failure != nil {
		return failure
	}
	applied := map[string]any{}
	if _, isDict := params["ui_meta"].(map[string]any); isDict {
		configureUIMeta(profileDir, params, applied)
	}
	if soul, isText := params["soul"].(string); isText {
		applied["soul"] = bestEffort(func() error { return os.WriteFile(filepath.Join(profileDir, "SOUL.md"), []byte(soul), 0o644) })
	}
	if description, isText := params["description"].(string); isText {
		trimmed, auto := strings.TrimSpace(description), false
		applied["description"] = bestEffort(func() error {
			return profiles.WriteProfileMeta(ctx, profileDir, profiles.MetaUpdate{Description: &trimmed, DescriptionAuto: &auto})
		})
	}
	confirmMessage := configureModel(ctx, profileDir, params, applied)
	for _, key := range []string{"disabled_skills", "enabled_toolsets", "enabled_mcp_servers"} {
		if _, isList := params[key].([]any); isList {
			configureCfgSections(ctx, profileDir, params, applied)
			break
		}
	}
	allApplied := true
	for _, value := range applied {
		allApplied = allApplied && truthy(value)
	}
	out := map[string]any{"ok": allApplied, "applied": applied}
	if confirmMessage != nil {
		out["confirm_required"], out["confirm_message"] = true, *confirmMessage
	}
	return ok(rid, out)
}

// Stores or clears the profile avatar, a PNG, JPEG or WebP of at most 2MB sniffed from its bytes
func profilesSetAsset(ctx context.Context, rid any, params map[string]any) map[string]any {
	asset := strings.ToLower(strings.TrimSpace(relayString(paramOr(params, "asset", "avatar"))))
	if !truthy(params["asset"]) {
		asset = "avatar"
	}
	if strings.TrimSpace(relayString(params["name"])) == "" {
		return rpcErr(rid, 4063, "name required", nil)
	}
	if asset != "avatar" {
		return rpcErr(rid, 4066, fmt.Sprintf("unknown asset '%s' (supported: avatar)", asset), nil)
	}
	_, profileDir, failure := resolveProfileParam(rid, params)
	if failure != nil {
		return failure
	}
	assetsDir := filepath.Join(profileDir, "assets")
	if isTruthyValue(paramOr(params, "clear", false)) {
		return ok(rid, map[string]any{"ok": true, "asset": asset, "size": 0, "removed": unlinkAssetFiles(assetsDir, asset)})
	}
	data := relayString(params["data"])
	if data == "" {
		return rpcErr(rid, 4067, "data required (data URL or base64)", nil)
	}
	body := data
	if match := assetDataURLRe.FindStringSubmatch(data); match != nil {
		body = match[2]
	}
	blob, err := pybase64.B64DecodeValidate(body)
	if err != nil {
		return rpcErr(rid, 4068, "data is not valid base64", nil)
	}
	if len(blob) > 2_000_000 {
		return rpcErr(rid, 4069, fmt.Sprintf("asset too large (%d bytes; max 2MB)", len(blob)), nil)
	}
	ext := ""
	for _, candidate := range assetExts {
		matched := true
		for _, magic := range assetMagic[candidate[0]] {
			if len(blob) < magic.end || !bytes.Equal(blob[magic.start:magic.end], []byte(magic.magic)) {
				matched = false
			}
		}
		if matched {
			ext = candidate[0]
			break
		}
	}
	if ext == "" {
		return rpcErr(rid, 4070, "unsupported image format (PNG/JPEG/WebP only)", nil)
	}
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		panic(err)
	}
	unlinkAssetFiles(assetsDir, asset)
	tmp := filepath.Join(assetsDir, asset+"."+ext+".tmp")
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		panic(err)
	}
	if err := os.Rename(tmp, filepath.Join(assetsDir, asset+"."+ext)); err != nil {
		panic(err)
	}
	return ok(rid, map[string]any{"ok": true, "asset": asset, "size": len(blob)})
}

// Profile asset as a data URL, found false when absent
func profilesGetAsset(ctx context.Context, rid any, params map[string]any) map[string]any {
	asset := "avatar"
	if truthy(params["asset"]) {
		asset = strings.ToLower(strings.TrimSpace(relayString(params["asset"])))
	}
	_, profileDir, failure := resolveProfileParam(rid, params)
	if failure != nil {
		return failure
	}
	for _, ext := range assetExts {
		target := filepath.Join(profileDir, "assets", asset+"."+ext[0])
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
			blob, err := os.ReadFile(target)
			if err != nil {
				panic(err)
			}
			return ok(rid, map[string]any{"found": true, "mime": ext[1], "size": len(blob),
				"data": "data:" + ext[1] + ";base64," + base64.StdEncoding.EncodeToString(blob)})
		}
	}
	return ok(rid, map[string]any{"found": false})
}

// Copies a launch file into the profile with owner only permissions when it exists and wanted allows it
func mirrorSecret(path, launchHome, name string, wanted func(src, dst string) bool) (bool, error) {
	src, dst := filepath.Join(launchHome, name), filepath.Join(path, name)
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() || !wanted(src, dst) {
		return false, nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return false, err
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	_ = os.Chmod(dst, 0o600)
	return true, nil
}

// Whether a dotenv file holds any line that is not blank or a comment
func envHasContent(envPath string) (bool, error) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(string(data), "\ufffd"), "\r\n", "\n"), "\r", "\n"), "\n") {
		if text := strings.TrimSpace(line); text != "" && !strings.HasPrefix(text, "#") {
			return true, nil
		}
	}
	return false, nil
}

// Copies the launch profile's stt, tts and voice sections the new profile lacks, true when written
func mirrorVoiceSections(ctx context.Context, path string) (written bool) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			written = false
		}
	}()
	srcCfg, err := config.ReadUser(home.Dir(ctx))
	if err != nil {
		return false
	}
	sections := map[string]any{}
	for _, key := range []string{"stt", "tts", "voice"} {
		if truthy(srcCfg[key]) {
			sections[key] = srcCfg[key]
		}
	}
	if len(sections) == 0 {
		return false
	}
	scoped := hermesHomeScope(ctx, path)
	dstCfg, err := config.ReadUser(path)
	if err != nil {
		return false
	}
	if dstCfg == nil {
		dstCfg = map[string]any{}
	}
	missing := map[string]any{}
	for key, value := range sections {
		if _, present := dstCfg[key]; !present {
			missing[key] = value
		}
	}
	if len(missing) > 0 {
		merged := make(map[string]any, len(dstCfg)+len(missing))
		for key, value := range dstCfg {
			merged[key] = value
		}
		for key, value := range missing {
			merged[key] = value
		}
		if err := config.SaveConfig(scoped, path, merged, config.SaveOptions{}); err != nil {
			return false
		}
	}
	return len(missing) > 0
}

// Inherits the launch model and its custom endpoint when the new profile has no model of its own
func inheritLaunchModel(ctx context.Context, path string) (bool, error) {
	scoped := hermesHomeScope(ctx, path)
	dstRaw, err := config.ReadUser(path)
	if err != nil {
		return false, err
	}
	dstModel, _ := dstRaw["model"].(map[string]any)
	if truthy(dstModel["provider"]) && truthy(dstModel["default"]) {
		return false, nil
	}
	launchHome := home.Dir(ctx)
	launchCfg, err := config.LoadReadonly(ctx, launchHome, nil)
	if err != nil {
		return false, err
	}
	modelCfg, _ := launchCfg["model"].(map[string]any)
	if !(truthy(modelCfg["provider"]) && truthy(modelCfg["default"])) {
		return false, nil
	}
	launchRaw, err := config.ReadUser(launchHome)
	if err != nil {
		return false, err
	}
	seed := provider.LaunchModelSeed(ctx, launchRaw)
	if truthy(seed["providers"]) || truthy(seed["custom_providers"]) {
		current, err := config.ReadUser(path)
		if err != nil {
			return false, err
		}
		if err := config.SaveConfig(scoped, path, provider.MergeEndpointSeed(ctx, current, seed), config.SaveOptions{}); err != nil {
			return false, err
		}
	}
	if err := pinProfileModel(ctx, path, pyrepr.Str(modelCfg["provider"]), pyrepr.Str(modelCfg["default"]), relayString(modelCfg["base_url"])); err != nil {
		return false, err
	}
	launchModel, _ := launchRaw["model"].(map[string]any)
	if rawURL, isText := launchModel["base_url"].(string); isText && strings.Contains(rawURL, "${") {
		cfg, err := config.ReadUser(path)
		if err != nil {
			return false, err
		}
		if pinned, isDict := cfg["model"].(map[string]any); isDict && pyjson.Equal(pinned["base_url"], modelCfg["base_url"]) {
			pinned["base_url"] = rawURL
			if err := config.SaveConfig(scoped, path, cfg, config.SaveOptions{}); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// Copies the launch dotenv, auth store and voice sections into a new profile, each item independently
func mirrorLaunchCredentials(ctx context.Context, path string, params map[string]any) map[string]any {
	shareAuth := isTruthyValue(paramOr(params, "share_auth", false))
	mirrored := map[string]any{"env": false, "auth": false, "model_inherited": false, "voice": false}
	if shareAuth {
		mirrored["auth"] = "shared"
	}
	if !isTruthyValue(paramOr(params, "mirror_credentials", true)) {
		return mirrored
	}
	launchHome := home.Dir(ctx)
	mirrored["env"] = tryOr(func() (bool, error) {
		return mirrorSecret(path, launchHome, ".env", func(src, dst string) bool {
			has, err := envHasContent(src)
			if err != nil || !has {
				return false
			}
			return !tryOr(func() (bool, error) { return envHasContent(dst) }, false)
		})
	}, false)
	if mirrored["env"] == true && !isTruthyValue(paramOr(params, "clone_channels", false)) {
		bestEffort(func() error {
			_, err := manage.StripChannelEnvFile(ctx, filepath.Join(path, ".env"), nil)
			return err
		})
	}
	if !shareAuth {
		mirrored["auth"] = tryOr(func() (bool, error) {
			return mirrorSecret(path, launchHome, "auth.json", func(_, dst string) bool {
				_, err := os.Stat(dst)
				return errors.Is(err, os.ErrNotExist)
			})
		}, false)
		if mirrored["auth"] == true {
			bestEffort(func() error { provider.StripClonedSingleUseOAuthGrants(path); return nil })
		}
	}
	mirrored["voice"] = mirrorVoiceSections(ctx, path)
	return mirrored
}

// Configurable toolsets as the tools checklist presents them and the pinned set, nil when not pinned
func describeToolsets(ctx context.Context, cfg config.Tree) ([]any, map[string]bool) {
	var pinned map[string]bool
	if names, isList := tools.CoercePlatformToolsetsValue(cfg.Sub("platform_toolsets")["cli"], "cli"); isList {
		pinned = cleanNames(names)
	}
	scope := tools.ScopeKey(home.Dir(ctx))
	platformEnabled := tryOr(func() (map[string]bool, error) {
		return tools.Default.PlatformTools(ctx, scope, cfg, "cli", tools.PlatformToolsOptions{}), nil
	}, map[string]bool{})
	out := []any{}
	for _, ts := range tools.EffectiveConfigurableToolsets(ctx) {
		var enabled bool
		if pinned != nil {
			enabled = pinned[ts.Key]
		} else {
			enabled = platformEnabled[ts.Key]
		}
		if !tools.ToolsetAllowedForPlatform(ts.Key, "cli") || ((tools.DefaultOffToolsets[ts.Key] || ts.Key == "yuanbao") && !enabled) {
			continue
		}
		toolCount := tryOr(func() (int, error) {
			distinct := map[string]bool{}
			for _, tool := range tools.Default.ResolveToolset(ctx, scope, ts.Key) {
				distinct[tool] = true
			}
			return len(distinct), nil
		}, 0)
		out = append(out, map[string]any{"name": ts.Key, "label": ts.Label, "description": ts.Description, "tool_count": toolCount, "enabled": enabled})
	}
	return out, pinned
}

// Bot Mode title of a ui_meta mapping
func botsTitle(uiMeta map[string]any) any {
	bots, isDict := uiMeta["hermes-bots"].(map[string]any)
	if !isDict {
		return nil
	}
	return bots["title"]
}

// Merges ui_meta key wise into profile.yaml under per key compare and swap revisions
func configureUIMeta(profileDir string, params map[string]any, applied map[string]any) {
	applied["ui_meta"] = false
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			applied["ui_meta"] = false
		}
	}()
	incoming := params["ui_meta"].(map[string]any)
	encoded, err := json.Marshal(incoming)
	if err != nil {
		panic(err)
	}
	if len([]rune(string(encoded))) > 65536 {
		return
	}
	expectedRaw, hasExpected := params["ui_meta_expected_revisions"]
	expected, expectedDict := expectedRaw.(map[string]any)
	if hasExpected && expectedRaw != nil && !expectedDict {
		panic(errors.New("ui_meta_expected_revisions must be an object"))
	}
	profileUIMetaMu.Lock()
	defer profileUIMetaMu.Unlock()
	existing := readProfileYAML(profileDir)
	rawRevisions, _ := existing["_ui_meta_revisions"].(map[string]any)
	revisions := cleanRevisions(rawRevisions)
	revisionOf := func(key string) int {
		n, _ := revisions[key].(int)
		return n
	}
	conflicts := map[string]any{}
	if expectedDict {
		for key := range incoming {
			wanted := expected[key]
			wantedInt, isInt := wanted.(int)
			if !isInt || wantedInt < 0 || wantedInt != revisionOf(key) {
				conflicts[key] = map[string]any{"expected": wanted, "actual": revisionOf(key)}
			}
		}
	}
	if len(conflicts) > 0 {
		applied["ui_meta_conflicts"] = conflicts
		current := map[string]any{}
		for key := range incoming {
			current[key] = revisionOf(key)
		}
		applied["ui_meta_revisions"] = current
		return
	}
	current, _ := existing["ui_meta"].(map[string]any)
	if current == nil {
		current = map[string]any{}
	}
	oldTitle := botsTitle(current)
	for key, value := range incoming {
		if value == nil {
			delete(current, key)
		} else {
			current[key] = value
		}
		revisions[key] = revisionOf(key) + 1
	}
	if len(current) > 0 {
		existing["ui_meta"] = current
	} else {
		delete(existing, "ui_meta")
	}
	existing["_ui_meta_revisions"] = revisions
	if err := config.AtomicYAMLWrite(context.Background(), filepath.Join(profileDir, "profile.yaml"), existing, ""); err != nil {
		panic(err)
	}
	applied["ui_meta"] = true
	written := map[string]any{}
	for key := range incoming {
		written[key] = revisions[key]
	}
	applied["ui_meta_revisions"] = written
	if newTitle := botsTitle(current); !pyjson.Equal(newTitle, oldTitle) {
		name := relayString(params["name"])
		if name == "" {
			name = filepath.Base(profileDir)
		}
		logger().Info(fmt.Sprintf("ui_meta hermes-bots title for profile %s: %s -> %s (revision %s)", name, pyrepr.Repr(oldTitle),
			pyrepr.Repr(newTitle), pyrepr.Repr(revisions["hermes-bots"])))
	}
}

// Pins the model unless the selection guard warns, the warning answered as a confirm message with nothing written
func configureModel(ctx context.Context, profileDir string, params map[string]any, applied map[string]any) *string {
	model, providerName := modelProviderParams(params)
	if model == "" || providerName == "" {
		return nil
	}
	var confirmMessage *string
	if !isTruthyValue(paramOr(params, "confirm_expensive_model", false)) {
		confirmMessage = tryOr(func() (*string, error) {
			warning := provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: model, Provider: providerName})
			if warning == nil {
				return nil, nil
			}
			message := warning.Message
			return &message, nil
		}, nil)
	}
	if confirmMessage == nil {
		applied["model"] = bestEffort(func() error { return pinProfileModel(ctx, profileDir, providerName, model, "") })
	}
	return confirmMessage
}

// Stripped non empty names of a list
func cleanNames[T any](values []T) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		if text := strings.TrimSpace(pyrepr.Str(value)); text != "" {
			out[text] = true
		}
	}
	return out
}

// Pins platform_toolsets.cli, an empty selection clearing the pin
func saveToolsetPin(ctx context.Context, h string, cfg config.Tree, enabled []any) error {
	wanted := cleanNames(enabled)
	if len(wanted) > 0 {
		if err := tools.Default.SavePlatformTools(ctx, tools.ScopeKey(h), cfg, "cli", wanted, tools.PluginToolsetKeys(ctx)); err != nil {
			return err
		}
	} else if platform, isDict := cfg["platform_toolsets"].(map[string]any); isDict {
		delete(platform, "cli")
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Whether an MCP entry is enabled, a legacy disabled true still reading as off
func mcpEntryEnabled(entry map[string]any) bool {
	return tools.MCPServerEnabled(entry) && !tools.ParseEnabledFlag(paramOr(entry, "disabled", false), false)
}

// Writes the enabled flag of every MCP server, copying wanted undefined ones from the launch catalog
func saveMCPToggles(ctx context.Context, h string, cfg config.Tree, enabled []any, launchMCP map[string]any) error {
	wanted := cleanNames(enabled)
	mcpCfg, isDict := cfg["mcp_servers"].(map[string]any)
	if !isDict {
		mcpCfg = map[string]any{}
	}
	for server := range wanted {
		if _, defined := mcpCfg[server].(map[string]any); !defined {
			if launch, isEntry := launchMCP[server].(map[string]any); isEntry {
				copied := make(map[string]any, len(launch))
				for key, value := range launch {
					copied[key] = value
				}
				mcpCfg[server] = copied
			}
		}
	}
	for server, raw := range mcpCfg {
		if entry, isEntry := raw.(map[string]any); isEntry {
			entry["enabled"] = wanted[server]
			delete(entry, "disabled")
		}
	}
	if len(mcpCfg) > 0 {
		cfg["mcp_servers"] = mcpCfg
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Applies disabled skills, the toolset pin and MCP toggles, each verified against the user layer as read back
func configureCfgSections(ctx context.Context, profileDir string, params map[string]any, applied map[string]any) {
	enabledMCP, wantMCP := params["enabled_mcp_servers"].([]any)
	launchMCP := map[string]any{}
	if wantMCP {
		launchMCP = tryOr(func() (map[string]any, error) {
			raw, err := config.ReadUser(home.Dir(ctx))
			if err != nil {
				return nil, err
			}
			servers, _ := raw["mcp_servers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			return servers, nil
		}, map[string]any{})
	}
	scoped := hermesHomeScope(ctx, profileDir)
	load := func() config.Tree {
		cfg, err := config.LoadContext(scoped, profileDir, nil)
		if err != nil {
			panic(err)
		}
		if cfg == nil {
			cfg = config.Tree{}
		}
		return cfg
	}
	cfg := load()
	if skillsList, isList := params["disabled_skills"].([]any); isList {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					if configOutage(failure) {
						panic(failure)
					}
					applied["skills"] = false
				}
			}()
			if err := skills.SaveDisabledSkills(scoped, profileDir, cfg, cleanNames(skillsList), ""); err != nil {
				applied["skills"] = false
				return
			}
			applied["skills"] = landed(profileDir, cfg, []string{"skills", "disabled"}, nil)
			cfg = load()
		}()
	}
	if toolsetList, isList := params["enabled_toolsets"].([]any); isList {
		applied["toolsets"] = bestEffort(func() error { return saveToolsetPin(scoped, profileDir, cfg, toolsetList) }) &&
			landed(profileDir, cfg, []string{"platform_toolsets", "cli"}, nil)
	}
	if wantMCP {
		mcpCfg := load()
		applied["mcp_servers"] = bestEffort(func() error { return saveMCPToggles(scoped, profileDir, mcpCfg, enabledMCP, launchMCP) }) &&
			landed(profileDir, mcpCfg, []string{"mcp_servers"}, mcpToggleState)
	}
}

// Marker of a toggle key a server entry does not hold
const toggleAbsent = "<absent>"

// Every toggle field the MCP save writes per server, the enabled flag and the legacy disabled key
func mcpToggleState(servers any) any {
	mapping, isDict := servers.(map[string]any)
	if !isDict {
		return servers
	}
	out := map[string]any{}
	for key, raw := range mapping {
		if entry, isEntry := raw.(map[string]any); isEntry {
			out[key] = []any{entry["enabled"], paramOr(entry, "disabled", toggleAbsent)}
		}
	}
	return out
}

// Whether a section a bulk save was asked to write reads back from the user layer as written
func landed(profileDir string, cfg map[string]any, path []string, key func(any) any) bool {
	if key == nil {
		key = func(v any) any { return v }
	}
	at := func(doc map[string]any) any {
		var node any = doc
		for _, seg := range path {
			mapping, isDict := node.(map[string]any)
			if !isDict {
				return nil
			}
			value, present := mapping[seg]
			if !present {
				return nil
			}
			node = value
		}
		return node
	}
	raw, err := config.ReadUser(profileDir)
	if err != nil {
		panic(err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return pyjson.Equal(key(at(raw)), key(at(cfg)))
}

// Deletes every file of an asset and returns how many existed
func unlinkAssetFiles(assetsDir, asset string) int {
	var present []string
	for _, ext := range assetExts {
		target := filepath.Join(assetsDir, asset+"."+ext[0])
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
			present = append(present, target)
		}
	}
	for _, target := range present {
		if err := os.Remove(target); err != nil {
			panic(err)
		}
	}
	return len(present)
}
