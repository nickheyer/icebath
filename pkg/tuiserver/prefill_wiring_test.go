package tuiserver_test

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Installs the prefill readers cmd/icebath/wiring.go gives production before any case of the binary runs
func init() {
	tuiserver.Prefill.ResolveFile, tuiserver.Prefill.Load = app.ResolvePrefillMessagesFile, app.LoadPrefillMessages
	gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = app.ResolvePrefillMessagesFile, app.LoadPrefillMessages
	gateway.ErrorSurfaceFromResult = agent.BuildErrorSurfaceFromResult
}
