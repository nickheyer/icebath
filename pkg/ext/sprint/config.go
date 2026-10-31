package sprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/tool/web"
)

// Name the extension registers under and the file under the configuration directory holding its settings
const (
	Name       = "sprint"
	ConfigFile = "ext/sprint.json"
)

// Roles of a sprint team in the order the workflow brings them in
const (
	RolePM       = "pm"
	RolePlanner  = "planner"
	RoleLead     = "lead"
	RoleEngineer = "engineer"
)

var Roles = []string{RolePM, RolePlanner, RoleLead, RoleEngineer}

// Kind a role runs as, registered with the app so its sessions show the role
func KindName(role string) string { return Name + "-" + role }

// Model, limits and instructions of one role
type Role struct {
	// Endpoint profile, the default endpoint when empty
	Profile string `json:"profile,omitempty"`
	// Model on that profile, its default when empty
	Model string `json:"model,omitempty"`
	// Output tokens one turn may produce, the app setting when zero
	MaxTokens int `json:"max_tokens,omitempty"`
	// Tool rounds one run may take, the app setting when zero
	MaxRounds int `json:"max_rounds,omitempty"`
	// Tokens the role may spend over one sprint across every turn, unlimited when zero
	Budget int `json:"budget,omitempty"`
	// Text added to the instructions of the role
	Append string `json:"append,omitempty"`
	// Tools offered, the defaults of the role when empty
	Tools []string `json:"tools,omitempty"`
	// Tools withheld beyond the defaults of the role
	Deny []string `json:"deny,omitempty"`
}

// Size and pace of the sprint team
type Team struct {
	MaxEngineers int `json:"max_engineers"`
	// Tickets one engineer takes, the count of engineers following from it until the maximum
	TicketsPerEngineer float64 `json:"tickets_per_engineer"`
	// Engineers work at the same time rather than one after another
	Parallel bool `json:"parallel"`
	// Times a ticket may be sent back to its engineer, by the direction of the lead or its review
	MaxRework int `json:"max_rework"`
	// Tickets the planner may cut a story into
	MaxTickets int `json:"max_tickets"`
}

// Time budgets, the whole sprint and each of its stages
type Limits struct {
	Total  Duration `json:"total"`
	Refine Duration `json:"refine"`
	Plan   Duration `json:"plan"`
	Build  Duration `json:"build"`
	Review Duration `json:"review"`
}

// Everything the sprint workflow reads before it starts
type Config struct {
	PM       Role   `json:"pm"`
	Planner  Role   `json:"planner"`
	Lead     Role   `json:"lead"`
	Engineer Role   `json:"engineer"`
	Team     Team   `json:"team"`
	Limits   Limits `json:"limits"`
	// The project manager talks with the person before handing off, resolving the prompt alone when off
	Interactive bool `json:"interactive"`
}

// Duration written as a string such as 30m or 2h in the settings file
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Takes a duration string, or a bare number of seconds
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		v, err := time.ParseDuration(text)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(data, &secs); err != nil {
		return fmt.Errorf("duration must be a string such as 30m, got %s", data)
	}
	*d = Duration(secs * float64(time.Second))
	return nil
}

// Settings a fresh file starts from
func Default() Config {
	return Config{
		Team:        Team{MaxEngineers: 4, TicketsPerEngineer: 1, Parallel: true, MaxRework: 1, MaxTickets: 12},
		Limits:      Limits{Total: Duration(3 * time.Hour), Refine: Duration(time.Hour), Plan: Duration(30 * time.Minute), Build: Duration(90 * time.Minute), Review: Duration(30 * time.Minute)},
		Interactive: true,
	}
}

// Tools a role gets while its settings name none
func DefaultTools(role string) []string {
	readOnly := []string{files.ReadName, files.GrepName, files.GlobName, files.OutlineName}
	switch role {
	case RolePM:
		return append(readOnly, ToolResolve, ToolAsk)
	case RolePlanner:
		return append(readOnly, ToolPlan)
	}
	return nil
}

// Tools a role never gets, however its settings read
func deniedTools(role string) []string {
	switch role {
	case RoleLead:
		return []string{ToolResolve, ToolPlan, ToolAsk, ToolReport, files.WriteName, files.EditName, agent.SubagentTool}
	case RoleEngineer:
		return []string{ToolResolve, ToolPlan, ToolAsk, ToolDirect, ToolReview, agent.SubagentTool, mission.ToolName}
	case RolePM:
		return append([]string{ToolReport, ToolDirect, ToolReview}, quietTools()...)
	case RolePlanner:
		return append([]string{ToolAsk, ToolReport, ToolDirect, ToolReview}, quietTools()...)
	}
	return nil
}

