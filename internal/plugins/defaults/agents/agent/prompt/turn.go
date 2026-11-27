package prompt

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/prompt"
)

// Names the profile the session runs on, the folder it reads and writes, and where the other profiles live
func activeProfile(env *prompt.Env) string {
	settings := env.Config
	name := settings.Profile()
	return fmt.Sprintf("Active Icebath profile: %s. This session reads and writes %s/. Other profiles (if any) live under %s/<name>/, "+
		"each with its own settings and SOUL.md that affect a different session than this one. Do NOT modify another profile's "+
		"files unless the user explicitly directs you to.", name, settings.ProfileDir(name), filepath.Dir(settings.ProfileDir(name)))
}

// The day the conversation started in the configured time zone, the day it is now when the prompt is built on a later one, and what
// the session runs as: its id when it is passed, the model, the provider and the surface
func timestamp(env *prompt.Env) string {
	now := time.Now().Local()
	location, err := env.Config.Zone()
	if err != nil {
		env.Notify("⚠ " + strings.TrimPrefix(err.Error(), "⚠ ") + " This computer's time zone is used instead.")
	} else if location != nil {
		now = now.In(location)
	}
	var bits []string
	if location != nil {
		bits = append(bits, location.String())
	}
	if abbreviation := now.Format("MST"); abbreviation != "" && (location == nil || abbreviation != location.String()) {
		bits = append(bits, abbreviation)
	}
	offset := now.Format("-0700")
	bits = append(bits, "UTC"+offset[:3]+":"+offset[3:])
	started := now
	if !env.Started.IsZero() {
		started = env.Started.In(now.Location())
	}
	const day = "Monday, January 02, 2006"
	line := "Conversation started: " + started.Format(day) + " (" + strings.Join(bits, ", ") + ")"
	if now.Format(time.DateOnly) != started.Format(time.DateOnly) {
		line += "\nToday's date (as of the last context rebuild): " + now.Format(day) +
			" — trust this over the start date for what day it is now; query tools for exact time."
	}
	session := ""
	if env.Config.GetBool(agentconfig.PassSessionID) {
		session = env.SessionID
	}
	for _, label := range [][2]string{{"Session ID", session}, {"Model", env.Model}, {"Provider", env.Provider}, {"Platform", env.Platform}} {
		if label[1] != "" {
			line += "\n" + label[0] + ": " + label[1]
		}
	}
	return line
}
