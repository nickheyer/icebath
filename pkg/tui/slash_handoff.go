package tui

import (
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Waits of a handoff, unclaimed before the request is failed, claimed before the view stops waiting, and between heartbeats
const (
	handoffPendingTimeout = 60 * time.Second
	handoffRunningTimeout = 900 * time.Second
	handoffHeartbeatEvery = 30 * time.Second
	handoffPollEvery      = 500 * time.Millisecond
)

// One handoff the view polls, the session it moves and the clocks of its two phases
type handoffWatch struct {
	r                *slashRun
	sid, platform    string
	title            string
	state            string
	pendingDeadline  time.Time
	runningDeadline  time.Time
	nextHeartbeat    time.Time
	previousStatus   string
	subscriptionHeld string
}

// Text of a refusal, the detail the gateway attached first, else its message
func refusalDetail(err error) string {
	var refused *rpcError
	if errors.As(err, &refused) {
		if detail, _ := refused.Data["detail"].(string); trimJS(detail) != "" {
			return detail
		}
	}
	return rpcErrorMessage(err)
}

// Queues the session for the platform and polls until the gateway completed, failed or outlasted the wait
func (a *app) startHandoff(r *slashRun, platform string) tea.Cmd {
	if r.sid == "" {
		r.sys(tr("slashCmd.core.status.noActiveSession"))
		return nil
	}
	if a.store.ui.Busy {
		r.sys(commandText(a.ctx, "shared.agent_busy", map[string]any{"command": "/handoff"}))
		return nil
	}
	w := &handoffWatch{r: r, sid: r.sid, platform: platform, subscriptionHeld: a.life.subscriptions[r.sid]}
	return gatewayCall(a.gw, "handoff.request", map[string]any{"session_id": r.sid, "platform": platform}, func(v any, err error) tea.Cmd {
		if err != nil {
			r.sys(refusalDetail(err))
			return nil
		}
		home := jsValueString(asRPCResult(v)["home_name"])
		return a.storedTitle(r.sid, func(title string) tea.Cmd {
			w.title = title
			if w.title == "" {
				w.title = pystr.Slice(r.sid, 0, 8)
			}
			r.sys(commandText(a.ctx, "handoff.queued", map[string]any{"title": w.title, "platform": platform, "home": home}) + "\n" +
				commandText(a.ctx, "handoff.waiting", nil))
			w.state, w.pendingDeadline = "pending", time.Now().Add(handoffPendingTimeout)
			w.previousStatus, a.store.ui.Status = a.store.ui.Status, tr("slashCmd.handoff.status")
			return a.pollHandoff(w)
		})
	})
}

// Stored title of a session from the gateway, the title the view holds when the gateway names none
func (a *app) storedTitle(sid string, then func(title string) tea.Cmd) tea.Cmd {
	held := trimJS(a.store.ui.SessionTitle)
	if a.gw.canonical() {
		return gatewayCall(a.gw, "session.list", map[string]any{"limit": 200}, func(v any, err error) tea.Cmd {
			sessions, _ := asRPCResult(v)["sessions"].([]any)
			for _, raw := range sessions {
				if row, _ := raw.(map[string]any); row["id"] == sid || row["session_id"] == sid {
					if title := trimJS(jsValueString(row["title"])); title != "" {
						return then(title)
					}
				}
			}
			return then(held)
		})
	}
	return gatewayCall(a.gw, "session.title", map[string]any{"session_id": sid}, func(v any, err error) tea.Cmd {
		if title := trimJS(jsValueString(asRPCResult(v)["title"])); err == nil && title != "" {
			return then(title)
		}
		return then(held)
	})
}

// Next read of the handoff row after the poll interval
func (a *app) pollHandoff(w *handoffWatch) tea.Cmd {
	return tea.Tick(handoffPollEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			return gatewayCall(a.gw, "handoff.state", map[string]any{"session_id": w.sid}, func(v any, err error) tea.Cmd {
				current := "pending"
				if err == nil {
					if state := jsValueString(asRPCResult(v)["state"]); state != "" {
						current = state
					}
				}
				return a.advanceHandoff(w, current, jsValueString(asRPCResult(v)["error"]))
			})
		}}
	})
}

