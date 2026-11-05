package config

import (
	"context"
	"errors"
	"reflect"

	"github.com/nickheyer/icebath/pkg/home"
)

type effectiveEntry struct {
	sig        []any
	data       map[string]any
	env        map[string]*string
	managedEnv map[string]*string
}

var (
	effectiveCache  = map[string]effectiveEntry{}
	lastGoodUserRaw = map[string]map[string]any{}
)

// User layer with refs expanded, the managed overlay applied and model keys canonical, no defaults merged
func effectiveOf(raw map[string]any, lookup Lookup) (map[string]any, error) {
	expanded, _ := Expand(raw, lookup).(map[string]any)
	if expanded == nil {
		expanded = map[string]any{}
	}
	merged := ApplyManagedOverlay(expanded)
	if merged == nil {
		merged = map[string]any{}
	}
	return Canonicalize(merged)
}

// Last good raw user mapping after a parse failure, this process first, then the newest good backup, else empty
func recoverUserRaw(configPath string, readErr error) (map[string]any, error) {
	loadCacheMu.Lock()
	raw := lastGoodUserRaw[configPath]
	loadCacheMu.Unlock()
	fallback := "last-known-good"
	if raw == nil {
		var err error
		if raw, err = loadNewestGoodBackupFn(configPath); err != nil {
			return nil, err
		}
		fallback = "last-known-good-backup"
	}
	if raw == nil {
		return map[string]any{}, WarnConfigParseFailure(configPath, readErr, "defaults")
	}
	if err := WarnConfigParseFailure(configPath, readErr, fallback); err != nil {
		return nil, err
	}
	return Clone(raw).(map[string]any), nil
}

// Effective user layer of a config path, a broken file served from the last good parse
func LoadUserEffective(configPath string, lookup Lookup) (map[string]any, error) {
	return loadUserEffective(context.Background(), configPath, lookup, false)
}

// Effective user layer of a config path, a broken file returning its parse error
func LoadUserEffectiveFailClosed(configPath string, lookup Lookup) (map[string]any, error) {
	return loadUserEffective(context.Background(), configPath, lookup, true)
}

// Cached on the user and managed signatures and every referenced env value, a fresh copy each call
func loadUserEffective(ctx context.Context, configPath string, lookup Lookup, failClosed bool) (map[string]any, error) {
	if snap := WorkerConfigSnapshot(); snap != nil {
		return effectiveOf(snap, lookup)
	}
	if configPath == "" {
		configPath = home.ConfigPath(home.Dir(ctx))
	}
	ctx, unlock := LockConfig(ctx)
	defer unlock()
	userSig, cacheSig, err := loadCacheSig(ctx, configPath)
	if err != nil {
		return nil, err
	}
	loadCacheMu.Lock()
	cached, hit := effectiveCache[configPath]
	loadCacheMu.Unlock()
	if hit && cacheSig != nil && reflect.DeepEqual(cached.sig, cacheSig) &&
		SnapshotCurrent(cached.env, lookup) && SnapshotCurrent(cached.managedEnv, processLookup) {
		return Clone(cached.data).(map[string]any), nil
	}
	raw := map[string]any{}
	recovered := false
	if userSig != nil {
		loadCacheMu.Lock()
		rawHit, ok := rawCache[configPath]
		loadCacheMu.Unlock()
		if ok && reflect.DeepEqual(rawHit.sig, userSig) {
			raw = Clone(rawHit.data).(map[string]any)
			loadCacheMu.Lock()
			if _, seen := lastGoodUserRaw[configPath]; !seen {
				lastGoodUserRaw[configPath] = Clone(raw).(map[string]any)
			}
			loadCacheMu.Unlock()
		} else if loaded, rerr := ReadConfigDoc(ctx, configPath); rerr != nil {
			var exit SystemExitError
			if failClosed || errors.As(rerr, &exit) {
				return nil, rerr
			}
			if raw, err = recoverUserRaw(configPath, rerr); err != nil {
				return nil, err
			}
			recovered = true
		} else {
			if doc, isMap := loaded.(map[string]any); isMap {
				raw = doc
			}
			loadCacheMu.Lock()
			rawCache[configPath] = rawEntry{sig: userSig, data: Clone(raw).(map[string]any)}
			lastGoodUserRaw[configPath] = Clone(raw).(map[string]any)
			loadCacheMu.Unlock()
			if configPath == home.ConfigPath(home.Dir(ctx)) {
				if _, err := BackupConfig(configPath, "good", DefaultBackupKeep); err != nil {
					return nil, err
				}
			}
		}
	}
	env := RefSnapshot(raw, lookup)
	managedEnv := RefSnapshot(LoadManagedConfig(), processLookup)
	effective, err := effectiveOf(raw, lookup)
	if err != nil {
		return nil, err
	}
	loadCacheMu.Lock()
	if cacheSig != nil && !recovered {
		effectiveCache[configPath] = effectiveEntry{sig: cacheSig, data: Clone(effective).(map[string]any), env: env, managedEnv: managedEnv}
	} else {
		delete(effectiveCache, configPath)
	}
	loadCacheMu.Unlock()
	return effective, nil
}
