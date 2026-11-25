package arena

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// Settings of the platform, read from the arena mapping of the display settings
type ArenaConfig struct {
	Display string
	Glyphs  string
	World   string
	Camera  string
	FPS     int
	Quality string
	Labels  bool
}

// Size of the crew the harness runs at once
type TeamConfig struct {
	Builders int
}

// Everything the arena takes from settings and the harness
type Config struct {
	Arena ArenaConfig
	Team  TeamConfig
}

// Outfit of a member, a dye name and the rig on its head
type Look struct {
	Color string
	Hat   string
}

// Values every arena setting accepts
var (
	displays  = []string{"auto", "glyphs", "image"}
	glyphSets = []string{"shapes", "half", "quadrant", "sextant", "octant", "legacy", "all"}
	worlds    = []string{"blackwall", "den", "nightcity", "soulkiller", "off"}
	cameras   = []string{"overview", "orbit", "follow", "cinematic"}
	qualities = []string{"low", "medium", "high", "ultra"}
)

// Settings the arena starts from, the concurrency of a stock harness for the team
func Default() Config {
	return Config{
		Arena: ArenaConfig{Display: "auto", Glyphs: "shapes", World: "blackwall", Camera: "overview", FPS: 30, Quality: "high", Labels: true},
		Team:  TeamConfig{Builders: 3},
	}
}

// Settings of an arena mapping over the defaults, each bad value named in the error and left default
func Parse(raw any) (Config, error) {
	cfg := Default()
	if raw == nil {
		return cfg, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return cfg, fmt.Errorf("arena is %s, it must be a mapping", errchain.Kind(raw))
	}
	var errs []error
	word := func(key string, allowed []string, dst *string) {
		v, has := m[key]
		if !has || v == nil {
			return
		}
		s, isStr := v.(string)
		s = strings.ToLower(strings.TrimSpace(s))
		if !isStr || !slices.Contains(allowed, s) {
			errs = append(errs, fmt.Errorf("arena.%s is %s, it must be one of %s", key, settingText(v), strings.Join(allowed, ", ")))
			return
		}
		*dst = s
	}
	word("display", displays, &cfg.Arena.Display)
	word("glyphs", glyphSets, &cfg.Arena.Glyphs)
	word("world", worlds, &cfg.Arena.World)
	word("camera", cameras, &cfg.Arena.Camera)
	word("quality", qualities, &cfg.Arena.Quality)
	if v, has := m["fps"]; has && v != nil {
		if n, ok := wholeNumber(v); ok && n >= 5 && n <= 120 {
			cfg.Arena.FPS = n
		} else {
			errs = append(errs, fmt.Errorf("arena.fps is %s, it must be a whole number from 5 to 120", settingText(v)))
		}
	}
	if v, has := m["labels"]; has && v != nil {
		if b, isBool := v.(bool); isBool {
			cfg.Arena.Labels = b
		} else {
			errs = append(errs, fmt.Errorf("arena.labels is %s, it must be true or false", settingText(v)))
		}
	}
	for key := range m {
		if !slices.Contains([]string{"display", "glyphs", "world", "camera", "quality", "fps", "labels"}, key) {
			errs = append(errs, fmt.Errorf("arena.%s is not a setting", key))
		}
	}
	return cfg, errors.Join(errs...)
}

// A setting value as an error names it: text, numbers and true or false as written, a list or a mapping by its kind
func settingText(v any) string {
	switch v.(type) {
	case string, bool, int, int64, uint64, float64:
		return errchain.Show(v)
	}
	return errchain.Kind(v)
}

// Whole number of a setting value as YAML or JSON decodes it
func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), n <= math.MaxInt32
	case float64:
		return int(n), n == math.Trunc(n) && math.Abs(n) <= math.MaxInt32
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// Dyes the crew wears in order of arrival, the director and the specialists keeping their own
var crewDyes = []string{"amber", "lime", "coral", "sky", "rose", "mint", "gold", "crimson"}

// Outfit of a member, the dye by its place in the crew and the rig by its role
func (c Config) LookOf(role Role, index int) Look {
	switch role {
	case RoleDirector:
		return Look{Color: "teal", Hat: "visor"}
	case RoleArchitect:
		return Look{Color: "violet", Hat: "antenna"}
	case RoleReviewer:
		return Look{Color: "indigo", Hat: "hood"}
	}
	i := max(index, 0)
	return Look{Color: crewDyes[i%len(crewDyes)], Hat: RigNames[i%len(RigNames)]}
}

// Handles the crew answers to, the director first and the rest in order of arrival
var crewNames = []string{"Ada", "Linus", "Grace", "Ken", "Alan", "Barbara", "Dennis", "Margaret", "Edsger", "Frances", "Donald", "Radia", "Bjarne", "Hedy", "Claude", "Katherine", "Niklaus", "Sophie", "Lovelace", "Tim"}

// Name of the director
const directorName = "Kay"

// Name of a member by its place in the crew, numbered once the handles run out
func (c Config) NameOf(role Role, index int) string {
	if role == RoleDirector {
		return directorName
	}
	i := max(index, 0)
	name := crewNames[i%len(crewNames)]
	if lap := i / len(crewNames); lap > 0 {
		name += " " + strconv.Itoa(lap+1)
	}
	return name
}
