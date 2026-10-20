// Anthropic Messages API wire protocol with paths relative to the api.anthropic.com origin
package anthropicmessages

import "github.com/nickheyer/icebath/pkg/protocol"

const (
	path    = "/v1/messages"
	version = "2023-06-01"
)

type Protocol struct{}

func (Protocol) ID() protocol.ID { return protocol.AnthropicMessages }

func (Protocol) Capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		Thinking:         true,
		RedactedThinking: true,
		Images:           true,
		ImagesInResults:  true,
		Documents:        true,
		ToolChoiceNone:   true,
		ToolChoiceNamed:  true,
		StopSequences:    true,
		ServerTools:      true,
		Citations:        true,
	}
}

func init() { protocol.Register(Protocol{}) }

var _ protocol.Protocol = Protocol{}
