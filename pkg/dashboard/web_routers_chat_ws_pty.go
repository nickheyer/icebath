package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Chat argv resolver, PTY spawner, bridge probe and marker allocator of the terminal socket, the module attributes hermes tests patch
var (
	chatResolveArgv        = resolveChatArgvAsync
	chatSpawnBridge        = tools.SpawnPtyBridge
	chatPtyBridgeAvailable = tools.PtyBridgeAvailable
	chatActiveSessionFile  = activeSessionFileForChannel
)

// Session id a channel marker file names, empty for an unreadable or undecodable file
func readActiveSessionFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return "", errors.New("UnicodeDecodeError: 'utf-8' codec can't decode the active session file")
	}
	parsed, err := pyjson.Loads(string(raw))
	if err != nil {
		return "", nil
	}
	data, ok := parsed.(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	if id := data.GetOr("session_id", nil); pyrepr.Truthy(id) {
		return pystr.Strip(pyrepr.Str(id)), nil
	}
	return "", nil
}

// Pid of the live process holding the single writer lease of a session in a home, nil when none
func leaseHolderPid(ctx context.Context, sessionID, registryHome string) *int {
	if sessionID == "" {
		return nil
	}
	entries, err := activesessions.RegistrySnapshot(ctx, registryHome, false)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.GetOr("session_id", nil) != sessionID {
			continue
		}
		switch pid := entry.GetOr("pid", nil).(type) {
		case int64:
			n := int(pid)
			return &n
		case bool:
			n := 0
			if pid {
				n = 1
			}
			return &n
		}
	}
	return nil
}

// Viewer socket whose client frames the keep alive writer reads
type ptyInputSocket interface {
	tools.PtyViewer
	Receive(ctx context.Context) (Message, error)
}

// Forwards client input of a kept alive PTY socket to its session, resize escapes applied and never written
func pumpKeepaliveInput(ctx context.Context, ws ptyInputSocket, session *tools.PtySession) error {
	for {
		msg, err := ws.Receive(ctx)
		if err != nil {
			if wsReceiveRuntimeError(err) {
				return nil
			}
			return err
		}
		if msg.Type == "websocket.disconnect" {
			return nil
		}
		raw := msg.Bytes
		if raw == nil {
			raw = []byte{}
			if msg.Text != nil {
				raw = []byte(*msg.Text)
			}
		}
		if len(raw) == 0 {
			continue
		}
		if m := ptyResizeRE.FindSubmatch(raw); m != nil && len(m[0]) == len(raw) {
			session.Resize(ws, resizeDimension(m[1]), resizeDimension(m[2]))
			continue
		}
		ok, err := session.Write(ctx, ws, raw, tools.PtyWriteTimeout, nil)
		if err != nil {
			return err
		}
		if !ok {
			if !session.Alive() {
				closeStalledPtyInput(ctx, ws, "keepalive")
			}
			return nil
		}
	}
}

// Marker file of the channel, whether it existed before this socket and the resume target it implies
func ptyChannelMarkerState(ws *WebSocket, resume string, forceFresh bool) (string, bool, string, error) {
	channel := channelOf(ws)
	if channel == "" {
		return "", false, resume, nil
	}
	_, preexisting := getPtyActiveSessionFiles(ws.App()).Get(channel)
	path, err := chatActiveSessionFile(ws.App(), channel)
	if err != nil {
		return "", false, resume, err
	}
	if forceFresh {
		_ = pypath.Unlink(path)
		return path, false, "", nil
	}
	if resume == "" {
		if resume, err = readActiveSessionFile(path); err != nil {
			return "", false, "", err
		}
	}
	return path, preexisting, resume, nil
}

// Whether a PTY start failure is one the socket reports to the user instead of raising
func ptyStartRefused(err error) bool {
	var unavailable *tools.PtyUnavailableError
	var full *tools.RegistryFullError
	return errors.As(err, &unavailable) || errors.As(err, &full) || pypath.IsOSError(err)
}

