package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Installs the routing entry builders the state mutation layer calls for a local reset and a local branch
func init() {
	state.ResetSessionEntry = func(previous *pyjson.Object, childID string) (*pyjson.Object, error) {
		prev, err := SessionEntryFromDict(context.Background(), previous)
		if err != nil {
			return nil, err
		}
		now := sessionNow()
		entry := NewSessionEntry(prev.SessionKey, childID, now, now)
		entry.Origin, entry.Platform, entry.ChatType, entry.DisplayName, entry.IsFreshReset = prev.Origin, prev.Platform, prev.ChatType, prev.DisplayName, true
		return entry.ToDict(), nil
	}
	state.BranchLocalRoute = func(childID, principalID string, originProfile, routeProfile any) (string, *pyjson.Object, *pyjson.Object, float64, error) {
		source := &SessionSource{Platform: PlatformLocal, ChatID: childID, ChatType: "dm", UserID: &principalID}
		if profile, ok := originProfile.(string); ok {
			source.Profile = &profile
		}
		profile := ""
		if text, ok := routeProfile.(string); ok {
			profile = text
		}
		route, err := BuildSessionKey(context.Background(), source, true, false, profile)
		if err != nil {
			return "", nil, nil, 0, err
		}
		now := sessionNow()
		entry := NewSessionEntry(route, childID, now, now)
		entry.Origin, entry.Platform = source, PlatformLocal
		return route, source.ToDict(), entry.ToDict(), now.timestamp(), nil
	}
}