// Reports a state change of the handoff row and decides whether to keep polling
func (a *app) advanceHandoff(w *handoffWatch, current, failure string) tea.Cmd {
	now := time.Now()
	if current != w.state && current == "running" {
		w.r.sys(commandText(a.ctx, "handoff.picked_up", nil))
		w.runningDeadline, w.nextHeartbeat = now.Add(handoffRunningTimeout), now.Add(handoffHeartbeatEvery)
	}
	w.state = current
	switch current {
	case "completed":
		a.store.ui.Status = w.previousStatus
		return a.leaveHandedOffSession(w, commandText(a.ctx, "handoff.complete", map[string]any{"platform": w.platform})+"\n"+tr("slashCmd.handoff.resumeHint", w.title))
	case "failed":
		a.store.ui.Status = w.previousStatus
		if failure == "" {
			failure = commandText(a.ctx, "handoff.unknown_error", nil)
		}
		w.r.sys(commandText(a.ctx, "handoff.failed", map[string]any{"error": failure}) + "\n" + tr("slashCmd.handoff.intactRetry"))
		return nil
	case "pending":
		if now.After(w.pendingDeadline) {
			return gatewayCall(a.gw, "handoff.fail", map[string]any{"session_id": w.sid, "error": "timed out waiting for gateway"}, func(v any, err error) tea.Cmd {
				if err == nil && !truthyJS(asRPCResult(v)["failed"]) {
					if state := jsValueString(asRPCResult(v)["state"]); state != "" && state != "pending" {
						return a.advanceHandoff(w, state, "")
					}
				}
				a.store.ui.Status = w.previousStatus
				w.r.sys(commandText(a.ctx, "handoff.timed_out", nil) + "\n" + tr("slashCmd.handoff.intact"))
				return nil
			})
		}
	default:
		if now.After(w.nextHeartbeat) {
			w.r.sys(commandText(a.ctx, "handoff.still_transferring", nil))
			w.nextHeartbeat = now.Add(handoffHeartbeatEvery)
		}
		if now.After(w.runningDeadline) {
			a.store.ui.Status = w.previousStatus
			w.r.sys(commandText(a.ctx, "handoff.taking_long", nil) + "\n" + commandText(a.ctx, "handoff.check_platform", map[string]any{"platform": w.platform}) +
				"\n" + tr("slashCmd.handoff.noLongerWaiting") + "\n" + commandText(a.ctx, "handoff.retry_when_settled", nil))
			return nil
		}
	}
	return a.pollHandoff(w)
}

// Releases the handed off session without ending it and opens a fresh one showing the report
func (a *app) leaveHandedOffSession(w *handoffWatch, report string) tea.Cmd {
	viewed := a.store.ui.Sid == w.sid
	if !viewed {
		w.r.sys(report)
	}
	open := func(map[string]any) tea.Cmd {
		if !viewed {
			return nil
		}
		a.store.ui.Status = tr("slashCmd.core.clear.forgingSession")
		return a.life.startNewSession(report, "", true, nil)
	}
	if a.gw.canonical() && w.subscriptionHeld != "" {
		return a.life.detachCanonical(w.sid, w.subscriptionHeld, "", true, open)
	}
	return open(nil)
}

// Slash command that hands this session off to a messaging platform's home channel
func (a *app) handoffCommands() []slashCommand {
	return []slashCommand{
		{name: "handoff", help: "hand off this session to a messaging platform (Telegram, Discord, etc.)", usage: "<platform>",
			run: func(r *slashRun) tea.Cmd {
				platform := pystr.Lower(trimJS(r.arg))
				if platform == "" {
					r.sys(commandText(a.ctx, "handoff.usage", nil) + "\n" + commandText(a.ctx, "handoff.usage_detail_1", nil) + "\n" + tr("slashCmd.handoff.usageDetail"))
					return r.ran()
				}
				return tea.Batch(r.ran(), a.startHandoff(r, platform))
			}},
	}
}
