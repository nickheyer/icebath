package commands

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// NeutralPersonalityNames are the names that mean no personality overlay
var NeutralPersonalityNames = map[string]bool{"": true, "none": true, "default": true, "neutral": true}

// BuiltinPersonalities are available on every surface without any config
var BuiltinPersonalities = map[string]string{
	"helpful":     "You are a helpful, friendly AI assistant.",
	"concise":     "You are a concise assistant. Keep responses brief and to the point.",
	"technical":   "You are a technical expert. Provide detailed, accurate technical information.",
	"creative":    "You are a creative assistant. Think outside the box and offer innovative solutions.",
	"teacher":     "You are a patient teacher. Explain concepts clearly with examples.",
	"kawaii":      "You are a kawaii assistant! Use cute expressions like (◕‿◕), ★, ♪, and ~! Add sparkles and be super enthusiastic about everything! Every response should feel warm and adorable desu~! ヽ(>∀<☆)ノ",
	"catgirl":     "You are Neko-chan, an anime catgirl AI assistant, nya~! Add 'nya' and cat-like expressions to your speech. Use kaomoji like (=^･ω･^=) and ฅ^•ﻌ•^ฅ. Be playful and curious like a cat, nya~!",
	"pirate":      "Arrr! Ye be talkin' to Captain Icebath, the most tech-savvy pirate to sail the digital seas! Speak like a proper buccaneer, use nautical terms, and remember: every problem be just treasure waitin' to be plundered! Yo ho ho!",
	"shakespeare": "Hark! Thou speakest with an assistant most versed in the bardic arts. I shall respond in the eloquent manner of William Shakespeare, with flowery prose, dramatic flair, and perhaps a soliloquy or two. What light through yonder terminal breaks?",
	"surfer":      "Duuude! You're chatting with the chillest AI on the web, bro! Everything's gonna be totally rad. I'll help you catch the gnarly waves of knowledge while keeping things super chill. Cowabunga!",
	"noir":        "The rain hammered against the terminal like regrets on a guilty conscience. They call me Icebath - I solve problems, find answers, dig up the truth that hides in the shadows of your codebase. In this city of silicon and secrets, everyone's got something to hide. What's your story, pal?",
	"uwu":         "hewwo! i'm your fwiendwy assistant uwu~ i wiww twy my best to hewp you! *nuzzles your code* OwO what's this? wet me take a wook! i pwomise to be vewy hewpful >w<",
	"philosopher": "Greetings, seeker of wisdom. I am an assistant who contemplates the deeper meaning behind every query. Let us examine not just the 'how' but the 'why' of your questions. Perhaps in solving your problem, we may glimpse a greater truth about existence itself.",
	"hype":        "YOOO LET'S GOOOO!!! I am SO PUMPED to help you today! Every question is AMAZING and we're gonna CRUSH IT together! This is gonna be LEGENDARY! ARE YOU READY?! LET'S DO THIS!",
}

