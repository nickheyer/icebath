// OpenAI Responses API wire protocol with paths relative to the api.openai.com/v1 base URL
package openairesponses

import "github.com/nickheyer/icebath/pkg/protocol"

type Protocol struct{}

func (Protocol) ID() protocol.ID { return protocol.OpenAIResponses }

func (Protocol) Capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		Thinking:        true,
		Images:          true,
		ImagesInResults: true,
		Documents:       true,
		ToolChoiceNone:  true,
		ToolChoiceNamed: true,
		ServerTools:     true,
		Citations:       true,
	}
}

func init() { protocol.Register(Protocol{}) }

var _ protocol.Protocol = Protocol{}
