package sprint

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Extension name, workflow name and the prefix of every setting and kind
const Name = "sprint"

// Seat on the team
type Role string

const (
	RoleDirector  Role = "director"
	RoleArchitect Role = "architect"
	RoleBuilder   Role = "builder"
	RoleReviewer  Role = "reviewer"
)

// Every role in the order the sprint brings them in
var Roles = []Role{RoleDirector, RoleArchitect, RoleBuilder, RoleReviewer}

// Endpoint, limits and spend of one role
type RoleConfig struct {
	Profile   string
	Model     string
	MaxTokens int
	MaxRounds int
	Budget    int
}

// Size and patience of the team
type Team struct {
	Builders   int
	Parallel   bool
	MaxRework  int
	MaxTickets int
}

// Time the sprint and each of its stages may take
type Limits struct {
	Total  time.Duration
	Brief  time.Duration
	Plan   time.Duration
	Build  time.Duration
	Review time.Duration
}

// How the arena draws
type Arena struct {
	Pixels string
	FPS    int
	Labels bool
	Camera string
}

// Outfit of one member
type Look struct {
	Color string
	Hat   string
}

// Every setting of the sprint as read from the configuration
type Config struct {
	Roles       map[Role]RoleConfig
	Team        Team
	Limits      Limits
	Interactive bool
	Arena       Arena
	Looks       map[Role][]Look
	Names       map[Role][]string
}

// Pixel modes the arena encodes cells with
var PixelModes = []string{"quadrant", "half"}

// Camera presets the arena starts on
var Cameras = []string{"overview", "follow", "orbit"}

// Colors an outfit may be dyed, by name
var ColorNames = []string{"teal", "amber", "violet", "lime", "coral", "sky", "rose", "mint", "crimson", "gold", "indigo", "slate"}

// Hats a member may wear
var HatNames = []string{"cap", "beanie", "fedora", "hardhat", "visor", "headphones", "bandana", "crown", "none"}

// Names builders take when the settings name none
var defaultBuilderNames = []string{"Ada", "Linus", "Grace", "Dennis", "Margaret", "Ken", "Barbara", "Alan"}

// Outfits builders wear when the settings dress none
var defaultBuilderLooks = []Look{{"teal", "cap"}, {"amber", "beanie"}, {"violet", "headphones"}, {"lime", "bandana"}, {"coral", "cap"}, {"sky", "beanie"}, {"rose", "headphones"}, {"mint", "bandana"}}

// Configuration every unset setting falls back to
func Default() Config {
	return Config{
		Roles:       map[Role]RoleConfig{RoleDirector: {}, RoleArchitect: {}, RoleBuilder: {}, RoleReviewer: {}},
		Team:        Team{Builders: 3, Parallel: true, MaxRework: 2, MaxTickets: 12},
		Limits:      Limits{Total: 3 * time.Hour, Brief: 45 * time.Minute, Plan: 30 * time.Minute, Build: 90 * time.Minute, Review: 20 * time.Minute},
		Interactive: true,
		Arena:       Arena{Pixels: "quadrant", FPS: 30, Labels: true, Camera: "overview"},
		Looks:       map[Role][]Look{RoleDirector: {{"crimson", "fedora"}}, RoleArchitect: {{"gold", "hardhat"}}, RoleReviewer: {{"indigo", "visor"}}, RoleBuilder: slices.Clone(defaultBuilderLooks)},
		Names:       map[Role][]string{RoleDirector: {"Director"}, RoleArchitect: {"Architect"}, RoleReviewer: {"Reviewer"}, RoleBuilder: slices.Clone(defaultBuilderNames)},
	}
}

// Setting key of a role field
func roleKey(role Role, field string) string { return Name + "." + string(role) + "." + field }

