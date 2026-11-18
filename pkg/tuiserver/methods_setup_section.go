package tuiserver

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	consentEnabledKey = "telemetry.shared_metrics.enabled"
	consentSendKey    = "telemetry.shared_metrics.send"
)

var setupSectionRegistry = &handlerRegistry{}

func init() {
	setupSectionRegistry.scopedMethod("setup.section.load", catch(5040, setupSectionLoad))
	setupSectionRegistry.scopedMethod("setup.section.save", catch(5041, setupSectionSave))
	setupSectionRegistry.install()
}

// Strings of a wire list in either slice type a transport delivers
func wireStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = strOr(item)
		}
		return out
	}
	return nil
}

// Objects of a wire list in either slice type a transport delivers
func wireMaps(v any) []map[string]any {
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, len(list))
		for i, item := range list {
			out[i], _ = item.(map[string]any)
		}
		return out
	}
	return nil
}

// Value of one write in the Go type its kind names, false when the value does not fit the kind
func sectionWriteValue(kind string, value any) (any, bool) {
	if number, isNumber := value.(json.Number); isNumber {
		parsed, err := number.Float64()
		if err != nil {
			return nil, false
		}
		value = parsed
	}
	switch kind {
	case "bool":
		answer, isBool := value.(bool)
		return answer, isBool
	case "whole":
		switch n := value.(type) {
		case int:
			return n, true
		case int64:
			return int(n), true
		case float64:
			return int(n), n == math.Trunc(n)
		}
		return nil, false
	case "decimal":
		switch n := value.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		}
		return nil, false
	case "text":
		text, isText := value.(string)
		return text, isText
	}
	return nil, false
}

// Whether the dotted key holds a non null value in the config tree
func configHolds(cfg map[string]any, key string) (any, bool) {
	value, found := config.CfgGet(cfg, strings.Split(key, ".")...)
	return value, found && value != nil
}

// Values the merged config and .env hold for the asked keys, secret keys by name only, and the asked facts
func setupSectionLoad(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	values, env, secrets := map[string]any{}, map[string]any{}, []any{}
	for _, key := range wireStrings(params["config_keys"]) {
		if value, held := configHolds(cfg, key); held {
			values[key] = value
		}
	}
	for _, key := range wireStrings(params["secret_config_keys"]) {
		if value, held := configHolds(cfg, key); held && value != "" {
			secrets = append(secrets, key)
		}
	}
	for _, key := range wireStrings(params["env_keys"]) {
		value, found, err := config.GetEnvValue(ctx, h, key)
		if err != nil {
			return nil, err
		}
		if found {
			env[key] = value
		}
	}
	for _, key := range wireStrings(params["secret_env_keys"]) {
		value, found, err := config.GetEnvValue(ctx, h, key)
		if err != nil {
			return nil, err
		}
		if found && value != "" {
			secrets = append(secrets, key)
		}
	}
	facts := map[string]any{}
	for _, name := range wireStrings(params["facts"]) {
		holds, err := setupFact(ctx, h, cfg, name)
		if err != nil {
			return nil, err
		}
		facts[name] = holds
	}
	probed, known, err := loadSetupValues(ctx, h, cfg, wireStrings(params["values"]))
	if err != nil {
		return nil, err
	}
	if !known {
		return rpcErr(rid, 4001, "unknown setup value in "+strings.Join(wireStrings(params["values"]), ", "), nil), nil
	}
	return ok(rid, map[string]any{"config": values, "env": env, "secrets": secrets, "facts": facts, "values": probed}), nil
}

// Writes the answers of one setup section, consent keys through the consent writer, and returns what the files hold
func setupSectionSave(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	h := home.Dir(ctx)
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — the configuration is read-only", nil), nil
	}
	sets, unsets, written := []config.KeyValue{}, []string{}, []string{}
	envSets, envUnsets := [][2]string{}, []string{}
	consent := map[string]bool{}
	for _, write := range wireMaps(params["writes"]) {
		key, kind := strOr(write["key"]), strOr(write["kind"])
		if kind == "held" && strOr(write["target"]) == "env" && key != "" {
			held, _, err := config.GetEnvValue(ctx, h, key)
			if err != nil {
				return nil, err
			}
			if held != "" {
				envSets = append(envSets, [2]string{key, held})
			}
			continue
		}
		value, fits := sectionWriteValue(kind, write["value"])
		if key == "" || !fits {
			return rpcErr(rid, 4001, "write "+key+" does not hold a "+kind+" value", nil), nil
		}
		answer, isBool := value.(bool)
		text, isText := value.(string)
		switch {
		case strOr(write["target"]) == "env" && isText:
			envSets = append(envSets, [2]string{key, text})
		case strOr(write["target"]) == "env":
			return rpcErr(rid, 4001, "env key "+key+" takes a text value", nil), nil
		case (key == consentEnabledKey || key == consentSendKey) && isBool:
			consent[key] = answer
			written = append(written, key)
		case key == consentEnabledKey || key == consentSendKey:
			return rpcErr(rid, 4001, key+" takes a bool value", nil), nil
		default:
			sets = append(sets, config.KeyValue{Key: key, Value: value})
			written = append(written, key)
		}
	}
	for _, removal := range wireMaps(params["removals"]) {
		if strOr(removal["target"]) == "env" {
			envUnsets = append(envUnsets, strOr(removal["key"]))
		} else {
			unsets = append(unsets, strOr(removal["key"]))
		}
	}
	if len(consent) == 1 {
		return rpcErr(rid, 4001, consentEnabledKey+" and "+consentSendKey+" are saved together", nil), nil
	}
	locked, unlock := provider.LockConfigMutation(ctx)
	defer unlock()
	before, err := loadCfgRaw(locked)
	if err != nil {
		return nil, err
	}
	if len(consent) == 2 {
		if err := observability.SaveConsent(locked, consent[consentEnabledKey], consent[consentSendKey], nil); err != nil {
			return nil, err
		}
	}
	fullSave := params["full_save"] == true
	switch {
	case fullSave:
		if err := fullSetupSave(locked, h, sets, unsets); err != nil {
			return nil, err
		}
	case len(sets)+len(unsets) > 0:
		if err := writeConfigChanges(locked, sets, unsets); err != nil {
			return nil, err
		}
	}
	saved, removed := []any{}, []any{}
	for _, pair := range envSets {
		if err := config.SaveEnvValueContext(locked, h, pair[0], pair[1]); err != nil {
			return nil, err
		}
		saved = append(saved, pair[0])
	}
	for _, key := range unsets {
		if _, held := config.CfgGet(before, strings.Split(key, ".")...); held {
			removed = append(removed, key)
		}
	}
	for _, key := range envUnsets {
		gone, err := config.RemoveHomeEnvValue(locked, h, key)
		if err != nil {
			return nil, err
		}
		if gone {
			removed = append(removed, key)
		}
	}
	dropCfgCache()
	var after map[string]any
	if fullSave {
		after, err = config.LoadContext(locked, h, nil)
	} else {
		after, err = loadCfgRaw(locked)
	}
	if err != nil {
		return nil, err
	}
	held := map[string]any{}
	for _, key := range written {
		if value, found := config.CfgGet(after, strings.Split(key, ".")...); found {
			held[key] = value
		}
	}
	return ok(rid, map[string]any{"config": held, "env": saved, "removed": removed}), nil
}
