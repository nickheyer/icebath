package gateway

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One dispatch handler of an authority connection taking the frame's session reference and params
type controlHandler func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error)

// Settles the owner request a viewer's response frame answers when this connection or a sibling may settle it
func (c *AuthorityConnection) resolveResponse(ctx context.Context, frame *pyjson.Object) error {
	var asked []string
	var settleErr error
	c.mu.Lock()
	connections := []*AuthorityConnection{c}
	for _, sibling := range c.siblings {
		connections = append(connections, sibling)
	}
	c.mu.Unlock()
	entitled := func(sessionID string, events *SessionEvents) bool {
		asked = append(asked, sessionID)
		for _, connection := range connections {
			ok, err := maySettle(ctx, connection.authority, connection.actor, sessionID, events)
			if err != nil {
				settleErr = err
				return false
			}
			if ok {
				return true
			}
		}
		return false
	}
	resolved := ResolveServerRequest(frame, entitled)
	if settleErr != nil {
		return settleErr
	}
	if !resolved {
		what := "unknown server request"
		if len(asked) > 0 {
			what = "a server request this connection may not settle"
		}
		slog.Debug("dropping response for "+what, "id", frame.GetOr("id", nil))
	}
	return nil
}

// Value of key in a Python dict of either decoded shape, the AttributeError Python raises for anything else
func pyGet(value any, key string, def any) (any, error) {
	switch dict := value.(type) {
	case map[string]any:
		if item, present := dict[key]; present {
			return item, nil
		}
		return def, nil
	case config.Tree:
		if item, present := dict[key]; present {
			return item, nil
		}
		return def, nil
	case *pyjson.Object:
		return pyDictGet(dict, key, def)
	}
	return pyDictGet(value, key, def)
}
