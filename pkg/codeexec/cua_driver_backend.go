package codeexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CuaDriverBackend is the default computer use backend over the cua-driver MCP server
type CuaDriverBackend struct {
	BackendDefaults
	env            *tools.Env
	PermissionMode string
	embeddedDaemon *embeddedCuaDaemon
	bridge         *cuaAsyncBridge
	session        *cuaDriverSession
	activePID      *int
	activeWindowID *int
	lastApp        *string
	lastTarget     *pyjson.Object
	snapshotTokens map[int]string
	sessionID      string
}

// Ensures a PM package on demand, swapped by tests
var pmEnsure = func(ctx context.Context, name string) error {
	_, err := pm.Ensure(ctx, name, pm.EnsureOptions{})
	return err
}

// Builds the backend for a permission mode, with a private daemon outside standard mode
func newCuaDriverBackend(env *tools.Env, permissionMode string) (*CuaDriverBackend, error) {
	if permissionMode != "standard" && permissionMode != "bounded" && permissionMode != "unrestricted" {
		return nil, fmt.Errorf("unsupported cua-driver permission mode: %s", permissionMode)
	}
	b := &CuaDriverBackend{env: env, PermissionMode: permissionMode}
	if permissionMode != "standard" {
		manifest := ""
		if raw, ok := computerUseCfg(env)["capability_manifest"].(string); ok {
			manifest = pystr.Strip(raw)
		}
		daemon, err := newEmbeddedCuaDaemon(env, "", permissionMode, manifest)
		if err != nil {
			return nil, err
		}
		b.embeddedDaemon = daemon
	}
	b.bridge = &cuaAsyncBridge{}
	b.session = newCuaDriverSession(env, b.bridge, b.embeddedDaemon)
	b.clearActiveTarget()
	b.sessionID = "icebath-" + cuaToken()
	b.session.setTransportResetCallback(b.handleTransportReset)
	return b, nil
}

// Invalidates every capability minted by the replaced transport
func (b *CuaDriverBackend) handleTransportReset() { b.clearActiveTarget() }

// Checks the runtime contract, starts the private daemon and the session, then declares this run's identity
func (b *CuaDriverBackend) Start() error {
	ctx := context.Background()
	sandbox, _, err := sandboxMCPInvocation(ctx, b.env)
	if err != nil {
		return err
	}
	override := pystr.Strip(os.Getenv(cuaDriverCmdEnv)) != ""
	ready, reason := true, any(nil)
	if sandbox == nil {
		if !override {
			if err := pmEnsure(ctx, "cua-driver"); err != nil {
				return err
			}
		}
		contract, err := cuaDriverRuntimeContractStatus(b.env, "")
		if err != nil {
			return err
		}
		ready, reason = pyrepr.Truthy(cuGet(contract, "ready")), cuGet(contract, "reason")
	}
	if !ready {
		why := "runtime contract is incomplete"
		if pyrepr.Truthy(reason) {
			why = pyrepr.Str(reason)
		}
		hint := "Run `icebath computer-use install` to repair it."
		if override {
			hint = "Update the binary selected by ICEBATH_CUA_DRIVER_CMD or remove that override."
		}
		return fmt.Errorf("cua-driver is not ready: %s. %s", why, hint)
	}
	if b.embeddedDaemon != nil {
		if err := b.embeddedDaemon.start(); err != nil {
			if serr := b.embeddedDaemon.stop(); serr != nil {
				return serr
			}
			return err
		}
	}
	if err := b.session.start(); err != nil {
		if b.embeddedDaemon != nil {
			if serr := b.embeddedDaemon.stop(); serr != nil {
				return serr
			}
		}
		return err
	}
	declare := pyjson.NewObject()
	declare.Set("session", b.sessionID)
	b.bestEffort("start_session failed (continuing anonymous)", func() error {
		_, err := b.session.callTool("start_session", declare, 30)
		return err
	})
	if b.session.started.Load() {
		maxDim, err := computerUseMaxImageDimension(b.env)
		if err != nil {
			return err
		}
		if maxDim != nil && *maxDim != 0 {
			cfg := pyjson.NewObject()
			cfg.Set("max_image_dimension", *maxDim)
			b.bestEffort("set_config(max_image_dimension) failed", func() error { b.SetConfig(cfg); return nil })
		}
		if cuaNoOverlay(b.env) {
			b.bestEffort("set_agent_cursor_enabled failed", func() error { b.SetAgentCursorEnabled(false, b.sessionID); return nil })
		}
	}
	return nil
}

// Ends the driver session, then tears down session, bridge and private daemon even when one step fails
func (b *CuaDriverBackend) Stop() error {
	if b.session.started.Load() {
		end := pyjson.NewObject()
		end.Set("session", b.sessionID)
		b.bestEffort("end_session failed (continuing teardown)", func() error {
			_, err := b.session.callTool("end_session", end, 30)
			return err
		})
	}
	b.session.stop()
	b.bridge.stop()
	if b.embeddedDaemon != nil {
		return b.embeddedDaemon.stop()
	}
	return nil
}

// Runs a non fatal driver call, logging its failure at debug
func (b *CuaDriverBackend) bestEffort(what string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("cua-driver %s: %v", what, r))
		}
	}()
	if err := fn(); err != nil {
		slog.Debug(fmt.Sprintf("cua-driver %s: %v", what, err))
	}
}

// Desktop platforms with a resolvable driver binary
func (b *CuaDriverBackend) IsAvailable() (bool, error) {
	if cuPlatform != "darwin" && cuPlatform != "win32" && cuPlatform != "linux" {
		return false, nil
	}
	return cuaDriverBinaryAvailable()
}

