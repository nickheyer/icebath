package gateway

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/state"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
		owner = jsonutil.NewObject()
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
			return nil, &keyError{"id"}
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
	parsed, err := jsonutil.Decode(policy.ConfigJSON)
	if err != nil {
		return nil, err
	}
	config, ok := parsed.(*jsonutil.Object)
	if !ok {
		return nil, &errorutil.Error{Kind: "TypeError", Msg: "policy config is not a mapping"}
	}
	model, _ := config.ValueOr("model", nil).(*jsonutil.Object)
	if model == nil {
		model = jsonutil.NewObject()
	}
	if composerPickHolds(owner, live) || (slices.Contains(picked, "model") && model.ValueOr("selection_profile", nil) == nil) {
		held, present := config.Get("model")
		if !present {
			held = jsonutil.NewObject()
			config.Set("model", held)
		}
		target, isDict := held.(*jsonutil.Object)
		if !isDict {
			return nil, &errorutil.Error{Kind: "TypeError", Msg: "the held bot chat record is " + errchain.Kind(held) + ", expected a mapping"}
		}
		name, providerName := profileTarget(live)
		target.Set("selection_profile", []any{name, providerName})
	}
	pins, _ := config.ValueOr(SessionPins, nil).(*jsonutil.Object)
	if pins == nil {
		pins = jsonutil.NewObject()
	}
	if _, isBool := pins.ValueOr("reasoning", nil).(bool); slices.Contains(picked, "reasoning") && !isBool {
		pinned := pins.Clone()
		pinned.Set("reasoning", true)
		config.Set(SessionPins, pinned)
	}
	adopted := config.Clone()
	adopted.Set(FollowAdopted, true)
	text, err := jsonutil.Encode(adopted, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	followed := *policy
	followed.ConfigJSON = text
	return &followed, nil
}
