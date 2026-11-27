package prompt

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
)

// Who the agent is when the profile's SOUL.md says nothing, and what a new profile's SOUL.md starts as
const defaultIdentity = provider.Identity

// Where the agent finds out about icebath itself
const helpGuidance = "For help with Icebath Agent itself — configuring, setting up, using, extending, or troubleshooting it — or to " +
	"understand your own features, tools, or capabilities, icebath's own help is the authoritative reference: `icebath --help` and " +
	"`icebath <command> --help` for every command and option, `/help` for the slash commands, and `/settings` for every setting. " +
	"Run them yourself when you have a terminal, or point the user there."

// Where the agent finds out about icebath itself when the turn lists the icebath-agent skill
const helpSkillGuidance = "For help with Icebath Agent itself — configuring, setting up, using, extending, or troubleshooting it — or to " +
	"understand your own features, tools, or capabilities, icebath's own help is the authoritative reference: `icebath --help` and " +
	"`icebath <command> --help` for every command and option, `/help` for the slash commands, and `/settings` for every setting. " +
	"The `icebath-agent` skill has the actual commands and proven workflows — load it with skill_view(name='icebath-agent') before " +
	"configuring, modifying, or troubleshooting Icebath so you don't guess or invent workarounds."

const soulFile = "SOUL.md"

// The profile's SOUL.md as the identity, seeded with the default identity when the profile has none, else the default identity. The
// user's own file: what looks like an injection in it is told to the user and loaded anyway
func identity(ctx context.Context, env *prompt.Env) string {
	if env.IgnoreRules {
		return defaultIdentity
	}
	path := filepath.Join(env.Config.ProfileDir(env.Config.Profile()), soulFile)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if info, err := os.Stat(filepath.Dir(path)); err == nil && info.IsDir() {
			if err := os.WriteFile(path, []byte(defaultIdentity), 0o600); err != nil {
				env.Notify("Could not write the default " + soulFile + " to " + path + ": " + err.Error())
			}
		}
	}
	text, ok := read(ctx, env, path)
	if text = strings.TrimSpace(text); !ok || text == "" {
		return defaultIdentity
	}
	if found := prompt.Threats(text, prompt.Context); len(found) > 0 {
		env.Notify("Context file " + soulFile + " matched injection pattern(s) " + strings.Join(found, ", ") + "; loaded anyway because it " +
			"is your own file in the profile folder — review " + path + " if you did not write that text.")
	}
	return truncate(env, text, soulFile, path)
}
