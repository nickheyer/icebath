package gatewayrun

import (
	"context"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Descriptor keys the identify verb copies from the session runtime descriptor
var identifyDescriptorKeys = []string{"instance_id", "runtime_protocol", "authority_epoch", "state", "api_origin",
	"served_profiles", "parked_profiles", "capabilities"}

// Identify payload of this runtime, its descriptor facts over the process identity
func identifyRuntime(r *GatewayRunner) *pyjson.Object {
	payload := gateway.BuildIdentifyPayload()
	descriptor := r.sessionRuntimeDescriptor.load()
	for _, key := range identifyDescriptorKeys {
		if v, ok := descriptor[key]; ok {
			payload.Set(key, v)
		}
	}
	if r.draining.Load() {
		payload.Set("state", "draining")
		if reason, _ := descriptor["drain_reason"].(string); reason != gateway.RestartWaitReason {
			payload.Set("capabilities", []any{})
		}
	}
	if state, _ := payload.Get("state"); state == "draining" {
		if reason := descriptor["drain_reason"]; pyrepr.Truthy(reason) {
			payload.Set("drain_reason", reason)
		}
	}
	supervisor, _ := payload.Get("supervisor")
	switch supervisor {
	case "manual", "desktop":
		payload.Set("supervisor", "none")
	case nil:
		if _, present := payload.Get("supervisor"); !present {
			payload.Set("supervisor", "none")
		}
	}
	return payload
}

// Control socket start, the real method swapped only where a hermes case installs a fake
var controlServerStart = (*gateway.GatewayControlServer).Start

// Starts the gateway control socket answering identify, status and the gateway verbs, nil when unavailable
func startGatewayStartControlSocket(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) *gateway.GatewayControlServer {
	server, err := func() (*gateway.GatewayControlServer, error) {
		pauseForUpdate := func(map[string]any) (any, error) {
			drain, err := gateway.GetRestartDrainTimeout(ctx)
			if err != nil {
				drain = 30.0
			}
			var accepted []bool
			done := make(chan struct{})
			request := func() {
				defer close(done)
				accepted = append(accepted, r.RequestRestart(false, true))
			}
			if err := loop.CallSoonThreadsafe(request); err != nil {
				return nil, err
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			ok := false
			select {
			case <-done:
				ok = len(accepted) > 0 && accepted[0]
			default:
			}
			return pyjson.ObjectOf("pausing", ok, "already_stopping", !ok, "pid", int64(os.Getpid()), "drain_timeout", drain), nil
		}
		rescanProfiles := func(map[string]any) (any, error) {
			result, timedOut, err := runOnLoopWithin(ctx, loop, "control-socket-rescan", 5*time.Second,
				func(c context.Context) (any, error) { return r.reconcileServedProfiles(c, "control-socket") })
			if err != nil {
				return nil, err
			}
			multiplex := r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles
			answer := pyjson.ObjectOf("multiplex", multiplex)
			if timedOut {
				answer.Set("pending", true)
				answer.Set("served_profiles", namesList(r.servedProfileNames()))
				return answer, nil
			}
			reconciled := result.(*pyjson.Object)
			for _, key := range reconciled.Keys {
				v, _ := reconciled.Get(key)
				answer.Set(key, v)
			}
			return answer, nil
		}
		activeWork := activeWorkVerb(ctx, r, loop)
		server, err := gateway.NewGatewayControlServer("", map[string]gateway.ControlVerbHandler{
			"pause-for-update":         pauseForUpdate,
			"identify":                 func(map[string]any) (any, error) { return identifyRuntime(r), nil },
			"rescan-profiles":          rescanProfiles,
			"migrate-profile-identity": migrateProfileIdentityVerb(ctx, r),
			"unserve-profile":          unserveProfileVerb(ctx, r, loop),
			"serve-profile":            serveProfileVerb(ctx, r, loop),
			"purge-profile-identity":   purgeProfileIdentityVerb(ctx, r),
			"active-work":              func(params map[string]any) (any, error) { return activeWork(params) },
			"reload-plugins":           reloadPluginsVerb(ctx, r, loop),
		})
		if err != nil {
			return nil, err
		}
		server.TicketStore = r.sessionTicketStore
		registerControlVerbs(ctx, r, home.Dir(ctx), server, nil)
		if !controlServerStart(server) {
			return nil, nil
		}
		registerBootstrapAtExit(server.CleanupFiles)
		r.sessionControlServer = server
		return server, nil
	}()
	if err != nil {
		runLog().Error("Control socket startup failed; the gateway cannot start without it: " + err.Error())
		return nil
	}
	return server
}
