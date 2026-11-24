package tuiserver_test

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Installs the prefill readers and the error surface cmd/icebath/wiring.go gives production before any case of the binary runs
func init() {
	gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = app.ResolvePrefillMessagesFile, app.LoadPrefillMessages
	gateway.ErrorSurfaceFromResult = agent.BuildErrorSurfaceFromResult
}
