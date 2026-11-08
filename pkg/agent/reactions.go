package agent

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Affection or gratitude aimed at the agent, the only reaction kind
const Vibe = "vibe"

// Gratitude and love aimed at the agent, hearts and the less than three emoticon but not a broken heart
var vibeRe = mustCompileVibe()

func mustCompileVibe() *regexp2.Regexp {
	re, err := pyre.CompileIgnoreCase(strings.Join([]string{
		`\bgood\s*bot\b`,
		`\bi\s*(?:love|luv)\s*(?:you|u|ya)\b`,
		`\b(?:love|luv)\s*(?:you|u|ya)\b`,
		`\bily(?:sm)?\b`,
		`\bthank\s*(?:you|u)\b`,
		`\b(?:thanks|thx|tysm|ty)\b`,
		`<3+`,
		"[❤♥\U0001F970\U0001F60D\U0001F618\U0001F495\U0001F496\U0001F497\U0001F49E\U0001F49B\U0001F49C\U0001F49A\U0001F499\U0001F493\U0001F498\U0001F49D\U0001FA77]",
	}, "|"))
	if err != nil {
		panic(err)
	}
	return re
}

// Reaction kind the text expresses, empty when none
func detectReaction(text any) string {
	if !pyrepr.Truthy(text) {
		return ""
	}
	s, ok := text.(string)
	if !ok {
		panic(fmt.Sprintf("TypeError: expected string or bytes-like object, got '%s'", pyrepr.TypeName(text)))
	}
	if m, _ := vibeRe.MatchString(s); m {
		return Vibe
	}
	return ""
}
