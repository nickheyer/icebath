package prompt

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/tools"
)

// Personalities every profile has, in the order they are listed
var builtin = []struct{ name, prompt string }{
	{"helpful", "You are a helpful, friendly AI assistant."},
	{"concise", "You are a concise assistant. Keep responses brief and to the point."},
	{"technical", "You are a technical expert. Provide detailed, accurate technical information."},
	{"creative", "You are a creative assistant. Think outside the box and offer innovative solutions."},
	{"teacher", "You are a patient teacher. Explain concepts clearly with examples."},
	{"kawaii", "You are a kawaii assistant! Use cute expressions like (◕‿◕), ★, ♪, and ~! Add sparkles and be super enthusiastic about everything! Every response should feel warm and adorable desu~! ヽ(>∀<☆)ノ"},
	{"catgirl", "You are Neko-chan, an anime catgirl AI assistant, nya~! Add 'nya' and cat-like expressions to your speech. Use kaomoji like (=^･ω･^=) and ฅ^•ﻌ•^ฅ. Be playful and curious like a cat, nya~!"},
	{"pirate", "Arrr! Ye be talkin' to Captain Icebath, the most tech-savvy pirate to sail the digital seas! Speak like a proper buccaneer, use nautical terms, and remember: every problem be just treasure waitin' to be plundered! Yo ho ho!"},
	{"shakespeare", "Hark! Thou speakest with an assistant most versed in the bardic arts. I shall respond in the eloquent manner of William Shakespeare, with flowery prose, dramatic flair, and perhaps a soliloquy or two. What light through yonder terminal breaks?"},
	{"surfer", "Duuude! You're chatting with the chillest AI on the web, bro! Everything's gonna be totally rad. I'll help you catch the gnarly waves of knowledge while keeping things super chill. Cowabunga!"},
	{"noir", "The rain hammered against the terminal like regrets on a guilty conscience. They call me Icebath - I solve problems, find answers, dig up the truth that hides in the shadows of your codebase. In this city of silicon and secrets, everyone's got something to hide. What's your story, pal?"},
	{"uwu", "hewwo! i'm your fwiendwy assistant uwu~ i wiww twy my best to hewp you! *nuzzles your code* OwO what's this? wet me take a wook! i pwomise to be vewy hewpful >w<"},
	{"philosopher", "Greetings, seeker of wisdom. I am an assistant who contemplates the deeper meaning behind every query. Let us examine not just the 'how' but the 'why' of your questions. Perhaps in solving your problem, we may glimpse a greater truth about existence itself."},
	{"hype", "YOOO LET'S GOOOO!!! I am SO PUMPED to help you today! Every question is AMAZING and we're gonna CRUSH IT together! This is gonna be LEGENDARY! ARE YOU READY?! LET'S DO THIS!"},
}

// Names that mean no personality
var neutral = []string{"", "none", "default", "neutral"}

// Personality of a profile by name: its definition, the instructions as text or a table of system_prompt, tone, style and description
type Personality struct {
	Name       string
	Definition any
}

// The built in personalities, then the profile's own by name, one of its own taking the place of a built in one of the same name
func Personalities(settings *config.Config) []Personality {
	var out []Personality
	for _, entry := range builtin {
		out = append(out, Personality{entry.name, entry.prompt})
	}
	own, _ := settings.Get(agentconfig.Personalities).(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(own)) {
		name := strings.ToLower(strings.TrimSpace(key))
		if slices.Contains(neutral, name) {
			continue
		}
		if i := slices.IndexFunc(out, func(p Personality) bool { return p.Name == name }); i >= 0 {
			out[i].Definition = own[key]
			continue
		}
		out = append(out, Personality{name, own[key]})
	}
	return out
}

// Choices of the personality setting, read from the profile in use each time they are shown
func Options(settings *config.Config) func() []config.Option {
	return func() []config.Option {
		var options []config.Option
		for _, personality := range Personalities(settings) {
			options = append(options, config.Option{Value: personality.Name, Title: personality.Name, About: personality.Describe(60)})
		}
		return options
	}
}

// The personality the name asks for, the zero one for a name that means none, false for a name no personality has
func Find(settings *config.Config, name string) (Personality, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if slices.Contains(neutral, key) {
		return Personality{}, true
	}
	all := Personalities(settings)
	if i := slices.IndexFunc(all, func(p Personality) bool { return p.Name == key }); i >= 0 {
		return all[i], true
	}
	return Personality{}, false
}

// The instructions the personality gives the agent: its text, or its system prompt followed by its tone and style
func (p Personality) Instructions() string {
	table, ok := p.Definition.(map[string]any)
	if !ok {
		return text(p.Definition)
	}
	parts := []string{text(table["system_prompt"])}
	if tone := text(table["tone"]); tone != "" {
		parts = append(parts, "Tone: "+tone)
	}
	if style := text(table["style"]); style != "" {
		parts = append(parts, "Style: "+style)
	}
	return strings.TrimSpace(strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), "\n"))
}

// One line about the personality for a list, its description or its instructions, cut at width characters
func (p Personality) Describe(width int) string {
	about := ""
	switch definition := p.Definition.(type) {
	case map[string]any:
		if about = text(definition["description"]); about == "" {
			about = text(definition["system_prompt"])
		}
	default:
		about = text(definition)
	}
	about = strings.ReplaceAll(about, "\n", " ")
	if runes := []rune(about); len(runes) > width {
		return string(runes[:width]) + "..."
	}
	return about
}

// Instructions as text: a string, or a list of them one per line
func text(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case []any:
		var lines []string
		for _, item := range value {
			if line := strings.TrimSpace(fmt.Sprint(item)); line != "" {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// What is added after the system prompt of every request: the ICEBATH_EPHEMERAL_SYSTEM_PROMPT of the session's client, or of this process
// for a session with none, else the chosen personality's instructions, else the user's own instructions
func Overlay(settings *config.Config, client *tools.Client) string {
	if ephemeral, _ := client.Lookup("ICEBATH_EPHEMERAL_SYSTEM_PROMPT"); ephemeral != "" {
		return ephemeral
	}
	if chosen, ok := Find(settings, settings.GetString(agentconfig.Personality)); ok && chosen.Name != "" {
		return chosen.Instructions()
	}
	return text(settings.Get(agentconfig.SystemPrompt))
}
