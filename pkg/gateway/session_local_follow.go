package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Policy following its profile when the session is a Bot Chat an earlier build froze, else the policy itself
func followBotChat(ctx context.Context, a *SessionAuthority, sessionID string, policy *LocalSessionPolicy, live map[string]any, picked []string) (*LocalSessionPolicy, error) {
	if _, named := policyObject(policy.RequestJSON)["follow_profile_config"]; policy.FollowsProfile() || policy.IgnoreUserConfig || named {
		return policy, nil
	}
	owner, err := a.DB.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		owner = pyjson.NewObject()
	}
	if !botChatRow(owner) {
		titled, err := a.DB.GetSessionByTitle(ctx, "Bot Chat")
		if err != nil {
			return nil, err
		}
		if titled == nil {
			return policy, nil
		}
		titledID, ok := titled.Get("id")
		if !ok {
			return nil, &pyKeyError{"id"}
		}
		var member bool
		if err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			ids, err := state.LocalConversationIDs(ctx, conn, sessionID)
			if err != nil {
				return err
			}
			text, isStr := titledID.(string)
			member = isStr && slices.Contains(ids, text)
			return nil
		}); err != nil {
			return nil, err
		}
		if !member {
			return policy, nil
		}
	}
	live = presentSections(live)
	parsed, err := pyjson.Loads(policy.ConfigJSON)
	if err != nil {
		return nil, err
	}
	config, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "policy config is not a mapping"}
	}
	model, _ := config.GetOr("model", nil).(*pyjson.Object)
	if model == nil {
		model = pyjson.NewObject()
	}
	if composerPickHolds(owner, live) || (slices.Contains(picked, "model") && model.GetOr("selection_profile", nil) == nil) {
		held, present := config.Get("model")
		if !present {
			held = pyjson.NewObject()
			config.Set("model", held)
		}
		target, isDict := held.(*pyjson.Object)
		if !isDict {
			return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(held) + "' object does not support item assignment"}
		}
		name, providerName := profileTarget(live)
		target.Set("selection_profile", []any{name, providerName})
	}
	pins, _ := config.GetOr(SessionPins, nil).(*pyjson.Object)
	if pins == nil {
		pins = pyjson.NewObject()
	}
	if _, isBool := pins.GetOr("reasoning", nil).(bool); slices.Contains(picked, "reasoning") && !isBool {
		pinned := pins.Copy()
		pinned.Set("reasoning", true)
		config.Set(SessionPins, pinned)
	}
	adopted := config.Copy()
	adopted.Set(FollowAdopted, true)
	text, err := pyjson.Dumps(adopted)
	if err != nil {
		return nil, err
	}
	followed := *policy
	followed.ConfigJSON = text
	return &followed, nil
}
