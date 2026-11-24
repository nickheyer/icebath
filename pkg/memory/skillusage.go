package memory

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"

	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Usage sidecar of a home as plain maps, empty when the file is missing, corrupt or undecodable
func LoadSkillUsage(h string) map[string]map[string]any {
	data, err := skills.NewScope(h, nil).LoadUsage()
	out := map[string]map[string]any{}
	if err != nil {
		return out
	}
	for _, k := range data.Keys() {
		if rec, ok := plainUsageValue(data.Value(k)).(map[string]any); ok {
			out[k] = rec
		}
	}
	return out
}

// Backfilled usage record of one skill in a home, fresh defaults when it has none
func GetSkillRecord(h, name string) (map[string]any, error) {
	rec, err := skills.NewScope(h, nil).GetRecord(name)
	if err != nil {
		return nil, err
	}
	out, _ := plainUsageValue(rec).(map[string]any)
	return out, nil
}

// Moves a curation eligible skill of the env home into the archive, false with the refusal message otherwise
func ArchiveSkill(ctx context.Context, env *tools.Env, name string) (bool, string, error) {
	return skills.ScopeFromEnv(env).ArchiveSkill(ctx, name)
}

// Decoded usage value with ordered objects as maps and integers as json numbers
func plainUsageValue(v any) any {
	switch x := v.(type) {
	case *jsonutil.Object:
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			m[k] = plainUsageValue(x.Value(k))
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainUsageValue(e)
		}
		return out
	case int64:
		return json.Number(strconv.FormatInt(x, 10))
	case *big.Int:
		return json.Number(x.String())
	}
	return v
}
