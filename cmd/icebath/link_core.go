package main

// Links the core packages whose init registers a hook or a bundled plugin
import (
	"context"

	_ "github.com/nickheyer/icebath/pkg/builtin/delegationdefault"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/plugins"
	_ "github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Steps the plugins and extras link files add to the core process, each slice run in link order
var (
	// Argv entries that own the whole process before anything else runs, true with the exit code when one ran
	tierArgvEntries []func(args []string) (int, bool)
	// Steps run on the raw argv before the process starts
	tierBeforeStart []func(args []string)
	// Hidden child entries of the plugins tier dispatched right after the scratch prune entry
	childEntriesAfterPrunePlugins []func(ctx context.Context)
	// Hidden child entries of the extras tier dispatched after those of the plugins tier
	childEntriesAfterPruneExtras []func(ctx context.Context)
	// Hidden child entries dispatched after the systemd stop mark entry
	childEntriesAfterStopMark []func(ctx context.Context)
	// Hidden child entries dispatched after the Codex tools MCP server entry
	childEntriesAfterCodex []func(ctx context.Context)
	// Hidden child entries dispatched after every core entry
	childEntriesLast []func(ctx context.Context)
	// Hooks a tier installs into the lower packages
	tierWiring []func()
	// CLI handlers of a tier bound onto the command tree
	tierCLI []func(cli *commands.CLI)
	// Subsystem teardown run at exit before the session host stops
	tierAtExit []func()
	// Exporter flushes run at exit once every other hook finished
	tierFlushAtExit []func()
	// Steps run before a chat launch reads its environment
	tierChatLaunch []func()
	// Collaborators a tier adds to the services pkg/plugins reaches
	tierServices []func(s *plugins.Services)
	// Plugin SDK operations a tier serves, handled false for an operation it does not own
	tierPluginSDK []func(ctx context.Context, h, op string, args []any, kwargs map[string]any) (any, bool, error)
)

func init() { linktier.Link(linktier.Core) }

// Runs the first tier argv entry that owns the process, true with its exit code
func runTierArgvEntries(args []string) (int, bool) {
	for _, entry := range tierArgvEntries {
		if code, ok := entry(args); ok {
			return code, true
		}
	}
	return 0, false
}