// Normalizes a YAML prompt value of string or list shape to text
func PromptText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case []string:
		var parts []string
		for _, item := range v {
			if s := strings.TrimSpace(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// Renders a string or structured personality definition to prompt text
func RenderPersonalityPrompt(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return PromptText(value)
	}
	parts := []string{fmt.Sprint(m["system_prompt"])}
	if m["system_prompt"] == nil {
		parts[0] = ""
	}
	if tone := strings.TrimSpace(fmt.Sprint(m["tone"])); m["tone"] != nil && tone != "" {
		parts = append(parts, "Tone: "+tone)
	}
	if style := strings.TrimSpace(fmt.Sprint(m["style"])); m["style"] != nil && style != "" {
		parts = append(parts, "Style: "+style)
	}
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// Short single line preview of a personality for list UIs, cut at width runes
func DescribePersonality(value any, width int) string {
	var preview string
	if m, ok := value.(map[string]any); ok {
		if d, _ := m["description"].(string); d != "" {
			preview = d
		} else if sp, ok := m["system_prompt"]; ok && sp != nil {
			preview = fmt.Sprint(sp)
		}
	} else {
		preview = fmt.Sprint(value)
	}
	preview = strings.ReplaceAll(strings.TrimSpace(preview), "\n", " ")
	r := []rune(preview)
	if len(r) > width {
		return string(r[:width]) + "..."
	}
	return preview
}

// Canonical lower case form of a personality name, empty for every neutral spelling
func NormalizePersonalityName(value any) string {
	name := ""
	if value != nil && pyrepr.Truthy(value) {
		name = strings.ToLower(strings.TrimSpace(pyrepr.Str(value)))
	}
	if NeutralPersonalityNames[name] {
		return ""
	}
	return name
}

// UnknownPersonalityError is the ValueError raised for a name no personality carries
type UnknownPersonalityError struct{ Msg string }

func (e *UnknownPersonalityError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *UnknownPersonalityError) PyTypeName() string { return "ValueError" }

// Canonical name and rendered prompt of a requested personality, neutral names resolving to empty
func ResolvePersonality(value any, cfg config.Tree) (string, string, error) {
	name := NormalizePersonalityName(value)
	if name == "" {
		return "", "", nil
	}
	personalities := AvailablePersonalities(cfg)
	def, ok := personalities[name]
	if !ok {
		names := make([]string, 0, len(personalities))
		for n := range personalities {
			names = append(names, "`"+n+"`")
		}
		sort.Strings(names)
		return "", "", &UnknownPersonalityError{Msg: fmt.Sprintf("Unknown personality: `%s`.\n\nAvailable: `none`, %s", strings.TrimSpace(pyrepr.Str(value)), strings.Join(names, ", "))}
	}
	return name, RenderPersonalityPrompt(def), nil
}

// Selected display.personality name when it names a known personality, empty otherwise
func ActivePersonalityName(cfg map[string]any) string {
	tree := config.Tree(cfg)
	name := NormalizePersonalityName(tree.Get("display.personality"))
	if name == "" {
		return ""
	}
	if _, ok := AvailablePersonalities(tree)[name]; ok {
		return name
	}
	return ""
}

// Session overlay prompt, the active personality render or else the user owned agent.system_prompt
func ResolveEphemeralSystemPrompt(cfg map[string]any) string {
	tree := config.Tree(cfg)
	if name := ActivePersonalityName(cfg); name != "" {
		return RenderPersonalityPrompt(AvailablePersonalities(tree)[name])
	}
	return PromptText(tree.Get("agent.system_prompt"))
}

// Writes the canonical name to display.personality of the home config, false on any failure
func PersistPersonality(ctx context.Context, value any) bool {
	name := NormalizePersonalityName(value)
	h := home.Dir(ctx)
	configPath := home.ConfigPath(h)
	if _, err := home.Mkdir(ctx, h); err != nil {
		return false
	}
	if err := config.WriteConfigKey(ctx, configPath, "display.personality", name); err != nil {
		return false
	}
	_ = os.Chmod(configPath, 0o600)
	return true
}

// Built in personalities overlaid by root then agent level user definitions, later wins
func AvailablePersonalities(cfg config.Tree) map[string]any {
	merged := map[string]any{}
	order := make([]string, 0, len(builtinPersonalityOrder))
	for _, name := range builtinPersonalityOrder {
		merged[name] = BuiltinPersonalities[name]
		order = append(order, name)
	}
	for _, path := range []string{"personalities", "agent.personalities"} {
		user := cfg.Map(path)
		for _, name := range config.MappingKeys(user) {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" || NeutralPersonalityNames[key] {
				continue
			}
			if _, present := merged[key]; !present {
				order = append(order, key)
			}
			merged[key] = user[name]
		}
	}
	config.SetMappingKeys(merged, order)
	return merged
}

// Names of a personalities mapping in the order the merge inserted them, builtins first then user entries
func PersonalityNames(personalities map[string]any) []string {
	return config.MappingKeys(personalities)
}

// Builtin personality names in their declaration order
var builtinPersonalityOrder = []string{"helpful", "concise", "technical", "creative", "teacher", "kawaii", "catgirl", "pirate",
	"shakespeare", "surfer", "noir", "uwu", "philosopher", "hype"}
