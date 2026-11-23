package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Asset extensions with their mime, in on disk lookup order
var assetExts = [][2]string{{"png", "image/png"}, {"jpg", "image/jpeg"}, {"webp", "image/webp"}}

var profilesRegistry = &handlerRegistry{}

func init() {
	profilesRegistry.method("profiles.create", profilesCreate)
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
	counted, err := liveCountField(ctx, db, tip)
	panicIf(err)
	for key, value := range counted {
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
			counted, err := liveCountField(ctx, db, pyrepr.Str(id))
			panicIf(err)
			for key, value := range counted {
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
	fields, err := tuihost.CachedSessionFields(profilePath, read)
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
	fields, err := tuihost.CachedUIMetaFields(profileDir, read)
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
