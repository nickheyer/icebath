package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const autoContinueFreshnessMinutesDefault = 15

// Enabled flag, freshness window in seconds and max attempts of desktop.auto_continue
func autoContinueConfig(ctx context.Context) (bool, float64, int) {
	cfg := map[string]any{}
	if desktop, ok := loadCfg(ctx)["desktop"].(map[string]any); ok {
		if ac, isMap := desktop["auto_continue"].(map[string]any); isMap {
			cfg = ac
		}
	}
	minutes := float64(autoContinueFreshnessMinutesDefault)
	if raw, present := cfg["freshness_minutes"]; present {
		if f, err := pyint.Float(raw); err == nil {
			minutes = f
		}
	}
	enabled := true
	if raw, present := cfg["enabled"]; present && raw != nil {
		enabled = isTruthyValue(raw)
	}
	return enabled, max(0.0, minutes) * 60.0, coerceIntConfigValue(cfg["max_attempts"], 2, 0)
}

// Home the session's durable state lives in, its profile home else the launch home
func sessionHome(ctx context.Context, session *tuigateway.Session) string {
	if h := strOr(session.Get("profile_home")); h != "" {
		return h
	}
	return launchHome()
}

// Drops the crash marker of the session key and of each rotated key right before the terminal frame
func retireTurnMarker(ctx context.Context, session *tuigateway.Session, keys ...string) {
	h := sessionHome(ctx, session)
	seen := map[string]bool{}
	for _, key := range append(append([]string{}, keys...), strOr(session.Get("session_key"))) {
		if key != "" && !seen[key] {
			seen[key] = true
			tuigateway.ClearTurnMarker(h, key)
		}
	}
}

// System note of an auto continued turn embedding the interrupted request
func autoContinueNote(prompt string) string {
	return tuigateway.AutoContinueNotePrefix + " — the app or its backend process stopped before the turn could finish. " +
		"Some of the work may already be complete; check the current state before redoing anything, then " +
		"finish the task. The interrupted request was:]\n\n" + prompt
}

// Stripped user text of the live turn
func acInflightOriginal(session *tuigateway.Session) string {
	turn, ok := session.Get("inflight_turn").(map[string]any)
	if !ok {
		return ""
	}
	return pystr.Strip(pyrepr.Str(orEmpty(turn["user"])))
}

// Queues a message for the next turn, merging text only arrivals, nil when it duplicates the live prompt
func enqueuePrompt(ctx context.Context, session *tuigateway.Session, text any, transport any, imagePaths []string, turnAuthor ...map[string]any) map[string]any {
	images := append([]string{}, imagePaths...)
	var author map[string]any
	if len(turnAuthor) > 0 {
		author = turnAuthor[0]
	}
	dropQueuedDuplicatesOfInflightUser(session)
	s, isText := text.(string)
	textOnly := len(images) == 0 && isText
	if textOnly && len(author) == 0 {
		if original := acInflightOriginal(session); original != "" && pystr.Strip(s) == original {
			return nil
		}
	}
	queued := map[string]any{"text": text, "transport": transport}
	if len(images) > 0 {
		queued["image_paths"] = images
	}
	if len(author) > 0 {
		queued["turn_author"] = author
	}
	existing, _ := session.Get("queued_prompt").(map[string]any)
	if len(existing) > 0 && textOnly && len(author) == 0 && !truthy(existing["image_paths"]) && !truthy(existing["turn_author"]) &&
		!truthy(session.Get("queued_prompts")) {
		if prev, prevText := existing["text"].(string); prevText {
			switch {
			case prev != "" && s != "":
				existing["text"] = prev + "\n\n" + s
			case prev != "":
				existing["text"] = prev
			default:
				existing["text"] = s
			}
			return existing
		}
	}
	if len(existing) > 0 {
		list, _ := session.Get("queued_prompts").([]any)
		session.Set("queued_prompts", append(list, queued))
	} else {
		session.Set("queued_prompt", queued)
	}
	return queued
}

// Queue envelope with a self duplicate of the live user text dropped or its glued original stripped
func sanitizeQueuedEntryVsInflightUser(entry any, original string) map[string]any {
	e, ok := entry.(map[string]any)
	if !ok {
		return nil
	}
	text, isText := e["text"].(string)
	if original == "" || truthy(e["image_paths"]) || truthy(e["turn_author"]) || !isText {
		return e
	}
	rest := text
	for _, sep := range []string{"\n\n", "\n"} {
		if strings.HasPrefix(text, original+sep) {
			rest = text[len(original+sep):]
			break
		}
	}
	rest = pystr.Strip(rest)
	if rest == "" || rest == original {
		return nil
	}
	if rest == pystr.Strip(text) {
		return e
	}
	out := make(map[string]any, len(e))
	for k, v := range e {
		out[k] = v
	}
	out["text"] = rest
	return out
}

