package testfakes

import "github.com/nickheyer/icebath/pkg/internal/fakellm"

// Model id every scripted completion of the loopback provider reports
const ModelID = fakellm.ModelID

// Responses, records and the server of the scripted loopback provider, the same types the packages under pkg use
type (
	Text           = fakellm.Text
	Call           = fakellm.Call
	ToolCall       = fakellm.ToolCall
	Error          = fakellm.Error
	Hang           = fakellm.Hang
	DropMidStream  = fakellm.DropMidStream
	StallMidStream = fakellm.StallMidStream
	Raw            = fakellm.Raw
	Request        = fakellm.Request
	Responder      = fakellm.Responder
	Options        = fakellm.Options
	Server         = fakellm.Server
)

// Starts a provider answering main turns from script in order, then with the default text
func NewFakeLLM(script []any, opts Options) (*Server, error) { return fakellm.New(script, opts) }

// Starts a provider whose every main turn is answered by responder
func NewFakeLLMResponder(responder Responder, opts Options) (*Server, error) {
	return fakellm.NewResponder(responder, opts)
}

// Writes a minimal config.yaml and .env routing the main model of a home to baseURL
func WriteFakeLLMHome(home, baseURL, apiKey, extraConfig string, offerVersion int) error {
	return fakellm.WriteHome(home, baseURL, apiKey, extraConfig, offerVersion)
}
