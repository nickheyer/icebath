package config

// Config a save persists, a frozen worker landing only its own edits on the live file
func RebaseForSave(cfg, liveRaw map[string]any, mergeExisting bool, loadConfig func() map[string]any) map[string]any {
	if snapshot := WorkerConfigSnapshot(); snapshot != nil {
		live := liveRaw
		if len(live) == 0 {
			live = map[string]any{}
		}
		merged, _ := threeWayFrozen(live, cfg, loadConfig(), frozenForDeletion(snapshot)).(map[string]any)
		return merged
	}
	if mergeExisting && len(liveRaw) > 0 {
		return MergePartialSave(liveRaw, cfg)
	}
	return cfg
}

// Override over raw for a partial save, omitted sections kept and shared sections deep merged
func MergePartialSave(raw, override map[string]any) map[string]any {
	result := Clone(override).(map[string]any)
	for key, value := range raw {
		current, has := result[key]
		if !has {
			result[key] = Clone(value)
			continue
		}
		cm, cok := current.(map[string]any)
		vm, vok := value.(map[string]any)
		if cok && vok {
			result[key] = DeepMerge(Clone(vm).(map[string]any), cm)
		}
	}
	return result
}

// Snapshot branches that count as operator state, schema default branches dropped for a bypass worker
func frozenForDeletion(snapshot map[string]any) any {
	policy := currentSafeWorkerPolicy()
	if policy == nil || !policy.ignoreUserConfig {
		return snapshot
	}
	return schemaDefaultBranch(snapshot, map[string]any(Defaults()))
}

// Snapshot without branches whose value is still the schema default
func schemaDefaultBranch(snapshot, defaults any) any {
	sm, sok := snapshot.(map[string]any)
	dm, dok := defaults.(map[string]any)
	if !sok || !dok {
		return Clone(snapshot)
	}
	kept := map[string]any{}
	for key, value := range sm {
		def, has := dm[key]
		if !has {
			kept[key] = Clone(value)
			continue
		}
		if pyEq(value, def) {
			continue
		}
		vm, vok := value.(map[string]any)
		ddm, ddok := def.(map[string]any)
		if vok && ddok {
			if nested := schemaDefaultBranch(vm, ddm).(map[string]any); len(nested) > 0 {
				kept[key] = nested
			}
			continue
		}
		kept[key] = Clone(value)
	}
	return kept
}

// Live with the caller edits of desired relative to base applied, base standing in for the frozen snapshot
func threeWay(live, desired, base any) any {
	return threeWayFrozen(live, desired, base, base)
}

// Live with the edits applied, a branch the live file lost since base staying lost unless it came only from defaults
func threeWayFrozen(live, desired, base, frozen any) any {
	if pyEq(desired, base) {
		return Clone(live)
	}
	dm, dok := desired.(map[string]any)
	bm, bok := base.(map[string]any)
	lm, lok := live.(map[string]any)
	if dok && bok && lok {
		frozenMap, _ := frozen.(map[string]any)
		merged := Clone(lm).(map[string]any)
		for key, value := range dm {
			bv, inBase := bm[key]
			if !inBase {
				merged[key] = Clone(value)
				continue
			}
			if pyEq(value, bv) {
				continue
			}
			current, inLive := lm[key]
			_, baseMap := bv.(map[string]any)
			_, baseList := bv.([]any)
			if baseMap || baseList {
				_, curMap := current.(map[string]any)
				_, curList := current.([]any)
				if (baseMap && !curMap) || (baseList && !curList) {
					if _, inFrozen := frozenMap[key]; inLive || inFrozen {
						continue
					}
					current = Clone(bv)
				}
			}
			sub, has := frozenMap[key]
			if !has {
				sub = map[string]any{}
			}
			merged[key] = threeWayFrozen(current, value, bv, sub)
		}
		return merged
	}
	dl, dlok := desired.([]any)
	bl, blok := base.([]any)
	ll, llok := live.([]any)
	if dlok && blok && llok {
		var removed, kept []any
		for _, item := range bl {
			if !pyIn(dl, item) {
				removed = append(removed, item)
			}
		}
		for _, item := range ll {
			if !pyIn(removed, item) {
				kept = append(kept, item)
			}
		}
		out := append([]any{}, kept...)
		for _, item := range dl {
			if !pyIn(bl, item) && !pyIn(kept, item) {
				out = append(out, Clone(item))
			}
		}
		return out
	}
	return Clone(desired)
}

// Reports whether a list holds an item equal to v under Python equality
func pyIn(list []any, v any) bool {
	for _, item := range list {
		if pyEq(item, v) {
			return true
		}
	}
	return false
}