// Forgets the capture or focus target so a failed lookup cannot misroute input
func (b *CuaDriverBackend) clearActiveTarget() {
	b.activePID, b.activeWindowID, b.lastApp, b.lastTarget = nil, nil, nil, nil
	b.snapshotTokens = map[int]string{}
}

// Pins the sticky target and disarms the prior snapshot's tokens
func (b *CuaDriverBackend) setActiveTarget(target *pyjson.Object) {
	pid, _ := cuGet(target, "pid").(int)
	wid, _ := cuGet(target, "window_id").(int)
	b.activePID, b.activeWindowID = &pid, &wid
	b.snapshotTokens = map[int]string{}
	b.lastTarget = pyjson.NewObject()
	b.lastTarget.Set("pid", pid)
	b.lastTarget.Set("window_id", wid)
}

// LaunchAppArgs are the keyword arguments of launch_app
type LaunchAppArgs struct {
	BundleID                      string
	Name                          string
	URLs                          []string
	AdditionalArguments           []string
	CreatesNewApplicationInstance bool
}

// Idempotent launch returning the driver's pid, bundle id, name and windows
func (b *CuaDriverBackend) LaunchApp(a LaunchAppArgs) (any, error) {
	if a.BundleID == "" && a.Name == "" {
		return nil, errors.New("launch_app requires either bundle_id or name")
	}
	args := pyjson.NewObject()
	args.Set("session", b.sessionID)
	if a.BundleID != "" {
		args.Set("bundle_id", a.BundleID)
	}
	if a.Name != "" {
		args.Set("name", a.Name)
	}
	if len(a.URLs) > 0 {
		args.Set("urls", stringsToAny(a.URLs))
	}
	if len(a.AdditionalArguments) > 0 {
		args.Set("additional_arguments", stringsToAny(a.AdditionalArguments))
	}
	if a.CreatesNewApplicationInstance {
		args.Set("creates_new_application_instance", true)
	}
	out, err := b.session.callTool("launch_app", args, 30)
	if err != nil {
		return nil, err
	}
	if sc := cuGet(out, "structuredContent"); pyrepr.Truthy(sc) {
		return sc, nil
	}
	data := pyjson.NewObject()
	data.Set("data", cuGet(out, "data"))
	return data, nil
}

// Activates a window so later foreground input lands on it
func (b *CuaDriverBackend) BringToFront(pid int, windowID *int) *ActionResult {
	args := pyjson.NewObject()
	args.Set("pid", pid)
	if windowID != nil {
		args.Set("window_id", *windowID)
	}
	return b.action("bring_to_front", args, false)
}

// Toggles the agent cursor overlay of this run
func (b *CuaDriverBackend) SetAgentCursorEnabled(enabled bool, cursorID string) *ActionResult {
	args := pyjson.NewObject()
	args.Set("enabled", enabled)
	if cursorID != "" {
		args.Set("cursor_id", cursorID)
	}
	return b.action("set_agent_cursor_enabled", args, true)
}

// Sets cua-driver config keys, unknown keys pass through for the driver to validate
func (b *CuaDriverBackend) SetConfig(config *pyjson.Object) *ActionResult {
	return b.action("set_config", config.Copy(), true)
}

// Calls any cua-driver MCP tool by name with the session label defaulted in
func (b *CuaDriverBackend) CallTool(name string, args *pyjson.Object, timeout float64) (*pyjson.Object, error) {
	payload := pyjson.NewObject()
	if args != nil {
		payload = args.Copy()
	}
	if _, has := payload.Get("session"); !has {
		payload.Set("session", b.sessionID)
	}
	return b.session.callTool(name, payload, timeout)
}

// Snapshot token of an element_index argument, bools counting as ints
func snapshotIndex(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// One driver action with the snapshot token attached, merged meta and the structured verdict
func (b *CuaDriverBackend) action(name string, args *pyjson.Object, injectSession bool) *ActionResult {
	if idx, ok := snapshotIndex(cuGet(args, "element_index")); ok {
		if token := b.snapshotTokens[idx]; token != "" && (b.session.supportsInputProperty(name, "element_token") ||
			b.session.supportsCapability("accessibility.element_tokens", name)) {
			args.Set("element_token", token)
		}
	}
	if _, has := args.Get("session"); injectSession && !has {
		args.Set("session", b.sessionID)
	}
	out, err := b.session.callTool(name, args, 30)
	if err != nil {
		slog.Error(fmt.Sprintf("cua-driver %s call failed", name), "error", err)
		return refuseAction(name, "cua-driver error: "+err.Error())
	}
	data := cuGet(out, "data")
	structured := cuGet(out, "structuredContent")
	if !pyrepr.Truthy(structured) {
		structured = pyjson.NewObject()
	}
	message := ""
	switch d := data.(type) {
	case *pyjson.Object:
		message = pyrepr.Str(getOr(d, "message", ""))
	case string:
		message = d
	}
	if message == "" {
		if s, ok := structured.(*pyjson.Object); ok {
			message = pyrepr.Str(getOr(s, "message", ""))
		}
	}
	meta := pyjson.NewObject()
	for _, part := range []any{data, structured} {
		if p, ok := part.(*pyjson.Object); ok {
			for _, k := range p.Keys {
				meta.Set(k, p.Vals[k])
			}
		}
	}
	var requested *string
	if s, ok := cuGet(args, "delivery_mode").(string); ok {
		requested = &s
	}
	return actionResultFrom(name, !pyrepr.Truthy(cuGet(out, "isError")), message, meta, structured, requested)
}

// dict.get with a default
func getOr(o *pyjson.Object, key string, def any) any {
	if v, has := o.Get(key); has {
		return v
	}
	return def
}
