package server

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/provider"
)

// What the session's route turned down of the reasoning its requests asked for: an effort, reasoning switched off, and whether it then
// needs the weakest effort rather than its own default
type refusals struct {
	effort  bool
	disable bool
	floor   bool
}

// Effort a request sends once the route turned reasoning down: an effort refused leaves the route's default, off refused takes the weakest
// effort where the route must reason, else its default
func (f refusals) apply(effort string) string {
	switch {
	case f.effort && effort != "" && effort != "none":
		return ""
	case f.disable && effort == "none" && f.floor:
		return floorEffort
	case f.disable && effort == "none":
		return ""
	}
	return effort
}

// Whether the failure is the route refusing the reasoning the request asked for
func reasoningMandatory(err error) bool {
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok || failure.Kind != provider.ErrInvalidRequest && failure.StatusCode != 400 && failure.StatusCode != 422 {
		return false
	}
	return failure.Reason == "reasoning_mandatory" || strings.Contains(strings.ToLower(err.Error()), "reasoning is mandatory") ||
		auxiliary.ReasoningRefused(err) || auxiliary.ReasoningRequired(err)
}

// Learns for the session what of its reasoning the route refused, so the request goes again without it, telling its windows; false when the
// failure is no such refusal or the session learned it already
func (a *Agent) refuseReasoning(r *run, route provider.Route, err error) bool {
	if !reasoningMandatory(err) {
		return false
	}
	sent := a.effort(r)
	r.mu.Lock()
	f := r.refused
	r.mu.Unlock()
	var note string
	switch {
	case sent != "" && sent != "none" && !f.effort:
		f.effort = true
		note = fmt.Sprintf("⚠ %s does not take reasoning effort %s, so this session uses the model's default. Trying again.", route.Model, sent)
		slog.Warn(fmt.Sprintf("Reasoning-effort recovery: dropping reasoning config for %s", route.Model))
	case f.disable:
		return false
	default:
		f.disable, f.floor = true, auxiliary.ReasoningRequired(err)
		note = fmt.Sprintf("⚠ %s won't turn reasoning off, so this session uses the model's default. Trying again.", route.Model)
		slog.Warn(fmt.Sprintf("Reasoning-disable recovery: dropping reasoning disable for %s", route.Model))
		if f.floor {
			note = fmt.Sprintf("⚠ %s can't turn reasoning off, so this session uses %s reasoning effort. Trying again.", route.Model, floorEffort)
			slog.Warn(fmt.Sprintf("Reasoning-disable recovery: stepping reasoning up to %s for %s", floorEffort, route.Model))
		}
	}
	r.mu.Lock()
	r.refused = f
	r.mu.Unlock()
	a.tell(r, note)
	return true
}
