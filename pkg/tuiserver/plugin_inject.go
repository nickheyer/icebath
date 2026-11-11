package tuiserver

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Identity of the process owned TUI host, a later owner's replacement surviving this owner's clear
var tuiInjectOwner = &struct{ name string }{"tui-inject"}

var tuiInjectAtExit sync.Once

// Queues content on the live session whose session key matches, draining an idle one so the prompt starts a turn
func injectTUISessionMessage(kwargs map[string]any) bool {
	sessionKey, _ := kwargs["session_key"].(string)
	if sessionKey == "" {
		return false
	}
	content, isText := kwargs["content"].(string)
	if !isText || pystr.Strip(content) == "" {
		return false
	}
	var sid string
	var session *tuigateway.Session
	sessionsLock.Lock()
	for _, live := range tuigateway.LiveSessions() {
		if key, _ := live.Session.Get("session_key").(string); key == sessionKey {
			sid, session = live.SID, live.Session
			break
		}
	}
	sessionsLock.Unlock()
	if session == nil {
		return false
	}
	if truthy(session.Get("lazy")) || truthy(session.Get("_closing")) || truthy(session.Get("_finalized")) {
		return false
	}
	if session.Get("history_lock") == nil {
		return false
	}
	ctx := context.Background()
	unlock := lockHistory(session)
	var keepTransport any
	if queued, isMap := session.Get("queued_prompt").(map[string]any); isMap {
		keepTransport = queued["transport"]
	}
	running := truthy(session.Get("running"))
	enqueuePrompt(ctx, session, content, keepTransport, nil)
	session.Set("last_active", nowSeconds())
	unlock()
	if running {
		return true
	}
	rid := "inject-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	go drainQueuedPrompt(ctx, rid, sid, session)
	return true
}

// Publishes the TUI host on manager, or process wide and on the active profile's manager when manager is nil
func InstallTUIMessageInjector(ctx context.Context, manager *plugins.Manager) {
	if manager == nil {
		plugins.PublishTUIMessageHost(tuiInjectOwner, injectTUISessionMessage)
		manager = plugins.GetPluginManager(ctx)
		tuiInjectAtExit.Do(func() {
			registerAtExit(func() { ClearTUIMessageInjector(context.Background(), nil) })
		})
	}
	manager.SetTUIMessageInjector(tuiInjectOwner, injectTUISessionMessage)
}

// Drops this process's TUI host, leaving a different owner in place
func ClearTUIMessageInjector(ctx context.Context, manager *plugins.Manager) {
	if manager == nil {
		plugins.ClearPublishedTUIMessageHost(tuiInjectOwner)
		manager = plugins.GetPluginManager(ctx)
	}
	manager.ClearTUIMessageInjector(tuiInjectOwner)
}
