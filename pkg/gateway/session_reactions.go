package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Parsed message.react params, a newest role standing in for an absent row id
type reactionRequest struct {
	rowID      *int64
	newestRole string
	emoji      *string
	author     string
}

// Validates the reaction params, invalid_params for unknown keys or mistyped fields
func parseReaction(params *pyjson.Object) (reactionRequest, error) {
	var req reactionRequest
	rawRow := params.GetOr("row_id", nil)
	newestRole, _ := params.GetOr("newest_role", nil).(string)
	rawEmoji := params.GetOr("emoji", nil)
	author := params.GetOr("author", nil)
	if !pyrepr.Truthy(author) {
		author = "user"
	}
	invalid := paramsOutside(params, "session_id", "profile", "row_id", "newest_role", "emoji", "author")
	if rawRow != nil {
		id, ok := rawRow.(int64)
		invalid = invalid || !ok
		req.rowID = &id
	} else if newestRole != "user" && newestRole != "assistant" {
		invalid = true
	}
	if rawEmoji != nil {
		text, ok := rawEmoji.(string)
		if !ok || pystr.Strip(text) == "" {
			invalid = true
		} else {
			stripped := pystr.Strip(text)
			req.emoji = &stripped
		}
	}
	if author != "user" && author != "agent" {
		invalid = true
	}
	if invalid {
		return reactionRequest{}, runtimeRefusal("invalid_params")
	}
	req.newestRole, req.author = newestRole, author.(string)
	return req, nil
}

// Sets or clears a reaction on the conversation's current physical session as a tracked write
func react(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	req, err := parseReaction(params)
	if err != nil {
		return nil, err
	}
	a := c.authority
	if _, held := a.live(ref.SessionID); ref.SessionID == "" || !held {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:submit"); err != nil {
		return nil, err
	}
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	type written struct {
		rowID     int64
		reactions []any
		found     bool
	}
	result, err := trackedWrite(ctx, a, func(wctx context.Context) (any, error) {
		rowID := int64(0)
		if req.rowID != nil {
			rowID = *req.rowID
		} else {
			latest, found, err := a.DB.LatestMessageRowID(wctx, target, req.newestRole, 0, true)
			if err != nil || !found {
				return written{}, err
			}
			rowID = latest
		}
		reactions, found, err := a.DB.SetMessageReaction(wctx, target, rowID, req.emoji, req.author)
		return written{rowID, reactions, found}, err
	}, nil, false, nil)
	if err != nil {
		return nil, err
	}
	out, _ := result.(written)
	if !out.found {
		return nil, runtimeRefusal("not_found")
	}
	return map[string]any{"row_id": out.rowID, "reactions": out.reactions}, nil
}
