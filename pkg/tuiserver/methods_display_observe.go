//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

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
