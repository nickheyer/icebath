package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Non flag resume tokens joined with a literal usage hint wrapper stripped
func stripResumeName(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p != "--all" && p != "--cross-room" {
			kept = append(kept, p)
		}
	}
	name := pystr.Strip(strings.Join(kept, " "))
	runes := []rune(name)
	if len(runes) >= 2 {
		switch string([]rune{runes[0], runes[len(runes)-1]}) {
		case "<>", "[]", `""`, "''":
			name = pystr.Strip(string(runes[1 : len(runes)-1]))
		}
	}
	return name
}

// Chat name of a source, its chat id when unnamed
func chatNameOrID(source *gateway.SessionSource) string {
	if source.ChatName != nil && *source.ChatName != "" {
		return *source.ChatName
	}
	return source.ChatID
}

// Titled sessions visible to the caller, origin scoped unless an admin asked for all
func (r *GatewayRunner) listTitledSessions(ctx context.Context, source *gateway.SessionSource, sessionKey string, allowAll bool) ([]*pyjson.Object, error) {
	widen := allowAll && r.resumeCallerIsAdmin(source)
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return nil, errors.New("AttributeError: 'NoneType' object has no attribute 'list_sessions_rich'")
	}
	opts := state.ListSessionsRichOptions{Source: string(source.Platform), Limit: new(10), OrderByLastActive: true}
	if !widen {
		opts.SessionKey = sessionKey
	}
	sessions, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) ([]*pyjson.Object, error) { return d.ListSessionsRich(ctx, opts) })
	if err != nil {
		return nil, err
	}
	var titled []*pyjson.Object
	for _, s := range sessions {
		if title, _ := s.Get("title"); pyrepr.Truthy(title) && len(titled) < 10 {
			titled = append(titled, s)
		}
	}
	var visible []*pyjson.Object
	for _, s := range titled {
		if r.resumeRowVisible(ctx, source, s, allowAll) {
			visible = append(visible, s)
		}
	}
	return visible, nil
}

// Target id and display name for a numbered choice, session id or title, else the error reply
func (r *GatewayRunner) resolveResumeTarget(ctx context.Context, source *gateway.SessionSource, sessionKey, name string, allowAll bool) (string, string, string, error) {
	db := r.sessionDB(ctx).DB()
	targetID := ""
	if pystr.StrIsDigit(name) {
		titled, err := r.listTitledSessions(ctx, source, sessionKey, allowAll)
		if err != nil {
			runLog().Debug(fmt.Sprintf("Failed to list titled sessions for numeric resume: %s", pyrepr.ExceptionMessage(err)))
			return "", "", i18n.T(ctx, "gateway.resume.list_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
		}
		index, ok := pystr.IntText(name)
		if !ok {
			return "", "", "", fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.StrRepr(name))
		}
		if index.Sign() < 1 || !index.IsInt64() || index.Int64() > int64(len(titled)) {
			return "", "", i18n.T(ctx, "gateway.resume.out_of_range", "", map[string]any{"index": index}), nil
		}
		target := titled[index.Int64()-1]
		targetID = rowStr(target, "id")
		if title := rowStr(target, "title"); title != "" {
			name = title
		}
	} else {
		session, err := db.GetSession(ctx, name)
		if err != nil {
			return "", "", "", err
		}
		if session != nil && session.Len() > 0 {
			id, _ := session.Get("id")
			targetID = pyrepr.Str(id)
		} else if targetID, _, err = db.ResolveSessionByTitle(ctx, name); err != nil {
			return "", "", "", err
		}
	}
	if targetID == "" {
		return "", "", i18n.T(ctx, "gateway.resume.not_found", "", map[string]any{"name": name}), nil
	}
	if resolved, err := db.ResolveResumeSessionID(targetID); err != nil {
		runLog().Debug(fmt.Sprintf("Failed to resolve resume continuation for %s: %s", targetID, pyrepr.ExceptionMessage(err)))
	} else {
		targetID = resolved
	}
	return targetID, name, "", nil
}

