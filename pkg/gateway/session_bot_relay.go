package gateway

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Fields each Desktop relay operation accepts
var relayOperationFields = map[string]map[string]bool{
	"roster": {"agents": true},
	"outbox": {},
	"reply":  {"id": true, "reply": true, "error": true, "reason": true},
}

// Desktop relay roster sync, outbox drain or reply write on the serving home, for a controlling principal
func relayOperation(ctx context.Context, c *AuthorityConnection, operation string, params *pyjson.Object) (any, error) {
	a, actor := c.authority, c.actor
	if err := requireServing(a); err != nil {
		return nil, err
	}
	homeDir, err := pypath.Resolve(filepath.Dir(a.DB.Path()))
	if err != nil {
		return nil, err
	}
	if _, err := botHome(a, actor, servedProfileName(homeDir)); err != nil {
		return nil, err
	}
	if !actor.Capabilities["session:control"] {
		return nil, runtimeRefusal("permission_denied")
	}
	for _, key := range params.Keys {
		if !relayOperationFields[operation][key] {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	switch operation {
	case "roster":
		count, err := botmode.WriteRemoteRoster(ctx, homeDir, params.GetOr("agents", nil))
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("count", count), nil
	case "outbox":
		claimed, err := botmode.ClaimPendingEnvelopes(ctx, homeDir)
		if err != nil {
			return nil, err
		}
		envelopes := make([]any, len(claimed))
		for i, envelope := range claimed {
			envelopes[i] = envelope
		}
		return pyjson.ObjectOf("envelopes", envelopes), nil
	}
	_, err = botmode.WriteReply(ctx, homeDir, params.GetOr("id", nil), params.GetOr("reply", ""), params.GetOr("error", ""), params.GetOr("reason", ""))
	var invalid *botmode.InvalidEnvelopeError
	if errors.As(err, &invalid) {
		refusal := state.NewRuntimeStoreError("admission_conflict", nil, nil)
		refusal.Cause = err
		return nil, refusal
	}
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true), nil
}
