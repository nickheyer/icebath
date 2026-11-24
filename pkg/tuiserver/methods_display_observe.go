//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
		approve = anyutil.IsTrue(raw)
	}
	image, err := tools.DecideSandboxImageSwitch(ctx, env, sw, approve)
	if err != nil {
		panic(err)
	}
	snap, err := displaySnapshot(ctx)
	if err != nil {
		panic(err)
	}
	out := jsonutil.ObjectOf("docker_image", image)
	for _, key := range snap.Keys() {
		out.Set(key, snap.Value(key))
	}
	return ok(rid, out)
}
