package commands

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
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

// Built in personalities overlaid by root then agent level user definitions, later wins
func AvailablePersonalities(cfg config.Tree) map[string]any {
	merged := map[string]any{}
	for k, v := range BuiltinPersonalities {
		merged[k] = v
	}
	for _, path := range []string{"personalities", "agent.personalities"} {
		user := cfg.Map(path)
		for name, def := range user {
			key := strings.ToLower(strings.TrimSpace(name))
			if key != "" && !NeutralPersonalityNames[key] {
				merged[key] = def
			}
		}
	}
	return merged
}
