package verify

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const DefaultMaxVerifyNudges = 3

const CodingVerifyGuidanceText = "[Coding] Before you run tests/linters or call this done: if this is " +
	"creative UI/visual work, hold off on tests and linters until the user says " +
	"they like the result or you're about to commit. And before every commit, " +
	"clean your work: keep it KISS/DRY, match the surrounding code style, and be " +
	"elitist, shorthand, clever, concise, efficient, and elegant."

// Active profile config the way load_config reads it, swapped by tests
var loadConfig = func() (map[string]any, error) {
	h := home.Dir(context.Background())
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	return tree, err
}

// Bound on consecutive pre_verify continue directives per turn, never below zero
func MaxVerifyNudges(cfg map[string]any) (int, error) {
	n, err := pyint.Of(agentCfg(cfg)["max_verify_nudges"])
	if err != nil {
		if e, ok := err.(*pyint.Error); ok && (e.Kind == "TypeError" || e.Kind == "ValueError") {
			return DefaultMaxVerifyNudges, nil
		}
		return 0, err
	}
	switch {
	case n.Sign() < 0:
		return 0, nil
	case !n.IsInt64() || n.Int64() > math.MaxInt:
		return math.MaxInt, nil
	}
	return int(n.Int64()), nil
}

// Guidance appended to verification stop nudges, empty when agent.verify_guidance turns it off
func CodingVerifyGuidance(cfg map[string]any) string {
	value, ok := agentCfg(cfg)["verify_guidance"]
	if !ok {
		value = true
	}
	if !isTruthyValue(value, true) {
		return ""
	}
	return CodingVerifyGuidanceText
}

// utils.is_truthy_value, a string truthy only when it spells 1, true, yes or on
func isTruthyValue(value any, def bool) bool {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		switch pystr.Lower(pystr.Strip(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(value)
}

// agent section of the given config or the loaded one, empty when either is not a mapping
func agentCfg(cfg map[string]any) map[string]any {
	if cfg == nil {
		loaded, err := loadConfig()
		if err != nil || loaded == nil {
			loaded = map[string]any{}
		}
		cfg = loaded
	}
	switch section := cfg["agent"].(type) {
	case map[string]any:
		return section
	case *pyjson.Object:
		return section.Vals
	}
	return map[string]any{}
}