// Every setting the extension registers, the roles first, then the team, the limits, the arena and the outfits
func Settings() []harness.Setting {
	var out []harness.Setting
	for _, role := range Roles {
		r := string(role)
		out = append(out,
			harness.Setting{Key: roleKey(role, "profile"), Help: "Endpoint profile the " + r + " runs on, the default endpoint when unset", Check: checkName("profile")},
			harness.Setting{Key: roleKey(role, "model"), Help: "Model the " + r + " runs on that profile, its default when unset"},
			harness.Setting{Key: roleKey(role, "max_tokens"), Help: "Output tokens one turn of the " + r + " may produce, the app setting when unset", Lo: 1, Step: 1024, Integer: true},
			harness.Setting{Key: roleKey(role, "max_rounds"), Help: "Tool rounds one run of the " + r + " may take, the app setting when unset", Lo: 1, Step: 1, Integer: true},
			harness.Setting{Key: roleKey(role, "budget"), Help: "Tokens the " + r + " may spend over one sprint across every turn, unlimited when unset", Lo: 1000, Step: 10000, Integer: true},
		)
	}
	out = append(out,
		harness.Setting{Key: Name + ".team.builders", Help: "Builders the team brings in, each taking one ticket at a time", Lo: 1, Hi: 8, Step: 1, Integer: true},
		harness.Setting{Key: Name + ".team.parallel", Help: "Builders work at the same time rather than one after another", Choices: []string{"true", "false"}},
		harness.Setting{Key: Name + ".team.max_rework", Help: "Times a ticket may be sent back to its builder by the reviewer", Lo: 0, Hi: 6, Step: 1, Integer: true},
		harness.Setting{Key: Name + ".team.max_tickets", Help: "Tickets the architect may cut the brief into", Lo: 1, Hi: 40, Step: 1, Integer: true},
		harness.Setting{Key: Name + ".limits.total", Help: "Time the whole sprint may take before it stops, such as 3h", Check: checkDuration("total")},
		harness.Setting{Key: Name + ".limits.brief", Help: "Time the director has to settle the brief before it is told to wrap up", Check: checkDuration("brief")},
		harness.Setting{Key: Name + ".limits.plan", Help: "Time the architect has to cut the plan before it is told to wrap up", Check: checkDuration("plan")},
		harness.Setting{Key: Name + ".limits.build", Help: "Time a builder has on one ticket before it is told to hand off", Check: checkDuration("build")},
		harness.Setting{Key: Name + ".limits.review", Help: "Time the reviewer has on one handoff before it is told to decide", Check: checkDuration("review")},
		harness.Setting{Key: Name + ".interactive", Help: "The director may ask the person questions, and a stalled stage waits for the person instead of failing the sprint", Choices: []string{"true", "false"}},
		harness.Setting{Key: Name + ".arena.pixels", Help: "How the arena turns pixels into cells, quadrant doubles the width, half keeps every color exact", Choices: PixelModes},
		harness.Setting{Key: Name + ".arena.fps", Help: "Frames per second the arena redraws at while anything moves", Lo: 5, Hi: 60, Step: 5, Integer: true},
		harness.Setting{Key: Name + ".arena.labels", Help: "Names float above every member in the arena", Choices: []string{"true", "false"}},
		harness.Setting{Key: Name + ".arena.camera", Help: "Camera the arena opens with, the whole floor, the selected member or a slow orbit", Choices: Cameras},
	)
	for _, role := range Roles {
		r := string(role)
		many := role == RoleBuilder
		lookHelp := "Outfit of the " + r + " as a color and a hat, such as crimson fedora, colors are " + strings.Join(ColorNames, ", ") + " and hats are " + strings.Join(HatNames, ", ")
		nameHelp := "Name the " + r + " goes by"
		if many {
			lookHelp = "Outfits of the builders in order, comma separated, each a color and a hat such as teal cap"
			nameHelp = "Names of the builders in order, comma separated"
		}
		out = append(out,
			harness.Setting{Key: Name + ".look." + r, Help: lookHelp, Check: checkLooks(many)},
			harness.Setting{Key: Name + ".name." + r, Help: nameHelp, Check: checkNames(many)},
		)
	}
	return out
}

// Refuses a value that is not a profile name
func checkName(what string) func(string) error {
	return func(v string) error {
		if !profiles.ValidName(v) {
			return fmt.Errorf("%s must be letters, digits, dots, dashes and underscores, got %q", what, v)
		}
		return nil
	}
}

// Refuses text that is not a duration above zero
func checkDuration(what string) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a duration above 0 such as 30m, got %q", what, v)
		}
		return nil
	}
}

// Refuses an outfit list with an unknown color or hat, one outfit at most unless many are allowed
func checkLooks(many bool) func(string) error {
	return func(v string) error {
		looks, err := parseLooks(v)
		if err != nil {
			return err
		}
		if !many && len(looks) > 1 {
			return fmt.Errorf("one outfit is expected, got %d", len(looks))
		}
		return nil
	}
}

