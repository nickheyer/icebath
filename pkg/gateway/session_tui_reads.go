package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Inputs a cached context breakdown was built from
type contextBreakdownKey struct {
	agent                   any
	marks                   any
	revision                state.TranscriptRevision
	systemPrompt, toolNames any
}

// Breakdown payload of a live session with the inputs it was built from
type contextBreakdownCache struct {
	key     contextBreakdownKey
	payload *jsonutil.Object
}

// Same agent, marks, transcript revision and frozen prompt and tool pin
func (k contextBreakdownKey) equal(other contextBreakdownKey) bool {
	return k.agent == other.agent && k.marks == other.marks && slices.Equal(k.revision.SessionIDs, other.revision.SessionIDs) &&
		k.revision.Count == other.revision.Count && k.revision.MaxID == other.revision.MaxID &&
		k.revision.Revision == other.revision.Revision && anyutil.Equal(k.systemPrompt, other.systemPrompt) &&
		anyutil.Equal(k.toolNames, other.toolNames)
}

// Stored session id behind a runtime id, the runtime id when no receipt resolves
func storedID(ctx context.Context, a *SessionAuthority, ref SessionRef) string {
	receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil || receipt == nil {
		return ref.SessionID
	}
	entry, ok := receipt.ValueOr("entry", nil).(*jsonutil.Object)
	if !ok {
		return ref.SessionID
	}
	if id, isText := entry.ValueOr("session_id", nil).(string); isText && id != "" {
		return id
	}
	return ref.SessionID
}

// Compression tip the session's model context lives in
func contextID(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	stored := storedID(ctx, a, ref)
	tip, err := a.DB.GetCompressionTip(ctx, stored)
	if err != nil {
		return "", err
	}
	if tip != "" {
		return tip, nil
	}
	return stored, nil
}

// Active rows of the tip as live replay loads them without session_meta bookkeeping
func modelHistory(ctx context.Context, db *state.SessionDB, tip string) ([]map[string]any, error) {
	rows, err := db.GetMessagesAsConversation(ctx, tip, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row["role"] != "session_meta" {
			out = append(out, row)
		}
	}
	return out, nil
}

// Category breakdown and context file manifest of a session, the frozen prompt beside stored usage without an agent
func contextBreakdown(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile") || ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	agent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	var usage *jsonutil.Object
	if nilAgent(agent) {
		agent = nil
		read, err := stats(ctx, c, ref, jsonutil.ObjectOf("session_id", ref.SessionID))
		if err != nil {
			return nil, err
		}
		usage = read.(*jsonutil.Object)
		if nested, isDict := usage.ValueOr("usage", nil).(*jsonutil.Object); isDict {
			usage = nested
		}
	}
	scoped, err := sessionWorkspace(ownerScope(ctx, a, false), a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	tip, err := contextID(scoped, a, ref)
	if err != nil {
		return nil, err
	}
	row := jsonutil.NewObject()
	if agent == nil {
		stored, err := a.DB.GetSession(scoped, tip)
		if err != nil {
			return nil, err
		}
		if stored != nil {
			row = stored
		}
	}
	revision, err := a.DB.ConversationRevision(scoped, tip, false)
	if err != nil {
		return nil, err
	}
	key := contextBreakdownKey{agent: agent, marks: ContextAgent.Marks(agent), revision: revision,
		systemPrompt: row.ValueOr("system_prompt", nil), toolNames: row.ValueOr("tool_names", nil)}
	a.mu.Lock()
	cached := live.contextBreakdown
	a.mu.Unlock()
	if cached != nil && cached.key.equal(key) {
		return cached.payload.Clone(), nil
	}
	conversation, err := modelHistory(scoped, a.DB, tip)
	if err != nil {
		return nil, err
	}
	var payload *jsonutil.Object
	if agent == nil {
		payload, err = storedBreakdown(scoped, row, conversation, usage)
	} else {
		payload, err = ContextAgent.Breakdown(scoped, agent, conversation)
	}
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	live.contextBreakdown = &contextBreakdownCache{key: key, payload: payload}
	a.mu.Unlock()
	return payload.Clone(), nil
}

// Stored breakdown of the row's frozen prompt and tool pin beside its stored usage
func storedBreakdown(ctx context.Context, row *jsonutil.Object, conversation []map[string]any, usage *jsonutil.Object) (*jsonutil.Object, error) {
	breakdown := jsonutil.ObjectOf("categories", []any{}, "estimated_total", 0)
	if prompt := row.ValueOr("system_prompt", nil); anyutil.IsTrue(prompt) {
		var pin any
		pinText, _ := row.ValueOr("tool_names", nil).(string)
		if pinText == "" {
			pinText = "null"
		}
		if loaded, err := jsonutil.Decode(pinText); err == nil {
			pin = jsonutil.Plain(loaded)
		}
		env := tools.ToolEnvFrom(ctx)
		if env == nil {
			env = &tools.Env{Home: home.Dir(ctx)}
		}
		var err error
		breakdown, err = ContextAgent.StoredBreakdown(ctx, anyutil.String(prompt), mcp.PinnedToolDefs(ctx, env, pin), conversation)
		if err != nil {
			return nil, err
		}
	}
	out := breakdown.Clone()
	for _, key := range []string{"context_max", "context_percent", "context_used"} {
		n, err := usageRowInt(usage, key)
		if err != nil {
			return nil, err
		}
		out.Set(key, n)
	}
	out.Set("context_estimated", anyutil.IsTrue(usage.ValueOr("context_estimated", false)))
	source := usage.ValueOr("context_source", nil)
	if !anyutil.IsTrue(source) {
		source = "provider_usage"
	}
	out.Set("context_source", anyutil.String(source))
	model := usage.ValueOr("model", nil)
	if !anyutil.IsTrue(model) {
		model = ""
	}
	out.Set("model", anyutil.String(model))
	return out, nil
}
