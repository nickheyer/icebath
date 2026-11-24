package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"sync"

	"github.com/nickheyer/icebath/pkg/profiles"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Seconds a peer member's dispatch grant lives before the driver refreshes it
	peerSeatGrantSeconds = 24 * 60 * 60
	// Seconds a peer member's grant still answers status after its dispatch horizon
	peerSeatStatusSeconds = 30 * 24 * 60 * 60
)

// A member seated on a peer gateway, invited there before the room exists and routed after it does
type peerSeat struct {
	memberID, peer, targetURL, targetProfile, grant string
	catalog                                         *GatewayRoomCatalog
	inviter                                         *PeerRunsHTTPClient
}

// Members a create seats on registered peers, each invited at its peer and rewritten to the exact peer target
func (c *CanonicalHostedRoomService) invitePeerMembers(roomID string, members any) (any, []peerSeat, error) {
	list, ok := jsonutil.Plain(members).([]any)
	if !ok {
		return members, nil, nil
	}
	local, err := LocalAuthorityGatewayID(c.ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]any, len(list))
	var seats []peerSeat
	fail := func(err error) (any, []peerSeat, error) {
		c.revokeSeats(seats)
		return nil, nil, err
	}
	for i, raw := range list {
		out[i] = raw
		member, _ := raw.(map[string]any)
		target, _ := member["target"].(map[string]any)
		name, named := target["peer"].(string)
		if !named || target["kind"] != "peer" {
			continue
		}
		spec := name
		if profile, _ := target["profile"].(string); profile != "" {
			spec += "/" + profile
		}
		peer, err := resolveSharePeer(c.ctx, spec)
		if err != nil {
			return fail(err)
		}
		inviter, err := NewDefaultPeerRunsHTTPClient(c.ctx, peer.base, peer.key, c.DBPath)
		if err != nil {
			return fail(roomRefusal("invalid_params", fmt.Sprintf("peer '%s' has no usable address: %s", name, err.Error())))
		}
		memberID, _ := member["member_id"].(string)
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return fail(err)
		}
		statusTTL := float64(peerSeatStatusSeconds)
		invitation, err := inviter.IssueInvitation(roomID, local, local, 1, memberID, "grant-"+hex.EncodeToString(nonce), peerSeatGrantSeconds, &statusTTL)
		if err != nil {
			return fail(roomRefusal("invalid_params", fmt.Sprintf("peer '%s' did not seat %s: %s", name, spec, err.Error())))
		}
		catalog, err := GatewayRoomCatalogFromMapping(invitation["catalog"])
		if err != nil {
			return fail(err)
		}
		grant := anyutil.String(invitation["grant"])
		profile := anyutil.String(invitation["target_profile"])
		seats = append(seats, peerSeat{memberID: memberID, peer: name, targetProfile: profile, grant: grant, catalog: catalog, inviter: inviter})
		if catalog.EndpointURL == nil || *catalog.EndpointURL == "" {
			return fail(roomRefusal("invalid_params", fmt.Sprintf("peer '%s' advertises no room link address, set room_link_url in its config", name)))
		}
		seats[len(seats)-1].targetURL = *catalog.EndpointURL
		row := map[string]any{}
		for k, v := range member {
			row[k] = v
		}
		row["profile"] = profile
		row["target"] = map[string]any{"kind": "peer", "peer_id": name, "installation_id": catalog.InstallationID, "profile": profile,
			"capability_digest": catalog.CatalogDigest}
		out[i] = row
	}
	return out, seats, nil
}

// Revokes the grants peers issued for seats of a room that will not exist
func (c *CanonicalHostedRoomService) revokeSeats(seats []peerSeat) {
	for _, seat := range seats {
		if _, err := seat.inviter.RevokeGrant(seat.grant); err != nil {
			pkgLog().Warn(fmt.Sprintf("hosted room: revoking the grant %s issued for %s failed", seat.memberID, seat.peer), "exc_info", err)
		}
	}
}

// Routes each peer seat of a created room through its grant, verified against the peer's live catalog
func (c *CanonicalHostedRoomService) registerPeerSeats(roomID string, seats []peerSeat) error {
	if len(seats) == 0 {
		return nil
	}
	room, err := c.room(roomID)
	if err != nil {
		return err
	}
	local, err := LocalAuthorityGatewayID(c.ctx)
	if err != nil {
		return err
	}
	if epoch, _ := hostedStrictInt(room["authority_epoch"]); epoch != 1 || room["authority_gateway_id"] != local {
		return roomRefusal("invalid_params", "the room's authority changed while its peer members were seated")
	}
	for _, seat := range seats {
		client, err := NewDefaultPeerRunsHTTPClient(c.ctx, seat.targetURL, "", c.DBPath)
		if err != nil {
			return err
		}
		probe, err := client.Probe(seat.grant)
		if err != nil {
			return err
		}
		live, err := GatewayRoomCatalogFromMapping(probe["catalog"])
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(*live, *seat.catalog) {
			return roomRefusal("invalid_params", fmt.Sprintf("peer '%s' changed its capabilities while it was seated", seat.peer))
		}
		route := PeerMemberRoute{HomeInstallID: local, MemberID: seat.memberID, TargetInstallID: seat.catalog.InstallationID,
			TargetProfile: seat.targetProfile, CapabilityDigest: seat.catalog.CatalogDigest, ExecutionPolicyDigest: seat.catalog.ExecutionPolicy.PolicyDigest,
			CancellationScopeID: "cancel-" + roomID, TraceID: "trace-" + roomID + "-" + seat.memberID, Grant: seat.grant}
		targetURL := seat.targetURL
		if err := c.RegisterPeerRoute(roomID, seat.memberID, route, client, &targetURL, seat.catalog); err != nil {
			return err
		}
	}
	return nil
}

// Process run store whose durability decides whether peer runs replay across restarts, opened once
var roomLinkRuns struct {
	sync.Mutex
	store *RunIdempotencyStore
}

// Whether this gateway can serve as a room link target, its catalog and address when it can, the reason when not
func groupRoomLinkStatus(ctx context.Context, installationID string) (*jsonutil.Object, error) {
	roomLinkRuns.Lock()
	if roomLinkRuns.store == nil {
		store, err := NewRunIdempotencyStore(ctx, "")
		if err != nil {
			roomLinkRuns.Unlock()
			return nil, err
		}
		roomLinkRuns.store = store
	}
	durable := roomLinkRuns.store.Durable()
	roomLinkRuns.Unlock()
	if !durable {
		return jsonutil.ObjectOf("enabled", false, "reason", "durable_run_storage_required"), nil
	}
	if _, err := GatewayRoomGrantSecret(ctx, ""); err != nil {
		return jsonutil.ObjectOf("enabled", false, "reason", "gateway_roomlink_secret_unavailable"), nil
	}
	profile := profiles.CurrentProfileName(ctx)
	if profile == "" {
		profile = "default"
	}
	policy, err := ExecutionPolicyMapping(ctx, profile, nil)
	if err != nil {
		return nil, err
	}
	text := true
	catalog, err := LocalCatalogMapping(ctx, CatalogOptions{InstallationID: installationID, ProtocolVersions: []any{int64(HostedProtocolVersion)},
		LinkModes: []string{"direct"}, Text: &text, Attachments: true, TargetProfile: profile, ExecutionPolicy: policy})
	if err != nil {
		return nil, err
	}
	plain, _ := jsonutil.Plain(catalog).(map[string]any)
	return jsonutil.ObjectOf("enabled", true, "profile", profile, "catalog", plain, "endpoint", plain["endpoint"]), nil
}
