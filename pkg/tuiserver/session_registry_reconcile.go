package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Durable identities of a row, its message uid and absorbed uids else its integer row id
func durableIDs(msg any) map[string]bool {
	out := map[string]bool{}
	m, isDict := msg.(map[string]any)
	if !isDict {
		return out
	}
	uids := []any{m[state.MessageUID]}
	switch absorbed := m[state.AbsorbedMessageUIDs].(type) {
	case []any:
		uids = append(uids, absorbed...)
	case []string:
		for _, uid := range absorbed {
			uids = append(uids, uid)
		}
	}
	for _, uid := range uids {
		if text, isText := uid.(string); isText && text != "" {
			out["uid\x00"+text] = true
		}
	}
	if _, isBool := m["_row_id"].(bool); !isBool {
		if rowID, isInt := pyint.Instance(m["_row_id"]); isInt {
			out["row\x00"+rowID.String()] = true
		}
	}
	return out
}

// Whether two identity sets share a member
func sharesID(a, b map[string]bool) bool {
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

// Role and text of a message as the legacy anchor compares them
type roleText struct {
	role any
	text string
}

// Index of the live row closing an identity free display prefix, minus one when the prefix never completes
func legacyDisplayAnchor(display []any, memory []any) int {
	prefix := make([]roleText, len(display))
	for i, msg := range display {
		m, _ := msg.(map[string]any)
		prefix[i] = roleText{role: m["role"], text: tuigateway.CoerceMessageText(m["content"], true)}
	}
	cursor := 0
	for index, msg := range memory {
		m, isDict := msg.(map[string]any)
		if !isDict {
			continue
		}
		value := roleText{role: m["role"], text: tuigateway.CoerceMessageText(m["content"], true)}
		found := -1
		for i := cursor; i < len(prefix); i++ {
			if pyrepr.Equal(prefix[i].role, value.role) && prefix[i].text == value.text {
				found = i
				break
			}
		}
		if found < 0 {
			continue
		}
		cursor = found + 1
		if cursor == len(prefix) {
			return index
		}
	}
	return -1
}

// Persisted display lineage with the unflushed live tail past the live copy of its last row appended
func reconcileDisplayWithLive(dbDisplay, inMemory []any) []any {
	if len(dbDisplay) == 0 {
		return inMemory
	}
	if len(inMemory) == 0 {
		return dbDisplay
	}
	anchorIDs := durableIDs(dbDisplay[len(dbDisplay)-1])
	lastShared := -1
	for idx, msg := range inMemory {
		if sharesID(durableIDs(msg), anchorIDs) {
			lastShared = idx
		}
	}
	if lastShared == -1 {
		if len(anchorIDs) > 0 {
			for _, msg := range inMemory {
				if len(durableIDs(msg)) > 0 {
					return dbDisplay
				}
			}
		}
		lastShared = legacyDisplayAnchor(dbDisplay, inMemory)
	}
	if lastShared == -1 {
		return dbDisplay
	}
	out := append([]any(nil), dbDisplay...)
	return append(out, inMemory[lastShared+1:]...)
}

// Session info of a live session, the lazy shape of its own workspace while the agent is unbuilt
func fallbackSessionInfo(ctx context.Context, session *tuigateway.Session) map[string]any {
	if session.Get("agent") != nil {
		return sessionInfo(ctx, sessionAgent(session), session)
	}
	cwd := sessionCwd(ctx, session)
	info := map[string]any{"cwd": cwd, "branch": tuigateway.Branch(cwd), "project": projectInfoForCwd(ctx, cwd), "lazy": true}
	for key, value := range lazyInfoRoute(ctx, session, map[string]any{}) {
		info[key] = value
	}
	info["skills"] = map[string]any{}
	info["tools"] = map[string]any{}
	info["desktop_contract"] = desktopBackendContract
	return info
}
