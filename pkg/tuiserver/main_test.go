package tuiserver

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	runTestChild()
	DispatchComputeHostEntry(context.Background())
	hermetic.BlockExternalHTTP()
	state.AdmissionMediaReferences = gateway.AdmissionMediaReferences
	state.HostedDocumentReferences = gateway.HostedDocumentReferences
	state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	tools.ProfileSecrets = provider.ProfileSecretSource()
	NormalizePersonalityName = commands.NormalizePersonalityName
	RenderPersonalityPrompt = commands.RenderPersonalityPrompt
	PersistPersonality = commands.PersistPersonality
	ResolveCommand = func(name string) (ResolvedCommand, bool) {
		d := commands.Resolve(name)
		if d == nil {
			return ResolvedCommand{}, false
		}
		return ResolvedCommand{Name: d.Name, Category: d.Category, Describe: d.Describe,
			DesktopSubcommands: commands.MetaOf(d.Name).DesktopSubcommands}, true
	}
	CommandsByCategory = func() []HelpCategory {
		var out []HelpCategory
		for _, g := range commands.CommandsByCategory(commands.Default) {
			category := HelpCategory{Category: g.Category}
			for _, row := range g.Rows {
				category.Rows = append(category.Rows, [2]string{row.Command, row.Description})
			}
			out = append(out, category)
		}
		return out
	}
	dir, err := os.MkdirTemp("", "tuiserver-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	hermesHome, hermesHomeAtImport = dir, dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Real agent on a dead loopback endpoint with the given route and reasoning config
func testAgent(t *testing.T, o agent.AIAgentOptions) *agent.AIAgent {
	t.Helper()
	if o.BaseURL == "" {
		o.BaseURL = "http://127.0.0.1:9/v1"
	}
	if o.APIKey == "" {
		o.APIKey = "test"
	}
	o.MaxIterations, o.QuietMode, o.SkipContextFiles, o.SkipMemory = 5, true, true, true
	a, err := agent.NewAIAgent(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