// Tools that change the repository or reach outside it, kept from the roles that only read and decide
func quietTools() []string {
	return []string{files.WriteName, files.EditName, shell.ShellName, shell.OutputName, shell.KillName, web.FetchName, facts.ToolName, mission.ToolName, agent.SubagentTool}
}

// Role settings by name
func (c *Config) Role(role string) *Role {
	switch role {
	case RolePM:
		return &c.PM
	case RolePlanner:
		return &c.Planner
	case RoleLead:
		return &c.Lead
	case RoleEngineer:
		return &c.Engineer
	}
	return nil
}

// Checks the team numbers and the time budgets
func (c Config) Validate() error {
	if c.Team.MaxEngineers < 1 {
		return errors.New("team.max_engineers must be at least 1")
	}
	if !(c.Team.TicketsPerEngineer > 0) {
		return errors.New("team.tickets_per_engineer must be above 0")
	}
	if c.Team.MaxRework < 0 {
		return errors.New("team.max_rework must not be negative")
	}
	if c.Team.MaxTickets < 1 {
		return errors.New("team.max_tickets must be at least 1")
	}
	for _, l := range []struct {
		name string
		d    Duration
	}{{"total", c.Limits.Total}, {"refine", c.Limits.Refine}, {"plan", c.Limits.Plan}, {"build", c.Limits.Build}, {"review", c.Limits.Review}} {
		if l.d <= 0 {
			return fmt.Errorf("limits.%s must be above 0", l.name)
		}
	}
	for _, role := range Roles {
		r := c.Role(role)
		if r.MaxTokens < 0 || r.MaxRounds < 0 || r.Budget < 0 {
			return fmt.Errorf("%s: max_tokens, max_rounds and budget must not be negative", role)
		}
	}
	return nil
}

// Kind a role runs as, with the model, limits, tools and instructions its settings give
func (c Config) Kind(role string) harness.Kind {
	r := c.Role(role)
	k := harness.Kind{Name: KindName(role), Description: roleDescription(role), System: roleSystem(role), Append: r.Append, MaxRounds: r.MaxRounds, MaxTokens: r.MaxTokens, Source: Name}
	k.Tools = append([]string(nil), r.Tools...)
	if len(k.Tools) == 0 {
		k.Tools = DefaultTools(role)
	}
	for _, name := range append(deniedTools(role), r.Deny...) {
		if !contains(k.Tools, name) && !contains(k.Deny, name) {
			k.Deny = append(k.Deny, name)
		}
	}
	if r.Profile != "" || r.Model != "" {
		k.Model = &harness.Target{Profile: r.Profile, Model: r.Model}
	}
	return k
}

// Kinds of every role
func (c Config) Kinds() []harness.Kind {
	out := make([]harness.Kind, 0, len(Roles))
	for _, role := range Roles {
		out = append(out, c.Kind(role))
	}
	return out
}

// Every setting as text keyed by name, zero optional numbers and empty lists as empty text
func (c Config) Values() map[string]string {
	out := map[string]string{}
	for _, role := range Roles {
		r := c.Role(role)
		out[role+".profile"] = r.Profile
		out[role+".model"] = r.Model
		out[role+".max_tokens"] = optional(r.MaxTokens)
		out[role+".max_rounds"] = optional(r.MaxRounds)
		out[role+".budget"] = optional(r.Budget)
		out[role+".append"] = r.Append
		out[role+".tools"] = strings.Join(r.Tools, ", ")
		out[role+".deny"] = strings.Join(r.Deny, ", ")
	}
	out["team.max_engineers"] = strconv.Itoa(c.Team.MaxEngineers)
	out["team.tickets_per_engineer"] = strconv.FormatFloat(c.Team.TicketsPerEngineer, 'f', -1, 64)
	out["team.parallel"] = strconv.FormatBool(c.Team.Parallel)
	out["team.max_rework"] = strconv.Itoa(c.Team.MaxRework)
	out["team.max_tickets"] = strconv.Itoa(c.Team.MaxTickets)
	out["limits.total"] = c.Limits.Total.String()
	out["limits.refine"] = c.Limits.Refine.String()
	out["limits.plan"] = c.Limits.Plan.String()
	out["limits.build"] = c.Limits.Build.String()
	out["limits.review"] = c.Limits.Review.String()
	out["interactive"] = strconv.FormatBool(c.Interactive)
	return out
}

