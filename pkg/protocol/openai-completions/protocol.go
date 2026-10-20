// OpenAI Chat Completions wire protocol with paths relative to a base URL ending in /v1
package openaicompletions

import "github.com/nickheyer/icebath/pkg/protocol"

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.OpenAICompletions }