// Refuses an empty name, or several names where one is expected
func checkNames(many bool) func(string) error {
	return func(v string) error {
		names := splitList(v)
		if len(names) == 0 {
			return fmt.Errorf("a name is required")
		}
		if !many && len(names) > 1 {
			return fmt.Errorf("one name is expected, got %d", len(names))
		}
		for _, n := range names {
			if len([]rune(n)) > 16 {
				return fmt.Errorf("name %q is longer than 16 characters", n)
			}
		}
		return nil
	}
}

// Outfits from a comma separated list of color and hat pairs
func parseLooks(v string) ([]Look, error) {
	var out []Look
	for _, item := range splitList(v) {
		fields := strings.Fields(item)
		if len(fields) != 2 {
			return nil, fmt.Errorf("outfit %q must be a color and a hat such as teal cap", item)
		}
		color, hat := strings.ToLower(fields[0]), strings.ToLower(fields[1])
		if !slices.Contains(ColorNames, color) {
			return nil, fmt.Errorf("unknown color %q, colors are %s", color, strings.Join(ColorNames, ", "))
		}
		if !slices.Contains(HatNames, hat) {
			return nil, fmt.Errorf("unknown hat %q, hats are %s", hat, strings.Join(HatNames, ", "))
		}
		out = append(out, Look{Color: color, Hat: hat})
	}
	return out, nil
}

// Trimmed items of a comma separated list, blanks dropped
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Text of an outfit as the setting holds it
func (l Look) String() string { return l.Color + " " + l.Hat }

// Outfit list as the setting holds it
func looksText(looks []Look) string {
	parts := make([]string, 0, len(looks))
	for _, l := range looks {
		parts = append(parts, l.String())
	}
	return strings.Join(parts, ", ")
}

// Configuration read from the app settings, unset ones at their defaults, a value that does not parse refused
func Load(c harness.Config) (Config, error) {
	cfg := Default()
	get := func(key string) string { return strings.TrimSpace(c.Setting(key)) }
	num := func(key string, into *int) error {
		v := get(key)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be an integer, got %q", key, v)
		}
		*into = n
		return nil
	}
	flag := func(key string, into *bool) error {
		v := get(key)
		if v == "" {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s must be true or false, got %q", key, v)
		}
		*into = b
		return nil
	}
	span := func(key string, into *time.Duration) error {
		v := get(key)
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a duration above 0, got %q", key, v)
		}
		*into = d
		return nil
	}
	choice := func(key string, into *string, choices []string) error {
		v := get(key)
		if v == "" {
			return nil
		}
		if !slices.Contains(choices, v) {
			return fmt.Errorf("%s must be one of %s, got %q", key, strings.Join(choices, ", "), v)
		}
		*into = v
		return nil
	}
	for _, role := range Roles {
		r := cfg.Roles[role]
		r.Profile, r.Model = get(roleKey(role, "profile")), get(roleKey(role, "model"))
		for _, step := range []error{num(roleKey(role, "max_tokens"), &r.MaxTokens), num(roleKey(role, "max_rounds"), &r.MaxRounds), num(roleKey(role, "budget"), &r.Budget)} {
			if step != nil {
				return Config{}, step
			}
		}
		cfg.Roles[role] = r
		if v := get(Name + ".look." + string(role)); v != "" {
			looks, err := parseLooks(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", Name+".look."+string(role), err)
			}
			cfg.Looks[role] = looks
		}
		if v := get(Name + ".name." + string(role)); v != "" {
			cfg.Names[role] = splitList(v)
		}
	}
	steps := []error{
		num(Name+".team.builders", &cfg.Team.Builders),
		flag(Name+".team.parallel", &cfg.Team.Parallel),
		num(Name+".team.max_rework", &cfg.Team.MaxRework),
		num(Name+".team.max_tickets", &cfg.Team.MaxTickets),
		span(Name+".limits.total", &cfg.Limits.Total),
		span(Name+".limits.brief", &cfg.Limits.Brief),
		span(Name+".limits.plan", &cfg.Limits.Plan),
		span(Name+".limits.build", &cfg.Limits.Build),
		span(Name+".limits.review", &cfg.Limits.Review),
		flag(Name+".interactive", &cfg.Interactive),
		choice(Name+".arena.pixels", &cfg.Arena.Pixels, PixelModes),
		num(Name+".arena.fps", &cfg.Arena.FPS),
		flag(Name+".arena.labels", &cfg.Arena.Labels),
		choice(Name+".arena.camera", &cfg.Arena.Camera, Cameras),
	}
	for _, step := range steps {
		if step != nil {
			return Config{}, step
		}
	}
	if cfg.Team.Builders < 1 || cfg.Team.Builders > 8 {
		return Config{}, fmt.Errorf("%s.team.builders must be from 1 to 8, got %d", Name, cfg.Team.Builders)
	}
	if cfg.Team.MaxTickets < 1 {
		return Config{}, fmt.Errorf("%s.team.max_tickets must be at least 1, got %d", Name, cfg.Team.MaxTickets)
	}
	if cfg.Team.MaxRework < 0 {
		return Config{}, fmt.Errorf("%s.team.max_rework must not be negative, got %d", Name, cfg.Team.MaxRework)
	}
	if cfg.Arena.FPS < 5 || cfg.Arena.FPS > 60 {
		return Config{}, fmt.Errorf("%s.arena.fps must be from 5 to 60, got %d", Name, cfg.Arena.FPS)
	}
	return cfg, nil
}

