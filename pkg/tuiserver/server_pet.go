package tuiserver

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/provider"
)

// display.pet of the canonical config, empty on any failure
func petCfg(ctx context.Context) (out map[string]any) {
	defer func() {
		if recover() != nil {
			out = map[string]any{}
		}
	}()
	cfg, err := loadConfig(ctx)
	if err != nil {
		return map[string]any{}
	}
	display, _ := cfg["display"].(map[string]any)
	if petConf, ok := display["pet"].(map[string]any); ok {
		return petConf
	}
	return map[string]any{}
}

// Python float of a number, bool or numeric string, a ValueError or TypeError otherwise
func pyFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case string:
		text := strings.TrimSpace(n)
		switch strings.ToLower(strings.TrimLeft(text, "+-")) {
		case "inf", "infinity":
			if strings.HasPrefix(text, "-") {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		case "nan":
			return math.NaN(), nil
		}
		valid := text != "" && !strings.ContainsAny(strings.ToLower(text), "xp")
		for i := 0; valid && i < len(text); i++ {
			if text[i] == '_' {
				valid = i > 0 && i+1 < len(text) && isASCIIDigit(text[i-1]) && isASCIIDigit(text[i+1])
			}
		}
		text = strings.ReplaceAll(text, "_", "")
		if valid {
			if f, err := strconv.ParseFloat(text, 64); err == nil || strings.Contains(err.Error(), "value out of range") {
				return f, nil
			}
		}
		return 0, fmt.Errorf("could not convert string to float: %s", pyrepr.Repr(n))
	}
	if whole, ok := pyrepr.Int(v); ok {
		f, _ := new(big.Float).SetInt(whole).Float64()
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))
}

// Configured pet selection resolver, swapped where a case installs a fake selection
var petActiveSelection = configuredPetSelection

// Configured pet selection, whether it is enabled, the resolved pet and its scale
func configuredPetSelection(ctx context.Context) (bool, *pet.InstalledPet, float64, error) {
	conf := petCfg(ctx)
	enabled := provider.IsTruthyValue(conf["enabled"])
	var active *pet.InstalledPet
	if enabled {
		slug := ""
		if value := conf["slug"]; pyrepr.Truthy(value) {
			slug = pyrepr.Str(value)
		}
		resolved, err := pet.ResolveActivePet(home.Dir(ctx), slug)
		if err != nil {
			return false, nil, 0, err
		}
		active = resolved
	}
	rawScale, present := conf["scale"]
	if !present || !pyrepr.Truthy(rawScale) {
		return enabled, active, pet.DefaultScale, nil
	}
	scale, err := pyFloat(rawScale)
	if err != nil {
		return false, nil, 0, err
	}
	return enabled, active, scale, nil
}

// Whether a byte is an ASCII decimal digit
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }
