package sprint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Sheet of every sprint setting, each saved to the settings file and put in force the moment it changes
func newSettingsCard(env *cards.Env) wm.Card {
	warned := false
	return cards.NewSettings(env, cards.SettingsSpec{
		Title:  "SPRINT SETTINGS",
		Key:    "ext:sprint:settings",
		Glyph:  env.Theme.Glyphs.Settings,
		Status: settingsFile,
		Fields: settingRows(env),
		Values: func() map[string]string { return Current().Values() },
		Choices: func() map[string][]string {
			c := Current()
			out := map[string][]string{}
			for _, role := range Roles {
				out[role+".profile"] = profileChoices(env, c.Role(role).Profile)
			}
			return out
		},
		Apply: func(key, value string) tea.Cmd {
			c := Current()
			if err := c.Set(key, value); err != nil {
				return wm.Notify(err.Error(), wm.Warn)
			}
			if err := Apply(env.Editor, c); err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			return nil
		},
		Init: func() tea.Cmd {
			err := LoadError()
			if err == nil || warned {
				return nil
			}
			warned = true
			return wm.Notify(err.Error(), wm.Warn)
		},
	})
}

// Fields of every role, then the team, the time limits and the general switches
func settingRows(env *cards.Env) []cards.Field {
	tokens := func(v float64) string { return render.Tokens(int(v)) }
	c, d := Current(), Default()
	var out []cards.Field
	for _, role := range Roles {
		defaults := "every tool"
		if tools := DefaultTools(role); tools != nil {
			defaults = strings.Join(tools, ", ")
		}
		out = append(out,
			cards.Field{Key: role + ".profile", Label: "profile", Group: role, Kind: cards.FieldChoice, Choices: profileChoices(env, c.Role(role).Profile), Optional: true, Help: "Endpoint profile the role runs on, the default endpoint when empty"},
			cards.Field{Key: role + ".model", Label: "model", Group: role, Kind: cards.FieldText, Optional: true, Placeholder: "profile default", Help: "Model on that profile, its default when empty"},
			cards.Field{Key: role + ".max_tokens", Label: "max tokens", Group: role, Kind: cards.FieldNumber, Integer: true, Optional: true, Lo: 1, Step: 1024, Start: 8192, Unit: tokens, Help: "Output tokens one turn may produce, the app setting when unset"},
			cards.Field{Key: role + ".max_rounds", Label: "max rounds", Group: role, Kind: cards.FieldNumber, Integer: true, Optional: true, Lo: 1, Step: 1, Start: 50, Help: "Tool rounds one run may take, the app setting when unset"},
			cards.Field{Key: role + ".budget", Label: "budget", Group: role, Kind: cards.FieldNumber, Integer: true, Optional: true, Lo: 1000, Step: 10000, Start: 200000, Unit: tokens, Help: "Tokens the role may spend over one sprint across every turn, unlimited when unset"},
			cards.Field{Key: role + ".append", Label: "append", Group: role, Kind: cards.FieldArea, Optional: true, Help: "Text added to the instructions of the role"},
			cards.Field{Key: role + ".tools", Label: "tools", Group: role, Kind: cards.FieldText, Optional: true, Help: "Comma separated tools the role is offered, when empty it gets " + defaults},
			cards.Field{Key: role + ".deny", Label: "deny", Group: role, Kind: cards.FieldText, Optional: true, Help: "Comma separated tools withheld beyond the defaults of the role"},
		)
	}
	out = append(out,
		cards.Field{Key: "team.max_engineers", Label: "max engineers", Group: "team", Kind: cards.FieldNumber, Integer: true, Lo: 1, Step: 1, Start: float64(d.Team.MaxEngineers), Help: "Most engineers the team brings in, however many tickets there are"},
		cards.Field{Key: "team.tickets_per_engineer", Label: "tickets per engineer", Group: "team", Kind: cards.FieldNumber, Lo: 0.25, Step: 0.25, Start: d.Team.TicketsPerEngineer, Help: "Tickets one engineer takes, the count of engineers following from it until the maximum"},
		cards.Field{Key: "team.parallel", Label: "parallel", Group: "team", Kind: cards.FieldChoice, Choices: []string{"true", "false"}, Help: "Engineers work at the same time rather than one after another"},
		cards.Field{Key: "team.max_rework", Label: "max rework", Group: "team", Kind: cards.FieldNumber, Integer: true, Lo: 0, Step: 1, Start: float64(d.Team.MaxRework), Help: "Review rounds that may send tickets back to their engineer"},
		cards.Field{Key: "team.max_tickets", Label: "max tickets", Group: "team", Kind: cards.FieldNumber, Integer: true, Lo: 1, Step: 1, Start: float64(d.Team.MaxTickets), Help: "Tickets the planner may cut a story into"},
		cards.Field{Key: "limits.total", Label: "total", Group: "limits", Kind: cards.FieldText, Placeholder: "3h", Check: durationCheck("total"), Help: "Time the whole sprint may take, every stage included"},
		cards.Field{Key: "limits.refine", Label: "refine", Group: "limits", Kind: cards.FieldText, Placeholder: "1h", Check: durationCheck("refine"), Help: "Time the project manager has to settle the objective, and again to report back"},
		cards.Field{Key: "limits.plan", Label: "plan", Group: "limits", Kind: cards.FieldText, Placeholder: "15m", Check: durationCheck("plan"), Help: "Time the planner has to size the story and cut it into tickets"},
		cards.Field{Key: "limits.build", Label: "build", Group: "limits", Kind: cards.FieldText, Placeholder: "90m", Check: durationCheck("build"), Help: "Time the engineers have for each build round"},
		cards.Field{Key: "limits.review", Label: "review", Group: "limits", Kind: cards.FieldText, Placeholder: "30m", Check: durationCheck("review"), Help: "Time the lead has for each review round"},
		cards.Field{Key: "interactive", Label: "interactive", Group: "general", Kind: cards.FieldChoice, Choices: []string{"true", "false"}, Help: "The project manager talks with the person before handing off, resolving the prompt alone when off"},
	)
	return out
}

// Default first, then every profile switched on, then the one set when it is neither
func profileChoices(env *cards.Env, set string) []string {
	out := []string{""}
	if env.Editor != nil {
		for _, in := range env.Editor.Instances() {
			if !in.Profile.Disabled {
				out = append(out, in.Profile.Name)
			}
		}
	}
	if !contains(out, set) {
		out = append(out, set)
	}
	return out
}

// Refuses text that is not a duration above 0, naming the limit in the reason
func durationCheck(name string) func(string) error {
	return func(v string) error {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return fmt.Errorf("%s must be a duration above 0 such as 30m, got %q", name, v)
		}
		return nil
	}
}

// Where the settings live with the home directory as a tilde, or why that cannot be found
func settingsFile() string {
	path, err := Path()
	if err != nil {
		return err.Error()
	}
	return tildeHome(path)
}

// Path with the home directory written as a tilde when it lies under it
func tildeHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}
