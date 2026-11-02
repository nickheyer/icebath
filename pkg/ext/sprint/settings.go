package sprint

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Sheet of every sprint setting under its section, each saved through the app the moment it changes
func newSettingsCard(env *cards.Env) wm.Card {
	return cards.NewSettings(env, cards.SettingsSpec{
		Title:  "SPRINT SETTINGS",
		Key:    SettingsKey,
		Glyph:  env.Theme.Glyphs.Settings,
		Status: func() string { return "saved to the app configuration" },
		Fields: settingRows(env),
		Values: func() map[string]string {
			if env.Settings == nil {
				return map[string]string{}
			}
			return env.Settings()
		},
		Choices: func() map[string][]string {
			out := map[string][]string{}
			for _, role := range Roles {
				out[roleKey(role, "profile")] = profileChoices(env)
			}
			return out
		},
		Apply: func(key, value string) tea.Cmd {
			return func() tea.Msg { return cards.SettingMsg{Key: key, Value: value, Quiet: true} }
		},
	})
}

// Rows of the sheet, every registered setting under the group its key names, with labels and kinds fit for editing
func settingRows(env *cards.Env) []cards.Field {
	tokens := func(v float64) string { return render.Tokens(int(v)) }
	var out []cards.Field
	for _, s := range Settings() {
		rest := strings.TrimPrefix(s.Key, Name+".")
		group, field, _ := strings.Cut(rest, ".")
		if field == "" {
			group, field = "general", group
		}
		f := cards.Field{Key: s.Key, Label: strings.ReplaceAll(field, "_", " "), Group: group, Help: s.Help, Optional: true, Check: s.Check}
		switch {
		case len(s.Choices) > 0:
			f.Kind, f.Choices = cards.FieldChoice, s.Choices
		case s.Number():
			f.Kind, f.Lo, f.Hi, f.Step, f.Integer = cards.FieldNumber, s.Lo, s.Hi, s.Step, s.Integer
			f.Start = numberStart(s.Key)
			if strings.HasSuffix(s.Key, "tokens") || strings.HasSuffix(s.Key, "budget") {
				f.Unit = tokens
			}
		case strings.HasSuffix(field, "profile"):
			f.Kind, f.Choices = cards.FieldChoice, profileChoices(env)
		default:
			f.Kind = cards.FieldText
			f.Placeholder = placeholder(env, s.Key)
		}
		out = append(out, f)
	}
	return out
}

// Value a number slider starts at while unset
func numberStart(key string) float64 {
	d := Default()
	switch {
	case strings.HasSuffix(key, "max_tokens"):
		return 8192
	case strings.HasSuffix(key, "max_rounds"):
		for _, role := range Roles {
			if key == roleKey(role, "max_rounds") && d.Roles[role].MaxRounds > 0 {
				return float64(d.Roles[role].MaxRounds)
			}
		}
		return 50
	case strings.HasSuffix(key, "budget"):
		return 200000
	case strings.HasSuffix(key, "team.builders"):
		return float64(d.Team.Builders)
	case strings.HasSuffix(key, "team.max_rework"):
		return float64(d.Team.MaxRework)
	case strings.HasSuffix(key, "team.max_tickets"):
		return float64(d.Team.MaxTickets)
	case strings.HasSuffix(key, "arena.fps"):
		return float64(d.Arena.FPS)
	}
	return 1
}

// Default shown in a text field while it is unset
func placeholder(env *cards.Env, key string) string {
	d := Default()
	switch {
	case strings.HasSuffix(key, "limits.total"):
		return durationText(d.Limits.Total)
	case strings.HasSuffix(key, "limits.brief"):
		return durationText(d.Limits.Brief)
	case strings.HasSuffix(key, "limits.plan"):
		return durationText(d.Limits.Plan)
	case strings.HasSuffix(key, "limits.build"):
		return durationText(d.Limits.Build)
	case strings.HasSuffix(key, "limits.review"):
		return durationText(d.Limits.Review)
	case strings.HasSuffix(key, ".model"):
		return modelPlaceholder(env, key)
	}
	for _, role := range Roles {
		switch key {
		case Name + ".look." + string(role):
			return looksText(d.Looks[role])
		case Name + ".name." + string(role):
			return strings.Join(d.Names[role], ", ")
		}
	}
	return ""
}

// Model a blank setting resolves to, the app default on the default profile, else the first listed
func modelPlaceholder(env *cards.Env, key string) string {
	profile := ""
	for _, role := range Roles {
		if key == roleKey(role, "model") && env.Settings != nil {
			profile = strings.TrimSpace(env.Settings()[roleKey(role, "profile")])
		}
	}
	if env.Editor == nil {
		return "the default model of the profile"
	}
	primary := env.Editor.Primary
	if (profile == "" || profile == primary.Profile.Name) && primary.Model != "" {
		return "default, " + primary.Model
	}
	if p, ok := env.Editor.FindProfile(profile); ok && p.Model() != "" {
		return "default, " + p.Model()
	}
	return "default, the first model the endpoint lists"
}

// Default first, then every profile switched on
func profileChoices(env *cards.Env) []string {
	out := []string{""}
	if env.Editor != nil {
		for _, in := range env.Editor.Instances() {
			if !in.Profile.Disabled {
				out = append(out, in.Profile.Name)
			}
		}
	}
	return out
}
