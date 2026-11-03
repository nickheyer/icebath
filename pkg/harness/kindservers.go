package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// Name a server a kind declares inline is registered under
func kindServerName(kind Kind, server string) string {
	return "agent_" + strings.ReplaceAll(kind.Name, ":", "_") + "_" + server
}

// Connects the inline servers of a kind as an agent of it starts, named references sharing the configured ones
func (a *App) connectKindServers(ctx context.Context, kind Kind) error {
	known := map[string]mcp.State{}
	for _, st := range a.MCP.Status() {
		known[st.Name] = st.State
	}
	for _, ref := range kind.Servers {
		if ref.Inline == nil {
			if _, ok := known[ref.Name]; !ok {
				return fmt.Errorf("kind %s names server %s, which is not configured", kind.Name, ref.Name)
			}
			continue
		}
		name := kindServerName(kind, ref.Name)
		srv := *ref.Inline
		srv.Name = name
		cfg, err := serverConfig(&srv)
		if err != nil {
			return err
		}
		if state, ok := known[name]; ok {
			if state == mcp.StateConnected || state == mcp.StateConnecting {
				continue
			}
			if err := a.MCP.Connect(ctx, name); err != nil {
				return err
			}
			continue
		}
		if err := a.MCP.Add(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

// Disconnects the inline servers of a kind once its agent ends
func (a *App) disconnectKindServers(kind Kind) {
	for _, ref := range kind.Servers {
		if ref.Inline != nil {
			a.MCP.Disconnect(kindServerName(kind, ref.Name))
		}
	}
}

// Hook that closes the inline servers of a kind when its run stops or fails
type serversHook struct {
	agent.Hooks
	app  *App
	kind Kind
}

func (h *serversHook) OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	h.app.disconnectKindServers(h.kind)
	return nil, nil
}

func (h *serversHook) OnFailure(ctx context.Context, err error) {
	h.app.disconnectKindServers(h.kind)
}

// Reports whether agents of a kind run in the background even when the parent would wait
func (a *App) kindBackground(kind string) bool {
	k, ok := a.Kind(kind)
	return ok && k.Background
}

// Hands the answer of a background child to the session of the run that spawned it
func (a *App) deliverToRun(ctx context.Context, text string) {
	sess := a.SessionFor(ctx)
	a.deliver(sess, text)
	a.Notify(context.WithoutCancel(ctx), sess, "agent_completed", "A background agent finished", "")
}

// Starts the monitors of enabled plugins whose condition matches, always at session start and on a skill invoke
func (a *App) startMonitors(when string) {
	lib := a.Library()
	if lib == nil {
		return
	}
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	if a.monitors == nil {
		a.monitors = map[string]string{}
	}
	for _, m := range lib.Monitors {
		cond := m.When
		if cond == "" {
			cond = "always"
		}
		if cond != when {
			continue
		}
		key := m.Plugin.ID + "/" + m.Name
		if id, running := a.monitors[key]; running {
			if job, ok := a.Shell.Job(id); ok && job.Running {
				continue
			}
		}
		line := plugin.SubstituteVars(m.Command, m.Plugin.Vars(a.Root))
		id, err := a.Shell.StartJob("monitor:"+key, m.Description, line, 0)
		if err != nil {
			a.send(NoticeMsg{Level: Warn, Text: "monitor " + key + ": " + err.Error()})
			continue
		}
		a.monitors[key] = id
	}
}

// Stops every monitor started
func (a *App) stopMonitors(ctx context.Context) {
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	for key, id := range a.monitors {
		a.Shell.KillJob(ctx, id)
		delete(a.monitors, key)
	}
}
