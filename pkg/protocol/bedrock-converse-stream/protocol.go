// Amazon Bedrock Converse and ConverseStream wire protocol, paths relative to the regional bedrock-runtime host
package bedrockconversestream

import "github.com/nickheyer/icebath/pkg/protocol"

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.BedrockConverseStream }

func (Protocol) Capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		Thinking:         true,
		RedactedThinking: true,
		Images:           true,
		ImagesInResults:  true,
		Documents:        true,
		ToolChoiceNamed:  true,
		StopSequences:    true,
		Citations:        true,
		Caching:          true,
	}
}
