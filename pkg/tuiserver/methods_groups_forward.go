package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// The owner was found but did not answer, a verdict for this call and never a local write instead
const ownerLostCode = 5124

// How long one call waits for a starting or restarting gateway, and how often it looks again
const (
	groupsReadyWait = 20 * time.Second
	groupsReadyPoll = 250 * time.Millisecond
)

// Gateway states with no owner importing rooms later, and states on the way to a ready owner
var (
	groupsLocalStates     = map[string]bool{"absent": true, "incompatible": true}
	groupsTransientStates = map[string]bool{"starting": true, "draining": true}
)

// A gateway serves or is about to serve the home but cannot answer now
type ownerBusy struct{ why string }

func (e *ownerBusy) Error() string { return e.why }

// Every room method the gateway route serves
func groupsForwarded() map[string]bool {
	out := map[string]bool{}
	for _, name := range gateway.GroupMethods() {
		out[name] = true
	}
	return out
}

// Home of the requested profile, the process home without one, a ValueError for the launch profile named explicitly
func groupsHome(ctx context.Context, params map[string]any) (string, error) {
	profile := strings.TrimSpace(relayString(params["profile"]))
	if profile != "" && profile != strings.TrimSpace(currentProfileName(ctx)) {
		profileDir, err := profileHome(profile)
		if err != nil {
			panic(err)
		}
		if profileDir == "" {
			return "", fmt.Errorf("profile '%s' is unavailable", profile)
		}
		return profileDir, nil
	}
	return home.Dir(ctx), nil
}

// The home whose gateway imports the standalone rooms
func groupsRootHome(ctx context.Context) string {
	return filepath.Dir(gateway.HostedRoomsDefaultDBPath(ctx))
}

// Ready gateway endpoint of a home, nil when none serves it, waits for a starting one, refuses a busy one
func ownerEndpoint(ctx context.Context, h string) (*gateway.GatewayEndpoint, error) {
	deadline := time.Now().Add(groupsReadyWait)
	for {
		discovery := gateway.DiscoverGatewayEndpoint(ctx, h, 5*time.Second)
		if discovery.State == "ready" && discovery.Endpoint != nil {
			return discovery.Endpoint, nil
		}
		if groupsLocalStates[discovery.State] {
			return nil, nil
		}
		if !groupsTransientStates[discovery.State] || !time.Now().Before(deadline) {
			why := discovery.ReasonCode
			if why == "" {
				why = discovery.State
			}
			return nil, &ownerBusy{why}
		}
		time.Sleep(groupsReadyPoll)
	}
}

// Where the standalone store says a room lives, gateway when handed over, local for any other, empty when absent
func standaloneHolder(ctx context.Context, roomID string) string {
	source := gateway.HostedRoomsDefaultDBPath(ctx)
	if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	authority, found, err := gateway.StoredRoomAuthority(ctx, source, roomID)
	if err != nil {
		panic(err)
	}
	if !found {
		return ""
	}
	handoff, err := gateway.HandoffGatewayID(ctx)
	if err != nil {
		panic(err)
	}
	if authority == handoff {
		return "gateway"
	}
	return "local"
}

// Standalone rooms not handed to the gateway, the gateway less list
func standaloneRooms(ctx context.Context, params map[string]any) []map[string]any {
	source := gateway.HostedRoomsDefaultDBPath(ctx)
	if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
		return []map[string]any{}
	}
	rooms, err := gateway.ListRooms(ctx, source, params != nil && params["include_disbanded"] == true, gateway.MaxRoomListLimit, 0)
	if err != nil {
		panic(err)
	}
	return localRooms(ctx, rooms)
}

// Answer of the owning gateway as this wire's envelope, an unanswered call refused as owner lost
func groupsCall(ctx context.Context, rid any, h string, endpoint *gateway.GatewayEndpoint, method string, params map[string]any) map[string]any {
	raw, err := forwardRPC(ctx, h, endpoint, method, params)
	if err != nil {
		var rpcFailure *gateway.GatewayRPCError
		if errors.As(err, &rpcFailure) {
			code := 5000
			if rpcFailure.Code != nil {
				code = int(*rpcFailure.Code)
			}
			var data any
			if len(rpcFailure.Data) > 0 {
				data = rpcFailure.Data
			}
			return rpcErr(rid, code, rpcFailure.Error(), data)
		}
		logger().Warn(fmt.Sprintf("Group Chat %s: the gateway that owns it did not answer: %s", method, err))
		return rpcErr(rid, ownerLostCode, "The Icebath gateway did not answer. Try again.", nil)
	}
	result, isDict := pyjson.Plain(raw).(map[string]any)
	if !isDict {
		result = map[string]any{}
	}
	if methods, isList := result["methods"].([]any); method == "groups.capabilities" && isList {
		carried := groupsForwarded()
		kept := []any{}
		for _, name := range methods {
			if text, isText := name.(string); isText && carried[text] {
				kept = append(kept, name)
			}
		}
		copied := make(map[string]any, len(result))
		for key, value := range result {
			copied[key] = value
		}
		copied["methods"] = kept
		result = copied
	}
	return ok(rid, result)
}

