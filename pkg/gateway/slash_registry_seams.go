package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Slash command row of the command registry or a plugin with its description and args hint
type SlashCommandEntry struct{ Name, Description, ArgsHint string }

// Skill autocomplete row with its description and the command key it runs
type SkillCommandEntry struct{ Name, Description, CmdKey string }

// Live adapters of one profile keyed by platform
type AdapterMap = pydict.Dict[Platform, PlatformAdapter]

// Gateway available registry commands in registry order, assigned by the binary to the commands package
var GatewayRegistryCommands func(ctx context.Context) ([]SlashCommandEntry, error)

// Plugin registered slash commands, assigned by the binary to the commands package
var PluginSlashCommandEntries func(ctx context.Context) ([]SlashCommandEntry, error)

// Discord skill rows grouped by category, the uncategorized rows and the clamp collision count
var DiscordSkillCommandsByCategory func(ctx context.Context, reserved map[string]bool) ([][]SkillCommandEntry, []SkillCommandEntry, int, error)

// Adapter map and gateway config of the profile owning an adapter, assigned by the binary to the runner
var ProfileAdaptersAndConfig func(ctx context.Context, runner any, profile string) (*AdapterMap, *GatewayConfig, error)
