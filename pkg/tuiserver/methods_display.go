package tuiserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const displayErr = 5300

var (
	displayRegistry        = &handlerRegistry{}
	leaseListenerInstalled atomic.Bool
	leaseListenerMu        sync.Mutex
	broadcastGlobalEvent   = func(event string, payload map[string]any) { tuigateway.BroadcastGlobalEvent(event, payload) }
)

func init() {
	displayRegistry.scopedMethod("display.status", displayStatus)
	displayRegistry.scopedMethod("display.thumbnail", displayThumbnail)
	displayRegistry.scopedMethod("display.start", displayStart)
	displayRegistry.scopedMethod("display.stop", displayStop)
	displayRegistry.scopedMethod("display.observe", displayObserve)
	displayRegistry.scopedMethod("display.switchSandboxImage", displaySwitchSandboxImage)
	displayRegistry.scopedMethod("display.install", displayInstall)
	displayRegistry.scopedMethod("display.lease.acquire", displayLeaseAcquire)
	displayRegistry.scopedMethod("display.lease.release", displayLeaseRelease)
	displayRegistry.install()
}

// Tool env of the profile home the request is scoped to, the env the bot desktop runtime reads
func displayEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
}

// Runtime status of the scoped profile screen with its lease public view and profile key
func displaySnapshot(ctx context.Context) (*pyjson.Object, error) {
	env := displayEnv(ctx)
	st, err := botdesktop.Status(env, "")
	if err != nil {
		return nil, err
	}
	out := st.AsDict().Copy()
	out.Set("lease", botdesktop.LeasePublicView(botdesktop.LeaseGet(home.Dir(ctx))))
	out.Set("profile_key", home.Key(home.Dir(ctx)))
	return out, nil
}

// Subscribes once per process so every lease change broadcasts to all connected clients
func installLeaseListener() {
	leaseListenerMu.Lock()
	defer leaseListenerMu.Unlock()
	if leaseListenerInstalled.Load() {
		return
	}
	botdesktop.LeaseOnChange(func(profileKey string, lease botdesktop.BotDesktopLease) {
		broadcastGlobalEvent("display.lease", map[string]any{"profile_key": profileKey, "lease": botdesktop.LeasePublicView(lease)})
	})
	leaseListenerInstalled.Store(true)
}

// Snapshot response of the scoped profile screen, the error text under the display code on failure
func snapshotResponse(ctx context.Context, rid any, extra func(*pyjson.Object)) map[string]any {
	snap, err := displaySnapshot(ctx)
	if err != nil {
		return rpcErr(rid, displayErr, err.Error(), nil)
	}
	if extra != nil {
		extra(snap)
	}
	return ok(rid, snap)
}

// Runtime and lease status of the scoped profile screen
func displayStatus(ctx context.Context, rid any, params map[string]any) map[string]any {
	installLeaseListener()
	return snapshotResponse(ctx, rid, nil)
}

// Starts the scoped profile screen and answers its fresh status
func displayStart(ctx context.Context, rid any, params map[string]any) map[string]any {
	installLeaseListener()
	if _, err := botdesktop.Start(displayEnv(ctx), 15.0); err != nil {
		return rpcErr(rid, displayErr, err.Error(), nil)
	}
	return snapshotResponse(ctx, rid, nil)
}

// Stops the screen, refused while a human holds the lease unless the caller forces it
func displayStop(ctx context.Context, rid any, params map[string]any) map[string]any {
	force := truthy(params["force"])
	released, err := botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ProfileKey: home.Dir(ctx), UnlessHuman: !force})
	if err != nil {
		panic(err)
	}
	if released.Holder == botdesktop.LeaseHuman {
		return rpcErr(rid, displayErr, "a human holds this screen; pass force: true to stop it anyway", map[string]any{"code": "viewer_mismatch"})
	}
	stopped, err := botdesktop.Stop(displayEnv(ctx), nil)
	if err != nil {
		return rpcErr(rid, displayErr, err.Error(), nil)
	}
	return snapshotResponse(ctx, rid, func(snap *pyjson.Object) { snap.Set("stopped", stopped) })
}

