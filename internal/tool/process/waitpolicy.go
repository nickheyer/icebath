package process

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	promoteAfter      = 2
	maxTrackedWaits   = 512
	raceWaitSeconds   = 5.0
	backgroundGateEnv = "ICEBATH_BACKGROUND_NOTIFICATIONS"
)

type waitObservation struct {
	total int
	count int
}

type waitPolicy struct {
	mu    sync.Mutex
	state map[string]waitObservation
	order []string
}

func (p *waitPolicy) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.state[id]; !ok {
		return
	}
	delete(p.state, id)
	for i, v := range p.order {
		if v == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}

func (p *waitPolicy) countUnchanged(id string, total int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	last, seen := p.state[id]
	count := 0
	if total == last.total {
		count = last.count + 1
	}
	if !seen {
		p.order = append(p.order, id)
	}
	p.state[id] = waitObservation{total: total, count: count}
	for len(p.state) > maxTrackedWaits {
		oldest := p.order[0]
		p.order = p.order[1:]
		delete(p.state, oldest)
	}
	return count
}

func stopNote(id string) string {
	return "The process is still running and has printed nothing new since your last check. icebath has turned on " +
		"notify_on_complete for it: when it exits, a message with its exit code and final output starts a new turn " +
		"automatically, even after you end this turn. Do NOT call process_manage wait or poll on " + id + " again, and do " +
		"not sleep or loop to wait for it. If you have other work, do it now; otherwise end your turn with a one-line " +
		"status and report the result when the completion message arrives."
}

func (r *Registry) autoNotifyEnabled(ctx context.Context) bool {
	c, err := r.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug("process_wait_auto_notify unreadable; using default", "err", err)
		return true
	}
	return c.Terminal.ProcessWaitAutoNotify
}

func gateEnv(ctx context.Context, scope *secret.Scope, name string) (string, error) {
	if scope != nil && scope.Multiplexed(ctx) {
		v, ok, err := scope.Lookup(ctx, name)
		if err != nil || !ok {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

func (r *Registry) asyncDelivery(c *tool.Call) bool {
	return c.Origin.KanbanTask == "" && !r.role.Worker() && c.Origin.AsyncDelivery
}

func (r *Registry) canDeliverCompletion(ctx context.Context, c *tool.Call) bool {
	if c.Presence.SingleQuery {
		return false
	}
	if !r.asyncDelivery(c) || c.Origin.DelegatedChild {
		return false
	}
	gate, err := gateEnv(ctx, c.Secrets, backgroundGateEnv)
	if err != nil {
		slog.Debug("background_process_notifications unreadable; assuming on", "err", err)
		return true
	}
	cfg, err := r.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug("background_process_notifications unreadable; assuming on", "err", err)
		return true
	}
	if strings.EqualFold(gate, "off") {
		return false
	}
	mode := cfg.Display.BackgroundProcessNotifications
	if v, ok := mode.AsBool(); ok && !v {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(mode.String()), "off")
}

func (r *Registry) callerReceivesCompletion(ctx context.Context, c *tool.Call, s *Session) bool {
	s.mu.Lock()
	ev := Event{Type: EventCompletion, SessionID: s.id, SessionKey: s.sessionKey, TaskID: s.taskID, OwnerTaskID: s.ownerTaskID}
	s.mu.Unlock()
	if ev.SessionKey != "" && c.SessionKey == "" {
		return false
	}
	return OwnsEvent(ev, c.SessionKey, nil) && r.ShouldSurface(ctx, ev)
}

func (r *Registry) claimNotifyIfRunning(s *Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, running := r.running.get(s.id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishClaimed || !running {
		return false
	}
	s.notify = true
	return true
}

func (r *Registry) promote(ctx context.Context, c *tool.Call, s *Session) {
	s.mu.Lock()
	if s.route.Platform == "" && c.Origin.Platform != "" {
		s.route = Route{
			Platform:  c.Origin.Platform,
			ChatID:    c.Origin.ChatID,
			UserID:    c.Origin.UserID,
			UserName:  c.Origin.UserName,
			ThreadID:  c.Origin.ThreadID,
			MessageID: c.Origin.MessageID,
		}
		s.parentSessionID = c.SessionID
	}
	platform, interval, key := s.route.Platform, s.watcherInterval, s.sessionKey
	s.mu.Unlock()
	if platform != "" && interval == 0 {
		r.RegisterWatcher(s, key)
	}
	r.writeCheckpoint(ctx, nil)
	slog.Info("repeated wait/poll with no new output; auto-enabled notify_on_complete", "process", s.id)
}

func (r *Registry) applyWaitPolicy(ctx context.Context, c *tool.Call, action, id string, res *Reply) (*Reply, error) {
	if !r.autoNotifyEnabled(ctx) {
		return res, nil
	}
	caller := CallerOf(c)
	s := r.Get(ctx, caller, id)
	if s == nil {
		return res, nil
	}
	running := action == "wait" && res.Status() == "timeout" || action == "poll" && res.Status() == "running"
	if !running {
		if action != "wait" && action != "poll" || res.Status() != "interrupted" {
			r.policy.forget(s.id)
		}
		return res, nil
	}
	s.mu.Lock()
	exited, total := s.exited, s.out.total
	s.mu.Unlock()
	if exited || r.policy.countUnchanged(s.id, total) < promoteAfter || !r.canDeliverCompletion(ctx, c) {
		return res, nil
	}
	if !r.callerReceivesCompletion(ctx, c, s) {
		return res, nil
	}
	s.mu.Lock()
	notify := s.notify
	s.mu.Unlock()
	if !notify {
		if !r.claimNotifyIfRunning(s) {
			r.policy.forget(s.id)
			if action == "poll" {
				return r.Poll(ctx, caller, id), nil
			}
			wait := raceWaitSeconds
			return r.Wait(ctx, caller, id, &wait)
		}
		r.promote(ctx, c, s)
	}
	note := stopNote(s.id)
	res.set("notify_on_complete", true)
	if action == "wait" {
		clamp := res.str("timeout_note")
		if strings.HasPrefix(clamp, "Requested wait") {
			clamp, _, _ = strings.Cut(clamp, ". Wait window")
			clamp += ". "
		} else {
			clamp = ""
		}
		res.set("timeout_note", clamp+note)
	} else {
		res.set("note", note)
	}
	return res, nil
}
