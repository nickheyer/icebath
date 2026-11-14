package terminal

import (
	"context"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
)

var errArgumentsChanged = errors.New("Terminal arguments changed after approval preparation; command was not started")

type guardKey struct {
	command    string
	kind       string
	hostAccess bool
}

type batchSlot struct {
	prep     *tool.Preparation
	callID   string
	args     tool.Args
	mu       sync.Mutex
	keyed    bool
	key      guardKey
	decision *tool.Decision
}

type slotView struct {
	t    *Terminal
	s    *batchSlot
	ctx  context.Context
	call *tool.Call
}

var _ approval.BatchSlot = slotView{}

func (v slotView) Preparing() bool { return v.s.prep.Preparing() }

func (v slotView) Published() { v.s.prep.Published() }

func (v slotView) Register(withdraw func()) error {
	if err := v.s.prep.Err(); err != nil {
		return err
	}
	return v.s.prep.OnClose(withdraw)
}

func (v slotView) Consume(command, envKind string, hostAccess bool) (tool.Decision, bool) {
	s := v.s
	if s.prep.Preparing() {
		return tool.Decision{}, false
	}
	if err := s.prep.Err(); err != nil {
		return tool.Decision{Message: err.Error(), Outcome: "cancelled"}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prep.FailureSeen() && s.decision != nil {
		s.decision = nil
		return tool.Decision{}, false
	}
	if v.call == nil || v.call.ID != s.callID || !s.keyed || s.key != (guardKey{command, envKind, hostAccess}) {
		return tool.Decision{}, false
	}
	d := s.decision
	s.decision = nil
	if d == nil {
		return tool.Decision{}, false
	}
	if d.Approved && !d.UserApproved {
		if !d.SmartApproved || approval.Mode(v.ctx, v.t.o.Config, v.call.Origin) != config.ApprovalSmart {
			return tool.Decision{}, false
		}
	}
	return *d, true
}

func (t *Terminal) slotFor(ctx context.Context, c *tool.Call) *batchSlot {
	prep, ok := tool.PreparationFrom(ctx)
	if !ok {
		return nil
	}
	v, ok := t.slots.Load(prep)
	if !ok {
		return nil
	}
	return v.(*batchSlot)
}

func (t *Terminal) withSlot(ctx context.Context, c *tool.Call, s *batchSlot) context.Context {
	return approval.WithBatchSlot(ctx, slotView{t: t, s: s, ctx: ctx, call: c})
}

func (t *Terminal) prepare(ctx context.Context, c *tool.Call, args tool.Args) error {
	prep, ok := tool.PreparationFrom(ctx)
	if !ok {
		return nil
	}
	if err := prep.Err(); err != nil {
		return err
	}
	slot := &batchSlot{prep: prep, callID: c.ID, args: copyArgs(args)}
	if err := prep.OnClose(func() { t.slots.Delete(prep) }); err != nil {
		return err
	}
	t.slots.Store(prep, slot)
	cfg, err := t.o.Envs.Settings().EnvConfig(ctx)
	if err != nil {
		return err
	}
	command, ok := args["command"].(string)
	if !ok {
		return nil
	}
	if c.Approver == nil {
		return &tool.UnfilledError{Name: "tool.Approver"}
	}
	ctx = withCall(ctx, c)
	if !truthy(args["background"]) {
		stripped, err := t.trailingAmpBackground(ctx, c, command)
		if err != nil {
			return err
		}
		if stripped != nil {
			command = *stripped
		}
	}
	key := guardKey{command: command, kind: cfg.Kind, hostAccess: cfg.DockerHostAccess()}
	d := c.Approver.Command(t.withSlot(ctx, c, slot), c, tool.CommandCheck{Command: key.command, EnvKind: key.kind, HostAccess: key.hostAccess, SudoGuard: true})
	slot.mu.Lock()
	slot.keyed, slot.key, slot.decision = true, key, &d
	slot.mu.Unlock()
	return nil
}

func (t *Terminal) validatePrepared(ctx context.Context, c *tool.Call, args tool.Args) error {
	slot := t.slotFor(ctx, c)
	if slot == nil {
		return nil
	}
	if err := slot.prep.Err(); err != nil {
		return err
	}
	if !valuesEqual(map[string]any(args), map[string]any(slot.args)) {
		slot.mu.Lock()
		slot.decision = nil
		slot.mu.Unlock()
		return errArgumentsChanged
	}
	return nil
}

func copyArgs(args tool.Args) tool.Args {
	if args == nil {
		return nil
	}
	return copyValue(map[string]any(args)).(map[string]any)
}

func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyValue(e)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	}
	return v
}

func valuesEqual(a, b any) bool {
	if fa, ok := number(a); ok {
		fb, ok := number(b)
		return ok && fa == fb
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, e := range x {
			f, ok := y[k]
			if !ok || !valuesEqual(e, f) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case []string:
		y, ok := b.([]string)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return a == b
}
