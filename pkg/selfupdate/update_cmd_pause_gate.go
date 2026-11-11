package selfupdate

import (
	"context"
	"reflect"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Baseline dicts of a pause token as one list, a typed list read the same as a decoded one
func tokenBaselines(token map[string]any) []any {
	switch list := token["baselines"].(type) {
	case []any:
		return list
	case []map[string]any:
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out
	}
	return nil
}

// Sorted distinct strings of a recorded list joined with newly bound ones
func mergedSorted(existing any, bound []string) []any {
	set := map[string]bool{}
	if list, ok := existing.([]any); ok {
		for _, item := range list {
			set[pyrepr.Str(item)] = true
		}
	}
	for _, item := range bound {
		set[item] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

// Token copy marked resume needed, the record every gate write persists
func resumeNeeded(token map[string]any) map[string]any {
	out := make(map[string]any, len(token)+1)
	for key, value := range token {
		out[key] = value
	}
	out["resume_needed"] = true
	return out
}

// Binds one git step to the pause record before git writes, returning its step record and settle call
func checkoutMove(ctx context.Context, token map[string]any, bind checkoutMoveBind) (map[string]any, func() error, error) {
	step := map[string]any{}
	distinct := map[string]bool{}
	for _, target := range bind.Targets {
		if target != "" {
			distinct[target] = true
		}
	}
	targets := make([]string, 0, len(distinct))
	for target := range distinct {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	pauseID := token["pause_id"]
	var root string
	var found map[string]any
	if pyrepr.Truthy(pauseID) && (len(targets) > 0 || len(bind.Paths) > 0 || bind.Revert) {
		root = updaterecord.PauseInstallRoot()
		if root != "" {
			found = updaterecord.TreeState(ctx, root)
		}
	}
	if found == nil {
		return step, func() error { return nil }, nil
	}
	paths := append([]string{}, bind.Paths...)
	if dirty, ok := found["dirty_at_pause"].([]any); ok && bind.Revert {
		for _, item := range dirty {
			paths = append(paths, pyrepr.Str(item))
		}
	}
	baselines := tokenBaselines(token)
	token["baselines"] = baselines
	moveID := pyrepr.Str(pauseID) + "@" + pyrepr.Str(found["pre_sha"])
	var baseline map[string]any
	for _, item := range baselines {
		candidate, ok := item.(map[string]any)
		if ok && pyrepr.Equal(candidate["pre_sha"], found["pre_sha"]) &&
			(pyrepr.Equal(candidate["pause_id"], pauseID) || pyrepr.Equal(candidate["pause_id"], moveID)) {
			baseline = candidate
			break
		}
	}
	if baseline == nil {
		baseline = map[string]any{"pause_id": moveID}
		for key, value := range found {
			baseline[key] = value
		}
		token["baselines"] = append(baselines, baseline)
	}
	if len(targets) > 0 {
		baseline["move_targets"] = mergedSorted(baseline["move_targets"], targets)
	}
	if len(paths) > 0 {
		baseline["move_paths"] = mergedSorted(baseline["move_paths"], paths)
	}
	if err := updaterecord.WritePause(ctx, resumeNeeded(token), nil, ""); err != nil {
		return nil, nil, err
	}
	return step, func() error { return settleCheckoutMove(ctx, token, baseline, found, root, step) }, nil
}

// The tree gate bound to token, the checkout mover the git and stash helpers take
func movesFor(token map[string]any) checkoutMoveFunc {
	return func(ctx context.Context, bind checkoutMoveBind) (map[string]any, func() error, error) {
		return checkoutMove(ctx, token, bind)
	}
}

// Records what a returned step left, a moved HEAD judged now and a whole step's own baseline retired
func settleCheckoutMove(ctx context.Context, token, baseline, found map[string]any, root string, step map[string]any) error {
	now := updaterecord.TreeState(ctx, root)
	if now == nil {
		return &pm.PyError{Class: "TypeError", Message: "'NoneType' object is not subscriptable"}
	}
	moved := !pyrepr.Equal(now["pre_sha"], found["pre_sha"])
	var torn []string
	tornKnown := false
	if moved {
		var changes []string
		dirty, changesKnown := now["dirty_at_pause"].([]any)
		for _, item := range dirty {
			changes = append(changes, pyrepr.Str(item))
		}
		var err error
		torn, tornKnown, err = updaterecord.TornByMove(ctx, root, found, changes, changesKnown)
		if err != nil {
			return err
		}
	}
	if moved && tornKnown {
		landed, ok := baseline["landed"].(map[string]any)
		if !ok {
			landed = map[string]any{}
			baseline["landed"] = landed
		}
		names := make([]any, len(torn))
		for i, name := range torn {
			names[i] = name
		}
		head := "null"
		if now["pre_sha"] != nil {
			head = pyrepr.Str(now["pre_sha"])
		}
		landed[head] = names
	}
	whole := tornKnown && len(torn) == 0
	if !moved {
		whole = pyrepr.Truthy(step["whole"])
		if !whole {
			whole = true
			for key, value := range found {
				if !pyrepr.Equal(now[key], value) {
					whole = false
					break
				}
			}
		}
	}
	if !pyrepr.Equal(baseline["pause_id"], token["pause_id"]) && whole {
		kept := []any{}
		own := reflect.ValueOf(baseline).Pointer()
		for _, item := range tokenBaselines(token) {
			if candidate, ok := item.(map[string]any); ok && reflect.ValueOf(candidate).Pointer() == own {
				continue
			}
			kept = append(kept, item)
		}
		token["baselines"] = kept
	}
	return updaterecord.WritePause(ctx, resumeNeeded(token), nil, "")
}
