// Gemini generateContent wire protocol with paths relative to the generativelanguage.googleapis.com origin
package googlegenerativeai

import "github.com/nickheyer/icebath/pkg/protocol"

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.GoogleGenerativeAI }

func (Protocol) Capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		Thinking:        true,
		Images:          true,
		ImagesInResults: true,
		Documents:       true,
		ToolChoiceNone:  true,
		ToolChoiceNamed: true,
		StopSequences:   true,
		ServerTools:     true,
		Citations:       true,
	}
}
