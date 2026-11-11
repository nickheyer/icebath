package gatewayrun_test

import (
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
)

// Installs the turn path hooks cmd/icebath wiring assigns at startup with the same production functions
func init() {
	gatewayrun.ResolveEphemeralSystemPromptFromConfig = commands.ResolveEphemeralSystemPrompt
	gatewayrun.ParseServiceTierConfig = app.ParseServiceTierConfig
	gatewayrun.ResolveCommand = func(name string) (string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, true
		}
		return "", false
	}
}