// Removes queue copies of the live turn's original user text so a redirect never restarts the original
func dropQueuedDuplicatesOfInflightUser(session *tuigateway.Session) {
	original := acInflightOriginal(session)
	if original == "" {
		return
	}
	var entries []any
	if head := session.Get("queued_prompt"); truthy(head) {
		entries = append(entries, head)
	}
	if rest, ok := session.Get("queued_prompts").([]any); ok {
		entries = append(entries, rest...)
	}
	var cleaned []any
	for _, e := range entries {
		if c := sanitizeQueuedEntryVsInflightUser(e, original); c != nil {
			cleaned = append(cleaned, c)
		}
	}
	acSetQueue(session, cleaned)
}

// Writes entries back as the queued head and the rest
func acSetQueue(session *tuigateway.Session, entries []any) {
	if len(entries) > 0 {
		session.Set("queued_prompt", entries[0])
	} else {
		session.Set("queued_prompt", nil)
	}
	if len(entries) > 1 {
		session.Set("queued_prompts", append([]any{}, entries[1:]...))
	} else {
		session.Pop("queued_prompts")
	}
}

// Applies a steer or redirect to the live turn and records the correction, nil when the agent refused it
func acTryCorrection(rid any, session *tuigateway.Session, a any, method, plainText, status string) map[string]any {
	accepted := false
	if failure := catchPanic(func() {
		if method == "steer" {
			accepted = a.(interface{ Steer(string) bool }).Steer(plainText)
		} else {
			accepted = a.(interface{ Redirect(string) bool }).Redirect(plainText)
		}
	}); failure != nil || !accepted {
		return nil
	}
	unlock := lockHistory(session)
	recordInflightCorrection(session, plainText)
	dropQueuedDuplicatesOfInflightUser(session)
	session.Set("last_active", nowSeconds())
	unlock()
	return ok(rid, map[string]any{"status": status})
}

// Whether a compression lock is held for the session's durable id
func sessionCompressionInFlight(ctx context.Context, session *tuigateway.Session) bool {
	if truthy(session.Get("_manual_compress_active")) {
		return true
	}
	sid := submitRowTargetKey(session)
	if sid == "" {
		return false
	}
	var holder *string
	if failure := catchPanic(func() {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return
		}
		h, err := db.GetCompressionLockHolder(sid)
		if err != nil {
			panic(err)
		}
		holder = h
	}); failure != nil {
		logger().Debug("compression in-flight check failed for session "+sid, "error", pyrepr.Str(failure))
		return false
	}
	return holder != nil && *holder != ""
}

// Closes a failed turn with the error message complete frame, retaining it for a resuming client
func emitTerminalTurnError(ctx context.Context, sid string, session *tuigateway.Session, failure any, errorSurface map[string]any, retireMarker bool) {
	a := sessionAgent(session)
	if err, isErr := failure.(error); errorSurface == nil && isErr {
		providerName, model := "", ""
		var apiKey any
		if a != nil {
			providerName, model, apiKey = a.Provider(), a.Model(), a.APIKey()
		}
		suppress(func() { errorSurface = agent.BuildErrorSurfaceFromException(err, providerName, model, apiKey) })
	}
	unlock := lockHistory(session)
	failInflightTurn(session, failure, errorSurface)
	turn, _ := session.Get("inflight_turn").(map[string]any)
	message := "turn failed"
	if truthy(turn["error"]) {
		message = pyrepr.Str(turn["error"])
	}
	partial := pyrepr.Str(orEmpty(turn["assistant"]))
	cols := 80
	if raw, present := session.Lookup("cols"); present {
		n, err := pyint.Int64(raw)
		if err != nil {
			unlock()
			panic(err)
		}
		cols = int(n)
	}
	unlock()
	text := partial
	if text == "" {
		text = turnErrorText(message, errorSurface, true)
	}
	rendered, _ := renderMessage(text, cols)
	usage := map[string]any{}
	if a != nil {
		usage = getUsage(ctx, a)
	}
	payload := map[string]any{"text": text, "usage": usage, "status": "error", "error": message, "recoverable": true}
	if len(errorSurface) > 0 {
		payload["error_surface"] = errorSurface
	}
	if partial != "" {
		payload["partial"] = true
	}
	if rendered != "" {
		payload["rendered"] = rendered
	}
	if retireMarker {
		retireTurnMarker(ctx, session)
	}
	tuigateway.Emit(ctx, "message.complete", sid, payload)
}

// Keeps a failed turn's working transcript so the next prompt sees what the agent persisted
func restoreAgentHistoryAfterTurnError(session *tuigateway.Session, a *agent.AIAgent) bool {
	if a == nil {
		return false
	}
	messages := agent.SessionMessageDicts(a.SessionMessages())
	unlock := lockHistory(session)
	defer unlock()
	session.Set("history", messagesAsHistory(messages))
	session.Set("history_version", intOr(session.Get("history_version"))+1)
	return true
}
