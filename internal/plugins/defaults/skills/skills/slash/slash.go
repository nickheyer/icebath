package slash

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct {
	config   *config.Config
	skills   *skills.Registry
	sessions *sessions.Registry
	// Names of the commands registered before the skills', which a skill's command never takes
	core []string
}

// Registers the commands of skills, the commands registered before them kept from skills; what says a name is taken
func Register(registry *slash.Registry, cfg *config.Config, skillset *skills.Registry, stores *sessions.Registry) skills.Taken {
	var core []string
	for _, command := range registry.Commands() {
		core = append(core, command.Name)
		core = append(core, command.Aliases...)
	}
	c := &commands{config: cfg, skills: skillset, sessions: stores, core: core}
	registry.Register(c)
	return c.taken
}

// Whether the name is one of the commands of icebath itself or of these
func (c *commands) taken(name string) bool {
	return slices.Contains(c.core, name) || slices.ContainsFunc(c.own(), func(command slash.Command) bool {
		return command.Name == name || slices.Contains(command.Aliases, name)
	})
}

func (c *commands) own() []slash.Command {
	return []slash.Command{
		{Name: "reload-skills", Aliases: []string{"reload_skills"}, Help: "Read the skills folders again", Run: c.reload},
		{Name: "bundles", Help: "List the skill bundles", Run: c.bundles},
		{Name: "learn", Usage: "<what to learn from>", Help: "Make a skill from a folder, a page or this chat", Run: c.learn},
	}
}

// The commands of skills, then the slash command of every skill and bundle of the profile in use as seen from where icebath started
func (c *commands) Commands() []slash.Command {
	out := c.own()
	cwd, _ := os.Getwd()
	scope := c.skills.Scope(c.config, cwd)
	table, err := scope.Slash(c.taken)
	if err != nil {
		table = scope.Commands(c.taken)
	}
	for _, key := range slices.Sorted(maps.Keys(table)) {
		command := table[key]
		help := command.Description
		if len(command.Skills) > 0 {
			bundle := fmt.Sprintf("a bundle of %d skills", len(command.Skills))
			if help = strings.TrimRight(strings.TrimSpace(help), "."); help == "" {
				help = "Load " + bundle
			} else {
				help += ", " + bundle
			}
		}
		out = append(out, slash.Command{Name: key, Usage: "[instruction]", Help: help, Skill: true, Run: func(ctx context.Context, call *slash.Call) (string, error) {
			return c.invoke(ctx, key, call)
		}})
	}
	return out
}

// Scope of a session: its profile and folder
func (c *commands) scope(ctx context.Context, session string) (*skills.Scope, error) {
	store, err := c.sessions.Store(c.config.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, err
	}
	found, err := store.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	scope := c.skills.Scope(c.config.In(found.GetProfile()), found.GetDirectory())
	scope.Session = session
	return scope, scope.Seed(ctx)
}

// Sends the skills of a command, more skills it stacks and the user's instruction to the model as the user's message
func (c *commands) invoke(ctx context.Context, key string, call *slash.Call) (string, error) {
	scope, err := c.scope(ctx, call.SessionID)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace("/" + key + " " + call.Argument)
	invocation, ok, err := scope.Invoke(ctx, line, c.taken)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("No skill or bundle named /%s is available in this session.", key)
	}
	if err := c.skills.Submit(ctx, call.SessionID, invocation.Message, line); err != nil {
		return "", err
	}
	if len(invocation.Missing) > 0 {
		return "These skills are missing, so they were left out: " + strings.Join(invocation.Missing, ", ") + ".", nil
	}
	return "", nil
}

// Count with its verb: 1 skill command is, 2 skill commands are
func agreeing(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strs.Number(float64(n)) + " " + many
}

func (c *commands) reload(ctx context.Context, call *slash.Call) (string, error) {
	scope, err := c.scope(ctx, call.SessionID)
	if err != nil {
		return "", err
	}
	reloaded, err := scope.Reload(c.taken)
	if err != nil {
		return "", fmt.Errorf("The skills could not be read again: %s", strings.TrimSuffix(err.Error(), ".")+".")
	}
	lines := []string{"**Read the skills again**", ""}
	if len(reloaded.Added) == 0 && len(reloaded.Removed) == 0 {
		lines = append(lines, "No skill was added or removed.")
	}
	for _, group := range []struct {
		header   string
		commands []skills.Command
	}{{"**Added skills:**", reloaded.Added}, {"**Removed skills:**", reloaded.Removed}} {
		if len(group.commands) == 0 {
			continue
		}
		lines = append(lines, group.header)
		for _, command := range group.commands {
			if command.Description != "" {
				lines = append(lines, fmt.Sprintf("    - %s: %s", command.Key, command.Description))
			} else {
				lines = append(lines, "    - "+command.Key)
			}
		}
	}
	lines = append(lines, "", fmt.Sprintf("%s available.", agreeing(reloaded.Total, "skill command is", "skill commands are")))
	return strings.Join(lines, "\n"), nil
}

func (c *commands) bundles(ctx context.Context, call *slash.Call) (string, error) {
	scope, err := c.scope(ctx, call.SessionID)
	if err != nil {
		return "", err
	}
	bundles, err := scope.BundleList()
	if err != nil {
		return "", fmt.Errorf("The skill bundles can't be read: %s", strings.TrimSuffix(err.Error(), ".")+".")
	}
	if len(bundles) == 0 {
		return fmt.Sprintf("You have no skill bundles yet. Make one with:\n  `icebath bundles create <name> --skill skill1 --skill skill2`\nBundles are kept in `%s`.", scope.BundlesDir()), nil
	}
	lines := []string{fmt.Sprintf("**%s:**", strs.Count(len(bundles), "skill bundle"))}
	for _, bundle := range bundles {
		lines = append(lines, fmt.Sprintf("- `/%s`: %s, %s", bundle.Slug, cmp.Or(bundle.Description, "No description"), strs.Count(len(bundle.Skills), "skill")))
	}
	return strings.Join(append(lines, "", "Type the command of a bundle to load all its skills."), "\n"), nil
}

func (c *commands) learn(ctx context.Context, call *slash.Call) (string, error) {
	return "", c.skills.Submit(ctx, call.SessionID, skills.Learn(call.Argument), strings.TrimSpace("/learn "+call.Argument))
}