// Refusal binding resume to the caller's own room or origin, false when the caller may resume the target
func (r *GatewayRunner) resumeAccessDeniedReply(ctx context.Context, source *gateway.SessionSource, targetID, name string, allowAll, allowCrossRoom bool) (string, bool) {
	if source.Platform == gateway.PlatformMatrix {
		targetOrigin := r.gatewaySessionOriginForID(ctx, targetID)
		if sameMatrixRoom(source, targetOrigin) || allowCrossRoom {
			return "", false
		}
		if targetOrigin == nil {
			return i18n.T(ctx, "gateway.resume.matrix_blocked_no_origin", "", map[string]any{"name": name}), true
		}
		return i18n.T(ctx, "gateway.resume.matrix_blocked_other_room", "", map[string]any{"name": name, "room": chatNameOrID(targetOrigin)}), true
	}
	if r.resumeTargetAllowed(ctx, source, targetID, allowAll || allowCrossRoom) {
		return "", false
	}
	return i18n.T(ctx, "gateway.resume.blocked_not_owner", "", map[string]any{"name": name}), true
}

// Lists the caller's titled sessions or switches the chat onto the named one
func (r *GatewayRunner) handleResumeCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx), nil
	}
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	parts, err := pyshlex.SplitPOSIX(pystr.Strip(event.GetCommandArgs()), false)
	if err != nil {
		return i18n.T(ctx, "gateway.resume.parse_error", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	allowAll, allowCrossRoom := false, false
	for _, p := range parts {
		allowAll = allowAll || p == "--all"
		allowCrossRoom = allowCrossRoom || p == "--cross-room"
	}
	name := stripResumeName(parts)
	if name == "" {
		titled, err := r.listTitledSessions(ctx, source, sessionKey, allowAll)
		if err != nil {
			runLog().Debug(fmt.Sprintf("Failed to list titled sessions: %s", pyrepr.ExceptionMessage(err)))
			return i18n.T(ctx, "gateway.resume.list_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
		}
		return r.resumeListingReply(ctx, source, titled, allowAll), nil
	}
	targetID, name, reply, err := r.resolveResumeTarget(ctx, source, sessionKey, name, allowAll)
	if err != nil || reply != "" {
		return reply, err
	}
	authority, localOwner, head, err := gateway.ConversationHead(ctx, r, targetID)
	if err != nil {
		return "", err
	}
	if localOwner != nil {
		targetID = head
	}
	if denied, refused := r.resumeAccessDeniedReply(ctx, source, targetID, name, allowAll, allowCrossRoom); refused {
		return denied, nil
	}
	store := r.slashStore()
	current, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	if current.SessionID == targetID {
		return i18n.T(ctx, "gateway.resume.already_on", "", map[string]any{"name": name}), nil
	}
	r.releaseRunningAgentState(ctx, sessionKey, nil)
	newEntry, err := store.SwitchSession(ctx, sessionKey, targetID, nil, false)
	if err != nil {
		return "", err
	}
	if newEntry == nil {
		return i18n.T(ctx, "gateway.resume.switch_failed", "", nil), nil
	}
	if localOwner != nil && authority != nil {
		owner, err := authority.LogicalOwner(ctx, targetID)
		if err != nil {
			return "", err
		}
		if err := gateway.FollowHandOff(ctx, authority, source, sessionKey, owner, localOwner); err != nil {
			return "", err
		}
	}
	r.clearConversationScope(ctx, sessionKey, "resume")
	if err := store.SetModelOverride(ctx, sessionKey, nil); err != nil {
		return "", err
	}
	r.evictCachedAgent(ctx, sessionKey, false, false)
	title, _, err := asyncDB.DB().GetSessionTitle(ctx, targetID)
	if err != nil {
		return "", err
	}
	if title == "" {
		title = name
	}
	raw, err := store.LoadTranscript(ctx, targetID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if !errors.As(err, &readErr) {
			return "", err
		}
		return i18n.T(ctx, "gateway.resume.resumed_no_count", "", map[string]any{"title": title}) + "\n" + historyUnreadable(ctx), nil
	}
	msgCount := 0
	for _, m := range transcriptMaps(raw) {
		if m["role"] == "user" {
			msgCount++
		}
	}
	if source.Platform == gateway.PlatformMatrix && allowCrossRoom {
		msgPart := ""
		if msgCount != 0 {
			key := "gateway.resume.msg_count_other"
			if msgCount == 1 {
				key = "gateway.resume.msg_count_one"
			}
			msgPart = i18n.T(ctx, key, "", map[string]any{"count": msgCount})
		}
		return i18n.T(ctx, "gateway.resume.matrix_cross_room_success", "", map[string]any{"title": title,
			"room": chatNameOrID(source), "msg_part": msgPart}), nil
	}
	switch msgCount {
	case 0:
		return i18n.T(ctx, "gateway.resume.resumed_no_count", "", map[string]any{"title": title}), nil
	case 1:
		return i18n.T(ctx, "gateway.resume.resumed_one", "", map[string]any{"title": title, "count": msgCount}), nil
	}
	return i18n.T(ctx, "gateway.resume.resumed_many", "", map[string]any{"title": title, "count": msgCount}), nil
}

// Numbered resume list, a non admin all request falling back to same origin scoping with a note
func (r *GatewayRunner) resumeListingReply(ctx context.Context, source *gateway.SessionSource, titled []*pyjson.Object, allowAll bool) string {
	scopeNote := ""
	if allowAll && !r.resumeCallerIsAdmin(source) {
		scopeNote = i18n.T(ctx, "gateway.resume.all_requires_admin", "", nil)
	}
	if len(titled) == 0 {
		if source.Platform == gateway.PlatformMatrix && !allowAll {
			return i18n.T(ctx, "gateway.resume.matrix_no_named_sessions", "", nil)
		}
		base := i18n.T(ctx, "gateway.resume.no_named_sessions", "", nil)
		if scopeNote != "" {
			return base + "\n" + scopeNote
		}
		return base
	}
	lines := []string{i18n.T(ctx, "gateway.resume.list_header", "", nil)}
	for idx, s := range titled[:min(len(titled), 10)] {
		titleValue, _ := s.Get("title")
		title := pyrepr.Str(titleValue)
		if source.Platform == gateway.PlatformMatrix && allowAll {
			if origin := r.gatewaySessionOriginForID(ctx, rowStr(s, "id")); origin != nil {
				title = title + " — " + chatNameOrID(origin)
			}
		}
		previewValue, _ := s.Get("preview")
		preview := ""
		if previewValue != nil {
			preview = pystr.Slice(pyrepr.Str(previewValue), 0, 40)
		}
		previewPart := ""
		if preview != "" {
			previewPart = i18n.T(ctx, "gateway.resume.list_preview_suffix", "", map[string]any{"preview": preview})
		}
		lines = append(lines, i18n.T(ctx, "gateway.resume.list_item_numbered", "", map[string]any{"index": idx + 1, "title": title, "preview_part": previewPart}))
	}
	if scopeNote != "" {
		lines = append(lines, scopeNote)
	}
	lines = append(lines, i18n.T(ctx, "gateway.resume.list_footer_numbered", "", nil))
	return strings.Join(lines, "\n")
}

// Lists previous sessions for gateway chats, a target argument delegating to resume
func (r *GatewayRunner) handleSessionsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx), nil
	}
	includeAll, includeUnnamed, target, searchQuery, err := state.ParseSessionListingArgs(pystr.Strip(event.GetCommandArgs()))
	if err != nil {
		return i18n.T(ctx, "gateway.resume.parse_error", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if searchQuery != nil && *searchQuery == "" {
		return i18n.T(ctx, "gateway.sessions.usage_search", "", nil), nil
	}
	if target != "" {
		return r.handleResumeCommand(ctx, gateway.ReplaceMessageEventText(event, "/resume "+target))
	}
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	crossOrigin := includeAll && r.resumeCallerIsAdmin(source)
	var scopeNotice *string
	if includeAll && !crossOrigin {
		notice := i18n.T(ctx, "gateway.sessions.all_requires_admin", "", nil)
		scopeNotice = &notice
	}
	current, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	query := state.SessionListingQuery{Source: string(source.Platform), CurrentSessionID: current.SessionID, IncludeCurrentSession: true,
		IncludeAllSources: crossOrigin, IncludeUnnamed: includeUnnamed, Limit: 10, ExcludeSources: []string{"tool"}}
	if !crossOrigin {
		query.SessionKey = sessionKey
	}
	if searchQuery != nil {
		query.SearchQuery = *searchQuery
		query.Limit = 50
	}
	rows, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) ([]*pyjson.Object, error) {
		return state.QuerySessionListing(ctx, d, query)
	})
	if err != nil {
		return "", err
	}
	if !crossOrigin {
		var visible []*pyjson.Object
		for _, row := range rows {
			if r.resumeRowVisible(ctx, source, row, false) {
				visible = append(visible, row)
			}
		}
		rows = visible
	}
	rows = rows[:min(len(rows), 10)]
	var title string
	switch {
	case searchQuery != nil && *searchQuery != "":
		title = i18n.T(ctx, "gateway.sessions.title_matching", "", map[string]any{"query": *searchQuery})
	case includeUnnamed:
		title = i18n.T(ctx, "gateway.sessions.title_all", "", nil)
	default:
		title = i18n.T(ctx, "gateway.sessions.title_named", "", nil)
	}
	return state.FormatGatewaySessionListing(rows, crossOrigin, title, scopeNotice), nil
}