// Viewer ids one weakly held transport minted
type mintedEntry struct {
	ref weak.Pointer[byte]
	ids map[string]bool
}

var (
	mintedMu         sync.Mutex
	mintedViewerIDs  = map[uintptr]*mintedEntry{}
	unweakableMinted = map[string]bool{}
)

// Viewer ids minted on the calling connection, one process wide set for a transport that is not a heap object
func mintedForThisConnection(ctx context.Context) map[string]bool {
	transport := tuigateway.CurrentTransport(ctx)
	v := reflect.ValueOf(transport)
	if transport == nil || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Type().Size() == 0 {
		return unweakableMinted
	}
	ptr := (*byte)(v.UnsafePointer())
	for addr, entry := range mintedViewerIDs {
		if entry.ref.Value() == nil {
			delete(mintedViewerIDs, addr)
		}
	}
	addr := uintptr(v.UnsafePointer())
	entry := mintedViewerIDs[addr]
	if entry == nil || entry.ref.Value() != ptr {
		entry = &mintedEntry{ref: weak.Make(ptr), ids: map[string]bool{}}
		mintedViewerIDs[addr] = entry
	}
	return entry.ids
}

// Server minted viewer identity, the requested id kept only when this connection minted it before
func mintViewerID(ctx context.Context, requested string) string {
	mintedMu.Lock()
	defer mintedMu.Unlock()
	mine := mintedForThisConnection(ctx)
	if mine[requested] {
		return requested
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	viewerID := base64.RawURLEncoding.EncodeToString(raw)
	mine[viewerID] = true
	return viewerID
}

// Refusal for a viewer id this connection never minted, nil for one it did
func foreignViewerID(ctx context.Context, rid any, viewerID string) map[string]any {
	mintedMu.Lock()
	minted := mintedForThisConnection(ctx)[viewerID]
	mintedMu.Unlock()
	if minted {
		return nil
	}
	return rpcErr(rid, displayErr, "viewer_id was not minted for this connection; call display.observe first", map[string]any{"code": "viewer_mismatch"})
}

// Starts the distro package install in the background, streaming its log and ending with a status snapshot
func displayInstall(ctx context.Context, rid any, params map[string]any) map[string]any {
	env := displayEnv(ctx)
	if !botdesktop.IsSupportedHost() {
		return rpcErr(rid, displayErr, "Bot Desktop runs on Linux gateway hosts only", nil)
	}
	inSandbox, err := botdesktop.InSandbox(env)
	if err != nil {
		panic(err)
	}
	if inSandbox {
		return rpcErr(rid, displayErr, "this profile's screen lives inside the terminal backend's sandbox; give that "+
			"sandbox an image with the desktop stack (nousresearch/hermes-sandbox:desktop) "+
			"instead of installing on the gateway host", nil)
	}
	if botdesktop.InstallCommand() == "" {
		return rpcErr(rid, displayErr, "no supported package manager (apt-get, dnf, pacman) on this host", nil)
	}
	profileKey := home.Key(home.Dir(ctx))
	worker := context.WithoutCancel(ctx)
	askPassword := func() (string, error) {
		timeout := 300.0
		return askRequest(worker, "display.install.sudo", "", map[string]any{"profile_key": profileKey}, &timeout), nil
	}
	line := func(text string) {
		broadcastGlobalEvent("display.install.log", map[string]any{"profile_key": profileKey, "line": text})
	}
	run := func() {
		code, err := installPackagesClaimed(env, askPassword, line)
		if err != nil {
			line(fmt.Sprintf("install failed: %s", err))
			code = 1
		}
		status, snapErr := displaySnapshot(worker)
		if snapErr != nil {
			fmt.Fprintf(os.Stderr, "Exception in thread bot-desktop-install:%s:\n%s\n", profileKey, snapErr)
			return
		}
		broadcastGlobalEvent("display.install.done", map[string]any{"profile_key": profileKey, "code": code, "status": status})
	}
	if _, err := botdesktop.Claim(env); err != nil {
		var busy *botdesktop.InstallBusy
		if errors.As(err, &busy) {
			return rpcErr(rid, displayErr, busy.Error(), nil)
		}
		panic(err)
	}
	go run()
	return ok(rid, map[string]any{"started": true, "command": botdesktop.InstallCommand(), "profile_key": profileKey})
}

// Package install over a claimed slot, a panic inside it surfacing as the failure it raised
func installPackagesClaimed(env *tools.Env, askPassword func() (string, error), line func(string)) (code int, err error) {
	defer func() {
		if r := recover(); r != nil {
			code, err = 1, errors.New(panicMessage(r))
		}
	}()
	return botdesktop.InstallPackagesWith(env, askPassword, line, 900.0, true)
}

// Human takeover of the screen by a viewer this connection minted, stamped as activity first
func displayLeaseAcquire(ctx context.Context, rid any, params map[string]any) map[string]any {
	viewerID := pystr.Strip(strOr(params["viewer_id"]))
	if viewerID == "" {
		return rpcErr(rid, displayErr, "viewer_id required", nil)
	}
	if refused := foreignViewerID(ctx, rid, viewerID); refused != nil {
		return refused
	}
	botdesktop.TouchActivity(displayEnv(ctx))
	lease, err := botdesktop.LeaseAcquire(viewerID, home.Dir(ctx), strOr(params["reason"]))
	if err != nil {
		panic(err)
	}
	return ok(rid, map[string]any{"lease": botdesktop.LeasePublicView(lease)})
}

// Hands control back, a bare release refused while a human holds unless forced
func displayLeaseRelease(ctx context.Context, rid any, params map[string]any) map[string]any {
	var viewerID *string
	if v := pystr.Strip(strOr(params["viewer_id"])); v != "" {
		viewerID = &v
	}
	force := truthy(params["force"])
	if viewerID != nil && !force {
		if refused := foreignViewerID(ctx, rid, *viewerID); refused != nil {
			return refused
		}
	}
	lease, err := botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ViewerID: viewerID, ProfileKey: home.Dir(ctx), UnlessHuman: viewerID == nil && !force})
	if err != nil {
		panic(err)
	}
	if viewerID == nil && lease.Holder == botdesktop.LeaseHuman {
		return rpcErr(rid, displayErr, "viewer_id required to release another viewer's lease (or pass force: true)", map[string]any{"code": "viewer_mismatch"})
	}
	return ok(rid, map[string]any{"lease": botdesktop.LeasePublicView(lease)})
}

// One JPEG grab of the scoped screen, withheld while a human holds the lease or it moved during the grab
func displayThumbnail(ctx context.Context, rid any, params map[string]any) map[string]any {
	before := botdesktop.LeaseGet(home.Dir(ctx))
	if before.Holder == botdesktop.LeaseHuman {
		return ok(rid, map[string]any{"data_url": nil, "suppressed": "human_has_control"})
	}
	dataURL, err := botdesktop.ThumbnailDataURL(displayEnv(ctx), botdesktop.ThumbMaxW, botdesktop.ThumbMaxH, 72)
	if err != nil {
		return rpcErr(rid, displayErr, err.Error(), nil)
	}
	if !pyrepr.Equal(botdesktop.LeaseGet(home.Dir(ctx)).Epoch, before.Epoch) {
		return ok(rid, map[string]any{"data_url": nil, "suppressed": "human_has_control"})
	}
	if dataURL == "" {
		return ok(rid, map[string]any{"data_url": nil})
	}
	return ok(rid, map[string]any{"data_url": dataURL})
}
