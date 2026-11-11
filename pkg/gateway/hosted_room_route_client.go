package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Peer client that recovers one exact admission by receipt or idempotent replay
type peerDispatchRecoverer interface {
	RecoverDispatch(dispatch any, grant string) (map[string]any, error)
}

// Peer client that resolves approval of an exact durable remote run
type peerApprover interface {
	ApproveReceipt(taskID string, executionGeneration int64, requestID, choice, grant string) (map[string]any, error)
}

// Peer client that renews a scoped grant while its frozen authority is unchanged
type peerGrantRefresher interface {
	RefreshGrant(grant string, ttlSeconds float64, capabilityDigest, executionPolicyDigest *string) (map[string]any, error)
}

// Peer client that revokes a scoped grant
type peerGrantRevoker interface {
	RevokeGrant(grant string) (map[string]any, error)
}

// Peer client that pins reads to one task attempt
type peerObservationBinder interface {
	BindObservation(taskID string, executionGeneration int64) error
}

// Peer client that keeps its receipts in the gateway store
type peerReceiptStoreBinder interface {
	BindReceiptStore(dbPath string) error
}

// Optional peer capability of a client, looked up on the wrapped client the way getattr reaches through the tracker
func peerHook[T any](client any) (T, bool) {
	var zero T
	if client == nil {
		return zero, false
	}
	probe := client
	if tracked, ok := client.(*routeStatusPeerClient); ok {
		probe = tracked.client
	}
	if _, ok := probe.(T); !ok {
		return zero, false
	}
	hook, ok := client.(T)
	return hook, ok
}

// Peer client that classifies scoped auth failures into route status without exposing credentials
type routeStatusPeerClient struct {
	client            HostedRoomPeerClient
	onReady           func() error
	onReauthorization func() error
	onUnavailable     func() error
	onRefreshed       func(grant string, catalog *GatewayRoomCatalog) error
}

// Rotates an expiring grant before dispatch, the grant to send
func (c *routeStatusPeerClient) refreshGrant(dispatch any, grant string) (string, error) {
	if !RoomGrantNeedsDispatchRefresh(grant, nil, RoomGrantRefreshLeewaySeconds) {
		return grant, nil
	}
	checked, err := HostedMemberDispatchFromMapping(dispatch)
	if err != nil {
		return "", err
	}
	refresher, ok := c.client.(peerGrantRefresher)
	if !ok {
		return grant, nil
	}
	capability, policy := checked.CapabilityDigest, checked.ExecutionPolicyDigest
	refreshed, err := refresher.RefreshGrant(grant, 24*60*60, &capability, &policy)
	if err != nil {
		var peerErr *PeerRunsHTTPError
		if (errors.As(err, &peerErr) && peerErr == err && peerErr.NeedsReauthorization) || RoomGrantNeedsDispatchRefresh(grant, nil, 0) {
			if hookErr := c.onReauthorization(); hookErr != nil {
				return "", hookErr
			}
			return "", err
		}
		return grant, nil
	}
	replacement := hostedStrOrEmpty(refreshed["grant"])
	if replacement == "" {
		return "", errors.New("peer returned no refreshed room grant")
	}
	var catalog *GatewayRoomCatalog
	if refreshed["catalog"] != nil {
		if catalog, err = GatewayRoomCatalogFromMapping(refreshed["catalog"]); err != nil {
			return "", err
		}
		if drift := DigestReauthorizationError(catalog, &capability, &policy); drift != nil {
			if hookErr := c.onReauthorization(); hookErr != nil {
				return "", hookErr
			}
			return "", drift
		}
	}
	if err := c.onRefreshed(replacement, catalog); err != nil {
		return "", err
	}
	return replacement, nil
}

// Runs one wrapped call, classifying its failure and marking the route ready after any call but prepare
func trackedCall[T any](c *routeStatusPeerClient, name string, call func() (T, error)) (T, error) {
	result, err := call()
	if err != nil {
		var zero T
		var peerErr *PeerRunsHTTPError
		notAdmitted, _ := hostedAttemptMarks(err)
		switch {
		case errors.As(err, &peerErr) && peerErr == err && peerErr.NeedsReauthorization:
			if hookErr := c.onReauthorization(); hookErr != nil {
				return zero, hookErr
			}
		case notAdmitted:
			if hookErr := c.onUnavailable(); hookErr != nil {
				return zero, hookErr
			}
		}
		return zero, err
	}
	if name != "prepare" {
		if err := c.onReady(); err != nil {
			var zero T
			return zero, err
		}
	}
	return result, nil
}

func (c *routeStatusPeerClient) Prepare(ctx context.Context, req PeerPrepare) (map[string]any, error) {
	return trackedCall(c, "prepare", func() (map[string]any, error) { return c.client.Prepare(ctx, req) })
}

func (c *routeStatusPeerClient) Dispatch(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	grant, err := c.refreshGrant(dispatch, grant)
	if err != nil {
		return nil, err
	}
	return trackedCall(c, "dispatch", func() (map[string]any, error) { return c.client.Dispatch(ctx, dispatch, grant) })
}

func (c *routeStatusPeerClient) History(ctx context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error) {
	return trackedCall(c, "history", func() ([]map[string]any, error) { return c.client.History(ctx, roomID, profile, sessionID, grant) })
}

func (c *routeStatusPeerClient) Status(ctx context.Context, roomID, profile, sessionID, grant string) (map[string]any, error) {
	return trackedCall(c, "status", func() (map[string]any, error) { return c.client.Status(ctx, roomID, profile, sessionID, grant) })
}

func (c *routeStatusPeerClient) Stop(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	return trackedCall(c, "stop", func() (map[string]any, error) { return c.client.Stop(ctx, dispatch, grant) })
}

func (c *routeStatusPeerClient) StopReceipt(ctx context.Context, taskID string, executionGeneration int64, grant string) (map[string]any, error) {
	return trackedCall(c, "stop_receipt", func() (map[string]any, error) {
		return c.client.(peerStopReceipter).StopReceipt(ctx, taskID, executionGeneration, grant)
	})
}

func (c *routeStatusPeerClient) RecoverDispatch(dispatch any, grant string) (map[string]any, error) {
	grant, err := c.refreshGrant(dispatch, grant)
	if err != nil {
		return nil, err
	}
	return trackedCall(c, "recover_dispatch", func() (map[string]any, error) {
		return c.client.(peerDispatchRecoverer).RecoverDispatch(dispatch, grant)
	})
}

func (c *routeStatusPeerClient) ApproveReceipt(taskID string, executionGeneration int64, requestID, choice, grant string) (map[string]any, error) {
	return trackedCall(c, "approve_receipt", func() (map[string]any, error) {
		return c.client.(peerApprover).ApproveReceipt(taskID, executionGeneration, requestID, choice, grant)
	})
}

func (c *routeStatusPeerClient) BindRoomScope(scope PeerRoomScope) error {
	_, err := trackedCall(c, "bind_room_scope", func() (struct{}, error) {
		return struct{}{}, c.client.(peerRoomScopeBinder).BindRoomScope(scope)
	})
	return err
}

func (c *routeStatusPeerClient) BindObservation(taskID string, executionGeneration int64) error {
	_, err := trackedCall(c, "bind_observation", func() (struct{}, error) {
		return struct{}{}, c.client.(peerObservationBinder).BindObservation(taskID, executionGeneration)
	})
	return err
}