// Owner lost refusal of a gateway that is not ready yet
func groupsBusy(rid any, why error) map[string]any {
	return rpcErr(rid, ownerLostCode, fmt.Sprintf("The Icebath gateway is not ready yet (%s). Try again in a moment.", why), nil)
}

// The owning gateway's answer to a room method, nil when the call runs locally
func forwardGroups(ctx context.Context, rid any, method string, params map[string]any) map[string]any {
	if !groupsForwarded()[method] {
		return nil
	}
	h, err := groupsHome(ctx, params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	roomID := relayString(params["room_id"])
	holder := ""
	if roomID != "" {
		holder = standaloneHolder(ctx, roomID)
	}
	if holder == "local" {
		return nil
	}
	if holder == "gateway" {
		h = groupsRootHome(ctx)
		stripped := make(map[string]any, len(params))
		for key, value := range params {
			if key != "profile" {
				stripped[key] = value
			}
		}
		params = stripped
	}
	if method == "groups.list" {
		reply, err := groupsList(ctx, rid, h, params)
		var busy *ownerBusy
		if errors.As(err, &busy) {
			return groupsBusy(rid, busy)
		}
		var conversion *pyint.Error
		if errors.As(err, &conversion) {
			if conversion.Kind != "ValueError" {
				panic(err)
			}
			return rpcErr(rid, 4000, err.Error(), nil)
		}
		return reply
	}
	endpoint, err := ownerEndpoint(ctx, h)
	if err != nil {
		return groupsBusy(rid, err)
	}
	if endpoint == nil {
		if holder == "gateway" {
			return rpcErr(rid, ownerLostCode, "The Icebath gateway that holds this Group Chat is not running. Start it and try again.", nil)
		}
		return nil
	}
	return groupsCall(ctx, rid, h, endpoint, method, params)
}

// Group Chat list hiding no room, the gateway rooms plus standalone and handed rooms on the first page
func groupsList(ctx context.Context, rid any, h string, params map[string]any) (map[string]any, error) {
	endpoint, err := ownerEndpoint(ctx, h)
	if err != nil {
		return nil, err
	}
	root := groupsRootHome(ctx)
	source := gateway.HostedRoomsDefaultDBPath(ctx)
	handed := map[string]bool{}
	if info, statErr := os.Stat(source); statErr == nil && info.Mode().IsRegular() {
		if handed, err = gateway.HandedRoomIDs(ctx, source); err != nil {
			panic(err)
		}
	}
	var rootEndpoint *gateway.GatewayEndpoint
	if len(handed) > 0 && resolveLax(h) != resolveLax(root) {
		if rootEndpoint, err = ownerEndpoint(ctx, root); err != nil {
			return nil, err
		}
	}
	if endpoint == nil && rootEndpoint == nil {
		return nil, nil
	}
	var reply map[string]any
	if endpoint == nil {
		reply = ok(rid, map[string]any{"rooms": []any{}, "next_offset": nil})
	} else {
		reply = groupsCall(ctx, rid, h, endpoint, "groups.list", params)
	}
	if _, failed := reply["error"]; failed {
		return reply, nil
	}
	if truthy(params["offset"]) {
		offset, err := pyint.Of(params["offset"])
		if err != nil {
			return nil, err
		}
		if offset.Sign() != 0 {
			return reply, nil
		}
	}
	result, _ := reply["result"].(map[string]any)
	rooms, _ := result["rooms"].([]any)
	rooms = append([]any(nil), rooms...)
	var extra []any
	for _, room := range standaloneRooms(ctx, params) {
		extra = append(extra, room)
	}
	if rootEndpoint != nil {
		page := groupsCall(ctx, rid, root, rootEndpoint, "groups.list",
			map[string]any{"include_disbanded": params["include_disbanded"] == true, "limit": gateway.MaxRoomListLimit})
		if _, failed := page["error"]; failed {
			return page, nil
		}
		pageResult, _ := page["result"].(map[string]any)
		pageRooms, _ := pageResult["rooms"].([]any)
		for _, room := range pageRooms {
			if row, isDict := room.(map[string]any); isDict && handed[pyrepr.Str(row["room_id"])] {
				extra = append(extra, room)
			}
		}
	}
	seen := map[string]bool{}
	for _, room := range rooms {
		if row, isDict := room.(map[string]any); isDict {
			seen[pyrepr.Str(row["room_id"])] = true
		}
	}
	for _, room := range extra {
		row, _ := room.(map[string]any)
		id := pyrepr.Str(row["room_id"])
		if !seen[id] {
			seen[id] = true
			rooms = append(rooms, room)
		}
	}
	merged := make(map[string]any, len(result)+1)
	for key, value := range result {
		merged[key] = value
	}
	merged["rooms"] = rooms
	return ok(rid, merged), nil
}

// A gateway less listing without the rooms already handed to the gateway's store
func localRooms(ctx context.Context, rooms []map[string]any) []map[string]any {
	handoff, err := gateway.HandoffGatewayID(ctx)
	if err != nil {
		panic(err)
	}
	out := []map[string]any{}
	for _, room := range rooms {
		if !pyjson.Equal(room["authority_gateway_id"], handoff) {
			out = append(out, room)
		}
	}
	return out
}