var ptyFreshValues = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Chat terminal socket, one PTY per socket without an attach token, else a kept alive PTY reattached by token
func ptyWS(ctx context.Context, ws *WebSocket) error {
	peer, mode, cred, ok, err := wsGate(ctx, ws, "pty")
	if err != nil || !ok {
		return err
	}
	if err := ws.Accept(ctx, nil, nil); err != nil {
		return err
	}
	routersLog().Info(fmt.Sprintf("pty accepted peer=%s mode=%s cred=%s", peer, mode, cred))
	if !chatPtyBridgeAvailable() {
		if err := ws.SendText(ctx, "\r\n\x1b[31mChat unavailable: the embedded terminal requires a "+
			"POSIX PTY, which native Windows Python doesn't provide.\x1b[0m\r\n"+
			"\x1b[33mInstall Icebath inside WSL2 to use the dashboard's /chat "+
			"tab — the rest of the dashboard works here.\x1b[0m\r\n"); err != nil {
			return err
		}
		return ws.Close(ctx, 1011, "")
	}
	app := ws.App()
	query := ws.QueryParams()
	rawResume := query.Get("resume")
	profile := query.Get("profile")
	channel := channelOf(ws)
	sidecarURL := ""
	if channel != "" {
		sidecarURL, _ = buildSidecarURL(app, channel)
	}
	forceFresh := ptyFreshValues[pystr.Lower(pystr.Strip(query.Get("fresh")))]
	activeSessionFile, markerPreexisting, resume, err := ptyChannelMarkerState(ws, rawResume, forceFresh)
	if err != nil {
		return err
	}
	if channel != "" && !forceFresh && resume != "" && resume != rawResume {
		if err := ws.SendJSON(ctx, pyjson.ObjectOf("type", "resume", "id", resume), false); err != nil {
			return err
		}
	}
	discardOwnMarker := func() {
		if !markerPreexisting {
			discardActiveSessionFile(app, channel, activeSessionFile)
		}
	}
	workspaceCwd := ""
	if resume == "" {
		var rawCwd *string
		if v, present := query.Lookup("cwd"); present {
			rawCwd = &v
		}
		cwd, err := resolveChatCwd(rawCwd)
		if err != nil {
			if _, isHTTP := asHTTPException(err); isHTTP {
				return ptyFail(ctx, ws, err, "Chat")
			}
			return err
		}
		if cwd != nil {
			workspaceCwd = *cwd
		}
	}
	argv, cwd, env, err := chatResolveArgv(ctx, app, resume, sidecarURL, profile, activeSessionFile, workspaceCwd)
	if err != nil {
		var install *pm.InstallError
		_, isHTTP := asHTTPException(err)
		switch {
		case isHTTP, pyrepr.TypeName(err) == "SystemExit":
			discardOwnMarker()
			return ptyFail(ctx, ws, err, "Chat")
		case errors.As(err, &install):
			return ptyFail(ctx, ws, err, "Chat")
		}
		return err
	}
	rawAttachToken, hasAttach := query.Get("attach"), query.Get("attach") != ""
	attachToken := rawAttachToken
	registryResume := rawResume
	if rawResume != "" && len(env) > 0 {
		if target := env["ICEBATH_TUI_RESUME"]; target != "" {
			registryResume = target
		}
	}
	if hasAttach && (registryResume != "" || profile != "") {
		attachToken = attachToken + "\x00" + profile + "\x00" + registryResume
	}
	spawn := func() (tools.TerminalBridge, error) {
		return chatSpawnBridge(ctx, nil, argv, tools.PtySpawnOptions{Cwd: cwd, Env: env})
	}
	if !hasAttach {
		bridge, err := spawn()
		if err != nil {
			var unavailable *tools.PtyUnavailableError
			switch {
			case errors.As(err, &unavailable):
				return ptyFail(ctx, ws, err, "Chat")
			case pypath.IsOSError(err):
				discardOwnMarker()
				return ptyFail(ctx, ws, err, "Chat")
			}
			return err
		}
		defer discardOwnMarker()
		return legacyPump(ctx, ws, bridge)
	}
	session, created, err := func() (*tools.PtySession, bool, error) {
		if err := ptyRegistry.CloseOtherSessions(ctx, rawAttachToken, attachToken); err != nil {
			return nil, false, err
		}
		holderPid := leaseHolderPid(ctx, registryResume, env["ICEBATH_HOME"])
		if err := ptyRegistry.CloseOrphanedSessions(ctx, registryResume, attachToken, holderPid); err != nil {
			return nil, false, err
		}
		return ptyRegistry.AttachOrSpawn(ctx, attachToken, spawn, activeSessionFile)
	}()
	if err != nil {
		if ptyStartRefused(err) {
			discardOwnMarker()
			return ptyFail(ctx, ws, err, "Chat")
		}
		return err
	}
	if created {
		if channel != "" && activeSessionFile != "" {
			session.SetActiveSessionCleanup(func() { discardActiveSessionFile(app, channel, activeSessionFile) })
		}
	} else {
		discardOwnMarker()
	}
	attached, err := session.Attach(ctx, ws, tools.PtyAttachOptions{ForceRedraw: !created})
	if err != nil {
		return err
	}
	if !attached {
		if session.Viewer() == tools.PtyViewer(ws) {
			closeStalledPtyInput(ctx, ws, "keepalive-redraw")
		}
		ptyRegistry.Detach(attachToken, ws)
		return nil
	}
	defer ptyRegistry.Detach(attachToken, ws)
	err = pumpKeepaliveInput(ctx, ws, session)
	var disconnect *WebSocketDisconnect
	if errors.As(err, &disconnect) {
		return nil
	}
	return err
}
