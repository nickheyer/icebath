package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Single use websocket ticket minter of the dashboard, assigned in cmd/icebath/wiring.go
var MintDisplayTicket func(userID, provider, issuer string, extra *pyjson.Object) string

// Browser credential generation a dashboard socket was admitted under, assigned in cmd/icebath/wiring.go
var DisplayAdmissionOf func(conn any) (string, bool)

// Mints a single use ticket for the screen bridge naming this profile home and a server minted viewer id
func displayObserve(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, displayErr, failureText(failure), nil)
		}
	}()
	env := displayEnv(ctx)
	if botdesktop.RFBSocketPath(env) == "" {
		running, err := botdesktop.SandboxScreenRunning(env)
		if err != nil {
			return rpcErr(rid, displayErr, err.Error(), nil)
		}
		if !running {
			return rpcErr(rid, displayErr, "this profile's Bot Desktop is not running; call display.start first", nil)
		}
	}
	viewerID := mintViewerID(ctx, strParam(params, "viewer_id", ""))
	extra := pyjson.ObjectOf("hermes_home", home.Dir(ctx), "viewer_id", viewerID)
	var conn any
	if ws, isWS := tuigateway.CurrentTransport(ctx).(*tuigateway.WSTransport); isWS && ws != nil {
		conn = ws.Conn()
	}
	if admitted, tracked := DisplayAdmissionOf(conn); tracked {
		extra.Set("admitted_with", admitted)
	}
	ticket := MintDisplayTicket(fmt.Sprintf("display:%s", viewerID), "bot-desktop", "", extra)
	return snapshotResponse(ctx, rid, func(snap *pyjson.Object) {
		out := pyjson.ObjectOf("ticket", ticket, "path", "/api/display/ws", "viewer_id", viewerID)
		for _, key := range snap.Keys {
			out.Set(key, snap.Vals[key])
		}
		*snap = *out
	})
}

// Answers the pending default image switch, approve pinning the new image and false the current one
func displaySwitchSandboxImage(ctx context.Context, rid any, params map[string]any) map[string]any {
	env := displayEnv(ctx)
	sw, err := tools.PendingSandboxImageSwitch(ctx, env)
	if err != nil {
		panic(err)
	}
	if sw == nil {
		return rpcErr(rid, displayErr, "no sandbox image switch is pending for this profile", nil)
	}
	approve := true
	if raw, present := params["approve"]; present {
		approve = truthy(raw)
	}
	image, err := tools.DecideSandboxImageSwitch(ctx, env, sw, approve)
	if err != nil {
		panic(err)
	}
	snap, err := displaySnapshot(ctx)
	if err != nil {
		panic(err)
	}
	out := pyjson.ObjectOf("docker_image", image)
	for _, key := range snap.Keys {
		out.Set(key, snap.Vals[key])
	}
	return ok(rid, out)
}