// Values of every setting as the configuration holds them, empty while unset
func Values(c harness.Config) map[string]string {
	out := map[string]string{}
	for _, s := range Settings() {
		out[s.Key] = c.Setting(s.Key)
	}
	return out
}

// Name the member at an index of a role goes by, the defaults filling in past the names set
func (c Config) NameOf(role Role, index int) string {
	names := c.Names[role]
	if index < len(names) {
		return names[index]
	}
	if role == RoleBuilder {
		if index < len(defaultBuilderNames) {
			return defaultBuilderNames[index]
		}
		return "Builder " + strconv.Itoa(index+1)
	}
	return strings.ToUpper(string(role[:1])) + string(role[1:])
}

// Outfit the member at an index of a role wears, the defaults filling in past the outfits set
func (c Config) LookOf(role Role, index int) Look {
	looks := c.Looks[role]
	if index < len(looks) {
		return looks[index]
	}
	if role == RoleBuilder {
		return defaultBuilderLooks[index%len(defaultBuilderLooks)]
	}
	return Default().Looks[role][0]
}

// Kind name of a role
func KindName(role Role) string { return Name + "-" + string(role) }

// Role a kind name stands for, false for a kind of another extension
func RoleOfKind(kind string) (Role, bool) {
	rest, ok := strings.CutPrefix(kind, Name+"-")
	if !ok {
		return "", false
	}
	role := Role(rest)
	if !slices.Contains(Roles, role) {
		return "", false
	}
	return role, true
}

// Tools a role is offered, nil for every tool, and the ones withheld
func roleTools(role Role) (offered, denied []string) {
	explore := []string{"Read", "Glob", "Grep", "Outline"}
	switch role {
	case RoleDirector:
		return append(explore, ToolBrief, ToolAsk), nil
	case RoleArchitect:
		return append(explore, ToolPlan), nil
	case RoleReviewer:
		return append(explore, "Shell", "ShellOutput", "KillShell", ToolVerdict), nil
	}
	return nil, []string{"Agent", facts.ToolName, ToolBrief, ToolAsk, ToolPlan, ToolVerdict}
}

// Kind a role runs as under the configuration, its endpoint, limits and tools
func (c Config) Kind(role Role) harness.Kind {
	r := c.Roles[role]
	offered, denied := roleTools(role)
	k := harness.Kind{Name: KindName(role), Description: roleDescription(role), System: roleSystem(role, c.Team), Tools: offered, Deny: denied, MaxRounds: r.MaxRounds, MaxTokens: r.MaxTokens}
	if r.Profile != "" {
		k.Model = &harness.Target{Profile: r.Profile, Model: r.Model}
	}
	return k
}

// Kinds of every role under the configuration
func (c Config) Kinds() []harness.Kind {
	out := make([]harness.Kind, 0, len(Roles))
	for _, role := range Roles {
		out = append(out, c.Kind(role))
	}
	return out
}

// One line on what a role does
func roleDescription(role Role) string {
	switch role {
	case RoleDirector:
		return "sprint director, settles the brief with the person and reports at the end"
	case RoleArchitect:
		return "sprint architect, cuts the brief into tickets a team can build side by side"
	case RoleBuilder:
		return "sprint builder, builds one ticket at a time and hands it off"
	}
	return "sprint reviewer, checks every handoff and directs the builders"
}