// Sets one setting from text, empty text returning an optional one to its default, refusing an unknown key or a bad value
func (c *Config) Set(key, value string) error {
	value = strings.TrimSpace(value)
	section, field, _ := strings.Cut(key, ".")
	if r := c.Role(section); r != nil {
		switch field {
		case "profile":
			r.Profile = value
		case "model":
			r.Model = value
		case "max_tokens":
			return setCount(&r.MaxTokens, key, value, 0, 0)
		case "max_rounds":
			return setCount(&r.MaxRounds, key, value, 0, 0)
		case "budget":
			return setCount(&r.Budget, key, value, 0, 0)
		case "append":
			r.Append = value
		case "tools":
			r.Tools = list(value)
		case "deny":
			r.Deny = list(value)
		default:
			return fmt.Errorf("unknown setting %s", key)
		}
		return nil
	}
	defaults := Default()
	switch key {
	case "team.max_engineers":
		return setCount(&c.Team.MaxEngineers, key, value, defaults.Team.MaxEngineers, 1)
	case "team.tickets_per_engineer":
		if value == "" {
			c.Team.TicketsPerEngineer = defaults.Team.TicketsPerEngineer
			return nil
		}
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || !(f > 0) || math.IsInf(f, 0) {
			return fmt.Errorf("%s must be a number above 0, got %q", key, value)
		}
		c.Team.TicketsPerEngineer = f
	case "team.parallel":
		return setFlag(&c.Team.Parallel, key, value, defaults.Team.Parallel)
	case "team.max_rework":
		return setCount(&c.Team.MaxRework, key, value, defaults.Team.MaxRework, 0)
	case "team.max_tickets":
		return setCount(&c.Team.MaxTickets, key, value, defaults.Team.MaxTickets, 1)
	case "limits.total":
		return setDuration(&c.Limits.Total, key, value, defaults.Limits.Total)
	case "limits.refine":
		return setDuration(&c.Limits.Refine, key, value, defaults.Limits.Refine)
	case "limits.plan":
		return setDuration(&c.Limits.Plan, key, value, defaults.Limits.Plan)
	case "limits.build":
		return setDuration(&c.Limits.Build, key, value, defaults.Limits.Build)
	case "limits.review":
		return setDuration(&c.Limits.Review, key, value, defaults.Limits.Review)
	case "interactive":
		return setFlag(&c.Interactive, key, value, defaults.Interactive)
	default:
		return fmt.Errorf("unknown setting %s", key)
	}
	return nil
}

func setCount(into *int, key, value string, def, floor int) error {
	if value == "" {
		*into = def
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < floor {
		return fmt.Errorf("%s must be a whole number of at least %d, got %q", key, floor, value)
	}
	*into = n
	return nil
}

func setFlag(into *bool, key, value string, def bool) error {
	if value == "" {
		*into = def
		return nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s must be true or false, got %q", key, value)
	}
	*into = b
	return nil
}

func setDuration(into *Duration, key, value string, def Duration) error {
	if value == "" {
		*into = def
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fmt.Errorf("%s must be a duration above 0 such as 30m, got %q", key, value)
	}
	*into = Duration(d)
	return nil
}

func optional(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// Names from comma separated text, blanks dropped
func list(text string) []string {
	var out []string
	for _, s := range strings.Split(text, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func contains(names []string, name string) bool {
	for _, have := range names {
		if have == name {
			return true
		}
	}
	return false
}

// File the settings live in, under the user configuration directory
func Path() (string, error) {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(ConfigFile)), nil
}

// Reads the settings file over the defaults, the defaults alone when there is no file
func Load() (Config, error) {
	c := Default()
	path, err := Path()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Writes the settings file, readable by the owner alone
func Save(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

var (
	cfgMu  sync.RWMutex
	cfg    Config
	cfgErr error
	loaded bool
)

// Settings in force, read from the file on first use and the defaults when the file cannot be read
func Current() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if !loaded {
		cfg, cfgErr = Load()
		loaded = true
	}
	return cfg
}

// Error of the first read of the settings file, nil when it read or was absent
func LoadError() error {
	Current()
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfgErr
}

// Saves the settings, makes them the ones in force and points the kinds of every role at them
func Apply(app *harness.App, c Config) error {
	if err := Save(c); err != nil {
		return err
	}
	cfgMu.Lock()
	cfg, cfgErr, loaded = c, nil, true
	cfgMu.Unlock()
	if app == nil {
		return nil
	}
	for _, k := range c.Kinds() {
		if err := app.PutKind(k); err != nil {
			return err
		}
	}
	return nil
}
